package auth

import (
	"errors"
	"fmt"
	"net/netip"
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
	// Reauthentication options are counted per call, not per failure: each
	// one stores a challenge. The limits leave room for a person who
	// reopens the prompt, but not for a loop. They're separate keys from
	// the login's, so asking for options never locks anyone out of logging
	// in.
	maxOptionsPerUser    = 20
	maxOptionsPerAddress = 60
	// maxThrottleEntries bounds memory if a client invents many emails or
	// addresses.
	maxThrottleEntries = 100_000
	// ipv6ThrottlePrefix groups IPv6 clients: one client usually holds a
	// whole /64 and can pick any address in it.
	ipv6ThrottlePrefix = 64
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
	max     int
	entries map[string]*throttleEntry
}

type throttleEntry struct {
	failures int
	start    time.Time
	// account marks a per-account counter, which pruning keeps longest:
	// it is what stops guessing one user's password.
	account bool
}

func newThrottle(now func() time.Time) *throttle {
	return &throttle{now: now, max: maxThrottleEntries, entries: map[string]*throttleEntry{}}
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
			if len(t.entries) >= t.max {
				t.prune(now)
			}
			e = &throttleEntry{start: now, account: k.account}
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

// prune makes room for a new entry. It drops ended windows, then, if the
// table is still full, every per-address counter, and only then the one
// account counter with the fewest failures (the newest of those). Account
// counters are never dropped wholesale, and the ones closest to their
// limit go last: a client that fills the table with invented emails from
// many addresses must not reset the count of a password it is guessing.
func (t *throttle) prune(now time.Time) {
	for k, e := range t.entries {
		if !now.Before(e.start.Add(throttleWindow)) {
			delete(t.entries, k)
		}
	}
	if len(t.entries) < t.max {
		return
	}
	for k, e := range t.entries {
		if !e.account {
			delete(t.entries, k)
		}
	}
	if len(t.entries) < t.max {
		return
	}
	var (
		victim string
		least  *throttleEntry
	)
	for k, e := range t.entries {
		if least == nil || e.failures < least.failures ||
			(e.failures == least.failures && e.start.After(least.start)) {
			victim, least = k, e
		}
	}
	delete(t.entries, victim)
}

type throttleKey struct {
	key     string
	limit   int
	account bool
}

func accountKey(email string) throttleKey {
	return throttleKey{key: "account:" + email, limit: maxFailuresPerAccount, account: true}
}

// addressKey counts an IPv4 client by its address and an IPv6 client by
// its /64, so rotating addresses within one network doesn't escape the
// limit. Input that isn't an address (the API always passes one) is used
// as is.
func addressKey(ip string) throttleKey {
	if a, err := netip.ParseAddr(ip); err == nil {
		a = a.Unmap()
		if a.Is6() {
			ip = netip.PrefixFrom(a.WithZone(""), ipv6ThrottlePrefix).Masked().String()
		} else {
			ip = a.String()
		}
	}
	return throttleKey{key: "address:" + ip, limit: maxFailuresPerAddress}
}

// optionsKeys are the counters of reauthentication option requests for a
// user and a client address.
func optionsKeys(userID, ip string) []throttleKey {
	a := addressKey(ip)
	return []throttleKey{
		{key: "reauth-options-user:" + userID, limit: maxOptionsPerUser},
		{key: "reauth-options-" + a.key, limit: maxOptionsPerAddress},
	}
}
