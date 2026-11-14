package auth

import (
	"cmp"
	"errors"
	"fmt"
	"net/netip"
	"slices"
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
	// Pending second-factor logins are counted the same way.
	maxPendingPerUser    = 20
	maxPendingPerAddress = 60
	// maxThrottleEntries bounds memory if a client invents many emails or
	// addresses. It applies to each kind of counter separately (see
	// throttleKind), so filling one can't push out another.
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

// throttleKind groups counters that are bounded and evicted separately. A
// client controls how many counters of some kinds exist (it can invent
// emails for the account kind, or hop between addresses for the address
// kind), so if they shared a table, filling it with one kind would evict
// the other and reset its limits.
type throttleKind int

const (
	// kindAccount counts failed logins per email address.
	kindAccount throttleKind = iota
	// kindAddress counts failed logins per client address.
	kindAddress
	// kindAction counts reauthentication option requests and pending
	// second-factor logins, per user and per address.
	kindAction
	throttleKinds
)

// throttle counts recent failures per key in memory.
type throttle struct {
	mu  sync.Mutex
	now func() time.Time
	// max bounds each kind's table.
	max     int
	entries [throttleKinds]map[string]*throttleEntry
}

type throttleEntry struct {
	failures int
	start    time.Time
}

func newThrottle(now func() time.Time) *throttle {
	t := &throttle{now: now, max: maxThrottleEntries}
	for i := range t.entries {
		t.entries[i] = map[string]*throttleEntry{}
	}
	return t
}

// check returns a *ThrottledError if any key has reached its limit.
func (t *throttle) check(keys ...throttleKey) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	var wait time.Duration
	for _, k := range keys {
		e := t.live(k, now)
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
		e := t.live(k, now)
		if e == nil {
			if len(t.entries[k.kind]) >= t.max {
				t.prune(k.kind, now)
			}
			e = &throttleEntry{start: now}
			t.entries[k.kind][k.key] = e
		}
		e.failures++
	}
}

// reset forgets the failures for a key, after a success.
func (t *throttle) reset(k throttleKey) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.entries[k.kind], k.key)
}

// live returns the entry for key if its window hasn't ended.
func (t *throttle) live(k throttleKey, now time.Time) *throttleEntry {
	e, ok := t.entries[k.kind][k.key]
	if !ok {
		return nil
	}
	if !now.Before(e.start.Add(throttleWindow)) {
		delete(t.entries[k.kind], k.key)
		return nil
	}
	return e
}

// prune makes room in one kind's table, and never touches another's. It
// drops ended windows, then, if the table is still full, evicts the
// counters furthest from their limit: the fewest failures first, the newest
// first among those. A client that fills the table with invented keys
// therefore evicts its own one-failure entries, not the counter of an
// account it is guessing. Evicting 1% at a time (at least one) keeps a
// full table from costing a scan on every new key.
func (t *throttle) prune(kind throttleKind, now time.Time) {
	table := t.entries[kind]
	for k, e := range table {
		if !now.Before(e.start.Add(throttleWindow)) {
			delete(table, k)
		}
	}
	if len(table) < t.max {
		return
	}
	type candidate struct {
		key string
		e   *throttleEntry
	}
	all := make([]candidate, 0, len(table))
	for k, e := range table {
		all = append(all, candidate{k, e})
	}
	slices.SortFunc(all, func(a, b candidate) int {
		if c := cmp.Compare(a.e.failures, b.e.failures); c != 0 {
			return c
		}
		return b.e.start.Compare(a.e.start)
	})
	for _, c := range all[:max(1, t.max/100)] {
		delete(table, c.key)
	}
}

type throttleKey struct {
	key   string
	limit int
	kind  throttleKind
}

func accountKey(email string) throttleKey {
	return throttleKey{key: "account:" + email, limit: maxFailuresPerAccount, kind: kindAccount}
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
	return throttleKey{key: "address:" + ip, limit: maxFailuresPerAddress, kind: kindAddress}
}

// optionsKeys are the counters of reauthentication option requests for a
// user and a client address.
func optionsKeys(userID, ip string) []throttleKey {
	a := addressKey(ip)
	return []throttleKey{
		{key: "reauth-options-user:" + userID, limit: maxOptionsPerUser, kind: kindAction},
		{key: "reauth-options-" + a.key, limit: maxOptionsPerAddress, kind: kindAction},
	}
}

// pendingKeys are the counters of pending second-factor logins started for
// a user and from a client address.
func pendingKeys(userID, ip string) []throttleKey {
	a := addressKey(ip)
	return []throttleKey{
		{key: "pending-login-user:" + userID, limit: maxPendingPerUser, kind: kindAction},
		{key: "pending-login-" + a.key, limit: maxPendingPerAddress, kind: kindAction},
	}
}
