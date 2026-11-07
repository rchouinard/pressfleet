package kubernetes

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
)

var t0 = time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)

// sitePod is a running site pod with both containers.
func sitePod(name string, age time.Duration, id string, restarts int32) *corev1.Pod {
	running := corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}
	ctr := func(n, mem string) corev1.Container {
		c := corev1.Container{Name: n}
		if mem != "" {
			c.Resources.Limits = corev1.ResourceList{corev1.ResourceMemory: resource.MustParse(mem)}
		}
		return c
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "pf-" + testSiteID,
			Labels:            map[string]string{ComponentLabel: SiteComponent},
			CreationTimestamp: metav1.NewTime(t0.Add(-age)),
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{ctr("php", "256Mi"), ctr("nginx", "")}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{
			{Name: "php", State: running, ContainerID: "cri://php-" + id, RestartCount: restarts},
			{Name: "nginx", State: running, ContainerID: "cri://nginx-" + id},
		}},
	}
}

func observedDeployment() *appsv1.Deployment {
	return &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: SiteDeployment, Namespace: "pf-" + testSiteID}}
}

func observed(t *testing.T, objs ...runtime.Object) *Provisioner {
	t.Helper()
	return cluster(t, nil, objs...)
}

func metricsFor(pod string, at time.Time, window time.Duration, phpCPU, phpMem string) podMetrics {
	pm := podMetrics{Metadata: metav1.ObjectMeta{Name: pod}, Timestamp: metav1.NewTime(at), Window: metav1.Duration{Duration: window}}
	add := func(name, cpu, mem string) {
		pm.Containers = append(pm.Containers, struct {
			Name  string              `json:"name"`
			Usage corev1.ResourceList `json:"usage"`
		}{name, corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(cpu), corev1.ResourceMemory: resource.MustParse(mem)}})
	}
	add("php", phpCPU, phpMem)
	add("nginx", "10m", "8Mi")
	return pm
}

