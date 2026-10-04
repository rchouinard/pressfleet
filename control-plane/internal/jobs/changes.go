package jobs

import "sync"

// Changes tells watchers in this process that a job may have changed, so
// that a job event stream reads it again at once instead of on its next
// poll. A notification carries nothing: the database stays the source of
// truth, and watchers must still poll for changes made by other processes
// (CLI commands run inside the control plane container are one).
//
// A nil *Changes is valid; it never notifies.
type Changes struct {
	mu       sync.Mutex
	watchers map[string]map[chan struct{}]struct{}
}

// NewChanges returns a Changes without watchers.
func NewChanges() *Changes {
	return &Changes{watchers: map[string]map[chan struct{}]struct{}{}}
}

// Watch returns a channel that receives a value after the job changes, and
// a function that stops watching. Notifications that arrive before the
// last one is received coalesce into one. On a nil Changes the channel
// never receives.
func (c *Changes) Watch(jobID string) (<-chan struct{}, func()) {
	if c == nil {
		return nil, func() {}
	}
	ch := make(chan struct{}, 1)
	c.mu.Lock()
	defer c.mu.Unlock()
	set := c.watchers[jobID]
	if set == nil {
		set = map[chan struct{}]struct{}{}
		c.watchers[jobID] = set
	}
	set[ch] = struct{}{}
	return ch, func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		// A job's set is only dropped once empty, so while ch is watching,
		// the current set is the one that holds it.
		set := c.watchers[jobID]
		delete(set, ch)
		if len(set) == 0 {
			delete(c.watchers, jobID)
		}
	}
}

// Notify wakes the job's watchers. It never blocks.
func (c *Changes) Notify(jobID string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for ch := range c.watchers[jobID] {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}
