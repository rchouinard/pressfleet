package jobs_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/jobs"
	"github.com/rchouinard/pressfleet/control-plane/internal/jobs/jobstest"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
)

func TestStaggerSpreadsSchedules(t *testing.T) {
	const sites = 50
	interval := 5 * time.Minute
	seconds := map[time.Duration]int{}
	buckets := make([]int, 10) // 30-second buckets
	for i := range sites {
		s := store.Schedule{Name: fmt.Sprintf("wp-cron:site-%02d", i), Interval: interval}
		p := s.Phase()
		if p < 0 || p >= interval {
			t.Fatalf("%s: phase %s outside [0, %s)", s.Name, p, interval)
		}
		if again := s.Phase(); again != p {
			t.Fatalf("%s: phase not deterministic: %s then %s", s.Name, p, again)
		}
		seconds[p]++
		buckets[p/(30*time.Second)]++
	}
	if len(seconds) < 40 {
		t.Errorf("%d schedules share only %d distinct seconds", sites, len(seconds))
	}
	for i, n := range buckets {
		if n == 0 || n > 12 {
			t.Errorf("bucket %d holds %d of %d schedules: %v", i, n, sites, buckets)
		}
	}

	// A window keeps the stagger after the offset: daily backups between
	// 01:00 and 05:00 UTC.
	for i := range sites {
		s := store.Schedule{Name: fmt.Sprintf("backup:site-%02d", i), Interval: 24 * time.Hour, Offset: time.Hour, Stagger: 4 * time.Hour}
		if p := s.Phase(); p < time.Hour || p >= 5*time.Hour {
			t.Errorf("%s: phase %s outside the backup window", s.Name, p)
		}
	}
	exact := store.Schedule{Name: "prune", Interval: 7 * 24 * time.Hour, Offset: 3 * time.Hour, Stagger: time.Second}
	if p := exact.Phase(); p != 3*time.Hour {
		t.Errorf("one-second stagger window: phase %s, want the offset", p)
	}
}

func TestNextRunAfter(t *testing.T) {
	s := store.Schedule{Name: "x", Interval: time.Hour, Offset: 10 * time.Minute, Stagger: time.Second}
	base := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	for _, tt := range []struct {
		after, want time.Time
	}{
		{base, base.Add(10 * time.Minute)},
		{base.Add(10 * time.Minute), base.Add(70 * time.Minute)}, // strictly after
		{base.Add(10*time.Minute - time.Nanosecond), base.Add(10 * time.Minute)},
		{time.Unix(0, 0), time.Unix(600, 0).UTC()},
		{time.Unix(-3600, 0), time.Unix(-3000, 0).UTC()},
	} {
		if got := s.NextRunAfter(tt.after); !got.Equal(tt.want) {
			t.Errorf("NextRunAfter(%s) = %s, want %s", tt.after, got, tt.want)
		}
	}
}

func newSchedule(t *testing.T, db *store.DB, name string, policy store.MissedRunPolicy, interval time.Duration, now time.Time) store.Schedule {
	t.Helper()
	s := store.Schedule{
		Name: name, JobType: "site.wp_cron", TargetType: "site", TargetID: "site-1",
		Payload: []byte(`{"site_id":"site-1"}`), LockKey: "site:site-1",
		Interval: interval, MissedRunPolicy: policy,
	}
	if err := store.NewSchedules(db).Upsert(context.Background(), &s, now); err != nil {
		t.Fatal(err)
	}
	return s
}

func newScheduler(db *store.DB, clock jobs.Clock) *jobs.Scheduler {
	return jobs.NewScheduler(db, jobs.SchedulerOptions{Clock: clock, Logger: storetest.Logger(), MaxCatchUp: 3})
}

func scheduledJobs(t *testing.T, db *store.DB, scheduleID string) []store.Job {
	t.Helper()
	page, err := store.NewJobs(db).List(context.Background(), store.JobFilter{ScheduleID: scheduleID, Limit: 200})
	if err != nil {
		t.Fatal(err)
	}
	return page.Jobs
}

