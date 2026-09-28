package sitebackup_test

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/backup/sitebackup"
	"github.com/rchouinard/pressfleet/control-plane/internal/id"
	"github.com/rchouinard/pressfleet/control-plane/internal/jobs"
	"github.com/rchouinard/pressfleet/control-plane/internal/jobs/jobstest"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
)

func (e *env) ensure(t *testing.T, siteID string, w sitebackup.Window) {
	t.Helper()
	if err := sitebackup.EnsureSchedule(context.Background(), e.db, siteID, w); err != nil {
		t.Fatal(err)
	}
}

// schedule returns the site's backup schedule, or false if it has none.
func (e *env) schedule(t *testing.T, siteID string) (store.Schedule, bool) {
	t.Helper()
	sch, err := store.NewSchedules(e.db).Get(context.Background(), sitebackup.ScheduleName(siteID))
	if errors.Is(err, store.ErrNotFound) {
		return store.Schedule{}, false
	}
	if err != nil {
		t.Fatal(err)
	}
	return sch, true
}

// timeOfDay returns t's time since midnight UTC.
func timeOfDay(t time.Time) time.Duration {
	t = t.UTC()
	return t.Sub(time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC))
}

// TestScheduledBackup drives a site's default schedule through the
// scheduler with a fake clock to a backup record (the acceptance criterion
// of #53: backups run on schedule and record results).
func TestScheduledBackup(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		site := e.site(t, "acme", store.SiteRunning)
		e.ensure(t, site.ID, sitebackup.DefaultWindow())

		sch, ok := e.schedule(t, site.ID)
		if !ok {
			t.Fatal("the site has no backup schedule")
		}
		var payload sitebackup.JobPayload
		if err := json.Unmarshal(sch.Payload, &payload); err != nil {
			t.Fatal(err)
		}
		if sch.JobType != sitebackup.JobType || sch.TargetType != "site" || sch.TargetID != site.ID ||
			sch.LockKey != sitebackup.LockKey(site.ID) || payload.Kind != store.BackupScheduled ||
			sch.Interval != 24*time.Hour || sch.MissedRunPolicy != store.MissedCoalesce {
			t.Errorf("schedule = %+v, payload %s", sch, sch.Payload)
		}
		if at := timeOfDay(sch.NextRunAt); at < 2*time.Hour || at >= 6*time.Hour {
			t.Errorf("next run at %s, want within the 02:00-06:00 UTC window", sch.NextRunAt)
		}

		clock := jobstest.NewClock(sch.NextRunAt.Add(-time.Second))
		scheduler := jobs.NewScheduler(db, jobs.SchedulerOptions{Clock: clock, Logger: storetest.Logger()})
		if n, err := scheduler.RunOnce(ctx); err != nil || n != 0 {
			t.Fatalf("RunOnce before the slot = %d, %v; want 0", n, err)
		}
		clock.Set(sch.NextRunAt.Add(time.Minute))
		if n, err := scheduler.RunOnce(ctx); err != nil || n != 1 {
			t.Fatalf("RunOnce at the slot = %d, %v; want 1", n, err)
		}
		page, err := store.NewJobs(db).List(ctx, store.JobFilter{ScheduleID: sch.ID})
		if err != nil || len(page.Jobs) != 1 {
			t.Fatalf("scheduled jobs = %+v, %v", page.Jobs, err)
		}
		job := page.Jobs[0]
		if job.Type != sitebackup.JobType || job.TargetID != site.ID || !job.ScheduledFor.Equal(sch.NextRunAt) {
			t.Errorf("job = %+v", job)
		}
		e.runJobs(t)

		if got := e.job(t, job.ID); got.State != store.JobSucceeded {
			t.Fatalf("job %s: %s", got.State, got.Error)
		}
		recs := e.records(t, site.ID)
		if len(recs) != 1 {
			t.Fatalf("records = %+v", recs)
		}
		if r := recs[0]; r.Kind != store.BackupScheduled || r.Status != store.BackupSucceeded || r.SnapshotID == "" || r.JobID != job.ID {
			t.Errorf("record = %+v", r)
		}
		if calls := e.backups(); len(calls) != 1 || calls[0].Options.Kind != string(store.BackupScheduled) {
			t.Errorf("engine backups = %+v", calls)
		}

		// The next backup is due a day later.
		next, _ := e.schedule(t, site.ID)
		if want := sch.NextRunAt.Add(24 * time.Hour); !next.NextRunAt.Equal(want) {
			t.Errorf("next run = %s, want %s", next.NextRunAt, want)
		}
	})
}

