package kubernetes

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	authv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"

	"github.com/rchouinard/pressfleet/control-plane/internal/config"
	"github.com/rchouinard/pressfleet/control-plane/internal/doctor"
)

// This file is the Kubernetes side of the doctor's cluster checks (#225):
// ClusterProbe implements doctor.ClusterProbe, because client-go is
// confined to this package (§4.2).

// ProbeConfig configures a ClusterProbe.
type ProbeConfig struct {
	Kubernetes config.KubernetesConfig
	// Image runs the probe pods: the control plane's own image
	// (task_image), which has bash and coreutils. Empty makes the probes
	// that need pods fail with doctor.ErrNoProbeImage.
	Image  string
	Logger *slog.Logger
}

// ClusterProbe answers the doctor's questions about the cluster.
type ClusterProbe struct {
	c   *Client
	cfg ProbeConfig
	log *slog.Logger
	// podTimeout bounds how long probe pods get to finish; tests shorten it.
	podTimeout time.Duration
	// settle is how long the denied pod waits for the CNI to program its
	// policy before it connects; tests set it to zero.
	settle int
}

var _ doctor.ClusterProbe = (*ClusterProbe)(nil)

// Labels of the doctor's probe namespaces. They differ from the driver's
// own, so nothing mistakes a probe namespace for a site.
const (
	probeLabel         = "pressfleet.io/doctor-probe"
	probeNamespaceRoot = "-doctor-"
)

// defaultPodTimeout covers pulling the image on a cold node.
const defaultPodTimeout = 2 * time.Minute

// NewClusterProbe connects to the cluster from the kubeconfig file or the
// pod's service account.
func NewClusterProbe(cfg ProbeConfig) (*ClusterProbe, error) {
	log := cfg.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	rc, err := restConfig(cfg.Kubernetes.Kubeconfig)
	if err != nil {
		return nil, err
	}
	c, err := connect(rc, log)
	if err != nil {
		return nil, err
	}
	return newClusterProbe(c, cfg), nil
}

func newClusterProbe(c *Client, cfg ProbeConfig) *ClusterProbe {
	log := cfg.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &ClusterProbe{c: c, cfg: cfg, log: log, podTimeout: defaultPodTimeout, settle: 5}
}

// ServerVersion implements doctor.ClusterProbe.
func (p *ClusterProbe) ServerVersion(context.Context) (doctor.ClusterVersion, error) {
	// ServerVersion has no context parameter; a hung API server ends with
	// the client's own timeout.
	info, err := p.c.typed.Discovery().ServerVersion()
	if err != nil {
		return doctor.ClusterVersion{}, fmt.Errorf("%w: %w", ErrConnect, err)
	}
	return doctor.ClusterVersion{Major: leadingInt(info.Major), Minor: leadingInt(info.Minor), Git: info.GitVersion}, nil
}

// leadingInt parses the digits at the start of s: GKE and EKS report a
// minor version like "34+".
func leadingInt(s string) int {
	end := 0
	for end < len(s) && s[end] >= '0' && s[end] <= '9' {
		end++
	}
	n, _ := strconv.Atoi(s[:end])
	return n
}

// Nodes implements doctor.ClusterProbe.
func (p *ClusterProbe) Nodes(ctx context.Context) ([]doctor.ClusterNode, error) {
	list, err := p.c.typed.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	out := make([]doctor.ClusterNode, 0, len(list.Items))
	for _, n := range list.Items {
		node := doctor.ClusterNode{
			Name:          n.Name,
			OS:            n.Status.NodeInfo.OperatingSystem,
			Arch:          n.Status.NodeInfo.Architecture,
			Unschedulable: n.Spec.Unschedulable,
		}
		for _, cond := range n.Status.Conditions {
			if cond.Type == corev1.NodeReady {
				node.Ready = cond.Status == corev1.ConditionTrue
			}
		}
		if mem, ok := n.Status.Allocatable[corev1.ResourceMemory]; ok {
			node.AllocatableMemory = uint64(max(mem.Value(), 0))
		}
		out = append(out, node)
	}
	return out, nil
}

// right is one permission to ask the API server about.
type right struct {
	// namespace is "" for cluster scope, or all namespaces for a
	// namespaced resource.
	namespace, verb, group, resource, subresource string
}

