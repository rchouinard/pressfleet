package auth

import (
	"fmt"
	"testing"
	"time"
)

func TestChallengeStore(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	c := newChallengeStore[string](func() time.Time { return now })

	c.put("a", "first", now.Add(time.Minute))
	if v, exp, ok := c.take("a"); !ok || v != "first" || !exp.Equal(now.Add(time.Minute)) {
		t.Errorf("take = %q, %v, %v", v, exp, ok)
	}
	// Taking uses it up.
	if _, _, ok := c.take("a"); ok {
		t.Error("taken twice")
	}

	// Entries expire.
	c.put("b", "second", now.Add(time.Minute))
	now = now.Add(time.Minute)
	if _, _, ok := c.take("b"); ok {
		t.Error("expired entry taken")
	}
	if n := c.size(); n != 0 {
		t.Errorf("%d entries left after taking", n)
	}
}

func TestChallengeStoreBounded(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	c := newChallengeStore[int](func() time.Time { return now })
	c.max = 3
	c.put("expired", 0, now.Add(time.Second))
	c.put("soonest", 1, now.Add(2*time.Minute))
	c.put("later", 2, now.Add(3*time.Minute))
	now = now.Add(2 * time.Second)

	// A full store drops expired entries first.
	c.put("new", 3, now.Add(5*time.Minute))
	if n := c.size(); n != 3 {
		t.Fatalf("%d entries, want 3", n)
	}
	// Then the one that would expire soonest.
	c.put("newer", 4, now.Add(5*time.Minute))
	if _, _, ok := c.take("soonest"); ok {
		t.Error("the soonest-expiring entry survived pruning")
	}
	for _, k := range []string{"later", "new", "newer"} {
		if _, _, ok := c.take(k); !ok {
			t.Errorf("%s was dropped", k)
		}
	}
	// Replacing an entry in a full store doesn't drop another.
	for i := range 3 {
		c.put(fmt.Sprint(i), i, now.Add(time.Minute))
	}
	c.put("0", 10, now.Add(time.Minute))
	if n := c.size(); n != 3 {
		t.Errorf("%d entries after replacing one, want 3", n)
	}
}

func TestChallengeStoreOwnerLimit(t *testing.T) {
	type v struct{ owner, name string }
	now := time.Unix(1_700_000_000, 0)
	c := newOwnedChallengeStore(func() time.Time { return now }, func(x v) string { return x.owner })
	c.perOwner = 3

	// A's challenges are put a second apart, so the oldest is clear.
	for i := range 3 {
		c.put(fmt.Sprint("a", i), v{"a", "x"}, now.Add(time.Minute+time.Duration(i)*time.Second))
	}
	c.put("b", v{"b", "x"}, now.Add(time.Minute))
	c.put("anon", v{"", "x"}, now.Add(time.Minute))
	c.put("a3", v{"a", "x"}, now.Add(time.Minute+3*time.Second))

	if _, _, ok := c.take("a0"); ok {
		t.Error("a's oldest survived its limit")
	}
	for _, k := range []string{"a1", "a2", "a3", "b", "anon"} {
		if _, _, ok := c.take(k); !ok {
			t.Errorf("%s was dropped", k)
		}
	}
	if n := c.size(); n != 0 || len(c.byOwner) != 0 {
		t.Errorf("%d entries and %d owners left after taking everything", n, len(c.byOwner))
	}

	// Expired entries go before live ones, and putting one back after a
	// take counts like any other put.
	c.put("old", v{"a", "x"}, now.Add(time.Second))
	c.put("live1", v{"a", "x"}, now.Add(time.Hour))
	c.put("live2", v{"a", "x"}, now.Add(2*time.Hour))
	now = now.Add(time.Minute)
	c.put("live3", v{"a", "x"}, now.Add(3*time.Hour))
	for _, k := range []string{"live1", "live2", "live3"} {
		if _, _, ok := c.take(k); !ok {
			t.Errorf("%s was dropped instead of the expired entry", k)
		}
	}
}

func TestChallengeStoreOwnerLimitWithinTotalBound(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	c := newOwnedChallengeStore(func() time.Time { return now }, func(s string) string { return s[:1] })
	c.max = 3
	c.put("a1", "a", now.Add(time.Minute))
	c.put("b1", "b", now.Add(2*time.Minute))
	c.put("c1", "c", now.Add(3*time.Minute))
	c.put("d1", "d", now.Add(4*time.Minute))
	if n, o := c.size(), len(c.byOwner); n != 3 || o != 3 {
		t.Errorf("%d entries, %d owners; want 3 and 3", n, o)
	}
}
