package id_test

import (
	"errors"
	"regexp"
	"sort"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/id"
)

var format = regexp.MustCompile(`^[0-7][0-9a-hjkmnp-tv-z]{25}$`)

func TestNewFormat(t *testing.T) {
	seen := map[string]bool{}
	for range 1000 {
		s := id.New()
		if !format.MatchString(s) {
			t.Fatalf("New() = %q, not a lowercase ULID", s)
		}
		if !id.Valid(s) {
			t.Fatalf("Valid(%q) = false", s)
		}
		if seen[s] {
			t.Fatalf("duplicate ID %q", s)
		}
		seen[s] = true
	}
}

func TestTimeRoundTrip(t *testing.T) {
	for _, ts := range []time.Time{
		time.UnixMilli(0).UTC(),
		time.Date(2026, 9, 26, 12, 34, 56, 789_000_000, time.UTC),
		time.UnixMilli(1<<48 - 1).UTC(),
	} {
		s := id.NewAt(ts)
		got, err := id.Time(s)
		if err != nil || !got.Equal(ts) {
			t.Errorf("Time(NewAt(%v)) = %v, %v", ts, got, err)
		}
	}
	if s := id.NewAt(time.Unix(-5, 0)); s[:10] != "0000000000" {
		t.Errorf("pre-epoch time not clamped: %q", s)
	}
}

func TestOrdering(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var ids []string
	for i := range 100 {
		ids = append(ids, id.NewAt(base.Add(time.Duration(i)*time.Millisecond)))
	}
	if !sort.StringsAreSorted(ids) {
		t.Error("IDs from increasing timestamps do not sort in order")
	}
}

func TestValid(t *testing.T) {
	tests := []struct {
		in   string
		want bool
	}{
		{"01j8z3q4r5s6t7v8w9x0y1z2a3", true},
		{"", false},
		{"01j8z3q4r5s6t7v8w9x0y1z2a", false},   // too short
		{"01j8z3q4r5s6t7v8w9x0y1z2a34", false}, // too long
		{"01J8Z3Q4R5S6T7V8W9X0Y1Z2A3", false},  // uppercase
		{"01j8z3q4r5s6t7v8w9x0y1z2i3", false},  // i is not in the alphabet
		{"01j8z3q4r5s6t7v8w9x0y1z2u3", false},  // u is not in the alphabet
		{"81j8z3q4r5s6t7v8w9x0y1z2a3", false},  // overflows 128 bits
		{"01j8z3q4r5s6t7v8w9x0y1z-a3", false},
	}
	for _, tt := range tests {
		if got := id.Valid(tt.in); got != tt.want {
			t.Errorf("Valid(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
	if _, err := id.Time("nope"); !errors.Is(err, id.ErrInvalid) {
		t.Errorf("Time(nope) err = %v, want ErrInvalid", err)
	}
}
