package sites_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/id"
	"github.com/rchouinard/pressfleet/control-plane/internal/jobs"
	"github.com/rchouinard/pressfleet/control-plane/internal/sites"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
)

// Sites stuck in updating (repair.go, #282).

// corruptData makes a job's data unreadable, so the job ends rollback_failed
// before any Undo runs.
func (e *env) corruptData(t *testing.T, jobID string) {
	t.Helper()
	if _, err := e.db.ExecContext(context.Background(), "UPDATE jobs SET data = ? WHERE id = ?", "{not json", jobID); err != nil {
		t.Fatal(err)
	}
}

// holdJob keeps a queued job from being leased.
func (e *env) holdJob(t *testing.T, jobID string) {
	t.Helper()
	if _, err := e.db.ExecContext(context.Background(), "UPDATE jobs SET run_after = ? WHERE id = ?", time.Now().Add(time.Hour), jobID); err != nil {
		t.Fatal(err)
	}
}

func TestRollbackFailedMarksSiteFailed(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		site := e.running(t, "Stuck")
		_, job, _, err := e.svc.Update(ctx, site.ID, sites.SiteUpdate{PHPVersion: ptr("8.3")})
		if err != nil {
			t.Fatal(err)
		}
		e.corruptData(t, job.ID)
		e.runJobs(t)
		d := e.job(t, job.ID)
		if d.Job.State != store.JobRollbackFailed {
			t.Fatalf("job %s: %s", d.Job.State, d.Job.Error)
		}
		got := e.site(t, site.ID)
		if got.Status != store.SiteFailed || !strings.Contains(got.StatusMessage, "decoding data of job") {
			t.Fatalf("site = %s %q, want failed with the job's error", got.Status, got.StatusMessage)
		}
		// The site is no longer refused as busy.
		if _, _, _, err := e.svc.Update(ctx, site.ID, sites.SiteUpdate{PHPVersion: ptr("8.3")}); err != nil {
			t.Errorf("update after the failed rollback: %v", err)
		}
	})
}

// TestRollbackFailedOfOtherTypeLeavesSite: a job that doesn't own the
// updating status ends rollback_failed while an update is queued.
func TestRollbackFailedOfOtherTypeLeavesSite(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		site := e.running(t, "Busy")
		_, update, _, err := e.svc.Update(ctx, site.ID, sites.SiteUpdate{PHPVersion: ptr("8.3")})
		if err != nil {
			t.Fatal(err)
		}
		e.holdJob(t, update.ID)
		other, err := jobs.Enqueue(ctx, db, jobs.Spec{
			Type: sites.PurgeCacheJobType, TargetType: store.JobTargetSite, TargetID: site.ID, LockKey: "other:" + id.New(),
		})
		if err != nil {
			t.Fatal(err)
		}
		e.corruptData(t, other.ID)
		e.runJobs(t)
		if d := e.job(t, other.ID); d.Job.State != store.JobRollbackFailed {
			t.Fatalf("other job %s: %s", d.Job.State, d.Job.Error)
		}
		if got := e.site(t, site.ID); got.Status != store.SiteUpdating {
			t.Errorf("site = %s %q, want it left updating for the queued update", got.Status, got.StatusMessage)
		}
		if n, err := e.svc.RepairStuckSites(ctx); err != nil || n != 0 {
			t.Errorf("repair with an update queued = %d, %v", n, err)
		}
		if got := e.site(t, site.ID); got.Status != store.SiteUpdating {
			t.Errorf("site after the repair = %s", got.Status)
		}
	})
}

// TestRepairStuckSites: a site left updating before the hook existed, or
// when it couldn't write, is repaired even with other jobs queued for it.
func TestRepairStuckSites(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		stuck := e.running(t, "Stuck")
		_, job, _, err := e.svc.Update(ctx, stuck.ID, sites.SiteUpdate{PHPVersion: ptr("8.3")})
		if err != nil {
			t.Fatal(err)
		}
		// Finish the job the way a worker does, without the hook.
		now := time.Now()
		sj := store.NewJobs(db)
		leased, err := sj.Lease(ctx, []string{sites.UpdateJobType}, "w", now, now.Add(time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		if err := sj.Finish(ctx, leased.ID, "w", store.JobRollbackFailed, "revert: boom", now); err != nil {
			t.Fatal(err)
		}
		if leased.ID != job.ID {
			t.Fatalf("leased %s, want %s", leased.ID, job.ID)
		}
		// Housekeeping jobs for the site are queued, as they always are.
		if _, err := e.svc.EnqueueReconcile(ctx); err != nil {
			t.Fatal(err)
		}
		if _, err := jobs.Enqueue(ctx, db, jobs.Spec{
			Type: "site.wp_cron", TargetType: store.JobTargetSite, TargetID: stuck.ID, LockKey: store.SiteLockKey(stuck.ID),
		}); err != nil {
			t.Fatal(err)
		}
		fine := e.running(t, "Fine")
		busy := e.running(t, "Updating")
		if _, _, _, err := e.svc.Update(ctx, busy.ID, sites.SiteUpdate{PHPVersion: ptr("8.3")}); err != nil {
			t.Fatal(err)
		}

		n, err := e.svc.RepairStuckSites(ctx)
		if err != nil || n != 1 {
			t.Fatalf("repaired %d, %v; want 1", n, err)
		}
		got := e.site(t, stuck.ID)
		if got.Status != store.SiteFailed || !strings.Contains(got.StatusMessage, "revert: boom") {
			t.Errorf("stuck site = %s %q", got.Status, got.StatusMessage)
		}
		if got := e.site(t, busy.ID); got.Status != store.SiteUpdating {
			t.Errorf("site with a queued update = %s, want updating", got.Status)
		}
		if got := e.site(t, fine.ID); got.Status != store.SiteRunning {
			t.Errorf("running site = %s", got.Status)
		}
		if n, err := e.svc.RepairStuckSites(ctx); err != nil || n != 0 {
			t.Errorf("second repair = %d, %v", n, err)
		}
	})
}
