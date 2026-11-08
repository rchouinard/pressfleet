package metrics

import (
	"context"
	"fmt"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
)

// NodeCollector reports the CPU and memory of each node of a cluster, the
// host metrics of a platform that runs on one (design doc §7.2). It emits
// the series [HostCollector] does, with a node label:
//
//	pressfleet_host_cpu_seconds_total{node,mode}   counter, mode "used" and "idle"
//	pressfleet_host_memory_total_bytes{node}       gauge, the node's allocatable memory
//	pressfleet_host_memory_available_bytes{node}   gauge, allocatable less the working set
//
// There are no filesystem or disk I/O series: a cluster's disks are for the
// operator's cluster monitoring to watch, so the "disk nearly full" alert
// rule has nothing to evaluate there.
type NodeCollector struct {
	// Nodes samples the cluster's nodes.
	Nodes provisioner.NodeStatsReader
	// Every is the collection interval; zero means the service default.
	Every time.Duration
}

var _ Collector = NodeCollector{}

// Name implements [Collector].
func (NodeCollector) Name() string { return "host" }

// Interval implements [Collector].
func (c NodeCollector) Interval() time.Duration { return c.Every }

// Collect implements [Collector].
func (c NodeCollector) Collect(ctx context.Context, _ []Target) ([]Sample, error) {
	nodes, err := c.Nodes.NodeStats(ctx)
	if err != nil {
		return nil, fmt.Errorf("node metrics: %w", err)
	}
	var out []Sample
	for _, n := range nodes {
		node := func() map[string]string { return map[string]string{"node": n.Node} }
		mode := func(m string) map[string]string { return map[string]string{"node": n.Node, "mode": m} }
		out = append(out,
			Sample{Name: "pressfleet_host_cpu_seconds_total", Labels: mode("used"), Value: n.CPUBusy.Seconds()},
			Sample{Name: "pressfleet_host_cpu_seconds_total", Labels: mode("idle"), Value: n.CPUIdle.Seconds()},
			Sample{Name: "pressfleet_host_memory_total_bytes", Labels: node(), Value: float64(n.MemoryTotalBytes)},
			Sample{Name: "pressfleet_host_memory_available_bytes", Labels: node(),
				Value: float64(n.MemoryTotalBytes - min(n.MemoryUsedBytes, n.MemoryTotalBytes))},
		)
	}
	return out, nil
}
