package sites_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/rchouinard/pressfleet/control-plane/internal/backup"
	"github.com/rchouinard/pressfleet/control-plane/internal/jobs"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
	"github.com/rchouinard/pressfleet/control-plane/internal/sitedb"
	"github.com/rchouinard/pressfleet/control-plane/internal/sites"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
)

// Restoring a site marked failed (restore.go, "Restoring a failed site").

// failedSite returns a site left failed the way an operator meets one: a
// restore failed and its rollback failed too, so it is in maintenance mode with
// broken data. It also returns a good snapshot, holding "post=good".
func (e *restoreEnv) failedSite(t *testing.T, name string) (store.Site, backup.Snapshot) {
	t.Helper()
	site := e.runningSite(t, name, "post=good")
	snap := e.snapshot(t, site.ID)
	e.setFailWP(func(_ string, args []string) error {
		if args[0] == "core" && args[1] == "update-db" {
			return &provisioner.ExecError{ExitCode: 255, Stderr: "PHP Fatal error"}
		}
		return nil
	})
	var restores int
	e.engine.restoreErr = func(int, backup.SnapshotID) error {
		if restores++; restores > 1 {
			return errors.New("repository unreachable")
		}
		return nil
	}
	if d := e.restore(t, site.ID, sites.Restore{SnapshotID: string(snap.ID)}); d.Job.State != store.JobFailed {
		t.Fatalf("restore job: %s %s\n%s", d.Job.State, d.Job.Error, jobLog(d))
	}
	e.setFailWP(nil)
	e.engine.restoreErr = nil
	e.dbs.SetData(site.ID, []byte("post=half-imported\n"))
	site = e.site(t, site.ID)
	if site.Status != store.SiteFailed || !site.Maintenance {
		t.Fatalf("site after the failed rollback: %s, maintenance %v", site.Status, site.Maintenance)
	}
	return site, snap
}

// requireRecovered checks the restored data, the status, and whether the site
// serves visitors.
func (e *restoreEnv) requireRecovered(t *testing.T, siteID string, d store.JobDetail, maintenance bool) store.Site {
	t.Helper()
	if d.Job.State != store.JobSucceeded {
		t.Fatalf("restore job: %s %s\n%s", d.Job.State, d.Job.Error, jobLog(d))
	}
	if got := e.data(siteID); !strings.Contains(got, "post=good") {
		t.Errorf("database after the restore = %q", got)
	}
	got := e.site(t, siteID)
	rt, ok := e.prov.Site(siteID)
	if !ok || rt.Phase != provisioner.SiteRunning {
		t.Errorf("containers after the restore: %+v (exist: %v)", rt, ok)
	}
	if got.Status != store.SiteRunning || got.Maintenance != maintenance || rt.Maintenance != maintenance {
		t.Errorf("site after the restore: %s, maintenance record %v, page %v; want running, %v", got.Status, got.Maintenance, rt.Maintenance, maintenance)
	}
	return got
}

func (e *restoreEnv) requireStep(t *testing.T, d store.JobDetail, wantInLog string) {
	t.Helper()
	if log := jobLog(d); !strings.Contains(log, wantInLog) {
		t.Errorf("job log lacks %q:\n%s", wantInLog, log)
	}
}

// requireNoMaintenanceClaim: a status message outlives the maintenance flag, so
// it mustn't say whether the site is in maintenance mode, or to turn it off.
// The earlier message it may quote is held to the same rule.
func requireNoMaintenanceClaim(t *testing.T, site store.Site) {
	t.Helper()
	if strings.Contains(strings.ToLower(site.StatusMessage), "maintenance") {
		t.Errorf("status message speaks of maintenance mode: %q", site.StatusMessage)
	}
}

func provOps(e *restoreEnv, siteID string) []string {
	var ops []string
	for _, c := range e.prov.Calls() {
		if c.SiteID == siteID {
			ops = append(ops, c.Op)
		}
	}
	return ops
}

