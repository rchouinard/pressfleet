package kubernetes

import (
	"context"
	"errors"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func testNode(name, cpu, allocatable string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: corev1.NodeStatus{
			Capacity:    corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(cpu), corev1.ResourceMemory: resource.MustParse("8Gi")},
			Allocatable: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(cpu), corev1.ResourceMemory: resource.MustParse(allocatable)},
		},
	}
}

func nodeSample(name string, at time.Time, window time.Duration, cpu, mem string) nodeMetrics {
	return nodeMetrics{
		Metadata:  metav1.ObjectMeta{Name: name},
		Timestamp: metav1.NewTime(at),
		Window:    metav1.Duration{Duration: window},
		Usage:     corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(cpu), corev1.ResourceMemory: resource.MustParse(mem)},
	}
}

func TestNodeStats(t *testing.T) {
	p := observed(t, testNode("a", "4", "7Gi"), testNode("b", "2", "3Gi"))
	samples := []nodeMetrics{nodeSample("a", t0, 10*time.Second, "1", "2Gi")}
	p.observe.nodeMetrics = func(context.Context) ([]nodeMetrics, error) { return samples, nil }

	got, err := p.NodeStats(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	// Node b has no metrics yet and is left out.
	if len(got) != 1 || got[0].Node != "a" {
		t.Fatalf("NodeStats = %+v, want node a only", got)
	}
	a := got[0]
	if a.MemoryTotalBytes != 7<<30 || a.MemoryUsedBytes != 2<<30 || !a.SampledAt.Equal(t0) {
		t.Errorf("a = %+v", a)
	}
	// The window's worth: 1 of 4 cores busy for 10 seconds.
	if a.CPUBusy != 10*time.Second || a.CPUIdle != 30*time.Second {
		t.Errorf("first CPU busy/idle = %v/%v, want 10s/30s", a.CPUBusy, a.CPUIdle)
	}

	// The same scrape again changes nothing.
	again, _ := p.NodeStats(t.Context())
	if again[0].CPUBusy != a.CPUBusy || again[0].CPUIdle != a.CPUIdle {
		t.Errorf("repeated scrape moved the CPU time to %v/%v", again[0].CPUBusy, again[0].CPUIdle)
	}

	// A newer scrape adds rate times elapsed time; b appears.
	samples = []nodeMetrics{
		nodeSample("a", t0.Add(20*time.Second), 10*time.Second, "2", "3Gi"),
		nodeSample("b", t0.Add(20*time.Second), 10*time.Second, "3", "1Gi"),
	}
	next, err := p.NodeStats(t.Context())
	if err != nil || len(next) != 2 {
		t.Fatalf("NodeStats = %+v, %v", next, err)
	}
	if want := 10*time.Second + 40*time.Second; next[0].CPUBusy != want {
		t.Errorf("a busy = %v, want %v", next[0].CPUBusy, want)
	}
	if want := 30*time.Second + 40*time.Second; next[0].CPUIdle != want {
		t.Errorf("a idle = %v, want %v", next[0].CPUIdle, want)
	}
	// Usage above the capacity (a burst the metrics window averaged badly)
	// never makes idle time go backwards.
	if next[1].Node != "b" || next[1].CPUBusy != 30*time.Second || next[1].CPUIdle != 0 {
		t.Errorf("b = %+v", next[1])
	}
}

func TestNodeStatsUnavailable(t *testing.T) {
	p := observed(t, testNode("a", "4", "7Gi"))
	p.observe.nodeMetrics = func(context.Context) ([]nodeMetrics, error) { return nil, ErrMetricsUnavailable }
	if _, err := p.NodeStats(t.Context()); !errors.Is(err, ErrMetricsUnavailable) {
		t.Errorf("err = %v, want ErrMetricsUnavailable", err)
	}
}

func TestTraefikServiceName(t *testing.T) {
	p := observed(t)
	if got, want := p.TraefikServiceName(testSiteID), p.SiteNamespace(testSiteID)+"-nginx-80@kubernetescrd"; got != want {
		t.Errorf("TraefikServiceName = %q, want %q", got, want)
	}
}
