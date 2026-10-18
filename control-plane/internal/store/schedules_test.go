package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
)

func TestSchedulePhase(t *testing.T) {
	day := 24 * time.Hour
	pinned := store.Schedule{Name: "a", Interval: day, Offset: 2 * time.Hour, Stagger: time.Second}
	if got := pinned.Phase(); got != 2*time.Hour {
		t.Errorf("one-second stagger: phase %s, want the offset", got)
	}
	for _, name := range []string{"wp-cron:a", "wp-cron:b", "backup:c"} {
		s := store.Schedule{Name: name, Interval: day, Offset: 23 * time.Hour, Stagger: 3 * time.Hour}
		p := s.Phase()
		if p != s.Phase() {
			t.Errorf("%s: phase isn't deterministic", name)
		}
		// Offset 23h plus a stagger under 3h wraps past midnight.
		if p%time.Second != 0 || !(p >= 23*time.Hour || p < 2*time.Hour) {
			t.Errorf("%s: phase %s outside 23:00-02:00", name, p)
		}
	}
	if got := (store.Schedule{Name: "x"}).Phase(); got != 0 {
		t.Errorf("no interval: phase %s", got)
	}
}

func TestScheduleNextRunAfter(t *testing.T) {
	daily := store.Schedule{Name: "d", Interval: 24 * time.Hour, Offset: 2 * time.Hour, Stagger: time.Second}
	date := func(y int, m time.Month, d, h int) time.Time { return time.Date(y, m, d, h, 0, 0, 0, time.UTC) }
	tests := []struct {
		s    store.Schedule
		t    time.Time
		want time.Time
	}{
		{daily, date(2026, 10, 18, 1), date(2026, 10, 18, 2)},
		{daily, date(2026, 10, 18, 2), date(2026, 10, 19, 2)}, // strictly after
		{daily, date(2026, 10, 18, 2).Add(-time.Nanosecond), date(2026, 10, 18, 2)},
		{daily, date(1969, 12, 31, 1), date(1969, 12, 31, 2)}, // before the epoch
		{daily, date(1969, 12, 31, 3), date(1970, 1, 1, 2)},
		{store.Schedule{Name: "m", Interval: time.Minute, Stagger: time.Second}, date(2026, 1, 1, 0).Add(90 * time.Second), date(2026, 1, 1, 0).Add(2 * time.Minute)},
		{store.Schedule{Name: "none"}, date(2026, 1, 1, 0), time.Time{}},
	}
	for _, tt := range tests {
		if got := tt.s.NextRunAfter(tt.t); !got.Equal(tt.want) {
			t.Errorf("%s every %s: NextRunAfter(%s) = %s, want %s", tt.s.Name, tt.s.Interval, tt.t, got, tt.want)
		}
	}
}