func TestRestoreFailedSite(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newRestoreEnv(t, db)
		site, snap := e.failedSite(t, "Acme")
		if !strings.Contains(site.StatusMessage, "Restore the pre-restore backup to try again") {
			t.Errorf("status message = %q", site.StatusMessage)
		}
		from := len(e.engine.Calls())

		queued, job, err := e.svc.Restore(context.Background(), site.ID, sites.Restore{SnapshotID: string(snap.ID)})
		if err != nil {
			t.Fatalf("Restore of a failed site: %v", err)
		}
		if queued.Status != store.SiteUpdating || job.Type != sites.RestoreJobType {
			t.Errorf("Restore returned %s, %+v", queued.Status, job)
		}
		// One at a time, as for a running site.
		if _, _, err := e.svc.Restore(context.Background(), site.ID, sites.Restore{SnapshotID: string(snap.ID)}); !errors.Is(err, sites.ErrBusy) {
			t.Errorf("Restore while one is queued = %v", err)
		}
		e.runJobs(t)
		d := e.job(t, job.ID)

		got := e.requireRecovered(t, site.ID, d, true)
		// Maintenance mode is on because the failure left it: the site says so.
		if !strings.Contains(got.StatusMessage, "recovered the site") {
			t.Errorf("status message = %q", got.StatusMessage)
		}
		requireNoMaintenanceClaim(t, got)
		e.requireStep(t, d, "not serving visitors yet")

		// The state it failed in was backed up first, in case that is all
		// that's left of what happened since the snapshot.
		pre := e.preRestoreBackups(t, site.ID)
		newest := pre[0]
		if newest.JobID != job.ID || newest.Status != store.BackupSucceeded {
			t.Fatalf("pre-restore backups = %+v", pre)
		}
		want := []string{"Backup " + site.ID + " pre_restore", "Restore " + site.ID + " " + string(snap.ID) + " " + site.ID}
		if ops := e.engineOps(from); !slices.Equal(ops, want) {
			t.Errorf("engine calls = %q, want %q", ops, want)
		}
		if res := jobResult(t, d.Job); res["pre_restore_backup_id"] != newest.ID {
			t.Errorf("job result = %v", res)
		}

		// Turning it off serves the site, and it can be restored like any other.
		if _, _, err := e.svc.SetMaintenance(context.Background(), site.ID, false, ""); err != nil {
			t.Fatal(err)
		}
		e.runJobs(t)
		if rt, _ := e.prov.Site(site.ID); rt.Maintenance {
			t.Error("still on the maintenance page")
		}
		d = e.restore(t, site.ID, sites.Restore{SnapshotID: string(snap.ID)})
		if got := e.requireRecovered(t, site.ID, d, false); got.StatusMessage != "" {
			t.Errorf("status message of a running site's restore = %q", got.StatusMessage)
		}
	})
}