func (r right) String() string {
	res := r.resource
	if r.subresource != "" {
		res += "/" + r.subresource
	}
	if r.group != "" {
		res += "." + r.group
	}
	if r.namespace != "" {
		return fmt.Sprintf("%s %s in namespace %s", r.verb, res, r.namespace)
	}
	return fmt.Sprintf("%s %s", r.verb, res)
}

// needed returns the rights the driver needs beyond what its namespaces'
// bound role gives it: creating and deleting site namespaces, reading the
// cluster, and running tasks in the platform namespace. The Helm chart's
// RBAC (#223) is the grant; this list must stay within it.
func (p *ClusterProbe) needed() []right {
	ns := p.cfg.Kubernetes.PlatformNamespace
	rights := []right{
		{"", "create", "", "namespaces", ""},
		{"", "get", "", "namespaces", ""},
		{"", "list", "", "namespaces", ""},
		{"", "delete", "", "namespaces", ""},
		{"", "list", "", "nodes", ""},
		{"", "get", "storage.k8s.io", "storageclasses", ""},
		{"", "list", "metrics.k8s.io", "nodes", ""},
	}
	for _, verb := range []string{"create", "get", "list", "watch", "delete"} {
		rights = append(rights, right{ns, verb, "", "pods", ""})
	}
	for _, verb := range []string{"get", "list", "create", "delete"} {
		rights = append(rights, right{ns, verb, "", "secrets", ""})
	}
	return append(rights,
		right{ns, "get", "", "pods", "log"},
		right{ns, "get", "", "services", ""},
		right{ns, "list", "metrics.k8s.io", "pods", ""},
	)
}

// forbidden returns the cluster-wide rights the control plane must not
// have (design doc §4.8): reading every Secret, reaching into nodes
// through the kubelet, running commands in any pod, and creating bindings
// freely.
func forbidden() []right {
	return []right{
		{"", "get", "", "secrets", ""},
		{"", "list", "", "secrets", ""},
		{"", "get", "", "nodes", "proxy"},
		{"", "create", "", "pods", "exec"},
		{"", "create", "rbac.authorization.k8s.io", "clusterrolebindings", ""},
		{"", "escalate", "rbac.authorization.k8s.io", "clusterroles", ""},
	}
}

func (p *ClusterProbe) allowed(ctx context.Context, r right) (bool, error) {
	review := &authv1.SelfSubjectAccessReview{Spec: authv1.SelfSubjectAccessReviewSpec{
		ResourceAttributes: &authv1.ResourceAttributes{
			Namespace: r.namespace, Verb: r.verb, Group: r.group, Resource: r.resource, Subresource: r.subresource,
		},
	}}
	res, err := p.c.typed.AuthorizationV1().SelfSubjectAccessReviews().Create(ctx, review, metav1.CreateOptions{})
	if err != nil {
		return false, err
	}
	return res.Status.Allowed, nil
}

// RBAC implements doctor.ClusterProbe with SelfSubjectAccessReviews.
func (p *ClusterProbe) RBAC(ctx context.Context) (doctor.RBACReport, error) {
	var rep doctor.RBACReport
	for _, r := range p.needed() {
		ok, err := p.allowed(ctx, r)
		if err != nil {
			return rep, err
		}
		if !ok {
			rep.Missing = append(rep.Missing, r.String())
		}
	}
	for _, r := range forbidden() {
		ok, err := p.allowed(ctx, r)
		if err != nil {
			return rep, err
		}
		if ok {
			if r.namespace == "" && r.resource != "nodes" && r.resource != "clusterrolebindings" && r.resource != "clusterroles" {
				rep.Excess = append(rep.Excess, r.String()+" in all namespaces")
			} else {
				rep.Excess = append(rep.Excess, r.String())
			}
		}
	}
	return rep, nil
}

// defaultClassAnnotation marks a cluster's default storage class.
const defaultClassAnnotation = "storageclass.kubernetes.io/is-default-class"

