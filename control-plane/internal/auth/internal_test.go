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
	if len(th.entries) != 0 {
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
		if len(th.entries) > th.max {
			t.Fatalf("%d entries, limit %d", len(th.entries), th.max)
		}
		if th.check(target) == nil {
			t.Fatalf("target account no longer throttled after %d invented failures", i+1)
		}
	}
}

func TestThrottlePrune(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	th := newThrottle(func() time.Time { return now })
	th.max = 3
	// Ended windows go first.
	th.fail(accountKey("ended@example.com"))
	now = now.Add(throttleWindow)
	th.fail(accountKey("a@example.com"), addressKey("192.0.2.1"))
	th.fail(accountKey("b@example.com"))
	if _, ok := th.entries["account:ended@example.com"]; ok {
		t.Errorf("entries = %v, want the ended window pruned", th.entries)
	}
	// Then address counters, before any live account counter.
	now = now.Add(time.Second)
	th.fail(accountKey("c@example.com"))
	if _, ok := th.entries["address:192.0.2.1"]; ok || len(th.entries) != 3 {
		t.Errorf("entries = %v, want a, b, and c", th.entries)
	}
	// Then the account counter with the fewest failures, newest first:
	// c, which has as few as b but came later.
	th.fail(accountKey("a@example.com"))
	now = now.Add(time.Second)
	th.fail(accountKey("d@example.com"))
	for _, k := range []string{"account:a@example.com", "account:b@example.com", "account:d@example.com"} {
		if _, ok := th.entries[k]; !ok {
			t.Errorf("entries = %v, want %s kept", th.entries, k)
		}
	}
}
