package sitecron_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/id"
	"github.com/rchouinard/pressfleet/control-plane/internal/jobs"
	"github.com/rchouinard/pressfleet/control-plane/internal/jobs/jobstest"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner/provisionertest"
	"github.com/rchouinard/pressfleet/control-plane/internal/sitecron"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
)

type fixture struct {
	db    *store.DB
	prov  *provisionertest.Fake
	clock *jobstest.Clock
	pool  *jobs.Pool
}

func newFixture(t *testing.T, db *store.DB) *fixture {
	t.Helper()
	// Jobs are enqueued at the real time; the pool's clock is ahead of it
	// so they are runnable at once.
	f := &fixture{db: db, prov: provisionertest.New(), clock: jobstest.NewClock(time.Now().Add(2 * sitecron.Interval))}
	f.pool = jobs.NewPool(db, jobs.Options{Clock: f.clock, Logger: storetest.Logger()})
	if err := sitecron.Register(f.pool, db, f.prov); err != nil {
		t.Fatal(err)
	}
	return f
}

// addSite inserts a site row with the given status and, unless phase is
// empty, creates its runtime in the fake with that phase.
func (f *fixture) addSite(t *testing.T, status string, phase provisioner.SitePhase) string {
	t.Helper()
	ctx := context.Background()
	group := &store.ClientGroup{Name: "group-" + id.New()}
	if err := store.NewClientGroups(f.db).Create(ctx, group); err != nil {
		t.Fatal(err)
	}
	siteID := id.New()
	uid, err := store.AllocateSiteUID(ctx, f.db)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if _, err := f.db.ExecContext(ctx, `INSERT INTO sites
		(id, client_group_id, name, slug, status, preview_domain, php_version, wp_core_version, uid, created_at, updated_at)
		VALUES (?, ?, 'Site', ?, ?, ?, '8.4', '7.1.2', ?, ?, ?)`,
		siteID, group.ID, siteID, status, siteID+".preview.example", uid, now, now); err != nil {
		t.Fatal(err)
	}
	if phase == "" {
		return siteID
	}
	cfg := &provisioner.SiteConfig{
		ID: siteID, ClientGroupID: group.ID, PreviewDomain: siteID + ".preview.example",
		PHPVersion: "8.4", WPCoreVersion: "7.1.2", DBSecretRef: "db", WPSaltsSecretRef: "salts",
		UID: uid, Resources: provisioner.DefaultResourceLimits(),
	}
	if _, err := f.prov.CreateSite(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	if phase == provisioner.SiteStopped {
		if err := f.prov.StopSite(ctx, siteID); err != nil {
			t.Fatal(err)
		}
	}
	return siteID
}

// enqueue queues a cron job for siteID as its schedule would.
func (f *fixture) enqueue(t *testing.T, siteID string) store.Job {
	t.Helper()
	j, err := jobs.Enqueue(context.Background(), f.db, jobs.Spec{
		Type: sitecron.JobType, TargetType: store.JobTargetSite, TargetID: siteID,
		LockKey: store.SiteLockKey(siteID), MaxAttempts: sitecron.MaxAttempts,
	})
	if err != nil {
		t.Fatal(err)
	}
	return j
}

// run runs the next job and returns its final detail.
func (f *fixture) run(t *testing.T, jobID string) store.JobDetail {
	t.Helper()
	ctx := context.Background()
	if ran, err := f.pool.RunNext(ctx); err != nil || !ran {
		t.Fatalf("RunNext = %v, %v", ran, err)
	}
	d, err := store.NewJobs(f.db).Detail(ctx, jobID)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func (f *fixture) wpcliCalls(siteID string) [][]string {
	var out [][]string
	for _, c := range f.prov.Calls() {
		if c.Op == "ExecWPCLI" && c.SiteID == siteID {
			out = append(out, c.Args)
		}
	}
	return out
}

func logText(d store.JobDetail) string {
	var b strings.Builder
	for _, l := range d.Logs {
		b.WriteString(l.Level + ": " + l.Message + "\n")
	}
	return b.String()
}

// TestScheduledRunExecutesDueEvents follows a run from the schedule to
// WP-CLI: the scheduler enqueues it with the site's lock key and one
// attempt, and the job runs exactly `wp cron event run --due-now`.
func TestScheduledRunExecutesDueEvents(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		ctx := context.Background()
		f := newFixture(t, db)
		siteID := f.addSite(t, "running", provisioner.SiteRunning)
		f.prov.WPCLI = func(string, []string) (string, error) {
			return "Executed the cron event 'wp_version_check' in 0.01s.\nSuccess: Executed a total of 1 cron event.\n", nil
		}
		if err := sitecron.EnsureSchedule(ctx, db, siteID); err != nil {
			t.Fatal(err)
		}
		sch, err := store.NewSchedules(db).Get(ctx, sitecron.ScheduleName(siteID))
		if err != nil {
			t.Fatal(err)
		}
		sched := jobs.NewScheduler(db, jobs.SchedulerOptions{Clock: jobstest.NewClock(sch.NextRunAt), Logger: storetest.Logger()})
		if n, err := sched.RunOnce(ctx); err != nil || n != 1 {
			t.Fatalf("scheduler enqueued %d jobs, %v; want 1", n, err)
		}
		page, err := store.NewJobs(db).List(ctx, store.JobFilter{TargetType: store.JobTargetSite, TargetID: siteID})
		if err != nil || len(page.Jobs) != 1 {
			t.Fatalf("jobs for site = %+v, %v", page.Jobs, err)
		}
		j := page.Jobs[0]
		if j.Type != sitecron.JobType || j.LockKey != "site:"+siteID || j.MaxAttempts != 1 {
			t.Errorf("enqueued job = type %s, lock key %q, max attempts %d", j.Type, j.LockKey, j.MaxAttempts)
		}

		d := f.run(t, j.ID)
		if d.Job.State != store.JobSucceeded {
			t.Fatalf("job %s: %s\n%s", d.Job.State, d.Job.Error, logText(d))
		}
		want := [][]string{{"cron", "event", "run", "--due-now"}}
		if got := f.wpcliCalls(siteID); !slices.EqualFunc(got, want, slices.Equal) {
			t.Errorf("WP-CLI calls = %q, want %q", got, want)
		}
		if !strings.Contains(logText(d), "Success: Executed a total of 1 cron event.") {
			t.Errorf("log lacks WP-CLI's summary:\n%s", logText(d))
		}
	})
}

