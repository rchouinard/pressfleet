package sites_test

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/backup"
	"github.com/rchouinard/pressfleet/control-plane/internal/jobs"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
	"github.com/rchouinard/pressfleet/control-plane/internal/sites"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
)

// loseHost removes what a lost host takes with it: the sites' containers
// and databases. The state database stands for the restored one.
func (e *restoreEnv) loseHost(t *testing.T, siteIDs ...string) {
	t.Helper()
	ctx := context.Background()
	for _, id := range siteIDs {
		if err := e.prov.DeleteSite(ctx, id); err != nil {
			t.Fatal(err)
		}
		if err := e.dbs.DropDatabase(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
}

func (e *restoreEnv) prepareRecovery(t *testing.T) sites.Recovery {
	t.Helper()
	var rec sites.Recovery
	err := e.db.InTx(context.Background(), func(q store.Querier) error {
		var err error
		rec, err = sites.PrepareRecovery(context.Background(), q, time.Now())
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return rec
}

func (e *restoreEnv) recoverJob(t *testing.T, siteID string) store.JobDetail {
	t.Helper()
	page, err := store.NewJobs(e.db).List(context.Background(), store.JobFilter{
		Types: []string{sites.RecoverJobType}, TargetType: store.JobTargetSite, TargetID: siteID, Limit: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Jobs) != 1 {
		t.Fatalf("%d recover jobs for site %s, want 1", len(page.Jobs), siteID)
	}
	return e.job(t, page.Jobs[0].ID)
}

func TestRecoverSites(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newRestoreEnv(t, db)
		running := e.runningSite(t, "Running", "post=running")
		e.snapshot(t, running.ID)
		// The newest snapshot is the one recovered.
		e.dbs.SetData(running.ID, []byte(strings.Replace(e.data(running.ID), "post=running", "post=newer", 1)))
		newest := e.snapshot(t, running.ID)

		stopped := e.runningSite(t, "Stopped", "post=stopped")
		e.snapshot(t, stopped.ID)
		if _, err := e.svc.Stop(ctx, stopped.ID, ""); err != nil {
			t.Fatal(err)
		}
		e.runJobs(t)

		noBackup := e.runningSite(t, "Unsaved", "post=lost")
		failed := e.runningSite(t, "Failed", "")
		e.setStatus(t, failed.ID, store.SiteFailed)
		// Its provisioning job never ran.
		provisioning, _ := e.create(t, "Interrupted")
		// A job the old host never ran.
		pending, err := jobs.Enqueue(ctx, db, jobs.Spec{Type: sites.PurgeCacheJobType, TargetType: store.JobTargetSite,
			TargetID: running.ID, LockKey: store.SiteLockKey(running.ID)})
		if err != nil {
			t.Fatal(err)
		}

		e.loseHost(t, running.ID, stopped.ID, noBackup.ID, failed.ID)
		rec := e.prepareRecovery(t)
		wantRecovering := []string{running.ID, stopped.ID, noBackup.ID}
		slices.Sort(wantRecovering)
		slices.Sort(rec.Recovering)
		if !slices.Equal(rec.Recovering, wantRecovering) || !slices.Equal(rec.Failed, []string{provisioning.ID}) || rec.InterruptedJobs < 2 {
			t.Errorf("recovery = %+v", rec)
		}
		if d := e.job(t, pending.ID); d.Job.State != store.JobFailed || !strings.Contains(d.Job.Error, "restored") {
			t.Errorf("pending job = %s %q", d.Job.State, d.Job.Error)
		}
		for _, id := range wantRecovering {
			if s := e.site(t, id); s.Status != store.SiteUpdating {
				t.Errorf("site %s is %s before its recovery", s.Name, s.Status)
			}
		}
		// The startup reconciliation runs alongside and leaves them alone.
		if _, err := e.svc.EnqueueReconcile(ctx); err != nil {
			t.Fatal(err)
		}
		e.runJobs(t)

		for _, tc := range []struct {
			site   store.Site
			status store.SiteStatus
			phase  provisioner.SitePhase
			data   string
		}{
			{running, store.SiteRunning, provisioner.SiteRunning, "post=newer"},
			{stopped, store.SiteStopped, provisioner.SiteStopped, "post=stopped"},
		} {
			d := e.recoverJob(t, tc.site.ID)
			if d.Job.State != store.JobSucceeded {
				t.Fatalf("recovering %s: %s %s\n%s", tc.site.Name, d.Job.State, d.Job.Error, jobLog(d))
			}
			if got := e.site(t, tc.site.ID); got.Status != tc.status || got.StatusMessage != "" {
				t.Errorf("site %s after recovery: %s %q", tc.site.Name, got.Status, got.StatusMessage)
			}
			if rt, ok := e.prov.Site(tc.site.ID); !ok || rt.Phase != tc.phase || rt.Config.PreviewDomain != tc.site.PreviewDomain {
				t.Errorf("site %s containers: %v %+v", tc.site.Name, ok, rt)
			}
			if got := e.data(tc.site.ID); !strings.Contains(got, tc.data) {
				t.Errorf("site %s database after recovery = %q", tc.site.Name, got)
			}
		}
		if calls := e.wpCalls(running.ID, "core update-db"); len(calls) != 1 {
			t.Errorf("update-db calls = %q", calls)
		}
		var restored []string
		for _, c := range e.engine.Calls() {
			if c.Op == "Restore" && c.SiteID == running.ID {
				restored = append(restored, string(c.Snapshot))
			}
		}
		if !slices.Equal(restored, []string{string(newest.ID)}) {
			t.Errorf("restored snapshots of the running site = %q, want the newest %s", restored, newest.ID)
		}

		d := e.recoverJob(t, noBackup.ID)
		if got := e.site(t, noBackup.ID); d.Job.State != store.JobFailed || got.Status != store.SiteFailed ||
			!strings.Contains(got.StatusMessage, "no backup") {
			t.Errorf("site without a backup: job %s %q, site %s %q", d.Job.State, d.Job.Error, got.Status, got.StatusMessage)
		}
		if got := e.site(t, provisioning.ID); got.Status != store.SiteFailed || !strings.Contains(got.StatusMessage, "interrupted") {
			t.Errorf("interrupted site: %s %q", got.Status, got.StatusMessage)
		}
		if got := e.site(t, failed.ID); got.Status != store.SiteFailed {
			t.Errorf("failed site: %s", got.Status)
		}
	})
}

// A snapshot from a newer core moves the site to that core first.
func TestRecoverMovesCore(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newRestoreEnv(t, db)
		site := e.runningSite(t, "Acme", "post=v1")
		if _, err := store.NewSites(db).SetWPCoreVersion(ctx, site.ID, "6.9"); err != nil {
			t.Fatal(err)
		}
		snap := e.snapshot(t, site.ID)
		e.engine.cores[snap.ID] = "7.1.2"
		e.loseHost(t, site.ID)
		e.prepareRecovery(t)
		e.runJobs(t)
		if d := e.recoverJob(t, site.ID); d.Job.State != store.JobSucceeded {
			t.Fatalf("recovery: %s %s\n%s", d.Job.State, d.Job.Error, jobLog(d))
		}
		if got := e.site(t, site.ID); got.WPCoreVersion != "7.1.2" {
			t.Errorf("core after recovery = %s", got.WPCoreVersion)
		}
		if rt, _ := e.prov.Site(site.ID); rt.Config.WPCoreVersion != "7.1.2" {
			t.Errorf("containers run core %s", rt.Config.WPCoreVersion)
		}
	})
}

// A retried recovery converges: restoring again is harmless.
func TestRecoverRetries(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newRestoreEnv(t, db)
		site := e.runningSite(t, "Acme", "post=v1")
		e.snapshot(t, site.ID)
		e.loseHost(t, site.ID)
		e.engine.restoreErr = func(n int, _ backup.SnapshotID) error {
			if n == 1 {
				return context.DeadlineExceeded
			}
			return nil
		}
		e.prepareRecovery(t)
		e.runJobs(t)
		d := e.recoverJob(t, site.ID)
		if d.Job.State == store.JobQueued {
			// Retried with backoff: run it now.
			if _, err := db.ExecContext(context.Background(), "UPDATE jobs SET run_after = ? WHERE id = ?", time.Now().Add(-time.Minute), d.Job.ID); err != nil {
				t.Fatal(err)
			}
			e.runJobs(t)
			d = e.recoverJob(t, site.ID)
		}
		if d.Job.State != store.JobSucceeded || d.Job.Attempts < 2 {
			t.Fatalf("recovery: %s after %d attempts: %s", d.Job.State, d.Job.Attempts, d.Job.Error)
		}
		if got := e.data(site.ID); !strings.Contains(got, "post=v1") {
			t.Errorf("database = %q", got)
		}
	})
}
