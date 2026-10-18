// Package sitecron runs WordPress cron for every site from the control plane
// (design doc §5.6).
//
// Sites set DISABLE_WP_CRON, so WordPress never runs cron on page loads and
// a site without traffic would never run its scheduled events. Instead each
// site has a recurring schedule, created with [EnsureSchedule] when the site
// is created and removed with [RemoveSchedule] when it is deleted, that
// enqueues a [JobType] job every [Interval]. The job runs
// `wp cron event run --due-now` in the site's php container.
//
// # Skipping
//
// The job succeeds without running WP-CLI, and says why in its log, when
// the site no longer exists, when its status says it isn't serving (stopped,
// or a job is provisioning or deleting it), or when its containers are
// missing or stopped. None of these is a cron failure: the next run tries
// again.
//
// # Failures and retries
//
// A failed run (WP-CLI exits non-zero or times out) fails the job with the
// exit code and the end of WP-CLI's standard error in the job log. It is not
// retried: runs are frequent and the next one runs whatever is still due,
// while retries would queue up behind the site's lock key.
//
// # Locking
//
// Jobs use the lock key "site:<id>", like every job that changes a site, so
// cron never runs while the site is being provisioned, updated, restored, or
// deleted. The other side of that is that a cron run delays those jobs until
// it finishes, so each run is bounded by [RunTimeout], well under the
// interval; the Compose driver also ends WP-CLI inside the container at the
// deadline. The scheduler doesn't enqueue a run while the previous one is
// still queued, so runs don't pile up behind a long site operation either.
package sitecron

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/jobs"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
)

const (
	// JobType is the type of the job that runs a site's due cron events.
	JobType = "site.wp_cron"
	// Interval is how often each site's cron runs. WordPress's most frequent
	// core schedule is hourly, and plugins rarely rely on anything finer
	// than a few minutes. Each site's runs are staggered across the
	// interval, so sites don't all run at once.
	Interval = 5 * time.Minute
	// RunTimeout bounds one run of WP-CLI. It is below Interval, so a hung
	// event can't hold the site's lock key past the next slot.
	RunTimeout = 3 * time.Minute
	// MaxAttempts is 1: a failed run isn't retried, the next slot runs
	// whatever is still due.
	MaxAttempts = 1

	// maxLoggedStderr is how much of WP-CLI's standard error goes into the
	// job log. The end is kept, where WP-CLI prints its "Error:" line.
	maxLoggedStderr = 2 << 10
	// stepName is the handler's only step. Keep it stable: queued jobs
	// resume by step name.
	stepName = "run_due_events"
)

// Args are the WP-CLI arguments each run executes.
var Args = []string{"cron", "event", "run", "--due-now"}

// ScheduleName returns the name of a site's cron schedule.
func ScheduleName(siteID string) string { return "wp-cron:" + siteID }

// EnsureSchedule creates or updates the site's cron schedule. It is
// idempotent. Call it with the transaction that creates the site, so the
// schedule exists exactly when the site row does.
func EnsureSchedule(ctx context.Context, q store.Querier, siteID string) error {
	if siteID == "" {
		return errors.New("ensure wp-cron schedule: site ID is required")
	}
	_, err := jobs.UpsertSchedule(ctx, q, jobs.ScheduleSpec{
		Name:            ScheduleName(siteID),
		JobType:         JobType,
		TargetType:      store.JobTargetSite,
		TargetID:        siteID,
		LockKey:         store.SiteLockKey(siteID),
		MaxAttempts:     MaxAttempts,
		Interval:        Interval,
		Stagger:         Interval,
		MissedRunPolicy: store.MissedCoalesce,
	})
	if err != nil {
		return fmt.Errorf("ensure wp-cron schedule for site %s: %w", siteID, err)
	}
	return nil
}

// RemoveSchedule deletes the site's cron schedule. It is idempotent: a site
// without a schedule is not an error. Call it with the transaction that
// deletes the site row. Jobs the schedule already enqueued stay queued and
// skip themselves once the site row is gone.
func RemoveSchedule(ctx context.Context, q store.Querier, siteID string) error {
	err := jobs.DeleteSchedule(ctx, q, ScheduleName(siteID))
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("remove wp-cron schedule for site %s: %w", siteID, err)
	}
	return nil
}