func TestSchedulesAreStaggered(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		w := sitebackup.Window{Start: 23 * time.Hour, Length: 3 * time.Hour} // wraps past midnight
		times := map[time.Duration]bool{}
		for _, slug := range []string{"a", "b", "c", "d", "e", "f"} {
			site := e.site(t, slug, store.SiteRunning)
			e.ensure(t, site.ID, w)
			sch, _ := e.schedule(t, site.ID)
			at := timeOfDay(sch.NextRunAt)
			if at < 23*time.Hour && at >= 2*time.Hour {
				t.Errorf("site %s backs up at %s, outside 23:00-02:00", slug, at)
			}
			times[at] = true
		}
		if len(times) < 4 {
			t.Errorf("6 sites back up at only %d different times: %v", len(times), times)
		}
	})
}

func TestSetSchedule(t *testing.T) {
	ctx := context.Background()
	hour := func(h int) *int { return &h }
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		site := e.site(t, "acme", store.SiteRunning)
		e.ensure(t, site.ID, sitebackup.Window{})

		got, err := e.svc.Schedule(ctx, site.ID)
		if err != nil {
			t.Fatal(err)
		}
		sch, _ := e.schedule(t, site.ID)
		if !got.Enabled || got.IntervalHours != 24 || got.Hour != nil || !got.NextRunAt.Equal(sch.NextRunAt) {
			t.Errorf("Schedule = %+v", got)
		}

		// An hour of the day: backups start within that hour.
		got, err = e.svc.SetSchedule(ctx, site.ID, store.SiteBackupSchedule{Enabled: true, IntervalHours: 24, Hour: hour(14)})
		if err != nil {
			t.Fatal(err)
		}
		if at := timeOfDay(got.NextRunAt); at < 14*time.Hour || at >= 15*time.Hour || got.Hour == nil || *got.Hour != 14 {
			t.Errorf("SetSchedule(hour 14) = %+v (at %s)", got, at)
		}
		// Every six hours, one of them in hour 14.
		got, err = e.svc.SetSchedule(ctx, site.ID, store.SiteBackupSchedule{Enabled: true, IntervalHours: 6, Hour: hour(14)})
		if err != nil {
			t.Fatal(err)
		}
		sch, _ = e.schedule(t, site.ID)
		if at := timeOfDay(got.NextRunAt) % (6 * time.Hour); sch.Interval != 6*time.Hour || at < 2*time.Hour || at >= 3*time.Hour {
			t.Errorf("SetSchedule(6h, hour 14) = %+v, schedule %+v", got, sch)
		}
		// Weekly, in the platform's window.
		got, err = e.svc.SetSchedule(ctx, site.ID, store.SiteBackupSchedule{Enabled: true, IntervalHours: 168})
		if err != nil {
			t.Fatal(err)
		}
		if at := timeOfDay(got.NextRunAt); at < 2*time.Hour || at >= 6*time.Hour {
			t.Errorf("SetSchedule(weekly) = %+v", got)
		}

		// Disabling removes the schedule; enabling brings it back.
		got, err = e.svc.SetSchedule(ctx, site.ID, store.SiteBackupSchedule{Enabled: false, IntervalHours: 24})
		if err != nil || got.Enabled || !got.NextRunAt.IsZero() {
			t.Errorf("SetSchedule(disabled) = %+v, %v", got, err)
		}
		if _, ok := e.schedule(t, site.ID); ok {
			t.Error("a disabled schedule still exists")
		}
		if got, err = e.svc.SetSchedule(ctx, site.ID, store.DefaultSiteBackupSchedule()); err != nil || got.NextRunAt.IsZero() {
			t.Errorf("SetSchedule(default) = %+v, %v", got, err)
		}

		for _, tt := range []struct {
			s     store.SiteBackupSchedule
			field string
		}{
			{store.SiteBackupSchedule{Enabled: true, IntervalHours: 0}, "/interval_hours"},
			{store.SiteBackupSchedule{Enabled: true, IntervalHours: 5}, "/interval_hours"},
			{store.SiteBackupSchedule{Enabled: true, IntervalHours: 36}, "/interval_hours"},
			{store.SiteBackupSchedule{Enabled: true, IntervalHours: 192}, "/interval_hours"},
			{store.SiteBackupSchedule{Enabled: true, IntervalHours: 24, Hour: hour(24)}, "/hour"},
			{store.SiteBackupSchedule{Enabled: true, IntervalHours: 24, Hour: hour(-1)}, "/hour"},
		} {
			_, err := e.svc.SetSchedule(ctx, site.ID, tt.s)
			var se *sitebackup.ScheduleError
			if !errors.As(err, &se) || se.Field != tt.field || !errors.Is(err, sitebackup.ErrInvalidSchedule) {
				t.Errorf("SetSchedule(%+v) = %v, want a ScheduleError on %s", tt.s, err, tt.field)
			}
		}
		if _, err := e.svc.SetSchedule(ctx, id.New(), store.DefaultSiteBackupSchedule()); !errors.Is(err, sitebackup.ErrSiteNotFound) {
			t.Errorf("SetSchedule of a missing site: %v", err)
		}
		if _, err := e.svc.Schedule(ctx, id.New()); !errors.Is(err, sitebackup.ErrSiteNotFound) {
			t.Errorf("Schedule of a missing site: %v", err)
		}
	})
}

