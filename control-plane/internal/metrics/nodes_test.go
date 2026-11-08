package metrics_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/metrics"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
)

type fakeNodes struct {
	nodes []provisioner.NodeStats
	err   error
}

func (f fakeNodes) NodeStats(context.Context) ([]provisioner.NodeStats, error) { return f.nodes, f.err }

func TestNodeCollector(t *testing.T) {
	c := metrics.NodeCollector{Nodes: fakeNodes{nodes: []provisioner.NodeStats{
		{Node: "n1", CPUBusy: 30 * time.Second, CPUIdle: 90 * time.Second, MemoryTotalBytes: 8000, MemoryUsedBytes: 3000},
		{Node: "n2", MemoryTotalBytes: 100, MemoryUsedBytes: 150},
	}}}
	got, err := c.Collect(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	type key struct{ name, node, mode string }
	have := map[key]float64{}
	for _, s := range got {
		if len(s.Labels) > 2 || s.Labels["node"] == "" {
			t.Errorf("%s labels = %v, want node (and mode)", s.Name, s.Labels)
		}
		have[key{s.Name, s.Labels["node"], s.Labels["mode"]}] = s.Value
	}
	want := map[key]float64{
		{"pressfleet_host_cpu_seconds_total", "n1", "used"}:  30,
		{"pressfleet_host_cpu_seconds_total", "n1", "idle"}:  90,
		{"pressfleet_host_memory_total_bytes", "n1", ""}:     8000,
		{"pressfleet_host_memory_available_bytes", "n1", ""}: 5000,
		{"pressfleet_host_memory_total_bytes", "n2", ""}:     100,
		// A working set above the allocatable memory leaves none, not a
		// negative number.
		{"pressfleet_host_memory_available_bytes", "n2", ""}: 0,
	}
	for k, v := range want {
		if got, ok := have[k]; !ok || got != v {
			t.Errorf("%+v = %v (present %v), want %v", k, got, ok, v)
		}
	}
	for k := range have {
		if k.name == "pressfleet_host_filesystem_size_bytes" || k.name == "pressfleet_host_disk_read_bytes_total" {
			t.Errorf("unexpected disk series %+v", k)
		}
	}
}

func TestNodeCollectorError(t *testing.T) {
	boom := errors.New("boom")
	if _, err := (metrics.NodeCollector{Nodes: fakeNodes{err: boom}}).Collect(t.Context(), nil); !errors.Is(err, boom) {
		t.Errorf("err = %v, want it to wrap boom", err)
	}
}