func TestStats(t *testing.T) {
	p := observed(t, observedDeployment(), sitePod("site-a", time.Hour, "1", 2))
	sample := metricsFor("site-a", t0, 15*time.Second, "500m", "100Mi")
	p.observe.podMetrics = func(context.Context, string) ([]podMetrics, error) { return []podMetrics{sample}, nil }

	got, err := p.Stats(t.Context(), testSiteID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Component != provisioner.ComponentPHP || got[1].Component != provisioner.ComponentNginx {
		t.Fatalf("Stats = %+v, want php and nginx", got)
	}
	php := got[0]
	if php.MemoryBytes != 100<<20 || php.MemoryLimitBytes != 256<<20 || php.Restarts != 2 || !php.SampledAt.Equal(t0) {
		t.Errorf("php = %+v", php)
	}
	if want := 7500 * time.Millisecond; php.CPUUsage != want {
		t.Errorf("first CPUUsage = %v, want %v (the window's worth)", php.CPUUsage, want)
	}
	if got[1].MemoryLimitBytes != 0 {
		t.Errorf("nginx has no limit, got %d", got[1].MemoryLimitBytes)
	}

	// The same scrape again changes nothing.
	again, _ := p.Stats(t.Context(), testSiteID)
	if again[0].CPUUsage != php.CPUUsage {
		t.Errorf("repeated scrape moved CPUUsage to %v", again[0].CPUUsage)
	}

	// A newer scrape adds rate times elapsed time.
	sample = metricsFor("site-a", t0.Add(30*time.Second), 15*time.Second, "1", "120Mi")
	next, _ := p.Stats(t.Context(), testSiteID)
	if want := 7500*time.Millisecond + 30*time.Second; next[0].CPUUsage != want {
		t.Errorf("CPUUsage = %v, want %v", next[0].CPUUsage, want)
	}

	// A restarted container (new ID) starts from zero again.
	restarted := sitePod("site-a", time.Hour, "2", 3)
	if _, err := p.typed.CoreV1().Pods(restarted.Namespace).Update(t.Context(), restarted, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	sample = metricsFor("site-a", t0.Add(45*time.Second), 15*time.Second, "200m", "90Mi")
	after, _ := p.Stats(t.Context(), testSiteID)
	if want := 3 * time.Second; after[0].CPUUsage != want || after[0].Restarts != 3 {
		t.Errorf("after restart = %+v, want CPUUsage %v and 3 restarts", after[0], want)
	}
}

func TestStatsPrefersNewestPodAndSkipsUnmeasured(t *testing.T) {
	old := sitePod("site-old", time.Hour, "1", 0)
	fresh := sitePod("site-new", time.Minute, "2", 0)
	p := observed(t, observedDeployment(), old, fresh)
	p.observe.podMetrics = func(context.Context, string) ([]podMetrics, error) {
		return []podMetrics{metricsFor("site-old", t0, time.Second, "1", "1Mi")}, nil
	}
	// The new pod has no metrics yet, so the old one answers.
	got, err := p.Stats(t.Context(), testSiteID)
	if err != nil || len(got) != 2 {
		t.Fatalf("Stats = %v, %v", got, err)
	}
	p.observe.podMetrics = func(context.Context, string) ([]podMetrics, error) {
		return []podMetrics{metricsFor("site-old", t0, time.Second, "1", "1Mi"), metricsFor("site-new", t0, time.Second, "1", "7Mi")}, nil
	}
	got, _ = p.Stats(t.Context(), testSiteID)
	if got[0].MemoryBytes != 7<<20 {
		t.Errorf("MemoryBytes = %d, want the new pod's", got[0].MemoryBytes)
	}
}

func TestStatsErrors(t *testing.T) {
	p := observed(t)
	if _, err := p.Stats(t.Context(), testSiteID); !errors.Is(err, provisioner.ErrSiteNotFound) {
		t.Errorf("missing site: %v, want ErrSiteNotFound", err)
	}

	p = observed(t, observedDeployment())
	p.observe.podMetrics = func(context.Context, string) ([]podMetrics, error) {
		t.Error("metrics asked for a site without pods")
		return nil, nil
	}
	if got, err := p.Stats(t.Context(), testSiteID); err != nil || len(got) != 0 {
		t.Errorf("stopped site: %v, %v, want nothing", got, err)
	}

	p = observed(t, observedDeployment(), sitePod("site-a", 0, "1", 0))
	if _, err := p.Stats(t.Context(), testSiteID); !errors.Is(err, ErrMetricsUnavailable) {
		t.Errorf("no metrics API: %v, want ErrMetricsUnavailable", err)
	}
}

func fakeLogs(streams map[string]string) func(context.Context, string, string, *corev1.PodLogOptions) (io.ReadCloser, error) {
	return func(_ context.Context, _, pod string, o *corev1.PodLogOptions) (io.ReadCloser, error) {
		s, ok := streams[pod+"/"+o.Container]
		if !ok {
			return nil, fmt.Errorf("no logs for %s/%s", pod, o.Container)
		}
		return io.NopCloser(strings.NewReader(s)), nil
	}
}

func TestLogs(t *testing.T) {
	p := observed(t, observedDeployment(), sitePod("site-a", 0, "1", 0))
	var seen []*corev1.PodLogOptions
	logs := fakeLogs(map[string]string{
		"site-a/php": "2026-11-06T12:00:01.5Z [06-Nov-2026 12:00:01] WARNING: [pool www] child 9 said into stderr\n" +
			"2026-11-06T12:00:03Z PHP Fatal error:  boom\n",
		"site-a/nginx": "2026-11-06T12:00:02Z {\"time\":\"2026-11-06T12:00:02Z\",\"status\":200}\n" +
			"2026-11-06T12:00:04Z 2026/11/06 12:00:04 [error] 7#7: *1 open() failed\r\n" +
			"no timestamp here\n" +
			"2026-11-06T12:00:05Z \n",
	})
	p.observe.podLogs = func(ctx context.Context, ns, pod string, o *corev1.PodLogOptions) (io.ReadCloser, error) {
		seen = append(seen, o)
		return logs(ctx, ns, pod, o)
	}
	collect := func(opts provisioner.LogOptions) []provisioner.LogEntry {
		t.Helper()
		var out []provisioner.LogEntry
		for e, err := range p.Logs(t.Context(), testSiteID, opts) {
			if err != nil {
				t.Fatal(err)
			}
			out = append(out, e)
		}
		return out
	}

	all := collect(provisioner.LogOptions{})
	if len(all) != 5 {
		t.Fatalf("got %d entries: %+v", len(all), all)
	}
	for i := 1; i < len(all); i++ {
		if all[i].Time.Before(all[i-1].Time) {
			t.Errorf("entries out of order at %d", i)
		}
	}
	if all[0].Component != provisioner.ComponentPHP || all[0].Stream != "" || all[0].Line == "" {
		t.Errorf("first = %+v", all[0])
	}
	if all[1].Source != provisioner.LogAccess || all[3].Source != provisioner.LogError || all[3].Level != provisioner.LevelError {
		t.Errorf("classification: %+v / %+v", all[1], all[3])
	}
	if strings.Contains(all[3].Line, "\r") {
		t.Errorf("carriage return kept: %q", all[3].Line)
	}

	// Tail without filters goes to the API server.
	seen = nil
	if got := collect(provisioner.LogOptions{Tail: 2}); len(got) != 2 {
		t.Errorf("Tail 2 gave %d entries", len(got))
	}
	for _, o := range seen {
		if o.TailLines == nil || *o.TailLines != 2 || !o.Timestamps || o.Follow {
			t.Errorf("options = %+v", o)
		}
	}

	// With a filter it is applied here, after filtering.
	seen = nil
	got := collect(provisioner.LogOptions{Tail: 1, Contains: "boom"})
	if len(got) != 1 || !strings.Contains(got[0].Line, "boom") {
		t.Errorf("filtered = %+v", got)
	}
	for _, o := range seen {
		if o.TailLines != nil {
			t.Errorf("TailLines pushed down with a filter: %d", *o.TailLines)
		}
	}

	seen = nil
	since := t0.Add(2500 * time.Millisecond)
	collect(provisioner.LogOptions{Components: []provisioner.Component{provisioner.ComponentNginx}, Since: since})
	if len(seen) != 1 || seen[0].Container != "nginx" || seen[0].SinceTime == nil || !seen[0].SinceTime.Time.Equal(since) {
		t.Errorf("options = %+v", seen)
	}
}

func TestLogsErrors(t *testing.T) {
	p := observed(t)
	for _, err := range p.Logs(t.Context(), testSiteID, provisioner.LogOptions{}) {
		if !errors.Is(err, provisioner.ErrSiteNotFound) {
			t.Errorf("missing site: %v", err)
		}
	}
	for _, err := range p.Logs(t.Context(), testSiteID, provisioner.LogOptions{Tail: -1}) {
		if !errors.Is(err, provisioner.ErrInvalidLogOptions) {
			t.Errorf("bad options: %v", err)
		}
	}
	p = observed(t, observedDeployment())
	for e, err := range p.Logs(t.Context(), testSiteID, provisioner.LogOptions{}) {
		t.Errorf("stopped site yielded %+v, %v", e, err)
	}
}

func TestLogsFollow(t *testing.T) {
	p := observed(t, observedDeployment(), sitePod("site-a", 0, "1", 0))
	var mu sync.Mutex
	opens := 0
	var sinces []time.Time
	pr, pw := io.Pipe()
	p.observe.podLogs = func(_ context.Context, _, _ string, o *corev1.PodLogOptions) (io.ReadCloser, error) {
		mu.Lock()
		defer mu.Unlock()
		if !o.Follow {
			return io.NopCloser(strings.NewReader("")), nil
		}
		opens++
		sinces = append(sinces, o.SinceTime.Time)
		if opens == 1 {
			return pr, nil
		}
		// After the stream breaks, the reattached one repeats the last
		// line and adds a new one.
		return io.NopCloser(strings.NewReader("2099-01-01T00:00:01Z first\n2099-01-01T00:00:02Z second\n")), nil
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() {
		_, _ = io.WriteString(pw, "2099-01-01T00:00:01Z first\n")
		_ = pw.Close() // the stream ends: reattach
	}()
	var lines []string
	for e, err := range p.Logs(ctx, testSiteID, provisioner.LogOptions{Follow: true, Components: []provisioner.Component{provisioner.ComponentPHP}}) {
		if err != nil {
			t.Fatal(err)
		}
		lines = append(lines, e.Line)
		if len(lines) == 2 {
			cancel()
		}
	}
	if strings.Join(lines, ",") != "first,second" {
		t.Errorf("lines = %v, want first,second once each", lines)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(sinces) < 2 || !sinces[1].Equal(time.Date(2099, 1, 1, 0, 0, 1, 0, time.UTC)) {
		t.Errorf("reattached at %v, want the last line's time", sinces)
	}
}