// TestSkips checks that runs on sites that aren't serving succeed without
// running cron, and say why.
func TestSkips(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		ctx := context.Background()
		tests := []struct {
			name     string
			status   string // "" for a site row that doesn't exist
			phase    provisioner.SitePhase
			wantExec bool
			wantLog  string
		}{
			{"deleted", "", "", false, "no longer exists"},
			{"stopped", "stopped", provisioner.SiteStopped, false, "skipped: site is stopped"},
			{"provisioning", "provisioning", "", false, "skipped: site is provisioning"},
			{"deleting", "deleting", provisioner.SiteRunning, false, "skipped: site is deleting"},
			{"maintenance", "maintenance", provisioner.SiteRunning, false, "skipped: site is maintenance"},
			{"containers stopped", "running", provisioner.SiteStopped, true, "containers aren't running"},
			{"containers missing", "failed", "", true, "containers don't exist"},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				f := newFixture(t, db)
				siteID := id.New()
				if tt.status != "" {
					siteID = f.addSite(t, tt.status, tt.phase)
				}
				if err := sitecron.EnsureSchedule(ctx, db, siteID); err != nil {
					t.Fatal(err)
				}
				j := f.enqueue(t, siteID)
				d := f.run(t, j.ID)
				if d.Job.State != store.JobSucceeded {
					t.Fatalf("job %s: %s\n%s", d.Job.State, d.Job.Error, logText(d))
				}
				if got := len(f.wpcliCalls(siteID)) > 0; got != tt.wantExec {
					t.Errorf("WP-CLI called = %v, want %v", got, tt.wantExec)
				}
				if !strings.Contains(logText(d), tt.wantLog) {
					t.Errorf("log lacks %q:\n%s", tt.wantLog, logText(d))
				}
				// A deleted site's leftover schedule is removed; others stay.
				_, err := store.NewSchedules(db).Get(ctx, sitecron.ScheduleName(siteID))
				if exists := err == nil; exists != (tt.status != "") {
					t.Errorf("schedule exists = %v (%v) after the run", exists, err)
				}
			})
		}
	})
}

// TestFailureIsRecordedAndNotRetried checks that a failed run fails the job
// at once, with the exit code and the end of stderr in its log.
func TestFailureIsRecordedAndNotRetried(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		f := newFixture(t, db)
		siteID := f.addSite(t, "running", provisioner.SiteRunning)
		stderr := strings.Repeat("PHP Notice: noise\n", 1000) + "Error: boom in my_plugin_cron()"
		f.prov.WPCLI = func(string, []string) (string, error) {
			return "", &provisioner.ExecError{ExitCode: 255, Stderr: stderr}
		}
		j := f.enqueue(t, siteID)
		d := f.run(t, j.ID)
		if d.Job.State != store.JobFailed || d.Job.Attempts != 1 {
			t.Fatalf("job %s after %d attempts, want failed after 1", d.Job.State, d.Job.Attempts)
		}
		if !strings.Contains(d.Job.Error, "status 255") || strings.Contains(d.Job.Error, "noise") {
			t.Errorf("job error = %q, want the exit status without stderr", d.Job.Error)
		}
		var warn string
		for _, l := range d.Logs {
			if l.Level == "warn" {
				warn = l.Message
			}
		}
		if !strings.Contains(warn, "status 255") || !strings.HasSuffix(warn, "Error: boom in my_plugin_cron()") {
			t.Errorf("warning = %q, want the exit status and the end of stderr", warn)
		}
		if len(warn) > 3<<10 {
			t.Errorf("warning is %d bytes; stderr should be truncated", len(warn))
		}
		if ran, err := f.pool.RunNext(context.Background()); err != nil || ran {
			t.Errorf("a failed run was retried (ran %v, %v)", ran, err)
		}
	})
}