// TestRestoreFailedSiteStates runs the restore against what a failed site may
// be missing, and against nothing missing.
func TestRestoreFailedSiteStates(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name    string
		breakIt func(t *testing.T, e *restoreEnv, siteID string)
		// lost is whether the database, and so what the undo point could
		// hold of it, is gone.
		lost bool
	}{
		{name: "containers up", breakIt: func(*testing.T, *restoreEnv, string) {}},
		{name: "containers stopped", breakIt: func(t *testing.T, e *restoreEnv, siteID string) {
			if err := e.prov.StopSite(ctx, siteID); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "containers missing", breakIt: func(t *testing.T, e *restoreEnv, siteID string) {
			if err := e.prov.DeleteSite(ctx, siteID); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "database missing", lost: true, breakIt: func(t *testing.T, e *restoreEnv, siteID string) {
			if err := e.dbs.DropDatabase(ctx, siteID); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "object cache user missing", breakIt: func(t *testing.T, e *restoreEnv, siteID string) {
			if err := e.cache.DeleteUser(ctx, siteID); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "everything missing", lost: true, breakIt: func(t *testing.T, e *restoreEnv, siteID string) {
			if err := e.prov.DeleteSite(ctx, siteID); err != nil {
				t.Fatal(err)
			}
			if err := e.dbs.DropDatabase(ctx, siteID); err != nil {
				t.Fatal(err)
			}
			if err := e.cache.DeleteUser(ctx, siteID); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			storetest.Run(t, func(t *testing.T, db *store.DB) {
				e := newRestoreEnv(t, db)
				site, snap := e.failedSite(t, "Acme")
				tc.breakIt(t, e, site.ID)
				d := e.restore(t, site.ID, sites.Restore{SnapshotID: string(snap.ID)})
				e.requireRecovered(t, site.ID, d, true)
				if !e.cache.HasUser(site.ID) {
					t.Error("the site has no object cache user")
				}
				rt, _ := e.prov.Site(site.ID)
				if rt.Config.PreviewDomain != site.PreviewDomain || rt.Config.RedisSecretRef == "" || rt.Config.DBSecretRef == "" {
					t.Errorf("site rendered with %+v", rt.Config)
				}
				// The backup that a restore of a broken site takes first isn't
				// lost when something is missing: it is of what is left.
				pre := e.preRestoreBackups(t, site.ID)
				if newest := pre[0]; newest.Status != store.BackupSucceeded || newest.JobID != d.Job.ID {
					t.Errorf("pre-restore backups = %+v", pre)
				}
				if !tc.lost {
					data := e.engine.dataOf(backup.SnapshotID(pre[0].SnapshotID))
					if !strings.Contains(data, "post=half-imported") {
						t.Errorf("the undo point holds %q", data)
					}
				}
			})
		})
	}
}

func (d *dataEngine) dataOf(snap backup.SnapshotID) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return string(d.data[snap])
}

// TestRestoreFailedSiteNoUndoPoint: a backup that can't be taken doesn't stop
// the restore of a failed site, and the job, the backup, and the site say there
// is nothing to undo it with.
func TestRestoreFailedSiteNoUndoPoint(t *testing.T) {
	for _, tc := range []struct {
		name     string
		err      error
		attempts int
	}{
		{"transient until it stops trying", errors.New("mariadb-dump: table is marked as crashed"), 3},
		{"permanent", sitedb.ErrDatabaseNotFound, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			storetest.Run(t, func(t *testing.T, db *store.DB) {
				e := newRestoreEnv(t, db)
				site, snap := e.failedSite(t, "Acme")
				from := len(e.engine.Calls())
				var backups int
				e.engine.Err = func(op, _ string) error {
					if op == "Backup" {
						backups++
						return tc.err
					}
					return nil
				}
				d := e.restore(t, site.ID, sites.Restore{SnapshotID: string(snap.ID)})
				e.engine.Err = nil

				got := e.requireRecovered(t, site.ID, d, true)
				if backups != tc.attempts || d.Job.Attempts != tc.attempts {
					t.Errorf("%d backups in %d job attempts, want %d", backups, d.Job.Attempts, tc.attempts)
				}
				if !strings.Contains(got.StatusMessage, "no pre-restore backup could be taken") ||
					!strings.Contains(got.StatusMessage, tc.err.Error()) {
					t.Errorf("status message = %q", got.StatusMessage)
				}
				requireNoMaintenanceClaim(t, got)
				e.requireStep(t, d, "Couldn't take a pre-restore backup")
				pre := e.preRestoreBackups(t, site.ID)
				newest := pre[0]
				if newest.JobID != d.Job.ID || newest.Status != store.BackupFailed || !strings.Contains(newest.Message, tc.err.Error()) {
					t.Errorf("pre-restore backup = %+v", newest)
				}
				// Nothing names a backup that can undo this.
				if res := jobResult(t, d.Job); res["pre_restore_backup_id"] != "" {
					t.Errorf("job result = %v", res)
				}
				if ops := e.engineOps(from); !slices.Contains(ops, "Restore "+site.ID+" "+string(snap.ID)+" "+site.ID) {
					t.Errorf("engine calls = %q", ops)
				}
			})
		})
	}
}

// TestRestoreFailedSiteShutdownKeepsUndoPoint: a step canceled by a shutdown or
// a lost lease says nothing about the site, even on the attempt that would give
// up on the backup.
func TestRestoreFailedSiteShutdownKeepsUndoPoint(t *testing.T) {
	e := newRestoreEnv(t, storetest.Open(t, store.SQLite))
	site, snap := e.failedSite(t, "Acme")
	_, job, err := e.svc.Restore(context.Background(), site.ID, sites.Restore{SnapshotID: string(snap.ID)})
	if err != nil {
		t.Fatal(err)
	}
	var (
		backups int
		cancel  context.CancelFunc
	)
	e.engine.Err = func(op, _ string) error {
		if op != "Backup" {
			return nil
		}
		switch backups++; backups {
		case 1, 2:
			return errors.New("connection reset")
		case 3:
			cancel()
			return context.Canceled
		}
		return nil
	}
	for {
		var ctx context.Context
		ctx, cancel = context.WithCancel(context.Background())
		ran, err := e.pool.RunNext(ctx)
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		if !ran {
			break
		}
	}
	d := e.job(t, job.ID)
	got := e.requireRecovered(t, site.ID, d, true)
	if backups != 4 || strings.Contains(got.StatusMessage, "no pre-restore backup") {
		t.Errorf("%d backup attempts; status message %q", backups, got.StatusMessage)
	}
	pre := e.preRestoreBackups(t, site.ID)
	if newest := pre[0]; newest.JobID != job.ID || newest.Status != store.BackupSucceeded {
		t.Errorf("pre-restore backup = %+v", newest)
	}
}

// TestRestoreRunningSiteNeedsUndoPoint: all of this is only for a failed site.
// A running site's failed backup stops its restore, and the job doesn't touch
// its stack, database, or object cache user first.
func TestRestoreRunningSiteNeedsUndoPoint(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newRestoreEnv(t, db)
		site := e.runningSite(t, "Acme", "post=v1")
		snap := e.snapshot(t, site.ID)
		before := len(provOps(e, site.ID))
		dbCalls, cacheCalls := len(e.dbs.Calls()), len(e.cache.Calls())
		e.engine.Err = func(op, _ string) error {
			if op == "Backup" {
				return sitedb.ErrDatabaseNotFound
			}
			return nil
		}
		from := len(e.engine.Calls())
		d := e.restore(t, site.ID, sites.Restore{SnapshotID: string(snap.ID)})
		if d.Job.State != store.JobFailed {
			t.Fatalf("restore job: %s %s\n%s", d.Job.State, d.Job.Error, jobLog(d))
		}
		if ops := e.engineOps(from); slices.ContainsFunc(ops, func(o string) bool { return strings.HasPrefix(o, "Restore") }) {
			t.Errorf("engine calls = %q", ops)
		}
		got := e.site(t, site.ID)
		if got.Status != store.SiteRunning || got.Maintenance || !strings.Contains(got.StatusMessage, "restore failed before changing the site") {
			t.Errorf("site: %s, maintenance %v, %q", got.Status, got.Maintenance, got.StatusMessage)
		}
		for _, op := range provOps(e, site.ID)[before:] {
			if op == "CreateSite" {
				t.Error("the job rendered a running site's stack")
			}
		}
		for _, c := range e.dbs.Calls()[dbCalls:] {
			if c.Op == "CreateDatabase" {
				t.Error("the job created a running site's database")
			}
		}
		for _, c := range e.cache.Calls()[cacheCalls:] {
			if c.Op == "CreateUser" {
				t.Error("the job created a running site's object cache user")
			}
		}
	})
}