func TestMissedRunPolicies(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		policy store.MissedRunPolicy
		late   time.Duration // after the slot five intervals past the first
		want   int
	}{
		{store.MissedSkip, 30 * time.Second, 1},
		{store.MissedSkip, 5 * time.Minute, 0},
		{store.MissedCoalesce, 5 * time.Minute, 1},
		{store.MissedCatchUp, 5 * time.Minute, 3}, // 6 missed, capped at MaxCatchUp
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("%s/%s", tt.policy, tt.late), func(t *testing.T) {
			storetest.Run(t, func(t *testing.T, db *store.DB) {
				clock := jobstest.NewClock(time.Now())
				s := newSchedule(t, db, "sched", tt.policy, time.Hour, clock.Now())
				clock.Set(s.NextRunAt.Add(5*time.Hour + tt.late))
				n, err := newScheduler(db, clock).RunOnce(ctx)
				if err != nil {
					t.Fatal(err)
				}
				got := scheduledJobs(t, db, s.ID)
				if n != tt.want || len(got) != tt.want {
					t.Fatalf("enqueued %d (%d in the database), want %d", n, len(got), tt.want)
				}
				lastSlot := s.NextRunAt.Add(5 * time.Hour)
				if tt.want > 0 {
					// Newest first.
					if !got[0].ScheduledFor.Equal(lastSlot) {
						t.Errorf("newest run for %s, want %s", got[0].ScheduledFor, lastSlot)
					}
					j := got[len(got)-1]
					if j.Type != "site.wp_cron" || j.TargetID != "site-1" || j.LockKey != "site:site-1" || string(j.Payload) != `{"site_id":"site-1"}` {
						t.Errorf("job = %+v", j)
					}
				}
				if tt.policy == store.MissedCatchUp && !got[2].ScheduledFor.Equal(lastSlot.Add(-2*time.Hour)) {
					t.Errorf("oldest catch-up run for %s", got[2].ScheduledFor)
				}
				after, err := store.NewSchedules(db).Get(ctx, "sched")
				if err != nil {
					t.Fatal(err)
				}
				if !after.NextRunAt.Equal(lastSlot.Add(time.Hour)) || after.LastRunAt.IsZero() {
					t.Errorf("next run %s, want %s", after.NextRunAt, lastSlot.Add(time.Hour))
				}
			})
		})
	}
}

// TestSchedulerFiresOnce: slots fire once across restarts and don't pile up
// behind a run that hasn't started.
func TestSchedulerFiresOnce(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		clock := jobstest.NewClock(time.Now())
		s := newSchedule(t, db, "sched", store.MissedCoalesce, time.Minute, clock.Now())
		if n, err := newScheduler(db, clock).RunOnce(ctx); err != nil || n != 0 {
			t.Fatalf("before the first slot: %d, %v", n, err)
		}
		clock.Set(s.NextRunAt)
		if n, err := newScheduler(db, clock).RunOnce(ctx); err != nil || n != 1 {
			t.Fatalf("at the first slot: %d, %v", n, err)
		}
		// A restarted scheduler at the same moment doesn't fire again.
		if n, err := newScheduler(db, clock).RunOnce(ctx); err != nil || n != 0 {
			t.Fatalf("after restart: %d, %v", n, err)
		}
		// The next slot is skipped while the first run hasn't started...
		clock.Advance(time.Minute)
		if n, err := newScheduler(db, clock).RunOnce(ctx); err != nil || n != 0 {
			t.Fatalf("with a run still queued: %d, %v", n, err)
		}
		// ...and fires once it has.
		pool := jobs.NewPool(db, jobs.Options{Clock: clock, Logger: storetest.Logger()})
		must(t, pool.Register(jobs.Handler{Type: "site.wp_cron", Steps: []jobs.Step{{Name: "run", Do: func(context.Context, *jobs.Run) error { return nil }}}}))
		if _, err := pool.RunNext(ctx); err != nil {
			t.Fatal(err)
		}
		clock.Advance(time.Minute)
		if n, err := newScheduler(db, clock).RunOnce(ctx); err != nil || n != 1 {
			t.Fatalf("after the run finished: %d, %v", n, err)
		}
		if got := len(scheduledJobs(t, db, s.ID)); got != 2 {
			t.Errorf("%d jobs, want 2", got)
		}
	})
}

// TestConcurrentSchedulers: many schedulers, as in several control plane
// processes, claim every due slot exactly once.
func TestConcurrentSchedulers(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		const schedules = 20
		clock := jobstest.NewClock(time.Now())
		for i := range schedules {
			s := store.Schedule{
				Name: fmt.Sprintf("s%02d", i), JobType: "t", TargetType: "site", TargetID: fmt.Sprint(i),
				Interval: time.Minute, MissedRunPolicy: store.MissedCatchUp,
			}
			must(t, store.NewSchedules(db).Upsert(ctx, &s, clock.Now()))
		}
		for round := 1; round <= 2; round++ {
			clock.Advance(time.Minute)
			var (
				wg    sync.WaitGroup
				mu    sync.Mutex
				total int
				errs  []error
			)
			for range 8 {
				wg.Go(func() {
					n, err := newScheduler(db, clock).RunOnce(ctx)
					mu.Lock()
					defer mu.Unlock()
					total += n
					if err != nil {
						errs = append(errs, err)
					}
				})
			}
			wg.Wait()
			if err := errors.Join(errs...); err != nil {
				t.Fatal(err)
			}
			page, err := store.NewJobs(db).List(ctx, store.JobFilter{Limit: 200})
			if err != nil {
				t.Fatal(err)
			}
			if total != schedules || len(page.Jobs) != round*schedules {
				t.Fatalf("round %d: schedulers reported %d jobs, database has %d; want %d new", round, total, len(page.Jobs), schedules)
			}
		}
	})
}