func TestEnsureScheduleOnlyForBackupableSites(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		for i, tt := range []struct {
			status store.SiteStatus
			salts  string
			want   bool
		}{
			{store.SiteProvisioning, "salts", true},
			{store.SiteRunning, "salts", true},
			{store.SiteStopped, "salts", true},
			{store.SiteFailed, "salts", true},
			{store.SiteFailed, "", false}, // provisioning failed
			{store.SiteDeleting, "salts", false},
		} {
			site := e.site(t, "s"+strconv.Itoa(i), tt.status)
			if tt.salts == "" {
				if err := store.NewSites(db).ClearSecretRefs(context.Background(), site.ID); err != nil {
					t.Fatal(err)
				}
			}
			e.ensure(t, site.ID, sitebackup.Window{})
			if _, ok := e.schedule(t, site.ID); ok != tt.want {
				t.Errorf("%s site with salts %q: has schedule = %t, want %t", tt.status, tt.salts, ok, tt.want)
			}
		}
	})
}

func TestReconcileSchedules(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		running := e.site(t, "running", store.SiteRunning)
		deleting := e.site(t, "deleting", store.SiteDeleting)
		disabled := e.site(t, "disabled", store.SiteRunning)
		if _, err := store.NewSites(db).SetBackupSchedule(ctx, disabled.ID, store.SiteBackupSchedule{IntervalHours: 24}); err != nil {
			t.Fatal(err)
		}
		gone := e.site(t, "gone", store.SiteRunning)
		e.ensure(t, gone.ID, sitebackup.Window{})
		e.ensure(t, disabled.ID, sitebackup.Window{Start: time.Hour, Length: time.Hour})
		if _, err := store.NewSites(db).Delete(ctx, gone.ID); err != nil {
			t.Fatal(err)
		}

		if err := sitebackup.ReconcileSchedules(ctx, db, sitebackup.DefaultWindow()); err != nil {
			t.Fatal(err)
		}
		first, ok := e.schedule(t, running.ID)
		if !ok {
			t.Error("an existing site got no schedule")
		}
		for name, siteID := range map[string]string{"deleting": deleting.ID, "disabled": disabled.ID, "deleted": gone.ID} {
			if _, ok := e.schedule(t, siteID); ok {
				t.Errorf("the %s site has a schedule", name)
			}
		}

		// Idempotent: the next run stays put.
		if err := sitebackup.ReconcileSchedules(ctx, db, sitebackup.DefaultWindow()); err != nil {
			t.Fatal(err)
		}
		if again, _ := e.schedule(t, running.ID); !again.NextRunAt.Equal(first.NextRunAt) || again.ID != first.ID {
			t.Errorf("reconciling again moved the schedule: %+v, was %+v", again, first)
		}
		// A new window moves it.
		if err := sitebackup.ReconcileSchedules(ctx, db, sitebackup.Window{Start: 12 * time.Hour, Length: time.Hour}); err != nil {
			t.Fatal(err)
		}
		moved, _ := e.schedule(t, running.ID)
		if at := timeOfDay(moved.NextRunAt); at < 12*time.Hour || at >= 13*time.Hour {
			t.Errorf("after a window change, next run at %s, want 12:00-13:00", moved.NextRunAt)
		}
	})
}

func TestScheduledBackupSkips(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		gone := e.site(t, "gone", store.SiteRunning)
		e.ensure(t, gone.ID, sitebackup.Window{})
		if _, err := store.NewSites(db).Delete(ctx, gone.ID); err != nil {
			t.Fatal(err)
		}
		busy := e.site(t, "busy", store.SiteProvisioning)

		var queued []store.Job
		for _, siteID := range []string{gone.ID, busy.ID} {
			j, err := sitebackup.Enqueue(ctx, db, siteID, store.BackupScheduled, "")
			if err != nil {
				t.Fatal(err)
			}
			queued = append(queued, j)
		}
		manual, err := sitebackup.Enqueue(ctx, db, busy.ID, store.BackupManual, "")
		if err != nil {
			t.Fatal(err)
		}
		e.runJobs(t)

		for _, j := range queued {
			if got := e.job(t, j.ID); got.State != store.JobSucceeded {
				t.Errorf("scheduled backup of an unavailable site: job %s: %s", got.State, got.Error)
			}
		}
		// Someone asked for the manual backup, so its failure shows.
		if got := e.job(t, manual.ID); got.State != store.JobFailed {
			t.Errorf("manual backup of a provisioning site: job %s", got.State)
		}
		if calls := e.backups(); len(calls) != 0 {
			t.Errorf("engine backups = %+v", calls)
		}
		if recs := e.records(t, busy.ID); len(recs) != 0 {
			t.Errorf("records = %+v", recs)
		}
		if _, ok := e.schedule(t, gone.ID); ok {
			t.Error("the deleted site's schedule survived its skipped backup")
		}
	})
}