// TestRestoreFailedSiteFailsAgain: a restore of a failed site that fails too
// leaves it failed, says truthfully where its data is, keeps what the site said
// before, and can be tried again.
func TestRestoreFailedSiteFailsAgain(t *testing.T) {
	wpFatal := func(_ string, args []string) error {
		if args[0] == "core" && args[1] == "update-db" {
			return &provisioner.ExecError{ExitCode: 255, Stderr: "PHP Fatal error"}
		}
		return nil
	}
	transient := errors.New("connection reset by peer")
	for _, tc := range []struct {
		name   string
		inject func(e *restoreEnv, snap backup.SnapshotID)
		msg    string
		// data is what the database holds after the job.
		data string
	}{
		{
			name: "the stack won't start",
			inject: func(e *restoreEnv, _ backup.SnapshotID) {
				e.prov.Err = func(op, _ string) error {
					if op == "CreateSite" {
						return transient
					}
					return nil
				}
			},
			msg:  "restore failed before changing the site's data",
			data: "post=half-imported",
		},
		{
			name: "maintenance mode can't be set",
			inject: func(e *restoreEnv, _ backup.SnapshotID) {
				e.prov.Err = func(op, _ string) error {
					if op == "SetMaintenance" {
						return transient
					}
					return nil
				}
			},
			msg:  "restore failed before changing the site's data",
			data: "post=half-imported",
		},
		{
			name: "rolled back to the state it failed in",
			inject: func(e *restoreEnv, _ backup.SnapshotID) {
				e.setFailWP(wpFatal)
			},
			msg:  "rolled back to its pre-restore backup",
			data: "post=half-imported",
		},
		{
			name: "the rollback fails too",
			inject: func(e *restoreEnv, _ backup.SnapshotID) {
				e.setFailWP(wpFatal)
				var n int
				e.engine.restoreErr = func(int, backup.SnapshotID) error {
					if n++; n > 1 {
						return transient
					}
					return nil
				}
			},
			msg:  "rolling back to the pre-restore backup failed too",
			data: "post=good",
		},
		{
			name: "no undo point to roll back to",
			inject: func(e *restoreEnv, _ backup.SnapshotID) {
				e.setFailWP(wpFatal)
				e.engine.Err = func(op, _ string) error {
					if op == "Backup" {
						return sitedb.ErrDatabaseNotFound
					}
					return nil
				}
			},
			msg:  "there is no pre-restore backup to roll back to",
			data: "post=good",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			storetest.Run(t, func(t *testing.T, db *store.DB) {
				e := newRestoreEnv(t, db)
				site, snap := e.failedSite(t, "Acme")
				tc.inject(e, snap.ID)

				d := e.restore(t, site.ID, sites.Restore{SnapshotID: string(snap.ID)})
				if d.Job.State != store.JobFailed {
					t.Fatalf("restore job: %s %s\n%s", d.Job.State, d.Job.Error, jobLog(d))
				}
				got := e.site(t, site.ID)
				if got.Status != store.SiteFailed || !got.Maintenance {
					t.Errorf("site: %s, maintenance %v; want failed, in maintenance mode", got.Status, got.Maintenance)
				}
				if !strings.Contains(got.StatusMessage, tc.msg) || !strings.Contains(got.StatusMessage, "to try again") {
					t.Errorf("status message = %q, want %q", got.StatusMessage, tc.msg)
				}
				if !strings.Contains(got.StatusMessage, "before this restore: restore failed, and rolling back") {
					t.Errorf("status message lost what the site said before: %q", got.StatusMessage)
				}
				requireNoMaintenanceClaim(t, got)
				if data := e.data(site.ID); !strings.Contains(data, tc.data) {
					t.Errorf("database = %q, want %q", data, tc.data)
				}

				e.prov.Err, e.engine.Err, e.engine.restoreErr = nil, nil, nil
				e.setFailWP(nil)
				d = e.restore(t, site.ID, sites.Restore{SnapshotID: string(snap.ID)})
				e.requireRecovered(t, site.ID, d, true)
			})
		})
	}
}

// TestRestoreAfterCoreRevertFailed: the other way a restore marks a site failed.
// The data was rolled back, but the containers kept the newer core. The status
// message's way out works, and puts the recorded core back on them.
func TestRestoreAfterCoreRevertFailed(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newRestoreEnv(t, db)
		site := e.runningSite(t, "Acme", "post=good")
		snap := e.snapshot(t, site.ID)
		e.engine.cores[snap.ID] = "7.1"
		if _, err := store.NewSites(db).SetWPCoreVersion(context.Background(), site.ID, "6.9"); err != nil {
			t.Fatal(err)
		}
		e.setFailWP(func(_ string, args []string) error {
			if args[0] == "core" && args[1] == "update-db" {
				return &provisioner.ExecError{ExitCode: 255, Stderr: "PHP Fatal error"}
			}
			return nil
		})
		var updates int
		e.prov.Err = func(op, _ string) error {
			if op == "UpdateSite" {
				if updates++; updates > 1 {
					return errors.New("image pull failed")
				}
			}
			return nil
		}
		d := e.restore(t, site.ID, sites.Restore{SnapshotID: string(snap.ID)})
		e.setFailWP(nil)
		e.prov.Err = nil
		if d.Job.State != store.JobFailed {
			t.Fatalf("restore job: %s %s\n%s", d.Job.State, d.Job.Error, jobLog(d))
		}
		got := e.site(t, site.ID)
		rt, _ := e.prov.Site(site.ID)
		if got.Status != store.SiteFailed || !strings.Contains(got.StatusMessage, "couldn't be put back") ||
			!strings.Contains(got.StatusMessage, "restore a backup to try again") {
			t.Fatalf("site: %s %q", got.Status, got.StatusMessage)
		}
		if got.WPCoreVersion != "6.9" || rt.Config.WPCoreVersion != "7.1.2" {
			t.Fatalf("core: record %s, containers %s", got.WPCoreVersion, rt.Config.WPCoreVersion)
		}

		pre := e.preRestoreBackups(t, site.ID)
		d = e.restore(t, site.ID, sites.Restore{SnapshotID: pre[0].SnapshotID})
		if d.Job.State != store.JobSucceeded {
			t.Fatalf("restore job: %s %s\n%s", d.Job.State, d.Job.Error, jobLog(d))
		}
		got = e.site(t, site.ID)
		rt, _ = e.prov.Site(site.ID)
		if got.Status != store.SiteRunning || got.WPCoreVersion != "6.9" || rt.Config.WPCoreVersion != "6.9" {
			t.Errorf("site: %s, core: record %s, containers %s", got.Status, got.WPCoreVersion, rt.Config.WPCoreVersion)
		}
	})
}