func TestSchedules(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		r := store.NewSchedules(db)
		now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
		valid := func(name, target string) *store.Schedule {
			return &store.Schedule{
				Name: name, JobType: "test.job", TargetType: store.JobTargetSite, TargetID: target,
				Interval: time.Hour, MissedRunPolicy: store.MissedCoalesce,
			}
		}

		for name, s := range map[string]store.Schedule{
			"no name":            {JobType: "j", TargetType: "t", TargetID: "1", Interval: time.Hour, MissedRunPolicy: store.MissedSkip},
			"sub-second":         {Name: "n", JobType: "j", TargetType: "t", TargetID: "1", Interval: 1500 * time.Millisecond, MissedRunPolicy: store.MissedSkip},
			"offset too big":     {Name: "n", JobType: "j", TargetType: "t", TargetID: "1", Interval: time.Hour, Offset: time.Hour, MissedRunPolicy: store.MissedSkip},
			"negative stagger":   {Name: "n", JobType: "j", TargetType: "t", TargetID: "1", Interval: time.Hour, Stagger: -time.Second, MissedRunPolicy: store.MissedSkip},
			"unknown policy":     {Name: "n", JobType: "j", TargetType: "t", TargetID: "1", Interval: time.Hour, MissedRunPolicy: "sometimes"},
			"payload not object": {Name: "n", JobType: "j", TargetType: "t", TargetID: "1", Interval: time.Hour, MissedRunPolicy: store.MissedSkip, Payload: json.RawMessage(`[1]`)},
		} {
			if err := r.Upsert(ctx, &s, now); err == nil {
				t.Errorf("%s: Upsert succeeded", name)
			}
		}

		a := valid("a", "site-a")
		if err := r.Upsert(ctx, a, now); err != nil {
			t.Fatal(err)
		}
		if a.ID == "" || a.MaxAttempts != store.DefaultMaxAttempts || a.Stagger != time.Hour || string(a.Payload) != "{}" ||
			!a.NextRunAt.After(now) || !a.LastRunAt.IsZero() {
			t.Errorf("after create: %+v", a)
		}
		got, err := r.Get(ctx, "a")
		if err != nil || got.ID != a.ID || !got.NextRunAt.Equal(a.NextRunAt) || got.TargetID != "site-a" {
			t.Errorf("Get = %+v, %v", got, err)
		}

		// Same timing keeps the next run even when upserted later; new
		// timing recomputes it.
		again := valid("a", "site-a")
		again.JobType = "test.other"
		if err := r.Upsert(ctx, again, now.Add(3*time.Hour)); err != nil {
			t.Fatal(err)
		}
		if again.ID != a.ID || !again.NextRunAt.Equal(a.NextRunAt) || again.JobType != "test.other" {
			t.Errorf("same timing: %+v, want ID and next run kept", again)
		}
		retimed := valid("a", "site-a")
		retimed.Interval = 2 * time.Hour
		if err := r.Upsert(ctx, retimed, now.Add(3*time.Hour)); err != nil {
			t.Fatal(err)
		}
		if !retimed.NextRunAt.After(now.Add(3 * time.Hour)) {
			t.Errorf("new timing: next run %s, want after the upsert", retimed.NextRunAt)
		}

		b := valid("b", "site-b")
		if err := r.Upsert(ctx, b, now); err != nil {
			t.Fatal(err)
		}
		c := valid("c", "site-b")
		if err := r.Upsert(ctx, c, now); err != nil {
			t.Fatal(err)
		}
		all, err := r.List(ctx)
		if err != nil || len(all) != 3 || all[0].Name != "a" || all[2].Name != "c" {
			t.Errorf("List = %d schedules, %v", len(all), err)
		}

		due, err := r.Due(ctx, b.NextRunAt, 10)
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range due {
			if s.NextRunAt.After(b.NextRunAt) {
				t.Errorf("Due returned %s, next run %s after %s", s.Name, s.NextRunAt, b.NextRunAt)
			}
		}
		if len(due) == 0 {
			t.Fatal("Due returned nothing at b's next run")
		}
		if limited, err := r.Due(ctx, now.Add(48*time.Hour), 1); err != nil || len(limited) != 1 {
			t.Errorf("Due with limit 1 = %d, %v", len(limited), err)
		}

		next := b.NextRunAfter(b.NextRunAt)
		if ok, err := r.Claim(ctx, b.ID, b.NextRunAt, next, b.NextRunAt); err != nil || !ok {
			t.Fatalf("first claim = %v, %v", ok, err)
		}
		if ok, err := r.Claim(ctx, b.ID, b.NextRunAt, next, b.NextRunAt); err != nil || ok {
			t.Errorf("second claim of the same slot = %v, %v; want false", ok, err)
		}
		if claimed, err := r.Get(ctx, "b"); err != nil || !claimed.NextRunAt.Equal(next) || !claimed.LastRunAt.Equal(b.NextRunAt) {
			t.Errorf("after claim: %+v, %v", claimed, err)
		}

		if n, err := r.DeleteForTarget(ctx, store.JobTargetSite, "site-b"); err != nil || n != 2 {
			t.Errorf("DeleteForTarget = %d, %v; want 2", n, err)
		}
		if err := r.Delete(ctx, "a"); err != nil {
			t.Fatal(err)
		}
		if err := r.Delete(ctx, "a"); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("deleting twice: %v, want ErrNotFound", err)
		}
		if _, err := r.Get(ctx, "a"); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("Get deleted: %v, want ErrNotFound", err)
		}
	})
}