func TestScheduleUpsertAndDelete(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		spec := jobs.ScheduleSpec{
			Name: "wp-cron:site-1", JobType: "site.wp_cron", TargetType: "site", TargetID: "site-1",
			Payload: map[string]string{"site_id": "site-1"}, Interval: 5 * time.Minute, MissedRunPolicy: store.MissedCoalesce,
		}
		first, err := jobs.UpsertSchedule(ctx, db, spec)
		if err != nil {
			t.Fatal(err)
		}
		if first.Stagger != spec.Interval || first.MaxAttempts != store.DefaultMaxAttempts || first.NextRunAt.IsZero() {
			t.Errorf("defaults not applied: %+v", first)
		}
		// Changing the payload keeps the next run; changing the interval
		// moves it.
		spec.Payload = map[string]string{"site_id": "site-1", "v": "2"}
		second, err := jobs.UpsertSchedule(ctx, db, spec)
		if err != nil {
			t.Fatal(err)
		}
		if second.ID != first.ID || !second.NextRunAt.Equal(first.NextRunAt) || string(second.Payload) != `{"site_id":"site-1","v":"2"}` {
			t.Errorf("payload change: %+v", second)
		}
		spec.Interval = time.Hour
		third, err := jobs.UpsertSchedule(ctx, db, spec)
		if err != nil {
			t.Fatal(err)
		}
		if third.ID != first.ID || third.NextRunAt.Equal(first.NextRunAt) {
			t.Errorf("interval change kept the next run: %+v", third)
		}

		for _, bad := range []jobs.ScheduleSpec{
			{Name: "x", JobType: "t", TargetType: "site", TargetID: "1", Interval: 0, MissedRunPolicy: store.MissedSkip},
			{Name: "x", JobType: "t", TargetType: "site", TargetID: "1", Interval: 1500 * time.Millisecond, MissedRunPolicy: store.MissedSkip},
			{Name: "x", JobType: "t", TargetType: "site", TargetID: "1", Interval: time.Minute, MissedRunPolicy: "sometimes"},
			{Name: "x", JobType: "t", TargetType: "site", TargetID: "1", Interval: time.Minute, Offset: time.Minute, MissedRunPolicy: store.MissedSkip},
			{JobType: "t", TargetType: "site", TargetID: "1", Interval: time.Minute, MissedRunPolicy: store.MissedSkip},
		} {
			if _, err := jobs.UpsertSchedule(ctx, db, bad); err == nil {
				t.Errorf("UpsertSchedule(%+v) accepted", bad)
			}
		}

		spec.Name = "backup:site-1"
		if _, err := jobs.UpsertSchedule(ctx, db, spec); err != nil {
			t.Fatal(err)
		}
		other := spec
		other.Name, other.TargetID = "backup:site-2", "site-2"
		if _, err := jobs.UpsertSchedule(ctx, db, other); err != nil {
			t.Fatal(err)
		}
		if n, err := jobs.DeleteTargetSchedules(ctx, db, "site", "site-1"); err != nil || n != 2 {
			t.Errorf("DeleteTargetSchedules = %d, %v", n, err)
		}
		if err := jobs.DeleteSchedule(ctx, db, "backup:site-2"); err != nil {
			t.Fatal(err)
		}
		if err := jobs.DeleteSchedule(ctx, db, "backup:site-2"); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("deleting a missing schedule: %v", err)
		}
		if list, err := store.NewSchedules(db).List(ctx); err != nil || len(list) != 0 {
			t.Errorf("schedules left: %+v, %v", list, err)
		}
	})
}

// TestSchedulerRun drives the Run loop end to end.
func TestSchedulerRun(t *testing.T) {
	db := storetest.Open(t, store.SQLite)
	clock := jobstest.NewClock(time.Now())
	s := newSchedule(t, db, "sched", store.MissedCoalesce, time.Minute, clock.Now())
	clock.Set(s.NextRunAt)
	sched := jobs.NewScheduler(db, jobs.SchedulerOptions{Clock: clock, PollInterval: 5 * time.Millisecond, Logger: storetest.Logger()})
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan error, 1)
	go func() { stopped <- sched.Run(ctx) }()
	deadline := time.Now().Add(5 * time.Second)
	for len(scheduledJobs(t, db, s.ID)) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("scheduler never enqueued the due run")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	if err := <-stopped; err != nil {
		t.Fatal(err)
	}
}