// TestRestoreFailedSiteAfterCompletion: a job that fails once the restore is
// complete has repaired the site, so it doesn't mark it failed again.
func TestRestoreFailedSiteAfterCompletion(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newRestoreEnv(t, db)
		site, snap := e.failedSite(t, "Acme")
		// Maintenance off, so the job has the provisioner turn it off at the end.
		if _, _, err := e.svc.SetMaintenance(context.Background(), site.ID, false, ""); err != nil {
			t.Fatal(err)
		}
		e.runJobs(t)
		var n int
		e.prov.Err = func(op, _ string) error {
			if op == "SetMaintenance" {
				if n++; n > 1 {
					return errors.New("edge router unreachable")
				}
			}
			return nil
		}
		d := e.restore(t, site.ID, sites.Restore{SnapshotID: string(snap.ID)})
		e.prov.Err = nil
		if d.Job.State != store.JobFailed {
			t.Fatalf("restore job: %s %s\n%s", d.Job.State, d.Job.Error, jobLog(d))
		}
		got := e.site(t, site.ID)
		if got.Status != store.SiteRunning || !strings.Contains(got.StatusMessage, "the restore finished") {
			t.Errorf("site: %s %q", got.Status, got.StatusMessage)
		}
		requireNoMaintenanceClaim(t, got)
		e.requireStep(t, d, "may still be in maintenance mode")
		if data := e.data(site.ID); !strings.Contains(data, "post=good") {
			t.Errorf("database = %q", data)
		}
	})
}

// TestStatusMessageSurvivesMaintenance: the message of a rolled back restore
// says what happened, says nothing about maintenance mode, and is still there,
// and still true, once maintenance mode is off. The job's log has the moment.
func TestStatusMessageSurvivesMaintenance(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newRestoreEnv(t, db)
		site := e.runningSite(t, "Acme", "post=good")
		snap := e.snapshot(t, site.ID)
		e.setFailWP(func(_ string, args []string) error {
			if args[0] == "core" && args[1] == "update-db" {
				return &provisioner.ExecError{ExitCode: 255, Stderr: "PHP Fatal error"}
			}
			return nil
		})
		d := e.restore(t, site.ID, sites.Restore{SnapshotID: string(snap.ID)})
		e.setFailWP(nil)
		if d.Job.State != store.JobFailed {
			t.Fatalf("restore job: %s %s\n%s", d.Job.State, d.Job.Error, jobLog(d))
		}
		e.requireStep(t, d, "stays in maintenance mode")
		got := e.site(t, site.ID)
		pre := e.preRestoreBackups(t, site.ID)
		if got.Status != store.SiteRunning || !got.Maintenance ||
			!strings.Contains(got.StatusMessage, "rolled back to its pre-restore backup") || !strings.Contains(got.StatusMessage, pre[0].ID) {
			t.Errorf("site after the rollback: %s, maintenance %v, %q", got.Status, got.Maintenance, got.StatusMessage)
		}
		requireNoMaintenanceClaim(t, got)
		if _, _, err := e.svc.SetMaintenance(ctx, site.ID, false, ""); err != nil {
			t.Fatal(err)
		}
		e.runJobs(t)
		if after := e.site(t, site.ID); after.Maintenance || after.StatusMessage != got.StatusMessage {
			t.Errorf("after turning maintenance mode off: maintenance %v, %q", after.Maintenance, after.StatusMessage)
		}
	})
}

// TestRestoreFailedSiteNotInMaintenance: a failed site that wasn't in
// maintenance mode serves visitors once it is restored, and needs no message.
func TestRestoreFailedSiteNotInMaintenance(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newRestoreEnv(t, db)
		site := e.runningSite(t, "Acme", "post=good")
		snap := e.snapshot(t, site.ID)
		if _, err := store.NewSites(db).SetStatus(context.Background(), site.ID, store.SiteFailed, "update failed"); err != nil {
			t.Fatal(err)
		}
		d := e.restore(t, site.ID, sites.Restore{SnapshotID: string(snap.ID)})
		got := e.requireRecovered(t, site.ID, d, false)
		if got.StatusMessage != "" {
			t.Errorf("status message = %q", got.StatusMessage)
		}
		e.requireStep(t, d, "serve the site again")
	})
}