// StorageClass implements doctor.ClusterProbe.
func (p *ClusterProbe) StorageClass(ctx context.Context, name string) (doctor.StorageClassInfo, bool, error) {
	if name != "" {
		sc, err := p.c.typed.StorageV1().StorageClasses().Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return doctor.StorageClassInfo{}, false, nil
		} else if err != nil {
			return doctor.StorageClassInfo{}, false, err
		}
		return doctor.StorageClassInfo{Name: sc.Name, Provisioner: sc.Provisioner}, true, nil
	}
	list, err := p.c.typed.StorageV1().StorageClasses().List(ctx, metav1.ListOptions{})
	if err != nil {
		return doctor.StorageClassInfo{}, false, err
	}
	for _, sc := range list.Items {
		if sc.Annotations[defaultClassAnnotation] == "true" {
			return doctor.StorageClassInfo{Name: sc.Name, Provisioner: sc.Provisioner}, true, nil
		}
	}
	return doctor.StorageClassInfo{}, false, nil
}

// MetricsAPI implements doctor.ClusterProbe. Discovery of an aggregated
// API fails while its APIService is unavailable, so a successful answer
// means metrics-server is serving.
func (p *ClusterProbe) MetricsAPI(context.Context) error {
	list, err := p.c.typed.Discovery().ServerResourcesForGroupVersion("metrics.k8s.io/v1beta1")
	if err != nil {
		return err
	}
	for _, r := range list.APIResources {
		if r.Name == "nodes" {
			return nil
		}
	}
	return errors.New("the API group has no nodes resource")
}

// TraefikAddresses implements doctor.ClusterProbe.
func (p *ClusterProbe) TraefikAddresses(ctx context.Context) ([]string, error) {
	svc, err := p.c.typed.CoreV1().Services(p.cfg.Kubernetes.PlatformNamespace).Get(ctx, p.cfg.Kubernetes.TraefikService, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	var out []string
	for _, ing := range svc.Status.LoadBalancer.Ingress {
		switch {
		case ing.IP != "":
			out = append(out, ing.IP)
		case ing.Hostname != "":
			out = append(out, ing.Hostname)
		}
	}
	return append(out, svc.Spec.ExternalIPs...), nil
}

// PlatformMemoryRequests implements doctor.ClusterProbe: the memory
// requests of the platform namespace's pods that hold or will hold a
// node's memory.
func (p *ClusterProbe) PlatformMemoryRequests(ctx context.Context) (uint64, error) {
	list, err := p.c.typed.CoreV1().Pods(p.cfg.Kubernetes.PlatformNamespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return 0, err
	}
	var total int64
	for _, pod := range list.Items {
		if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
			continue
		}
		for _, c := range pod.Spec.Containers {
			if m, ok := c.Resources.Requests[corev1.ResourceMemory]; ok {
				total += m.Value()
			}
		}
	}
	return uint64(max(total, 0)), nil
}

// probe is one pod the doctor runs and the script that decides its result.
type probe struct {
	name   string
	labels map[string]string
	node   string
	script string
}

// probeResult is how a probe pod ended.
type probeResult struct {
	done      bool
	succeeded bool
	// stuck is true for a pod that cannot start (a bad image), as opposed to
	// one that ran and failed.
	stuck bool
	// why says what a pod that didn't finish was waiting for.
	why string
}

// randomSuffix returns a short random hex string.
func randomSuffix() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b) // crypto/rand.Read never fails
	return hex.EncodeToString(b)
}

