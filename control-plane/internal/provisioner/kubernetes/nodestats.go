package kubernetes

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"

	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
)

var _ provisioner.NodeStatsReader = (*Provisioner)(nil)

// nodeMetricsPath is where metrics-server serves node metrics.
const nodeMetricsPath = "/apis/metrics.k8s.io/v1beta1/nodes"

// nodeCPUState is what the driver remembers of a node to accumulate its
// CPU time.
type nodeCPUState struct {
	sampled    time.Time
	busy, idle time.Duration
}

// NodeStats samples the cluster's nodes (provisioner.NodeStatsReader) from
// metrics.k8s.io, the Kubernetes counterpart of the host's procfs (design
// doc §7.2). Like Stats, it integrates the rate metrics-server reports into
// cumulative CPU time: busy time is the usage, and idle time what the
// node's CPU capacity had left over. Memory is the node's allocatable
// memory, which is what pods can be scheduled into, and its working set.
// Nodes metrics-server hasn't scraped yet are left out.
func (p *Provisioner) NodeStats(ctx context.Context) ([]provisioner.NodeStats, error) {
	nodes, err := p.typed.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("read node stats: %w", err)
	}
	metrics, err := p.listNodeMetrics(ctx)
	if err != nil {
		return nil, fmt.Errorf("read node stats: %w", err)
	}
	byName := make(map[string]nodeMetrics, len(metrics))
	for _, m := range metrics {
		byName[m.Metadata.Name] = m
	}

	p.observe.mu.Lock()
	defer p.observe.mu.Unlock()
	if p.observe.nodeCPU == nil {
		p.observe.nodeCPU = map[string]nodeCPUState{}
	}
	var out []provisioner.NodeStats
	live := map[string]bool{}
	for i := range nodes.Items {
		n := &nodes.Items[i]
		live[n.Name] = true
		m, ok := byName[n.Name]
		if !ok {
			continue
		}
		cores := m.Usage.Cpu().AsApproximateFloat64()
		capacity := n.Status.Capacity.Cpu().AsApproximateFloat64()

		st, known := p.observe.nodeCPU[n.Name]
		advance := func(d time.Duration) {
			busy := time.Duration(cores * float64(d))
			st.busy += busy
			// Usage can briefly exceed the capacity; idle never goes
			// backwards.
			st.idle += max(time.Duration(capacity*float64(d))-busy, 0)
		}
		switch {
		case !known:
			// The first sample: the window is all that is known to have run.
			st = nodeCPUState{sampled: m.Timestamp.Time}
			advance(m.Window.Duration)
		case m.Timestamp.After(st.sampled):
			advance(m.Timestamp.Sub(st.sampled))
			st.sampled = m.Timestamp.Time
		}
		p.observe.nodeCPU[n.Name] = st

		total := n.Status.Allocatable.Memory().Value()
		if total <= 0 {
			total = n.Status.Capacity.Memory().Value()
		}
		out = append(out, provisioner.NodeStats{
			Node:             n.Name,
			CPUBusy:          st.busy,
			CPUIdle:          st.idle,
			MemoryTotalBytes: uint64(max(total, 0)),
			MemoryUsedBytes:  uint64(max(m.Usage.Memory().Value(), 0)),
			SampledAt:        m.Timestamp.Time,
		})
	}
	// Forget nodes that left the cluster.
	for k := range p.observe.nodeCPU {
		if !live[k] {
			delete(p.observe.nodeCPU, k)
		}
	}
	return out, nil
}

// listNodeMetrics returns metrics.k8s.io's metrics of the cluster's nodes.
func (p *Provisioner) listNodeMetrics(ctx context.Context) ([]nodeMetrics, error) {
	if p.observe.nodeMetrics != nil {
		return p.observe.nodeMetrics(ctx)
	}
	rc := p.typed.CoreV1().RESTClient()
	if c, ok := rc.(*rest.RESTClient); rc == nil || (ok && c == nil) {
		return nil, ErrMetricsUnavailable
	}
	raw, err := rc.Get().AbsPath(nodeMetricsPath).Do(ctx).Raw()
	if err != nil {
		if apierrors.IsNotFound(err) || apierrors.IsServiceUnavailable(err) {
			return nil, fmt.Errorf("%w: %w", ErrMetricsUnavailable, err)
		}
		return nil, fmt.Errorf("list node metrics: %w", err)
	}
	var list struct {
		Items []nodeMetrics `json:"items"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, fmt.Errorf("decode node metrics: %w", err)
	}
	return list.Items, nil
}
