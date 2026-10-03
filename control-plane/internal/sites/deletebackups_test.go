package sites_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/rchouinard/pressfleet/control-plane/internal/backup"
	"github.com/rchouinard/pressfleet/control-plane/internal/backup/backuptest"
	"github.com/rchouinard/pressfleet/control-plane/internal/sites"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
)

// backedUp gives the site n snapshots, each with a succeeded backup
// record, as the backup job would.
func (e *restoreEnv) backedUp(t *testing.T, site store.Site, n int) {
	t.Helper()
	ctx := context.Background()
	records := store.NewBackupRecords(e.db)
	for range n {
		rec := &store.BackupRecord{SiteID: site.ID, ClientGroupID: site.ClientGroupID, Kind: store.BackupManual}
		if err := records.Create(ctx, rec); err != nil {
			t.Fatal(err)
		}
		snap, err := e.engine.Backup(ctx, site.ID, backup.Options{Kind: string(rec.Kind), BackupID: rec.ID})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := records.Succeed(ctx, rec.ID, store.BackupResult{SnapshotID: string(snap.ID)}); err != nil {
			t.Fatal(err)
		}
	}
}

func (e *restoreEnv) snapshotCount(t *testing.T, siteID string) int {
	t.Helper()
	snaps, err := e.engine.ListSnapshots(context.Background(), siteID)
	if err != nil {
		t.Fatal(err)
	}
	return len(snaps)
}

func (e *restoreEnv) recordCount(t *testing.T, siteID string) int {
	t.Helper()
	return e.count(t, "SELECT COUNT(*) FROM backup_records WHERE site_id = ?", siteID)
}

func jobSteps(d store.JobDetail) string {
	var steps []string
	for _, s := range d.Steps {
		steps = append(steps, s.Name)
	}
	return strings.Join(steps, ",")
}

func TestDeletionWithBackups(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newRestoreEnv(t, db)
		doomed := e.runningSite(t, "Doomed", "")
		neighbor := e.runningSite(t, "Neighbor", "")
		e.backedUp(t, doomed, 2)
		e.backedUp(t, neighbor, 1)

		job, err := e.svc.Delete(ctx, doomed.ID, "", sites.DeleteOptions{DeleteBackups: true})
		if err != nil {
			t.Fatal(err)
		}
		e.runJobs(t)
		d := e.job(t, job.ID)
		if d.Job.State != store.JobSucceeded {
			t.Fatalf("job %s: %s", d.Job.State, d.Job.Error)
		}
		if got := jobSteps(d); got != "mark_deleting,delete_stack,drop_database,delete_object_cache,delete_secrets,delete_backups,delete_record" {
			t.Errorf("steps = %s", got)
		}
		var res map[string]int
		if err := json.Unmarshal(d.Job.Result, &res); err != nil {
			t.Fatal(err)
		}
		if res["forgotten_snapshots"] != 2 || res["deleted_backup_records"] != 2 {
			t.Errorf("result = %s", d.Job.Result)
		}
		// assertGone would count the neighbor's secrets.
		if _, err := store.NewSites(db).Get(ctx, doomed.ID); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("site row: err = %v, want ErrNotFound", err)
		}
		if n := e.snapshotCount(t, doomed.ID); n != 0 {
			t.Errorf("%d snapshots of the deleted site left", n)
		}
		if n := e.recordCount(t, doomed.ID); n != 0 {
			t.Errorf("%d backup records of the deleted site left", n)
		}
		// The other site of the group keeps its backups.
		if n := e.snapshotCount(t, neighbor.ID); n != 1 {
			t.Errorf("neighbor has %d snapshots, want 1", n)
		}
		if n := e.recordCount(t, neighbor.ID); n != 1 {
			t.Errorf("neighbor has %d backup records, want 1", n)
		}
	})
}

func TestDeletionKeepsBackupsByDefault(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newRestoreEnv(t, db)
		site := e.runningSite(t, "Archived", "")
		e.backedUp(t, site, 2)

		job, err := e.svc.Delete(ctx, site.ID, "", sites.DeleteOptions{})
		if err != nil {
			t.Fatal(err)
		}
		e.runJobs(t)
		d := e.job(t, job.ID)
		if d.Job.State != store.JobSucceeded {
			t.Fatalf("job %s: %s", d.Job.State, d.Job.Error)
		}
		if strings.Contains(string(d.Job.Result), "forgotten_snapshots") {
			t.Errorf("result = %s", d.Job.Result)
		}
		e.assertGone(t, site.ID)
		if n := e.snapshotCount(t, site.ID); n != 2 {
			t.Errorf("%d snapshots left, want 2", n)
		}
		if n := e.recordCount(t, site.ID); n != 2 {
			t.Errorf("%d backup records left, want 2", n)
		}
		if slices.ContainsFunc(e.engine.Calls(), func(c backuptest.Call) bool { return c.Op == "ForgetSite" }) {
			t.Error("ForgetSite was called")
		}
	})
}