// runProbes creates a namespace of its own, with the policies given, runs
// the pods in it, and waits for them to end. The namespace is deleted
// whatever happens, which removes everything in it.
func (p *ClusterProbe) runProbes(ctx context.Context, probes []probe, policies []*networkingv1.NetworkPolicy) (map[string]probeResult, error) {
	if p.cfg.Image == "" {
		return nil, doctor.ErrNoProbeImage
	}
	nsName := p.cfg.Kubernetes.PlatformNamespace + probeNamespaceRoot + randomSuffix()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: nsName,
		Labels: map[string]string{
			probeLabel:                           "true",
			"pod-security.kubernetes.io/enforce": "restricted",
		},
	}}
	if _, err := p.c.typed.CoreV1().Namespaces().Create(ctx, ns, metav1.CreateOptions{}); err != nil {
		return nil, fmt.Errorf("creating the probe namespace: %w", err)
	}
	defer func() {
		// The caller's context may be what ended; the namespace must go
		// anyway.
		dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		if err := p.c.typed.CoreV1().Namespaces().Delete(dctx, nsName, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			p.log.WarnContext(ctx, "couldn't delete the doctor's probe namespace", "namespace", nsName, "error", err)
		}
	}()

	var pullSecrets []corev1.LocalObjectReference
	for _, name := range p.cfg.Kubernetes.ImagePullSecrets {
		src, err := p.c.typed.CoreV1().Secrets(p.cfg.Kubernetes.PlatformNamespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return nil, fmt.Errorf("copying image pull secret %s: %w", name, err)
		}
		cp := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: nsName},
			Type:       src.Type, Data: src.Data,
		}
		if _, err := p.c.typed.CoreV1().Secrets(nsName).Create(ctx, cp, metav1.CreateOptions{}); err != nil {
			return nil, fmt.Errorf("copying image pull secret %s: %w", name, err)
		}
		pullSecrets = append(pullSecrets, corev1.LocalObjectReference{Name: name})
	}
	for _, pol := range policies {
		pol = pol.DeepCopy()
		pol.Namespace = nsName
		if _, err := p.c.typed.NetworkingV1().NetworkPolicies(nsName).Create(ctx, pol, metav1.CreateOptions{}); err != nil {
			return nil, fmt.Errorf("creating the probe NetworkPolicy: %w", err)
		}
	}
	for _, pr := range probes {
		if _, err := p.c.typed.CoreV1().Pods(nsName).Create(ctx, p.pod(nsName, pr, pullSecrets), metav1.CreateOptions{}); err != nil {
			return nil, fmt.Errorf("creating probe pod %s: %w", pr.name, err)
		}
	}
	return p.waitProbes(ctx, nsName, probes)
}

// pod builds a probe pod that satisfies Pod Security's restricted profile.
func (p *ClusterProbe) pod(ns string, pr probe, pullSecrets []corev1.LocalObjectReference) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: pr.name, Namespace: ns, Labels: pr.labels},
		Spec: corev1.PodSpec{
			NodeName:                     pr.node,
			RestartPolicy:                corev1.RestartPolicyNever,
			AutomountServiceAccountToken: probePtr(false),
			ImagePullSecrets:             pullSecrets,
			SecurityContext: &corev1.PodSecurityContext{
				RunAsNonRoot:   probePtr(true),
				RunAsUser:      probePtr(int64(65534)),
				SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
			},
			Containers: []corev1.Container{{
				Name:            "probe",
				Image:           p.cfg.Image,
				ImagePullPolicy: corev1.PullIfNotPresent,
				Command:         []string{"bash", "-c", pr.script},
				SecurityContext: &corev1.SecurityContext{
					AllowPrivilegeEscalation: probePtr(false),
					Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
				},
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("10m"),
						corev1.ResourceMemory: resource.MustParse("16Mi"),
					},
					Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("64Mi")},
				},
			}},
		},
	}
}

// probePtr returns a pointer to v, for pod spec fields. k8s.io/utils/ptr does
// the same, but would make that module a direct dependency for this.
func probePtr[T any](v T) *T { return &v }

// stuckReasons are container waiting reasons that no amount of waiting
// resolves.
var stuckReasons = map[string]bool{
	"ErrImagePull": true, "ImagePullBackOff": true, "InvalidImageName": true,
	"CreateContainerConfigError": true, "CreateContainerError": true,
}

// waitProbes waits until every probe pod ended, or one can't start, or the
// timeout passes.
func (p *ClusterProbe) waitProbes(ctx context.Context, ns string, probes []probe) (map[string]probeResult, error) {
	results := map[string]probeResult{}
	ctx, cancel := context.WithTimeout(ctx, p.podTimeout)
	defer cancel()
	err := wait.PollUntilContextCancel(ctx, p.c.poll, true, func(ctx context.Context) (bool, error) {
		all := true
		for _, pr := range probes {
			pod, err := p.c.typed.CoreV1().Pods(ns).Get(ctx, pr.name, metav1.GetOptions{})
			if err != nil {
				return false, err
			}
			res := probeResult{why: "pod is " + strings.ToLower(string(pod.Status.Phase))}
			switch pod.Status.Phase {
			case corev1.PodSucceeded:
				res.done, res.succeeded = true, true
			case corev1.PodFailed:
				res.done = true
			default:
				for _, cs := range pod.Status.ContainerStatuses {
					if w := cs.State.Waiting; w != nil && w.Reason != "" {
						res.why = w.Reason
						if stuckReasons[w.Reason] {
							res.done, res.stuck = true, true
							res.why = w.Reason + ": " + w.Message
						}
					}
				}
				if pod.Status.Phase == corev1.PodPending {
					for _, cond := range pod.Status.Conditions {
						if cond.Type == corev1.PodScheduled && cond.Status == corev1.ConditionFalse {
							res.why = "unschedulable: " + cond.Message
						}
					}
				}
			}
			results[pr.name] = res
			all = all && res.done
		}
		return all, nil
	})
	if err != nil && !errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
		return nil, err
	}
	if parent := ctx.Err(); err != nil && errors.Is(parent, context.Canceled) {
		return nil, parent
	}
	return results, nil
}