// TestRestoreNeverProvisioned: sites whose provisioning or clone failed have no
// secrets left and stay refused, with nothing queued.
func TestRestoreNeverProvisioned(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newPushEnv(t, db)
		snap := e.snapshot(t, e.prod.ID)
		e.prov.Err = func(op, _ string) error {
			if op == "CreateSite" {
				return errors.New("no space left on device")
			}
			return nil
		}
		created, _ := e.create(t, "Never")
		clone := e.createStaging(t, e.prod.ID, sites.NewStaging{})
		e.runJobs(t)
		e.prov.Err = nil
		before := e.count(t, "SELECT COUNT(*) FROM jobs WHERE type = ?", sites.RestoreJobType)
		for name, siteID := range map[string]string{"provisioning": created.ID, "clone": clone.Site.ID} {
			got := e.site(t, siteID)
			if got.Status != store.SiteFailed || got.WPSaltsSecretRef != "" {
				t.Fatalf("%s: site is %s with salts %q", name, got.Status, got.WPSaltsSecretRef)
			}
			_, _, err := e.svc.Restore(ctx, siteID, sites.Restore{SnapshotID: string(snap.ID), SourceSiteID: e.prod.ID})
			if !errors.Is(err, sites.ErrNotProvisioned) {
				t.Errorf("Restore after a failed %s = %v", name, err)
			}
			if after := e.site(t, siteID); after.Status != store.SiteFailed || after.StatusMessage != got.StatusMessage {
				t.Errorf("%s: the refused restore changed the site: %s %q", name, after.Status, after.StatusMessage)
			}
		}
		if n := e.count(t, "SELECT COUNT(*) FROM jobs WHERE type = ?", sites.RestoreJobType); n != before {
			t.Errorf("%d restore jobs queued", n-before)
		}
	})
}

// TestRestoreConcurrentRequests: of requests that all see the site restorable,
// one queues a job. The status change decides it, on both databases.
func TestRestoreConcurrentRequests(t *testing.T) {
	for _, status := range []store.SiteStatus{store.SiteRunning, store.SiteFailed} {
		t.Run(string(status), func(t *testing.T) {
			storetest.Run(t, func(t *testing.T, db *store.DB) {
				e := newRestoreEnv(t, db)
				site := e.runningSite(t, "Acme", "post=good")
				snap := e.snapshot(t, site.ID)
				e.setStatus(t, site.ID, status)

				const requests = 8
				var (
					wg      sync.WaitGroup
					errs    [requests]error
					barrier = make(chan struct{})
				)
				for i := range requests {
					wg.Go(func() {
						<-barrier
						_, _, errs[i] = e.svc.Restore(context.Background(), site.ID, sites.Restore{SnapshotID: string(snap.ID)})
					})
				}
				close(barrier)
				wg.Wait()
				queued := 0
				for _, err := range errs {
					switch {
					case err == nil:
						queued++
					case !errors.Is(err, sites.ErrBusy):
						t.Errorf("concurrent Restore = %v, want ErrBusy", err)
					}
				}
				if queued != 1 {
					t.Errorf("%d requests queued a job, want 1: %v", queued, errs)
				}
				if n := e.count(t, "SELECT COUNT(*) FROM jobs WHERE type = ?", sites.RestoreJobType); n != 1 {
					t.Errorf("%d restore jobs", n)
				}
			})
		})
	}
}

// TestPushAndRefreshRefuseFailedTargets: restore is the only job of the pipeline
// that takes a failed target.
func TestPushAndRefreshRefuseFailedTargets(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newRefreshEnv(t, db)
		restored := func(siteID string) {
			t.Helper()
			if got := e.site(t, siteID); got.Status != store.SiteFailed || got.StatusMessage != "" {
				t.Errorf("the refused request changed the site: %s %q", got.Status, got.StatusMessage)
			}
			e.setStatus(t, siteID, store.SiteRunning)
		}
		before := e.count(t, "SELECT COUNT(*) FROM jobs")

		e.setStatus(t, e.prod.ID, store.SiteFailed)
		_, err := e.svc.PushStaging(ctx, e.stg.ID, sites.StagingPush{Files: backup.RestoreFilesAll, Database: true})
		if !errors.Is(err, sites.ErrNotRunning) {
			t.Errorf("push to a failed production site = %v, want ErrNotRunning", err)
		}
		restored(e.prod.ID)

		e.setStatus(t, e.stg.ID, store.SiteFailed)
		snap := e.newestSnapshot(t, e.prod.ID)
		_, err = e.svc.RefreshStaging(ctx, e.stg.ID, sites.StagingRefresh{Files: backup.RestoreFilesAll, Database: true, SnapshotID: string(snap.ID)})
		if !errors.Is(err, sites.ErrNotRunning) {
			t.Errorf("refresh of a failed staging site = %v, want ErrNotRunning", err)
		}
		restored(e.stg.ID)

		if n := e.count(t, "SELECT COUNT(*) FROM jobs"); n != before {
			t.Errorf("%d jobs queued", n-before)
		}
	})
}

