package sites_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/rchouinard/pressfleet/control-plane/internal/jobs"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
	"github.com/rchouinard/pressfleet/control-plane/internal/sites"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
)

// assertGone checks that nothing of the site is left: no stack, database,
// secrets, schedules, or row.
func (e *env) assertGone(t *testing.T, siteID string) {
	t.Helper()
	if _, ok := e.prov.Site(siteID); ok {
		t.Error("the stack is still there")
	}
	if _, ok := e.dbs.Data(siteID); ok {
		t.Error("the database is still there")
	}
	if e.cache.HasUser(siteID) {
		t.Error("the object cache user is still there")
	}
	// Every secret in these tests belongs to a site (the fake database and
	// object cache managers store none).
	if n := e.count(t, "SELECT COUNT(*) FROM secrets"); n != 0 {
		t.Errorf("%d secrets left", n)
	}
	if n := e.count(t, "SELECT COUNT(*) FROM schedules WHERE target_id = ?", siteID); n != 0 {
		t.Errorf("%d schedules left", n)
	}
	if _, err := store.NewSites(e.db).Get(context.Background(), siteID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("site row: err = %v, want ErrNotFound", err)
	}
}

func TestDeletion(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		site, _ := e.create(t, "Doomed")
		e.runJobs(t)
		if got := e.site(t, site.ID); got.Status != store.SiteRunning {
			t.Fatalf("site = %+v", got)
		}

		job, err := e.svc.Delete(ctx, site.ID, "", sites.DeleteOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if job.Type != sites.DeleteJobType || job.TargetID != site.ID || job.LockKey != sites.LockKey(site.ID) {
			t.Errorf("job = %+v", job)
		}
		if got := e.site(t, site.ID); got.Status != store.SiteDeleting {
			t.Errorf("status = %s, want deleting", got.Status)
		}
		// Asking again returns the queued job rather than a second one.
		again, err := e.svc.Delete(ctx, site.ID, "", sites.DeleteOptions{})
		if err != nil || again.ID != job.ID {
			t.Errorf("second Delete = %s, %v; want %s", again.ID, err, job.ID)
		}

		e.runJobs(t)
		d := e.job(t, job.ID)
		if d.Job.State != store.JobSucceeded {
			t.Fatalf("job %s: %s", d.Job.State, d.Job.Error)
		}
		var steps []string
		for _, s := range d.Steps {
			steps = append(steps, s.Name)
		}
		if got := strings.Join(steps, ","); got != "mark_deleting,delete_stack,drop_database,delete_object_cache,delete_secrets,delete_backups,delete_record" {
			t.Errorf("steps = %s", got)
		}
		e.assertGone(t, site.ID)

		// The site is gone for the API too.
		if _, err := e.svc.Delete(ctx, site.ID, "", sites.DeleteOptions{}); !errors.Is(err, sites.ErrNotFound) {
			t.Errorf("Delete of a deleted site: err = %v, want ErrNotFound", err)
		}
		// A deletion job for a site that is already gone finishes quietly.
		late, err := jobs.Enqueue(ctx, db, jobs.Spec{Type: sites.DeleteJobType, TargetType: "site", TargetID: site.ID, LockKey: sites.LockKey(site.ID)})
		if err != nil {
			t.Fatal(err)
		}
		e.runJobs(t)
		if d := e.job(t, late.ID); d.Job.State != store.JobSucceeded {
			t.Errorf("deleting a deleted site: job %s: %s", d.Job.State, d.Job.Error)
		}
	})
}

func TestDeletionRetriesAreIdempotent(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		site, _ := e.create(t, "Flaky")
		e.runJobs(t)
		// Each removal fails once after doing its work, as if the worker
		// had died before recording it.
		failedOnce := map[string]bool{}
		e.prov.Err = func(op, _ string) error {
			if op == "DeleteSite" && !failedOnce[op] {
				failedOnce[op] = true
				return errors.New("docker daemon hiccup")
			}
			return nil
		}
		e.dbs.Err = func(op, _ string) error {
			if op == "DropDatabase" && !failedOnce[op] {
				failedOnce[op] = true
				return errors.New("mariadb went away")
			}
			return nil
		}
		job, err := e.svc.Delete(ctx, site.ID, "", sites.DeleteOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if attempts := e.runJobs(t); attempts != 3 {
			t.Errorf("ran %d attempts, want 3", attempts)
		}
		if d := e.job(t, job.ID); d.Job.State != store.JobSucceeded {
			t.Fatalf("job %s: %s", d.Job.State, d.Job.Error)
		}
		e.assertGone(t, site.ID)
	})
}

