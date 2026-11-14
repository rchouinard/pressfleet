package auth

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/redact"
)

// cheap keeps tests fast; verification reads parameters from the hash.
var cheap = Params{Memory: 64, Time: 1, Threads: 1, SaltLen: 16, KeyLen: 32}

func TestPasswordHash(t *testing.T) {
	ctx := context.Background()
	h := newHasher(cheap)
	enc, err := h.hash(ctx, "correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(enc.Reveal(), "$argon2id$v=19$m=64,t=1,p=1$") {
		t.Errorf("encoded hash = %q", enc.Reveal())
	}
	if again, _ := h.hash(ctx, "correct horse battery"); again == enc {
		t.Error("two hashes of the same password are equal; salt not random")
	}
	if ok, rehash, err := h.verify(ctx, enc, "correct horse battery"); !ok || rehash || err != nil {
		t.Errorf("verify correct = %v, %v, %v", ok, rehash, err)
	}
	if ok, _, err := h.verify(ctx, enc, "wrong password!!"); ok || err != nil {
		t.Errorf("verify wrong = %v, %v", ok, err)
	}
	// A hash made with other parameters still verifies and asks for a rehash.
	stronger := newHasher(Params{Memory: 128, Time: 2, Threads: 1, SaltLen: 16, KeyLen: 32})
	if ok, rehash, err := stronger.verify(ctx, enc, "correct horse battery"); !ok || !rehash || err != nil {
		t.Errorf("verify with new params = %v, %v, %v", ok, rehash, err)
	}
	for _, bad := range []string{"", "plain", "$argon2i$v=19$m=64,t=1,p=1$c2FsdHNhbHQ$aGFzaGhhc2hoYXNoaGFzaA",
		"$argon2id$v=19$m=0,t=1,p=1$c2FsdHNhbHQ$aGFzaGhhc2hoYXNoaGFzaA",
		"$argon2id$v=19$m=99999999,t=1,p=1$c2FsdHNhbHQ$aGFzaGhhc2hoYXNoaGFzaA"} {
		if _, _, err := h.verify(ctx, redact.Secret(bad), "x"); err == nil {
			t.Errorf("verify accepted malformed hash %q", bad)
		}
	}
}

func TestPasswordPolicy(t *testing.T) {
	for pw, ok := range map[string]bool{
		"short":                  false,
		"elevenchars":            false,
		"twelve chars":           true,
		"ünïcödé ünïc":           true, // 12 characters, more bytes
		strings.Repeat("a", 256): true,
		strings.Repeat("a", 257): false,
	} {
		if err := CheckPasswordPolicy(redact.Secret(pw)); (err == nil) != ok {
			t.Errorf("CheckPasswordPolicy(%d chars) = %v", len(pw), err)
		}
	}
}

func TestTokens(t *testing.T) {
	a, b := newAPIToken(), newAPIToken()
	if a == b || !looksLikeAPIToken(a) || len(a.Reveal()) != 56 || !strings.HasPrefix(a.Reveal(), "pft_") {
		t.Errorf("API token %q", a.Reveal())
	}
	if displayPrefix(a) != a.Reveal()[:8] {
		t.Errorf("prefix %q", displayPrefix(a))
	}
	for _, bad := range []string{"", "pft_", "xyz_" + a.Reveal()[4:], a.Reveal() + "a"} {
		if looksLikeAPIToken(redact.Secret(bad)) {
			t.Errorf("looksLikeAPIToken(%q) = true", bad)
		}
	}
	if hashToken(a) == hashToken(b) || len(hashToken(a)) != 64 {
		t.Error("token hashes")
	}

	s1, s2 := newSessionToken(), newSessionToken()
	c1 := CSRFToken(s1)
	if !ValidCSRFToken(s1, c1) || ValidCSRFToken(s2, c1) || ValidCSRFToken(s1, "") || ValidCSRFToken(s1, "!!") {
		t.Error("CSRF token validation")
	}
	if CSRFToken(s1) != c1 || c1 == CSRFToken(s2) || c1.Reveal() == s1.Reveal() {
		t.Error("CSRF tokens must be deterministic per session and differ between sessions")
	}
}

func TestThrottle(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	th := newThrottle(func() time.Time { return now })
	acct, addr := accountKey("a@example.com"), addressKey("192.0.2.1")
	for i := range maxFailuresPerAccount {
		if err := th.check(acct, addr); err != nil {
			t.Fatalf("throttled after %d failures", i)
		}
		th.fail(acct, addr)
	}
	err := th.check(acct, addr)
	var te *ThrottledError
	if !errors.Is(err, ErrThrottled) || !errors.As(err, &te) || te.RetryAfter != throttleWindow {
		t.Fatalf("check = %v", err)
	}
	// Another account from the same address still works until the
	// address limit.
	if err := th.check(accountKey("b@example.com"), addr); err != nil {
		t.Errorf("other account throttled: %v", err)
	}
	now = now.Add(throttleWindow)
	if err := th.check(acct, addr); err != nil {
		t.Errorf("still throttled after the window: %v", err)
	}
	th.fail(acct)
	th.reset(acct)
	if th.size() != 0 {
		t.Errorf("entries after reset: %v", th.entries)
	}
}

func TestAddressKey(t *testing.T) {
	tests := []struct {
		a, b string
		same bool
	}{
		{"192.0.2.1", "192.0.2.1", true},
		{"192.0.2.1", "192.0.2.2", false},
		{"::ffff:192.0.2.1", "192.0.2.1", true},
		{"2001:db8:1:2::1", "2001:db8:1:2:ffff:ffff:ffff:ffff", true},
		{"2001:db8:1:2::1", "2001:db8:1:3::1", false},
		{"fe80::1%eth0", "fe80::2", true},
		{"not an address", "not an address", true},
	}
	for _, tt := range tests {
		if got := addressKey(tt.a).key == addressKey(tt.b).key; got != tt.same {
			t.Errorf("addressKey(%q) == addressKey(%q) is %v, want %v (%q, %q)",
				tt.a, tt.b, got, tt.same, addressKey(tt.a).key, addressKey(tt.b).key)
		}
	}
}