// TestRestoreRecoversProductionAfterFailedPush is the case this exists for: a
// push fails, rolling it back fails, and restoring the pre-push backup the
// status message names brings production back.
func TestRestoreRecoversProductionAfterFailedPush(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newPushEnv(t, db)
		e.setFailWP(func(siteID string, args []string) error {
			if siteID == e.prod.ID && slices.Equal(args, []string{"core", "update-db"}) {
				return &provisioner.ExecError{ExitCode: 255, Stderr: "PHP Fatal error"}
			}
			return nil
		})
		var restores int
		e.engine.restoreErr = func(int, backup.SnapshotID) error {
			if restores++; restores > 1 {
				return errors.New("repository unreachable")
			}
			return nil
		}
		_, d := e.push(t, sites.StagingPush{Files: backup.RestoreFilesAll, Database: true})
		if d.Job.State != store.JobFailed {
			t.Fatalf("push job: %s %s\n%s", d.Job.State, d.Job.Error, jobLog(d))
		}
		e.setFailWP(nil)
		e.engine.restoreErr = nil

		prod := e.site(t, e.prod.ID)
		pre := e.preRestoreBackups(t, e.prod.ID)
		undo := pre[0]
		if prod.Status != store.SiteFailed || !prod.Maintenance ||
			!strings.Contains(prod.StatusMessage, "Restore the pre-push backup to try again") || !strings.Contains(prod.StatusMessage, undo.ID) {
			t.Fatalf("production after the failed push: %s, maintenance %v, %q", prod.Status, prod.Maintenance, prod.StatusMessage)
		}
		requireNoMaintenanceClaim(t, prod)
		// Its data is the staging site's, half pushed; a second push is refused.
		if _, err := e.svc.PushStaging(ctx, e.stg.ID, sites.StagingPush{Files: backup.RestoreFilesAll, Database: true}); !errors.Is(err, sites.ErrNotRunning) {
			t.Errorf("push to the failed production site = %v", err)
		}

		rd := e.restore(t, e.prod.ID, sites.Restore{SnapshotID: undo.SnapshotID})
		if rd.Job.State != store.JobSucceeded {
			t.Fatalf("restore job: %s %s\n%s", rd.Job.State, rd.Job.Error, jobLog(rd))
		}
		if got := e.data(e.prod.ID); got != e.prodData {
			t.Errorf("production's database = %q, want %q", got, e.prodData)
		}
		prod = e.site(t, e.prod.ID)
		if prod.Status != store.SiteRunning || !prod.Maintenance || !strings.Contains(prod.StatusMessage, "recovered the site") {
			t.Errorf("production after the restore: %s, maintenance %v, %q", prod.Status, prod.Maintenance, prod.StatusMessage)
		}
		requireNoMaintenanceClaim(t, prod)
		if _, _, err := e.svc.SetMaintenance(ctx, e.prod.ID, false, ""); err != nil {
			t.Fatal(err)
		}
		e.runJobs(t)
		if rt, _ := e.prov.Site(e.prod.ID); rt.Maintenance {
			t.Error("production is still on the maintenance page")
		}
		// And it is an ordinary site again: the push can be tried again.
		if _, d := e.push(t, sites.StagingPush{Files: backup.RestoreFilesAll, Database: true}); d.Job.State != store.JobSucceeded {
			t.Errorf("push after recovering: %s %s\n%s", d.Job.State, d.Job.Error, jobLog(d))
		}
	})
}

// TestRestoreFailedStagingSite: the job forgets how old a staging site's data
// is, as for a running one, and a failed staging site can be restored from its
// production site's snapshot.
func TestRestoreFailedStagingSite(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newRefreshEnv(t, db)
		giveAsOf(t, db, e.stg.ID, stagingAsOf{wasDB, wasUploads})
		snap := e.newestSnapshot(t, e.prod.ID)
		if _, err := store.NewSites(db).SetStatus(context.Background(), e.stg.ID, store.SiteFailed, "refresh failed"); err != nil {
			t.Fatal(err)
		}
		d := e.restore(t, e.stg.ID, sites.Restore{SnapshotID: string(snap.ID), SourceSiteID: e.prod.ID})
		if d.Job.State != store.JobSucceeded {
			t.Fatalf("restore job: %s %s\n%s", d.Job.State, d.Job.Error, jobLog(d))
		}
		got := e.site(t, e.stg.ID)
		if got.Status != store.SiteRunning {
			t.Errorf("staging site: %s %q", got.Status, got.StatusMessage)
		}
		requireAsOf(t, "after restoring the failed staging site", asOfOf(got), stagingAsOf{})
		if data := e.data(e.stg.ID); strings.Contains(data, e.prod.PreviewDomain) || !strings.Contains(data, e.stg.PreviewDomain) {
			t.Errorf("the staging site's database still names production: %q", data)
		}
	})
}