// connectScript connects to the API server's Service address, which every
// pod learns from its environment. bash and timeout are the control plane
// image's, so the probes need nothing else in the image.
const connectCmd = `timeout 3 bash -c 'exec 3<>/dev/tcp/$KUBERNETES_SERVICE_HOST/$KUBERNETES_SERVICE_PORT' 2>/dev/null`

// NetworkPolicy implements doctor.ClusterProbe. Two pods connect to the
// API server's Service: one in a namespace where a NetworkPolicy denies all
// egress, and one with no policy. Only an enforcing CNI makes the first
// fail while the second succeeds.
func (p *ClusterProbe) NetworkPolicy(ctx context.Context) (doctor.NetworkPolicyResult, error) {
	allowed := probe{
		name:   "allowed",
		labels: map[string]string{"role": "allowed"},
		script: `for i in 1 2 3 4 5; do ` + connectCmd + ` && exit 0; sleep 2; done; exit 1`,
	}
	// The denied pod waits for the CNI to program its policy, which can
	// lag the pod's start, and fails if any attempt connects.
	denied := probe{
		name:   "denied",
		labels: map[string]string{"role": "denied"},
		script: fmt.Sprintf(`sleep %d; for i in 1 2 3; do `+connectCmd+` && exit 1; done; exit 0`, p.settle),
	}
	deny := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "deny-egress"},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{"role": "denied"}},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
		},
	}
	res, err := p.runProbes(ctx, []probe{allowed, denied}, []*networkingv1.NetworkPolicy{deny})
	if err != nil {
		return doctor.NetworkPolicyResult{}, err
	}
	a, d := res["allowed"], res["denied"]
	switch {
	case !a.succeeded:
		return doctor.NetworkPolicyResult{State: doctor.NetworkPolicyInconclusive,
			Detail: "the pod without a policy couldn't connect to the API server (" + a.why + ")"}, nil
	case d.succeeded:
		return doctor.NetworkPolicyResult{State: doctor.NetworkPolicyEnforced}, nil
	case d.done:
		if d.stuck {
			return doctor.NetworkPolicyResult{State: doctor.NetworkPolicyInconclusive, Detail: "the denied pod didn't start (" + d.why + ")"}, nil
		}
		return doctor.NetworkPolicyResult{State: doctor.NetworkPolicyIgnored}, nil
	}
	return doctor.NetworkPolicyResult{State: doctor.NetworkPolicyInconclusive, Detail: "the denied pod didn't finish (" + d.why + ")"}, nil
}

// CgroupV2 implements doctor.ClusterProbe: a pod pinned to each node asks
// which filesystem its cgroup mount is. Pinning with nodeName skips the
// scheduler, so a node's taints don't keep the probe off it.
func (p *ClusterProbe) CgroupV2(ctx context.Context, nodes []doctor.ClusterNode) ([]doctor.NodeCgroup, error) {
	if len(nodes) == 0 {
		return nil, nil
	}
	probes := make([]probe, len(nodes))
	for i, n := range nodes {
		probes[i] = probe{
			name:   "cgroup-" + strconv.Itoa(i),
			node:   n.Name,
			script: `[ "$(stat -f -c %T /sys/fs/cgroup)" = cgroup2fs ]`,
		}
	}
	res, err := p.runProbes(ctx, probes, nil)
	if err != nil {
		return nil, err
	}
	out := make([]doctor.NodeCgroup, len(nodes))
	for i, n := range nodes {
		r := res[probes[i].name]
		out[i] = doctor.NodeCgroup{Node: n.Name, V2: r.succeeded}
		if !r.done {
			out[i].Err = r.why
		} else if r.stuck {
			out[i].Err = r.why
		}
	}
	return out, nil
}
