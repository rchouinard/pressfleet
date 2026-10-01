// Package alertstest provides fakes for testing code that uses package
// alerts: a metrics querier whose results a test sets, a notifier that
// records events, and a clock a test advances.
package alertstest

import (
	"context"
	"sync"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/alerts"
)

// Querier is an alerts.Querier that answers from results a test sets.
// Query returns what Set stored for the exact expression, or no samples if
// none, so a rule whose expression the test didn't mention sees no series.
type Querier struct {
	mu      sync.Mutex
	results map[string][]alerts.Sample
	err     error
	queries []string
}

// NewQuerier returns an empty Querier.
func NewQuerier() *Querier {
	return &Querier{results: map[string][]alerts.Sample{}}
}

// Set makes queries for expr return samples, replacing earlier ones.
func (q *Querier) Set(expr string, samples ...alerts.Sample) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.results[expr] = samples
}

// SetError makes every query fail with err until it is called with nil.
func (q *Querier) SetError(err error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.err = err
}

// Queries returns the expressions queried so far, in order.
func (q *Querier) Queries() []string {
	q.mu.Lock()
	defer q.mu.Unlock()
	return append([]string(nil), q.queries...)
}

// Query implements alerts.Querier.
func (q *Querier) Query(_ context.Context, expr string, _ time.Time) ([]alerts.Sample, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.queries = append(q.queries, expr)
	if q.err != nil {
		return nil, q.err
	}
	return append([]alerts.Sample(nil), q.results[expr]...), nil
}

// Sample returns a sample with the labels given as key, value pairs.
func Sample(value float64, labels ...string) alerts.Sample {
	s := alerts.Sample{Labels: map[string]string{}, Value: value}
	for i := 0; i+1 < len(labels); i += 2 {
		s.Labels[labels[i]] = labels[i+1]
	}
	return s
}

// Notifier is an alerts.Notifier that records the events it accepts.
type Notifier struct {
	mu     sync.Mutex
	events []alerts.Event
	err    error
	calls  int
}

// Notify implements alerts.Notifier.
func (n *Notifier) Notify(_ context.Context, e alerts.Event) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.calls++
	if n.err != nil {
		return n.err
	}
	n.events = append(n.events, e)
	return nil
}

// SetError makes Notify fail with err, accepting nothing, until it is called
// with nil.
func (n *Notifier) SetError(err error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.err = err
}

// Events returns the accepted events, in order.
func (n *Notifier) Events() []alerts.Event {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]alerts.Event(nil), n.events...)
}

// Calls returns how many times Notify was called, accepted or not.
func (n *Notifier) Calls() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.calls
}

// Reset forgets the accepted events.
func (n *Notifier) Reset() {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.events, n.calls = nil, 0
}

// Clock is a clock a test moves by hand. Its Now method is an
// alerts.Options.Now.
type Clock struct {
	mu sync.Mutex
	t  time.Time
}

// NewClock returns a Clock set to t.
func NewClock(t time.Time) *Clock { return &Clock{t: t} }

// Now returns the clock's time.
func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

// Advance moves the clock forward by d.
func (c *Clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}