// TestRestoreFailedSiteChecks: the checks of the request hold for a failed
// target too, and a refused request leaves the site and its message alone.
func TestRestoreFailedSiteChecks(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newRestoreEnv(t, db)
		site, _ := e.failedSite(t, "Acme")
		other := e.runningSite(t, "Acme Shop", "post=good")
		otherSnap := e.snapshot(t, other.ID)
		newer := e.snapshot(t, site.ID)
		e.engine.cores[newer.ID] = "8.0"

		for _, tt := range []struct {
			name  string
			req   sites.Restore
			field string
		}{
			{"unknown snapshot", sites.Restore{SnapshotID: "snap-999"}, "/snapshot_id"},
			{"another site's snapshot", sites.Restore{SnapshotID: string(otherSnap.ID)}, "/snapshot_id"},
			{"core too new", sites.Restore{SnapshotID: string(newer.ID)}, "/snapshot_id"},
		} {
			_, _, err := e.svc.Restore(ctx, site.ID, tt.req)
			var ve *sites.ValidationError
			if !errors.As(err, &ve) || len(ve.Fields) != 1 || ve.Fields[0].Field != tt.field {
				t.Errorf("%s: Restore = %v, want a validation error on %s", tt.name, err, tt.field)
			}
		}
		if got := e.site(t, site.ID); got.Status != store.SiteFailed || got.StatusMessage != site.StatusMessage {
			t.Errorf("site after refused restores: %s %q", got.Status, got.StatusMessage)
		}

		// A snapshot of another site of the group works as for a running site.
		d := e.restore(t, site.ID, sites.Restore{SnapshotID: string(otherSnap.ID), SourceSiteID: other.ID})
		e.requireRecovered(t, site.ID, d, true)
		data := e.data(site.ID)
		if strings.Contains(data, other.PreviewDomain) || !strings.Contains(data, "home=https://"+site.PreviewDomain) {
			t.Errorf("database after restoring another site's snapshot = %q", data)
		}
	})
}

// TestRestoreQueuedBeforeFailedSites: a job queued by an earlier release has a
// payload with no mark on it, and is a running site's: it doesn't render the
// site, and a failed backup stops it.
func TestRestoreQueuedBeforeFailedSites(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newRestoreEnv(t, db)
		site := e.runningSite(t, "Acme", "post=good")
		snap := e.snapshot(t, site.ID)
		e.setStatus(t, site.ID, store.SiteUpdating)
		job, err := jobs.Enqueue(ctx, db, jobs.Spec{
			Type: sites.RestoreJobType, TargetType: store.JobTargetSite, TargetID: site.ID, LockKey: store.SiteLockKey(site.ID),
			Payload: map[string]string{"source_site_id": site.ID, "snapshot_id": string(snap.ID)},
		})
		if err != nil {
			t.Fatal(err)
		}
		before := len(provOps(e, site.ID))
		e.runJobs(t)
		d := e.job(t, job.ID)
		e.requireRecovered(t, site.ID, d, false)
		if slices.Contains(provOps(e, site.ID)[before:], "CreateSite") {
			t.Error("the job rendered the site's stack")
		}

		e.setStatus(t, site.ID, store.SiteUpdating)
		e.engine.Err = func(op, _ string) error {
			if op == "Backup" {
				return sitedb.ErrDatabaseNotFound
			}
			return nil
		}
		job, err = jobs.Enqueue(ctx, db, jobs.Spec{
			Type: sites.RestoreJobType, TargetType: store.JobTargetSite, TargetID: site.ID, LockKey: store.SiteLockKey(site.ID),
			Payload: map[string]string{"source_site_id": site.ID, "snapshot_id": string(snap.ID)},
		})
		if err != nil {
			t.Fatal(err)
		}
		e.runJobs(t)
		if d := e.job(t, job.ID); d.Job.State != store.JobFailed {
			t.Errorf("job with a failed backup: %s", d.Job.State)
		}
	})
}

// TestRecoveryOfSiteBeingRestoredFromFailed: going from failed to updating
// forgets any earlier resume status, so a site stopped long before it failed
// comes back running from a new host, as the operator was asking for.
func TestRecoveryOfSiteBeingRestoredFromFailed(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newRestoreEnv(t, db)
		site := e.runningSite(t, "Acme", "post=good")
		snap := e.snapshot(t, site.ID)
		sitesStore := store.NewSites(db)
		// Stopped, updating (which records stopped), then failed.
		if _, err := e.svc.Stop(ctx, site.ID, ""); err != nil {
			t.Fatal(err)
		}
		e.runJobs(t)
		if _, err := sitesStore.SetStatus(ctx, site.ID, store.SiteUpdating, ""); err != nil {
			t.Fatal(err)
		}
		if resume, err := sitesStore.ResumeStatus(ctx, site.ID); err != nil || resume != store.SiteStopped {
			t.Fatalf("resume status = %q, %v", resume, err)
		}
		if _, err := sitesStore.SetStatus(ctx, site.ID, store.SiteFailed, "update failed"); err != nil {
			t.Fatal(err)
		}

		if _, _, err := e.svc.Restore(ctx, site.ID, sites.Restore{SnapshotID: string(snap.ID)}); err != nil {
			t.Fatal(err)
		}
		if resume, err := sitesStore.ResumeStatus(ctx, site.ID); err != nil || resume != "" {
			t.Fatalf("resume status while restoring a failed site = %q, %v", resume, err)
		}

		// The state was backed up here, and the host was lost.
		var rec sites.Recovery
		err := db.InTx(ctx, func(q store.Querier) error {
			var err error
			rec, err = sites.PrepareRecovery(ctx, q, testNow)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Contains(rec.Recovering, site.ID) {
			t.Fatalf("recovery = %+v", rec)
		}
		e.runJobs(t)
		if d := e.recoverJob(t, site.ID); d.Job.State != store.JobSucceeded {
			t.Fatalf("recover job: %s %s\n%s", d.Job.State, d.Job.Error, jobLog(d))
		}
		if got := e.site(t, site.ID); got.Status != store.SiteRunning {
			t.Errorf("site after the recovery: %s %q", got.Status, got.StatusMessage)
		}
	})
}
