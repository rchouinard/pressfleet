// Package metricstest provides fakes of the metrics package's interfaces for
// tests that must not need VictoriaMetrics.
package metricstest

import (
	"context"
	"sync"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/metrics"
)

// Writer is a [metrics.Writer] that records what it is given.
type Writer struct {
	mu      sync.Mutex
	samples []metrics.Sample
	calls   int
	err     error
}

var _ metrics.Writer = (*Writer)(nil)

// Write implements [metrics.Writer]. A failing write records nothing.
func (w *Writer) Write(_ context.Context, samples []metrics.Sample) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.calls++
	if w.err != nil {
		return w.err
	}
	w.samples = append(w.samples, samples...)
	return nil
}

// FailWith makes later writes fail with err; nil makes them succeed again.
func (w *Writer) FailWith(err error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.err = err
}

// Calls returns how many times Write was called.
func (w *Writer) Calls() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.calls
}

// Samples returns a copy of every sample written.
func (w *Writer) Samples() []metrics.Sample {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]metrics.Sample(nil), w.samples...)
}

// Named returns the written samples called name.
func (w *Writer) Named(name string) []metrics.Sample {
	var out []metrics.Sample
	for _, s := range w.Samples() {
		if s.Name == name {
			out = append(out, s)
		}
	}
	return out
}

// Targets is a [metrics.TargetSource] whose list a test changes while a
// service runs.
type Targets struct {
	mu      sync.Mutex
	targets []metrics.Target
	err     error
}

var _ metrics.TargetSource = (*Targets)(nil)

// Set replaces the list.
func (t *Targets) Set(targets ...metrics.Target) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.targets = targets
}

// FailWith makes Targets fail with err; nil makes it succeed again.
func (t *Targets) FailWith(err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.err = err
}

// Targets implements [metrics.TargetSource].
func (t *Targets) Targets(context.Context) ([]metrics.Target, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.err != nil {
		return nil, t.err
	}
	return append([]metrics.Target(nil), t.targets...), nil
}

// Querier is a [metrics.Querier] that returns canned results and records
// the queries it got.
type Querier struct {
	mu sync.Mutex
	// Result and Err are what both methods return.
	Result  metrics.Result
	Err     error
	queries []string
}

var _ metrics.Querier = (*Querier)(nil)

// Query implements [metrics.Querier].
func (q *Querier) Query(_ context.Context, query string, _ time.Time) (metrics.Result, error) {
	return q.answer(query)
}

// QueryRange implements [metrics.Querier].
func (q *Querier) QueryRange(_ context.Context, query string, _, _ time.Time, _ time.Duration) (metrics.Result, error) {
	return q.answer(query)
}

func (q *Querier) answer(query string) (metrics.Result, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.queries = append(q.queries, query)
	return q.Result, q.Err
}

// Queries returns the query strings received, in order.
func (q *Querier) Queries() []string {
	q.mu.Lock()
	defer q.mu.Unlock()
	return append([]string(nil), q.queries...)
}