// size is the number of counters of every kind.
func (t *throttle) size() (n int) {
	for _, table := range t.entries {
		n += len(table)
	}
	return n
}

// A client that fills the table from many addresses, with invented
// emails, must not reset the counter of an account it is guessing.
func TestThrottlePruneKeepsAccounts(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	th := newThrottle(func() time.Time { return now })
	th.max = 10
	target := accountKey("victim@example.com")
	for range maxFailuresPerAccount {
		th.fail(target, addressKey("192.0.2.1"))
	}
	for i := range 10 * th.max {
		now = now.Add(time.Second)
		th.fail(accountKey(fmt.Sprintf("ghost%d@example.com", i)), addressKey(fmt.Sprintf("2001:db8:%x::1", i)))
		if th.size() > int(throttleKinds)*th.max {
			t.Fatalf("%d entries, limit %d per kind", th.size(), th.max)
		}
		if th.check(target) == nil {
			t.Fatalf("target account no longer throttled after %d invented failures", i+1)
		}
	}
}

// Filling the account table with live entries (invented emails) must not
// clear the per-address or reauthentication counters: before, it deleted
// all of them on every failed login, so one address could guess without
// limit.
func TestThrottleKindsAreIndependent(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	th := newThrottle(func() time.Time { return now })
	th.max = 100
	addr := addressKey("192.0.2.1")
	opts := optionsKeys("user1", "192.0.2.1")
	pend := pendingKeys("user1", "192.0.2.1")
	for range maxFailuresPerAddress {
		th.fail(addr)
	}
	for range maxOptionsPerUser {
		th.fail(opts...)
		th.fail(pend...)
	}
	for i := range 5 * th.max {
		now = now.Add(time.Millisecond)
		th.fail(accountKey(fmt.Sprintf("ghost%d@example.com", i)), addressKey("192.0.2.1"))
		if i >= th.max { // the account table has been full for a while
			if th.check(addr) == nil {
				t.Fatalf("address no longer throttled after %d invented emails", i+1)
			}
			if th.check(opts...) == nil || th.check(pend...) == nil {
				t.Fatalf("action counters cleared after %d invented emails", i+1)
			}
		}
		if n := len(th.entries[kindAccount]); n > th.max {
			t.Fatalf("%d account entries, limit %d", n, th.max)
		}
	}
	if len(th.entries[kindAccount]) < th.max-th.max/100 {
		t.Errorf("account table = %d entries, want it near its limit %d", len(th.entries[kindAccount]), th.max)
	}

	// And the other way: filling the address table leaves accounts alone.
	target := accountKey("victim@example.com")
	for range maxFailuresPerAccount {
		th.fail(target)
	}
	for i := range 5 * th.max {
		now = now.Add(time.Millisecond)
		th.fail(addressKey(fmt.Sprintf("2001:db8:%x::1", i)))
		if th.check(target) == nil {
			t.Fatalf("account counter cleared after %d invented addresses", i+1)
		}
	}
	if n := len(th.entries[kindAddress]); n > th.max {
		t.Errorf("%d address entries, limit %d", n, th.max)
	}
}

// Acceptance: with a full account table at the real cap, a single address is
// still throttled after its allowed failures.
func TestThrottleFullAccountTableAtDefaultCap(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	th := newThrottle(func() time.Time { return now })
	for i := range th.max {
		th.fail(accountKey(fmt.Sprintf("ghost%d@example.com", i)))
	}
	if len(th.entries[kindAccount]) != th.max {
		t.Fatalf("account table = %d, want %d", len(th.entries[kindAccount]), th.max)
	}
	addr := addressKey("192.0.2.1")
	for i := range maxFailuresPerAddress {
		if err := th.check(addr); err != nil {
			t.Fatalf("throttled after %d failures", i)
		}
		th.fail(accountKey(fmt.Sprintf("more%d@example.com", i)), addr)
	}
	if err := th.check(addr); !errors.Is(err, ErrThrottled) {
		t.Errorf("check = %v, want throttled", err)
	}
}

func TestThrottlePrune(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	th := newThrottle(func() time.Time { return now })
	th.max = 3
	// Ended windows go first.
	th.fail(accountKey("ended@example.com"))
	now = now.Add(throttleWindow)
	th.fail(accountKey("a@example.com"))
	th.fail(accountKey("b@example.com"))
	now = now.Add(time.Second)
	th.fail(accountKey("c@example.com"))
	if _, ok := th.entries[kindAccount]["account:ended@example.com"]; ok || len(th.entries[kindAccount]) != 3 {
		t.Errorf("entries = %v, want the ended window pruned", th.entries[kindAccount])
	}
	// Then the counter with the fewest failures, newest first: c is evicted
	// when d arrives, since it has as few failures as b but came later.
	th.fail(accountKey("a@example.com"))
	now = now.Add(time.Second)
	th.fail(accountKey("d@example.com"))
	for _, k := range []string{"account:a@example.com", "account:b@example.com", "account:d@example.com"} {
		if _, ok := th.entries[kindAccount][k]; !ok {
			t.Errorf("entries = %v, want %s kept", th.entries[kindAccount], k)
		}
	}
	if _, ok := th.entries[kindAccount]["account:c@example.com"]; ok {
		t.Errorf("entries = %v, want c evicted", th.entries[kindAccount])
	}
}