// TestNoScheduledEvents checks that a site without any cron events, which
// WP-CLI reports as an error, isn't a failure.
func TestNoScheduledEvents(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		f := newFixture(t, db)
		siteID := f.addSite(t, "running", provisioner.SiteRunning)
		f.prov.WPCLI = func(string, []string) (string, error) {
			return "", &provisioner.ExecError{ExitCode: 1, Stderr: "Error: You currently have no scheduled cron events."}
		}
		d := f.run(t, f.enqueue(t, siteID).ID)
		if d.Job.State != store.JobSucceeded {
			t.Fatalf("job %s: %s", d.Job.State, d.Job.Error)
		}
	})
}

// TestTransientErrorFails checks that an error other than a failed command
// fails the run too.
func TestTransientErrorFails(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		f := newFixture(t, db)
		siteID := f.addSite(t, "running", provisioner.SiteRunning)
		daemon := errors.New("cannot connect to the Docker daemon")
		f.prov.Err = func(op, _ string) error {
			if op == "ExecWPCLI" {
				return daemon
			}
			return nil
		}
		d := f.run(t, f.enqueue(t, siteID).ID)
		if d.Job.State != store.JobFailed || !strings.Contains(d.Job.Error, daemon.Error()) {
			t.Fatalf("job %s: %q", d.Job.State, d.Job.Error)
		}
	})
}

// TestScheduleHelpers checks that EnsureSchedule and RemoveSchedule are
// idempotent and work inside a transaction.
func TestScheduleHelpers(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		ctx := context.Background()
		siteID := id.New()
		schedules := store.NewSchedules(db)

		for range 2 {
			if err := db.InTx(ctx, func(q store.Querier) error {
				return sitecron.EnsureSchedule(ctx, q, siteID)
			}); err != nil {
				t.Fatal(err)
			}
		}
		first, err := schedules.Get(ctx, "wp-cron:"+siteID)
		if err != nil {
			t.Fatal(err)
		}
		if first.JobType != sitecron.JobType || first.TargetType != store.JobTargetSite || first.TargetID != siteID ||
			first.LockKey != "site:"+siteID || first.MaxAttempts != 1 || first.Interval != 5*time.Minute ||
			first.Stagger != first.Interval || first.MissedRunPolicy != store.MissedCoalesce {
			t.Errorf("schedule = %+v", first)
		}
		if err := sitecron.EnsureSchedule(ctx, db, siteID); err != nil {
			t.Fatal(err)
		}
		again, err := schedules.Get(ctx, "wp-cron:"+siteID)
		if err != nil {
			t.Fatal(err)
		}
		if again.ID != first.ID || !again.NextRunAt.Equal(first.NextRunAt) {
			t.Errorf("EnsureSchedule again changed the schedule: %+v, was %+v", again, first)
		}
		all, err := schedules.List(ctx)
		if err != nil || len(all) != 1 {
			t.Errorf("schedules = %d, %v; want 1", len(all), err)
		}

		// A rolled-back transaction leaves nothing behind.
		other := id.New()
		rollback := errors.New("rollback")
		if err := db.InTx(ctx, func(q store.Querier) error {
			if err := sitecron.EnsureSchedule(ctx, q, other); err != nil {
				return err
			}
			return rollback
		}); !errors.Is(err, rollback) {
			t.Fatal(err)
		}
		if _, err := schedules.Get(ctx, sitecron.ScheduleName(other)); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("schedule after rollback: %v, want ErrNotFound", err)
		}

		for range 2 {
			if err := db.InTx(ctx, func(q store.Querier) error {
				return sitecron.RemoveSchedule(ctx, q, siteID)
			}); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := schedules.Get(ctx, sitecron.ScheduleName(siteID)); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("schedule after remove: %v, want ErrNotFound", err)
		}
		if err := sitecron.EnsureSchedule(ctx, db, ""); err == nil {
			t.Error("EnsureSchedule accepted an empty site ID")
		}
	})
}
