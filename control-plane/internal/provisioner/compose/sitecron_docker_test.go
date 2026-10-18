package compose_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/jobs"
	"github.com/rchouinard/pressfleet/control-plane/internal/jobs/jobstest"
	"github.com/rchouinard/pressfleet/control-plane/internal/sitecron"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
)

// cronTestPlugin records that its cron event ran. It is a regular plugin:
// the site's must-use plugin directory is read-only.
const cronTestPlugin = `<?php
/*
 * Plugin Name: PressFleet cron test
 */
add_action( 'pf_cron_test', function () {
	update_option( 'pf_cron_marker', 'ran-' . time() );
} );
`

// TestSiteCronDocker proves issue #30's acceptance criterion against real
// Docker: a site that receives no HTTP traffic (and has DISABLE_WP_CRON set)
// runs its scheduled WordPress events because the control plane's schedule
// enqueues a site.wp_cron job that runs them through the Compose driver. It
// lives here, rather than in package sitecron, because it reuses this
// package's throwaway-site harness, and only the Compose driver's tests may
// drive Docker directly.
func TestSiteCronDocker(t *testing.T) {
	s := newDockerSite(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	siteID := s.cfg.ID
	if _, err := s.prov.CreateSite(ctx, s.cfg); err != nil {
		t.Fatal(err)
	}
	s.waitDBHealthy(t, ctx)
	wp := func(args ...string) string {
		t.Helper()
		out, err := s.prov.ExecWPCLI(ctx, siteID, args)
		if err != nil {
			t.Fatalf("wp %q: %v", args, err)
		}
		return strings.TrimSpace(out)
	}
	wp("core", "install", "--url=http://127.0.0.1:8080", "--title=Cron",
		"--admin_user=admin", "--admin_password=test-admin-pw", "--admin_email=admin@example.com", "--skip-email")
	s.exec(t, ctx, "php", "sh", "-c",
		`printf '%s' "$1" > /var/www/wordpress/wp-content/plugins/pf-cron-test.php`, "sh", cronTestPlugin)
	wp("plugin", "activate", "pf-cron-test")
	wp("cron", "event", "schedule", "pf_cron_test", "now")

	// The state database has the site, running, and its cron schedule.
	db := storetest.Open(t, store.SQLite)
	group := &store.ClientGroup{Name: "Cron"}
	if err := store.NewClientGroups(db).Create(ctx, group); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if _, err := db.ExecContext(ctx, `INSERT INTO sites
		(id, client_group_id, name, slug, status, preview_domain, php_version, wp_core_version, uid, created_at, updated_at)
		VALUES (?, ?, 'Cron', 'cron', 'running', ?, ?, ?, ?, ?, ?)`,
		siteID, group.ID, s.cfg.PreviewDomain, s.cfg.PHPVersion, s.cfg.WPCoreVersion, s.cfg.UID, now, now); err != nil {
		t.Fatal(err)
	}
	if err := sitecron.EnsureSchedule(ctx, db, siteID); err != nil {
		t.Fatal(err)
	}
	sch, err := store.NewSchedules(db).Get(ctx, sitecron.ScheduleName(siteID))
	if err != nil {
		t.Fatal(err)
	}
	pool := jobs.NewPool(db, jobs.Options{Logger: storetest.Logger()})
	if err := sitecron.Register(pool, db, s.prov); err != nil {
		t.Fatal(err)
	}

	// Nothing has run the event: no request reached the site, and
	// WordPress doesn't run cron itself.
	if _, err := s.prov.ExecWPCLI(ctx, siteID, []string{"option", "get", "pf_cron_marker"}); err == nil {
		t.Fatal("the cron event ran before the control plane triggered it")
	}

	// The schedule's slot comes round: the scheduler enqueues a job and a
	// worker runs it.
	sched := jobs.NewScheduler(db, jobs.SchedulerOptions{Clock: jobstest.NewClock(sch.NextRunAt), Logger: storetest.Logger()})
	if n, err := sched.RunOnce(ctx); err != nil || n != 1 {
		t.Fatalf("scheduler enqueued %d jobs, %v; want 1", n, err)
	}
	job := runCronJob(t, ctx, db, pool, siteID)
	if job.Job.State != store.JobSucceeded {
		t.Fatalf("cron job %s: %s", job.Job.State, job.Job.Error)
	}
	if got := wp("option", "get", "pf_cron_marker"); !strings.HasPrefix(got, "ran-") {
		t.Errorf("marker = %q, want the event to have run", got)
	}
	if list := wp("cron", "event", "list", "--fields=hook", "--format=csv"); strings.Contains(list, "pf_cron_test") {
		t.Errorf("the single event is still scheduled:\n%s", list)
	}

	// On a stopped site the run is skipped, not failed.
	if err := s.prov.StopSite(ctx, siteID); err != nil {
		t.Fatal(err)
	}
	if _, err := jobs.Enqueue(ctx, db, jobs.Spec{
		Type: sitecron.JobType, TargetType: store.JobTargetSite, TargetID: siteID,
		LockKey: store.SiteLockKey(siteID), MaxAttempts: sitecron.MaxAttempts,
	}); err != nil {
		t.Fatal(err)
	}
	job = runCronJob(t, ctx, db, pool, siteID)
	if job.Job.State != store.JobSucceeded || len(job.Logs) == 0 || !strings.Contains(job.Logs[0].Message, "skipped") {
		t.Errorf("cron job on a stopped site: %s (%q), logs %+v", job.Job.State, job.Job.Error, job.Logs)
	}
}

// runCronJob runs the next job and returns the newest job for the site.
func runCronJob(t *testing.T, ctx context.Context, db *store.DB, pool *jobs.Pool, siteID string) store.JobDetail {
	t.Helper()
	if ran, err := pool.RunNext(ctx); err != nil || !ran {
		t.Fatalf("RunNext = %v, %v", ran, err)
	}
	page, err := store.NewJobs(db).List(ctx, store.JobFilter{TargetType: store.JobTargetSite, TargetID: siteID, Limit: 1})
	if err != nil || len(page.Jobs) == 0 {
		t.Fatalf("listing jobs: %v", err)
	}
	d, err := store.NewJobs(db).Detail(ctx, page.Jobs[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	return d
}
