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
