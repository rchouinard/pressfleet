package auth

import (
	"sync"
	"time"
)

// maxChallenges bounds a challenge store's memory. Only a correct password
// creates a pending login, so filling the store takes valid credentials;
// the bound still keeps a client with some from growing it without limit.
const maxChallenges = 10_000

// maxChallengesPerOwner bounds the live challenges one user holds (logins
// and reauthentications, which each start one per call). A user's oldest
// goes first, so a user who floods the store evicts only their own, never
// another user's in-flight login. It leaves room for a few open tabs.
const maxChallengesPerOwner = 5

// challengeStore holds short-lived, single-use values in memory, keyed by
// the hash of a random challenge (a pending-login token, later a WebAuthn
// challenge) rather than by user, so flows that start before the user is
// known (passwordless sign-in) can use it too: a value with no owner counts
// against the store's bound only. Like the throttle, it is
// lost on restart, which only means starting again.
type challengeStore[T any] struct {
	mu  sync.Mutex
	now func() time.Time
	max int
	// owner names the user a value belongs to, or nil if the store has no
	// per-user bound. An empty owner is a value that isn't tied to a user.
	owner    func(T) string
	perOwner int
	entries  map[string]challengeEntry[T]
	// byOwner indexes entries by owner, so the cap doesn't scan the store.
	byOwner map[string]map[string]struct{}
	seq     uint64
}

type challengeEntry[T any] struct {
	value   T
	expires time.Time
	// seq orders entries put with the same expiry (a clock with coarse
	// ticks, or calls in one instant), so "oldest" is never a coin flip.
	seq uint64
}

func newChallengeStore[T any](now func() time.Time) *challengeStore[T] {
	return &challengeStore[T]{
		now: now, max: maxChallenges, perOwner: maxChallengesPerOwner,
		entries: map[string]challengeEntry[T]{}, byOwner: map[string]map[string]struct{}{},
	}
}

// newOwnedChallengeStore returns a store that also keeps each owner to
// maxChallengesPerOwner live values.
func newOwnedChallengeStore[T any](now func() time.Time, owner func(T) string) *challengeStore[T] {
	c := newChallengeStore[T](now)
	c.owner = owner
	return c
}

// put stores v under key until expires. An owner already at its limit
// loses its entry that would expire soonest (its oldest). A full store
// then drops expired entries, then the one that would expire soonest.
func (c *challengeStore[T]) put(key string, v T, expires time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	owner := ""
	if c.owner != nil {
		owner = c.owner(v)
	}
	if _, ok := c.entries[key]; ok {
		c.remove(key)
	}
	if owner != "" {
		c.evictOwned(owner)
	}
	if len(c.entries) >= c.max {
		c.prune()
	}
	c.seq++
	c.entries[key] = challengeEntry[T]{value: v, expires: expires, seq: c.seq}
	if owner != "" {
		if c.byOwner[owner] == nil {
			c.byOwner[owner] = map[string]struct{}{}
		}
		c.byOwner[owner][key] = struct{}{}
	}
}

// evictOwned makes room for one more entry of owner: expired ones go
// first, then the oldest live ones.
func (c *challengeStore[T]) evictOwned(owner string) {
	now := c.now()
	for len(c.byOwner[owner]) >= c.perOwner {
		var victim string
		for k := range c.byOwner[owner] {
			e := c.entries[k]
			if !now.Before(e.expires) {
				victim = k
				break
			}
			if victim == "" || older(e, c.entries[victim]) {
				victim = k
			}
		}
		c.remove(victim)
	}
}

// remove deletes the entry under key and its owner index.
func (c *challengeStore[T]) remove(key string) {
	e, ok := c.entries[key]
	if !ok {
		return
	}
	delete(c.entries, key)
	if c.owner == nil {
		return
	}
	if o := c.owner(e.value); o != "" {
		delete(c.byOwner[o], key)
		if len(c.byOwner[o]) == 0 {
			delete(c.byOwner, o)
		}
	}
}

// take removes and returns the value under key, and when it expires. It
// reports false if there is none or it has expired. Taking is what makes a
// challenge single use: of two concurrent requests, only one gets it.
func (c *challengeStore[T]) take(key string) (T, time.Time, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	c.remove(key)
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
			c.remove(k)
			continue
		}
		if victim == "" || e.expires.Before(first) {
			victim, first = k, e.expires
		}
	}
	if len(c.entries) >= c.max {
		c.remove(victim)
	}
}

// size returns the number of entries, expired ones included, for tests.
func (c *challengeStore[T]) size() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}

// older reports whether a would expire before b, or was put before it.
func older[T any](a, b challengeEntry[T]) bool {
	if !a.expires.Equal(b.expires) {
		return a.expires.Before(b.expires)
	}
	return a.seq < b.seq
}
