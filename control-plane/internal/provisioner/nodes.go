package provisioner

import (
	"context"
	"time"
)

// NodeStats is one node's CPU and memory, for platforms whose host is a
// cluster of them (design doc §7.2). Where the platform runs on one host,
// the metrics collector reads that host's procfs and needs no driver.
type NodeStats struct {
	// Node is the node's name.
	Node string
	// CPUBusy and CPUIdle are cumulative CPU time over all the node's
	// cores. The driver integrates them from a rate, so they restart from
	// zero with the control plane; consumers treat a decrease as a reset.
	CPUBusy, CPUIdle time.Duration
	// MemoryTotalBytes is the memory the node offers to workloads
	// (allocatable, which excludes what the system reserves), and
	// MemoryUsedBytes the part in use (the working set).
	MemoryTotalBytes, MemoryUsedBytes uint64
	// SampledAt is when the node's metrics were scraped.
	SampledAt time.Time
}

// NodeStatsReader samples the nodes of a cluster. It is its own interface,
// outside [Driver], so that only the host metrics collector needs it.
type NodeStatsReader interface {
	// NodeStats returns the stats of every node that has metrics yet.
	NodeStats(ctx context.Context) ([]NodeStats, error)
}