// Register adds the handler for JobType jobs to pool. db is where it reads
// site statuses; prov runs WP-CLI.
func Register(pool *jobs.Pool, db *store.DB, prov provisioner.Provisioner) error {
	return pool.Register(Handler(db, prov))
}

// Handler returns the handler for JobType jobs.
func Handler(db *store.DB, prov provisioner.Provisioner) jobs.Handler {
	r := runner{db: db, prov: prov}
	return jobs.Handler{
		Type:  JobType,
		Steps: []jobs.Step{{Name: stepName, Do: r.run}},
	}
}

// runnableStatuses are the site statuses (the API's SiteStatus) in which
// cron runs. "failed" is included because a site whose last job failed may
// still be serving; if its containers are gone or stopped, ExecWPCLI says so
// and the run is skipped. Any other status, including ones added later such
// as maintenance, skips the run.
var runnableStatuses = map[string]bool{"running": true, "failed": true}

type runner struct {
	db   *store.DB
	prov provisioner.Provisioner
}

func (rn runner) run(ctx context.Context, r *jobs.Run) error {
	job := r.Job()
	if job.TargetType != store.JobTargetSite || job.TargetID == "" {
		return jobs.Permanent(fmt.Errorf("%s job targets %s %q, want a site", JobType, job.TargetType, job.TargetID))
	}
	siteID := job.TargetID

	status, err := siteStatus(ctx, rn.db, siteID)
	if errors.Is(err, store.ErrNotFound) {
		// The site was deleted after this job was queued. Its schedule
		// should have gone with it; remove it in case it didn't, so it
		// stops enqueuing jobs for nothing.
		r.Logf("skipped: site %s no longer exists", siteID)
		return RemoveSchedule(ctx, rn.db, siteID)
	}
	if err != nil {
		return err
	}
	if !runnableStatuses[status] {
		r.Logf("skipped: site is %s", status)
		return nil
	}

	runCtx, cancel := context.WithTimeout(ctx, RunTimeout)
	defer cancel()
	out, err := rn.prov.ExecWPCLI(runCtx, siteID, Args)
	var execErr *provisioner.ExecError
	switch {
	case err == nil:
		r.Logf("%s", lastLine(out, "ran due cron events"))
		return nil
	case errors.Is(err, provisioner.ErrSiteNotFound):
		r.Logf("skipped: the site's containers don't exist")
		return nil
	case errors.Is(err, provisioner.ErrSiteNotRunning):
		r.Logf("skipped: the site's containers aren't running")
		return nil
	case errors.As(err, &execErr):
		if strings.Contains(execErr.Stderr, "You currently have no scheduled cron events") {
			r.Logf("no cron events are scheduled")
			return nil
		}
		r.Warnf("wp %s exited with status %d: %s", strings.Join(Args, " "), execErr.ExitCode, tail(execErr.Stderr, maxLoggedStderr))
		return fmt.Errorf("wp-cron on site %s: WP-CLI exited with status %d", siteID, execErr.ExitCode)
	case runCtx.Err() != nil && ctx.Err() == nil:
		r.Warnf("wp %s timed out after %s", strings.Join(Args, " "), RunTimeout)
		return fmt.Errorf("wp-cron on site %s: timed out after %s: %w", siteID, RunTimeout, err)
	default:
		return fmt.Errorf("wp-cron on site %s: %w", siteID, err)
	}
}

// siteStatus returns the site's lifecycle status from the state database,
// or store.ErrNotFound if the site doesn't exist.
func siteStatus(ctx context.Context, q store.Querier, siteID string) (string, error) {
	var status string
	err := q.QueryRowContext(ctx, "SELECT status FROM sites WHERE id = ?", siteID).Scan(&status)
	if err != nil {
		return "", fmt.Errorf("status of site %s: %w", siteID, err)
	}
	return status, nil
}

// lastLine returns the last non-empty line of s, WP-CLI's summary ("Success:
// Executed a total of 3 cron events."), or fallback if there is none.
func lastLine(s, fallback string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if last := strings.TrimSpace(lines[len(lines)-1]); last != "" {
		return tail(last, maxLoggedStderr)
	}
	return fallback
}

// tail returns at most the last n bytes of s, trimmed, marking a cut.
func tail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return "..." + strings.ToValidUTF8(s[len(s)-n:], "")
}
