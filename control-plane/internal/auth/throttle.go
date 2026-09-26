package auth

import (
	"errors"
	"fmt"
	"sync"
	"time"
)

// Throttling limits: after this many failures within throttleWindow of the
// first one, further attempts are refused until the window ends. The
// per-account limit stops guessing one user's password; the higher
// per-address limit stops one client from spraying guesses across many
// accounts.
const (
	throttleWindow        = 15 * time.Minute
	maxFailuresPerAccount = 5
	maxFailuresPerAddress = 20
	// maxThrottleEntries bounds memory if a client invents many emails or
	// addresses.
	maxThrottleEntries = 100_000
)

// ErrThrottled means too many recent failed attempts. Errors that wrap it
// are *ThrottledError.
var ErrThrottled = errors.New("too many failed attempts; try again later")

// ThrottledError reports when the caller may try again.
type ThrottledError struct {
	RetryAfter time.Duration
}

func (e *ThrottledError) Error() string {
	return fmt.Sprintf("%v (retry after %s)", ErrThrottled, e.RetryAfter.Round(time.Second))
}

// Is makes errors.Is(err, ErrThrottled) true.
func (e *ThrottledError) Is(target error) bool { return target == ErrThrottled }

// throttle counts recent failures per key in memory.
type throttle struct {
	mu      sync.Mutex
	now     func() time.Time
	entries map[string]*throttleEntry
}

type throttleEntry struct {
	failures int
	start    time.Time
}

func newThrottle(now func() time.Time) *throttle {
	return &throttle{now: now, entries: map[string]*throttleEntry{}}
}

// check returns a *ThrottledError if any key has reached its limit.
func (t *throttle) check(keys ...throttleKey) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	var wait time.Duration
	for _, k := range keys {
		e := t.live(k.key, now)
		if e != nil && e.failures >= k.limit {
			wait = max(wait, e.start.Add(throttleWindow).Sub(now))
		}
	}
	if wait > 0 {
		return &ThrottledError{RetryAfter: wait}
	}
	return nil
}

// fail records a failure for every key.
func (t *throttle) fail(keys ...throttleKey) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	for _, k := range keys {
		e := t.live(k.key, now)
		if e == nil {
			if len(t.entries) >= maxThrottleEntries {
				t.prune(now)
			}
			e = &throttleEntry{start: now}
			t.entries[k.key] = e
		}
		e.failures++
	}
}

// reset forgets the failures for a key, after a success.
func (t *throttle) reset(k throttleKey) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.entries, k.key)
}

// live returns the entry for key if its window hasn't ended.
func (t *throttle) live(key string, now time.Time) *throttleEntry {
	e, ok := t.entries[key]
	if !ok {
		return nil
	}
	if !now.Before(e.start.Add(throttleWindow)) {
		delete(t.entries, key)
		return nil
	}
	return e
}

// prune drops ended windows. If every entry is live, it drops them all:
// forgetting some counters is better than growing without bound.
func (t *throttle) prune(now time.Time) {
	for k, e := range t.entries {
		if !now.Before(e.start.Add(throttleWindow)) {
			delete(t.entries, k)
		}
	}
	if len(t.entries) >= maxThrottleEntries {
		clear(t.entries)
	}
}

type throttleKey struct {
	key   string
	limit int
}

func accountKey(email string) throttleKey {
	return throttleKey{key: "account:" + email, limit: maxFailuresPerAccount}
}

func addressKey(ip string) throttleKey {
	return throttleKey{key: "address:" + ip, limit: maxFailuresPerAddress}
}
