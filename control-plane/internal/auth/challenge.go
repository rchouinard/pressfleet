package auth

import (
	"sync"
	"time"
)

// maxChallenges bounds a challenge store's memory. Only a correct password
// creates a pending login, so filling the store takes valid credentials;
// the bound still keeps a client with some from growing it without limit.
const maxChallenges = 10_000

// challengeStore holds short-lived, single-use values in memory, keyed by
// the hash of a random challenge (a pending-login token, later a WebAuthn
// challenge) rather than by user, so flows that start before the user is
// known (passwordless sign-in) can use it too. Like the throttle, it is
// lost on restart, which only means starting again.
type challengeStore[T any] struct {
	mu      sync.Mutex
	now     func() time.Time
	max     int
	entries map[string]challengeEntry[T]
}

type challengeEntry[T any] struct {
	value   T
	expires time.Time
}

func newChallengeStore[T any](now func() time.Time) *challengeStore[T] {
	return &challengeStore[T]{now: now, max: maxChallenges, entries: map[string]challengeEntry[T]{}}
}

// put stores v under key until expires. A full store first drops expired
// entries, then the one that would expire soonest.
func (c *challengeStore[T]) put(key string, v T, expires time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.entries[key]; !ok && len(c.entries) >= c.max {
		c.prune()
	}
	c.entries[key] = challengeEntry[T]{value: v, expires: expires}
}

// take removes and returns the value under key, and when it expires. It
// reports false if there is none or it has expired. Taking is what makes a
// challenge single use: of two concurrent requests, only one gets it.
func (c *challengeStore[T]) take(key string) (T, time.Time, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	delete(c.entries, key)
	if !ok || !c.now().Before(e.expires) {
		var zero T
		return zero, time.Time{}, false
	}
	return e.value, e.expires, true
}

func (c *challengeStore[T]) prune() {
	now := c.now()
	var (
		victim string
		first  time.Time
	)
	for k, e := range c.entries {
		if !now.Before(e.expires) {
			delete(c.entries, k)
			continue
		}
		if victim == "" || e.expires.Before(first) {
			victim, first = k, e.expires
		}
	}
	if len(c.entries) >= c.max {
		delete(c.entries, victim)
	}
}

// size returns the number of entries, expired ones included, for tests.
func (c *challengeStore[T]) size() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}
