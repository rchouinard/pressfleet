package metrics_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/metrics"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
)

type statsFake map[string]error

func (f statsFake) Stats(_ context.Context, id string) ([]provisioner.ComponentStats, error) {
	if err := f[id]; err != nil {
		return nil, err
	}
	return []provisioner.ComponentStats{
		{Component: provisioner.ComponentPHP, CPUUsage: 1500 * time.Millisecond, MemoryBytes: 1024, Restarts: 2},
	}, nil
}

func TestContainerCollector(t *testing.T) {
	boom := errors.New("boom")
	c := metrics.ContainerCollector{Stats: statsFake{"gone": provisioner.ErrSiteNotFound, "bad": boom}}
	targets := []metrics.Target{
		{SiteID: "a", ClientGroupID: "g"}, {SiteID: "gone"}, {SiteID: "bad"},
	}
	got, err := c.Collect(context.Background(), targets)
	if !errors.Is(err, boom) {
		t.Errorf("err = %v, want boom", err)
	}
	want := map[string]float64{
		metrics.MetricContainerCPUSeconds: 1.5,
		metrics.MetricContainerMemory:     1024,
		metrics.MetricContainerRestarts:   2,
	}
	if len(got) != len(want) {
		t.Fatalf("got %d samples: %+v", len(got), got)
	}
	for _, s := range got {
		if v, ok := want[s.Name]; !ok || v != s.Value {
			t.Errorf("sample %+v unexpected", s)
		}
		if s.Labels["container"] != "php" || s.Labels[metrics.LabelSiteID] != "a" || s.Labels[metrics.LabelClientGroupID] != "g" {
			t.Errorf("labels = %v", s.Labels)
		}
	}
}