func TestDeletionDuringProvisioning(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		// The stack fails to start, so provisioning waits to retry while the
		// deletion is requested.
		e.prov.Err = func(op, _ string) error {
			if op == "CreateSite" {
				return errors.New("docker daemon hiccup")
			}
			return nil
		}
		site, create := e.create(t, "Short-lived")
		if ran, err := e.pool.RunNext(ctx); err != nil || !ran {
			t.Fatalf("RunNext = %v, %v", ran, err)
		}
		if d := e.job(t, create.ID); d.Job.State != store.JobQueued || d.Job.Attempts != 1 {
			t.Fatalf("create job %s after %d attempts: %s", d.Job.State, d.Job.Attempts, d.Job.Error)
		}
		del, err := e.svc.Delete(ctx, site.ID, "", sites.DeleteOptions{})
		if err != nil {
			t.Fatal(err)
		}
		e.prov.Err = nil
		e.runJobs(t)

		// Provisioning stopped at its next step and rolled back; the deletion
		// ran after it and finished the site off.
		d := e.job(t, create.ID)
		if d.Job.State != store.JobFailed || !strings.Contains(d.Job.Error, "being deleted") {
			t.Errorf("create job %s: %s", d.Job.State, d.Job.Error)
		}
		if d := e.job(t, del.ID); d.Job.State != store.JobSucceeded || d.Job.FinishedAt.Before(e.job(t, create.ID).Job.FinishedAt) {
			t.Errorf("delete job %s: %s", d.Job.State, d.Job.Error)
		}
		for _, c := range e.prov.Calls() {
			if c.Op == "ExecWPCLI" {
				t.Errorf("WordPress was installed on a site being deleted: %+v", c)
			}
		}
		e.assertGone(t, site.ID)
	})
}

func TestFailedDeletionCanBeRetried(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		site, _ := e.create(t, "Stubborn")
		e.runJobs(t)
		e.prov.Err = func(op, _ string) error {
			if op == "DeleteSite" {
				return errors.New("device or resource busy")
			}
			return nil
		}
		first, err := e.svc.Delete(ctx, site.ID, "", sites.DeleteOptions{})
		if err != nil {
			t.Fatal(err)
		}
		e.runJobs(t)
		if d := e.job(t, first.ID); d.Job.State != store.JobFailed {
			t.Fatalf("job %s: %s", d.Job.State, d.Job.Error)
		}
		if got := e.site(t, site.ID); got.Status != store.SiteFailed || !strings.Contains(got.StatusMessage, "device or resource busy") {
			t.Errorf("site after a failed deletion = %+v", got)
		}

		e.prov.Err = nil
		second, err := e.svc.Delete(ctx, site.ID, "", sites.DeleteOptions{})
		if err != nil || second.ID == first.ID {
			t.Fatalf("Delete again = %s, %v", second.ID, err)
		}
		e.runJobs(t)
		if d := e.job(t, second.ID); d.Job.State != store.JobSucceeded {
			t.Fatalf("retry job %s: %s", d.Job.State, d.Job.Error)
		}
		e.assertGone(t, site.ID)
	})
}

func TestDeletingAFailedSite(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		e.wp.failInstall = &provisioner.ExecError{ExitCode: 1, Stderr: "Error: no"}
		site, _ := e.create(t, "Never Was")
		e.runJobs(t)
		if got := e.site(t, site.ID); got.Status != store.SiteFailed {
			t.Fatalf("site = %+v", got)
		}
		job, err := e.svc.Delete(ctx, site.ID, "", sites.DeleteOptions{})
		if err != nil {
			t.Fatal(err)
		}
		e.runJobs(t)
		if d := e.job(t, job.ID); d.Job.State != store.JobSucceeded {
			t.Fatalf("job %s: %s", d.Job.State, d.Job.Error)
		}
		e.assertGone(t, site.ID)
	})
}
