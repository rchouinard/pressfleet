package kubernetes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"

	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
)

// ErrMetricsUnavailable is returned by Stats when the cluster doesn't
// serve metrics.k8s.io (metrics-server isn't installed, or isn't ready).
var ErrMetricsUnavailable = errors.New("kubernetes: the metrics API (metrics.k8s.io) is not available")

// metricsPath is where metrics-server serves pod metrics, by namespace.
const metricsPath = "/apis/metrics.k8s.io/v1beta1/namespaces/"

// cpuState is what the driver remembers of a container to accumulate its
// CPU usage.
type cpuState struct {
	// containerID tells a restarted container, whose total starts again,
	// from the one the state was built for.
	containerID string
	sampled     time.Time
	total       time.Duration
}

// Stats samples the site's running containers (provisioner.StatsReader)
// from metrics.k8s.io, which, unlike the kubelet's summary API, needs no
// access to nodes/proxy.
//
// The metrics API reports CPU as a rate over a short window, but the
// contract asks for cumulative CPU time, so each sample is integrated:
// the rate is held constant between samples of the container, and the
// total starts again when the container does. The total lives in the
// control plane's memory, so it also starts again with the control plane;
// consumers that derive rates from successive samples treat a decrease as
// a reset. Samples are as fresh as metrics-server's (about 15 seconds);
// SampledAt is the time of its scrape, and a repeated scrape returns the
// same values. Memory is the working set, the limit comes from the pod
// spec, and restarts from the container status. Containers that aren't
// running, or that metrics-server hasn't scraped yet, are left out; a
// stopped site has none. A site that doesn't exist yields
// provisioner.ErrSiteNotFound.
func (p *Provisioner) Stats(ctx context.Context, siteID string) ([]provisioner.ComponentStats, error) {
	if _, err := p.deployment(ctx, siteID); err != nil {
		return nil, fmt.Errorf("read stats: %w", err)
	}
	ns := p.SiteNamespace(siteID)
	pods, err := p.typed.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: sitePodSelector})
	if err != nil {
		return nil, fmt.Errorf("read stats of site %s: %w", siteID, err)
	}
	if len(pods.Items) == 0 {
		return nil, nil
	}
	metrics, err := p.listPodMetrics(ctx, ns)
	if err != nil {
		return nil, fmt.Errorf("read stats of site %s: %w", siteID, err)
	}

	p.observe.mu.Lock()
	defer p.observe.mu.Unlock()
	if p.observe.cpu == nil {
		p.observe.cpu = map[string]cpuState{}
	}
	var out []provisioner.ComponentStats
	byAge := sortedByAge(pods.Items)
	for _, c := range []provisioner.Component{provisioner.ComponentPHP, provisioner.ComponentNginx} {
		// During a rollout the old pod and the new one both run; the new
		// one is the site.
		for i := range byAge {
			if cs, ok := p.containerStats(&byAge[i], string(c), metrics, ns); ok {
				out = append(out, cs)
				break
			}
		}
	}
	// Forget containers of pods that are gone, so the map doesn't grow
	// with every rollout.
	live := map[string]bool{}
	for _, pod := range pods.Items {
		for _, ctr := range pod.Spec.Containers {
			live[cpuKey(ns, pod.Name, ctr.Name)] = true
		}
	}
	for k := range p.observe.cpu {
		if strings.HasPrefix(k, ns+"/") && !live[k] {
			delete(p.observe.cpu, k)
		}
	}
	return out, nil
}

func cpuKey(ns, pod, container string) string { return ns + "/" + pod + "/" + container }

// sortedByAge returns the pods newest first.
func sortedByAge(pods []corev1.Pod) []corev1.Pod {
	out := slices.Clone(pods)
	slices.SortStableFunc(out, func(a, b corev1.Pod) int {
		return b.CreationTimestamp.Compare(a.CreationTimestamp.Time)
	})
	return out
}

// containerStats samples one container of a pod, or reports false if it
// isn't running or has no metrics yet. The caller holds observe.mu.
func (p *Provisioner) containerStats(pod *corev1.Pod, name string, metrics []podMetrics, ns string) (provisioner.ComponentStats, bool) {
	var spec *corev1.Container
	for i := range pod.Spec.Containers {
		if pod.Spec.Containers[i].Name == name {
			spec = &pod.Spec.Containers[i]
		}
	}
	var status *corev1.ContainerStatus
	for i := range pod.Status.ContainerStatuses {
		if pod.Status.ContainerStatuses[i].Name == name {
			status = &pod.Status.ContainerStatuses[i]
		}
	}
	if spec == nil || status == nil || status.State.Running == nil {
		return provisioner.ComponentStats{}, false
	}
	var pm *podMetrics
	var usage corev1.ResourceList
	for i := range metrics {
		if metrics[i].Metadata.Name != pod.Name {
			continue
		}
		for _, mc := range metrics[i].Containers {
			if mc.Name == name {
				pm, usage = &metrics[i], mc.Usage
			}
		}
	}
	if pm == nil {
		return provisioner.ComponentStats{}, false
	}

	cs := provisioner.ComponentStats{
		Component:   provisioner.Component(name),
		MemoryBytes: uint64(max(usage.Memory().Value(), 0)),
		Restarts:    int(status.RestartCount),
		SampledAt:   pm.Timestamp.Time,
	}
	if lim := spec.Resources.Limits.Memory(); lim != nil {
		cs.MemoryLimitBytes = uint64(max(lim.Value(), 0))
	}

	key := cpuKey(ns, pod.Name, name)
	st, known := p.observe.cpu[key]
	cores := usage.Cpu().AsApproximateFloat64()
	switch {
	case !known || st.containerID != status.ContainerID:
		// The first sample of this container: the window is all that is
		// known to have run.
		st = cpuState{containerID: status.ContainerID, sampled: pm.Timestamp.Time}
		st.total = time.Duration(cores * float64(pm.Window.Duration))
	case pm.Timestamp.After(st.sampled):
		st.total += time.Duration(cores * float64(pm.Timestamp.Sub(st.sampled)))
		st.sampled = pm.Timestamp.Time
	}
	p.observe.cpu[key] = st
	cs.CPUUsage = st.total
	return cs, true
}

// listPodMetrics returns metrics.k8s.io's metrics of the site's pods.
func (p *Provisioner) listPodMetrics(ctx context.Context, ns string) ([]podMetrics, error) {
	if p.observe.podMetrics != nil {
		return p.observe.podMetrics(ctx, ns)
	}
	rc := p.typed.CoreV1().RESTClient()
	if c, ok := rc.(*rest.RESTClient); rc == nil || (ok && c == nil) {
		// The fake clientset's client is a typed nil.
		return nil, ErrMetricsUnavailable
	}
	raw, err := rc.Get().AbsPath(metricsPath+ns+"/pods").Param("labelSelector", sitePodSelector).Do(ctx).Raw()
	if err != nil {
		// 404: the API group isn't registered; 503: its APIService has no
		// ready backend.
		if apierrors.IsNotFound(err) || apierrors.IsServiceUnavailable(err) {
			return nil, fmt.Errorf("%w: %w", ErrMetricsUnavailable, err)
		}
		return nil, fmt.Errorf("list pod metrics: %w", err)
	}
	var list struct {
		Items []podMetrics `json:"items"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, fmt.Errorf("decode pod metrics: %w", err)
	}
	return list.Items, nil
}
