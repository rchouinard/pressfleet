// Package jobstest provides test helpers for code that uses package jobs.
package jobstest

import (
	"sync"
	"time"
)

// Clock is a manual clock for jobs.Options.Clock and
// jobs.SchedulerOptions.Clock. It only moves when told to.
type Clock struct {
	mu  sync.Mutex
	now time.Time
}

// NewClock returns a clock set to start.
func NewClock(start time.Time) *Clock {
	return &Clock{now: start}
}

// Now returns the clock's time.
func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// Advance moves the clock forward by d.
func (c *Clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// Set moves the clock to t.
func (c *Clock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = t
}