func TestDeletionWithBackupsFailureKeepsThem(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newRestoreEnv(t, db)
		site := e.runningSite(t, "Locked", "")
		e.backedUp(t, site, 2)
		e.engine.Err = func(op, _ string) error {
			if op == "ForgetSite" {
				return errors.New("repository is already locked exclusively")
			}
			return nil
		}

		first, err := e.svc.Delete(ctx, site.ID, "", sites.DeleteOptions{DeleteBackups: true})
		if err != nil {
			t.Fatal(err)
		}
		e.runJobs(t)
		if d := e.job(t, first.ID); d.Job.State != store.JobFailed {
			t.Fatalf("job %s: %s", d.Job.State, d.Job.Error)
		}
		// The site's resources are gone, but its record and every backup
		// remain, and it can be deleted again.
		got := e.site(t, site.ID)
		if got.Status != store.SiteFailed || !strings.Contains(got.StatusMessage, "locked exclusively") {
			t.Errorf("site after a failed deletion = %+v", got)
		}
		if _, ok := e.prov.Site(site.ID); ok {
			t.Error("the stack is still there")
		}
		if n := e.snapshotCount(t, site.ID); n != 2 {
			t.Errorf("%d snapshots left, want 2", n)
		}
		if n := e.recordCount(t, site.ID); n != 2 {
			t.Errorf("%d backup records left, want 2", n)
		}

		e.engine.Err = nil
		second, err := e.svc.Delete(ctx, site.ID, "", sites.DeleteOptions{DeleteBackups: true})
		if err != nil || second.ID == first.ID {
			t.Fatalf("Delete again = %s, %v", second.ID, err)
		}
		e.runJobs(t)
		if d := e.job(t, second.ID); d.Job.State != store.JobSucceeded {
			t.Fatalf("retry job %s: %s", d.Job.State, d.Job.Error)
		}
		e.assertGone(t, site.ID)
		if n := e.snapshotCount(t, site.ID); n != 0 {
			t.Errorf("%d snapshots left after the retry", n)
		}
		if n := e.recordCount(t, site.ID); n != 0 {
			t.Errorf("%d backup records left after the retry", n)
		}
	})
}

// forgetsThenFails is a backup engine whose first ForgetSite forgets the
// snapshots and then fails, as if the prune had been interrupted.
type forgetsThenFails struct {
	backup.Engine
	failed bool
}

func (f *forgetsThenFails) ForgetSite(ctx context.Context, siteID string) (backup.PruneResult, error) {
	res, err := f.Engine.ForgetSite(ctx, siteID)
	if err == nil && !f.failed {
		f.failed = true
		return backup.PruneResult{}, errors.New("prune interrupted")
	}
	return res, err
}

func TestDeletionWithBackupsRetryCountsEveryAttempt(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newRestoreEnvWith(t, db, restoreManifest, func(o *sites.Options) {
			o.BackupEngine = &forgetsThenFails{Engine: o.BackupEngine}
		})
		site := e.runningSite(t, "Flaky", "")
		e.backedUp(t, site, 3)
		job, err := e.svc.Delete(ctx, site.ID, "", sites.DeleteOptions{DeleteBackups: true})
		if err != nil {
			t.Fatal(err)
		}
		e.runJobs(t)
		d := e.job(t, job.ID)
		if d.Job.State != store.JobSucceeded {
			t.Fatalf("job %s: %s", d.Job.State, d.Job.Error)
		}
		var res map[string]int
		if err := json.Unmarshal(d.Job.Result, &res); err != nil {
			t.Fatal(err)
		}
		// The retry found nothing left to forget; the records went with it.
		if res["forgotten_snapshots"] != 0 || res["deleted_backup_records"] != 3 {
			t.Errorf("result = %s", d.Job.Result)
		}
		e.assertGone(t, site.ID)
		if n := e.recordCount(t, site.ID); n != 0 {
			t.Errorf("%d backup records left", n)
		}
	})
}

func TestDeletionWithBackupsNeedsAnEngine(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		site, _ := e.create(t, "Plain")
		e.runJobs(t)
		if _, err := e.svc.Delete(context.Background(), site.ID, "", sites.DeleteOptions{DeleteBackups: true}); !errors.Is(err, sites.ErrNoBackupEngine) {
			t.Errorf("Delete = %v, want ErrNoBackupEngine", err)
		}
		if got := e.site(t, site.ID); got.Status != store.SiteRunning {
			t.Errorf("status = %s, want running", got.Status)
		}
	})
}
