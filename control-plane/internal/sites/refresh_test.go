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
	"github.com/rchouinard/pressfleet/control-plane/internal/backup/sitebackup"
	"github.com/rchouinard/pressfleet/control-plane/internal/id"
	"github.com/rchouinard/pressfleet/control-plane/internal/jobs"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
	"github.com/rchouinard/pressfleet/control-plane/internal/sites"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
)

// refreshEnv has a production site and a staging site cloned from it, whose
// contents have since gone separate ways.
type refreshEnv struct {
	*restoreEnv
	prod, stg store.Site
	// stgData is the staging site's database before the refresh.
	stgData string
}

func newRefreshEnv(t *testing.T, db *store.DB) *refreshEnv {
	t.Helper()
	e := &refreshEnv{restoreEnv: newCloneEnv(t, db)}
	e.prod = e.productionSite(t, "Acme")
	out := e.createStaging(t, e.prod.ID, sites.NewStaging{})
	e.runJobs(t)
	if d := e.job(t, out.Job.ID); d.Job.State != store.JobSucceeded {
		t.Fatalf("clone job: %s %s\n%s", d.Job.State, d.Job.Error, jobLog(d))
	}
	e.stg = e.site(t, out.Site.ID)
	e.stgData = strings.Replace(e.data(e.stg.ID), "post=hello", "post=staging-edit", 1)
	e.dbs.SetData(e.stg.ID, []byte(e.stgData))
	e.dbs.SetData(e.prod.ID, []byte(strings.Replace(e.data(e.prod.ID), "post=hello", "post=prod-new", 1)))
	return e
}

// refresh queues a refresh and runs it, returning what was queued and the
// job's detail.
func (e *refreshEnv) refresh(t *testing.T, req sites.StagingRefresh) (sites.StagingRefreshQueued, store.JobDetail) {
	t.Helper()
	out, err := e.svc.RefreshStaging(context.Background(), e.stg.ID, req)
	if err != nil {
		t.Fatalf("RefreshStaging: %v", err)
	}
	e.runJobs(t)
	return out, e.job(t, out.Job.ID)
}

// rewritten is the production site's database as the staging site should have
// it: with the staging site's URLs.
func (e *refreshEnv) rewritten() string {
	from, to := "https://"+e.prod.PreviewDomain, "https://"+e.stg.PreviewDomain
	return strings.ReplaceAll(strings.ReplaceAll(e.data(e.prod.ID), from, to),
		strings.ReplaceAll(from, "/", `\/`), strings.ReplaceAll(to, "/", `\/`))
}

// provOps counts the provisioner's calls on the site from index from on, by
// operation.
func (e *refreshEnv) provOps(siteID string, from int) map[string]int {
	out := map[string]int{}
	for _, c := range e.prov.Calls()[from:] {
		if c.SiteID == siteID {
			out[c.Op]++
		}
	}
	return out
}

// requireSettingsKept checks what a refresh must never change, whatever the
// copied data says.
func (e *refreshEnv) requireSettingsKept(t *testing.T) {
	t.Helper()
	got := e.site(t, e.stg.ID)
	if got.ParentSiteID != e.prod.ID || got.PreviewDomain != e.stg.PreviewDomain || got.PrimaryDomain != "" ||
		got.MailDisabled != e.stg.MailDisabled || got.PreviewProtection != e.stg.PreviewProtection ||
		got.PreviewUsername != e.stg.PreviewUsername || got.PreviewPasswordHash != e.stg.PreviewPasswordHash ||
		got.PreviewPasswordSecretRef != e.stg.PreviewPasswordSecretRef || got.WPSaltsSecretRef != e.stg.WPSaltsSecretRef ||
		got.UID != e.stg.UID {
		t.Errorf("staging site's settings changed: %+v", got)
	}
	rt, ok := e.prov.Site(e.stg.ID)
	if !ok || rt.Config.Environment != provisioner.EnvironmentStaging || rt.Config.DisableMail != e.stg.MailDisabled ||
		rt.Config.PreviewAuth == nil || rt.Config.UID != e.stg.UID {
		t.Errorf("rendered staging site = %+v", rt.Config)
	}
	if !e.cache.HasUser(e.stg.ID) {
		t.Error("the staging site lost its object cache user")
	}
}

func TestRefreshStagingScopes(t *testing.T) {
	for _, tc := range []struct {
		name     string
		files    backup.RestoreFiles
		database bool
		restart  bool
	}{
		{"everything", backup.RestoreFilesAll, true, true},
		{"code and database", backup.RestoreFilesCode, true, true},
		{"database only", backup.RestoreFilesNone, true, false},
		{"all files only", backup.RestoreFilesAll, false, true},
		{"code only", backup.RestoreFilesCode, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			storetest.Run(t, func(t *testing.T, db *store.DB) {
				e := newRefreshEnv(t, db)
				prodData := e.data(e.prod.ID)
				engineFrom, provFrom := len(e.engine.Calls()), len(e.prov.Calls())
				e.wpCalls(e.stg.ID, "")
				flushes := e.cache.Flushes(e.stg.ID)

				out, d := e.refresh(t, sites.StagingRefresh{Files: tc.files, Database: tc.database})
				if out.Site.Status != store.SiteUpdating {
					t.Errorf("queued site status = %s", out.Site.Status)
				}
				if d.Job.State != store.JobSucceeded {
					t.Fatalf("refresh job: %s %s\n%s", d.Job.State, d.Job.Error, jobLog(d))
				}

				// The chain: a sync backup of production under its lock key,
				// the refresh under the staging site's, after it.
				if out.BackupJob == nil || out.BackupJob.Type != sitebackup.JobType || out.BackupJob.TargetID != e.prod.ID ||
					out.BackupJob.LockKey != store.SiteLockKey(e.prod.ID) || !strings.Contains(string(out.BackupJob.Payload), `"sync"`) {
					t.Fatalf("backup job = %+v", out.BackupJob)
				}
				if out.Job.Type != sites.RefreshJobType || out.Job.TargetID != e.stg.ID ||
					out.Job.LockKey != store.SiteLockKey(e.stg.ID) || out.Job.AfterJobID != out.BackupJob.ID {
					t.Errorf("refresh job = %+v", out.Job)
				}

				// The engine: production's backup, the undo point, and the
				// snapshot restored with exactly the scope.
				page, err := store.NewBackupRecords(db).List(context.Background(), store.BackupRecordFilter{SiteID: e.prod.ID})
				if err != nil {
					t.Fatal(err)
				}
				sync := page.Records[0]
				if sync.Kind != store.BackupSync || sync.Status != store.BackupSucceeded {
					t.Fatalf("production's newest backup = %+v", sync)
				}
				wantOps := []string{
					"Backup " + e.prod.ID + " sync",
					"Backup " + e.stg.ID + " pre_restore",
					"Restore " + e.prod.ID + " " + sync.SnapshotID + " " + e.stg.ID,
				}
				if got := e.engineOps(engineFrom); !slices.Equal(got, wantOps) {
					t.Errorf("engine calls = %q, want %q", got, wantOps)
				}
				want := backup.RestoreOptions{Files: tc.files, Database: tc.database}
				for _, c := range e.engine.Calls()[engineFrom:] {
					if c.Op == "Restore" && c.Restore != want {
						t.Errorf("restore scope = %+v, want %+v", c.Restore, want)
					}
				}
				if pre := e.preRestoreBackups(t, e.stg.ID); len(pre) != 1 || pre[0].Status != store.BackupSucceeded || pre[0].JobID != d.Job.ID {
					t.Errorf("pre-refresh backups = %+v", pre)
				}

				// The database steps ran if and only if the database came over.
				calls := e.wpCalls(e.stg.ID, "")
				has := func(prefix string) bool {
					return slices.ContainsFunc(calls, func(c string) bool { return strings.HasPrefix(c, prefix) })
				}
				if tc.database {
					if got := e.data(e.stg.ID); got != e.rewritten() || !strings.Contains(got, "post=prod-new") || strings.Contains(got, "//"+e.prod.PreviewDomain) {
						t.Errorf("staging database = %q, want %q", got, e.rewritten())
					}
					for _, prefix := range []string{"core update-db", "search-replace //" + e.prod.PreviewDomain + " //" + e.stg.PreviewDomain, "plugin list"} {
						if !has(prefix) {
							t.Errorf("no %q in WP-CLI calls %q", prefix, calls)
						}
					}
				} else {
					if got := e.data(e.stg.ID); got != e.stgData {
						t.Errorf("staging database changed: %q, want %q", got, e.stgData)
					}
					if len(calls) != 0 {
						t.Errorf("WP-CLI ran on a refresh that keeps the database: %q", calls)
					}
				}
				if e.data(e.prod.ID) != prodData {
					t.Errorf("production's database changed: %q", e.data(e.prod.ID))
				}

				// Restart for files; maintenance and purges for every scope.
				ops := e.provOps(e.stg.ID, provFrom)
				if got := ops["StopSite"] == 1 && ops["StartSite"] == 1; got != tc.restart {
					t.Errorf("restarted = %v, want %v (calls %v)", got, tc.restart, ops)
				}
				if ops["StopSite"] != ops["StartSite"] {
					t.Errorf("unbalanced restart: %v", ops)
				}
				if ops["SetMaintenance"] != 2 {
					t.Errorf("SetMaintenance called %d times, want on and off", ops["SetMaintenance"])
				}
				if ops["PurgeCache"] != 1 {
					t.Errorf("PurgeCache called %d times", ops["PurgeCache"])
				}
				if e.cache.Flushes(e.stg.ID) <= flushes {
					t.Error("the object cache wasn't flushed")
				}

				// The steps don't depend on the scope.
				if got := stepNames(d); !slices.Equal(got, refreshSteps) {
					t.Errorf("steps = %q, want %q", got, refreshSteps)
				}

				final := e.site(t, e.stg.ID)
				if final.Status != store.SiteRunning || final.StatusMessage != "" || final.Maintenance ||
					final.StagingSnapshotID != sync.SnapshotID || final.StagingRefreshedAt.Before(e.stg.StagingRefreshedAt) {
					t.Errorf("staging site after the refresh = %+v", final)
				}
				e.requireSettingsKept(t)
				if p := e.site(t, e.prod.ID); p.Status != store.SiteRunning || p.Maintenance {
					t.Errorf("production site changed: %+v", p)
				}

				var res map[string]string
				if err := json.Unmarshal(d.Job.Result, &res); err != nil {
					t.Fatal(err)
				}
				if res["site_id"] != e.stg.ID || res["source_site_id"] != e.prod.ID || res["snapshot_id"] != sync.SnapshotID ||
					res["backup_id"] != sync.ID || res["pre_restore_backup_id"] == "" {
					t.Errorf("result = %v", res)
				}
			})
		})
	}
}

// refreshSteps are the site.refresh job's steps. Keep them stable: a queued
// job resumes by step name.
var refreshSteps = []string{
	"prepare", sitebackup.StepRecord, sitebackup.StepSnapshot, "enable_maintenance", "update_core", "restore",
	"restart_site", "upgrade_database", "rewrite_urls", "configure_plugins", "purge_object_cache", "purge_page_cache",
	"disable_maintenance", "finish",
}

func stepNames(d store.JobDetail) []string {
	var out []string
	for _, st := range d.Steps {
		out = append(out, st.Name)
	}
	return out
}

// TestRefreshStagingDatabaseKeepsCode is the issue's first acceptance
// criterion: production's content, the staging site's plugins and themes, and
// the default stack working despite what production's database says about it.
func TestRefreshStagingDatabaseKeepsCode(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newRefreshEnv(t, db)
		// The staging site has a plugin production doesn't, which only the
		// refreshed database's active list could affect; the dump arrives with
		// the object cache plugin off and no drop-in in place.
		e.wp.edit(e.stg.ID, func(s *wpSite) {
			s.plugins["being-tested"] = "active"
			s.plugins[sites.PluginRedisCache] = "inactive"
			s.dropin = ""
		})
		from := len(e.engine.Calls())

		_, d := e.refresh(t, sites.StagingRefresh{Files: backup.RestoreFilesNone, Database: true})
		if d.Job.State != store.JobSucceeded {
			t.Fatalf("refresh job: %s %s\n%s", d.Job.State, d.Job.Error, jobLog(d))
		}
		for _, c := range e.engine.Calls()[from:] {
			if c.Op == "Restore" && c.Restore.Files != backup.RestoreFilesNone {
				t.Errorf("a database-only refresh restored files: %+v", c.Restore)
			}
		}
		if got := e.data(e.stg.ID); !strings.Contains(got, "post=prod-new") || !strings.Contains(got, "home=https://"+e.stg.PreviewDomain) {
			t.Errorf("staging database = %q", got)
		}
		st := e.wp.state(e.stg.ID)
		if st.plugins["being-tested"] != "active" {
			t.Errorf("the staging site's own plugin = %q", st.plugins["being-tested"])
		}
		if st.plugins[sites.PluginRedisCache] != "active" || st.dropin != redisDropIn || st.plugins[sites.PluginNginxHelper] != "active" {
			t.Errorf("default stack after the refresh = %v, drop-in %q", st.plugins, st.dropin)
		}
	})
}

func TestRefreshStagingFromExistingSnapshot(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newRefreshEnv(t, db)
		snap := e.snapshot(t, e.prod.ID)
		// Production moves on; the refresh gets the snapshot's content.
		e.dbs.SetData(e.prod.ID, []byte(strings.Replace(e.data(e.prod.ID), "post=prod-new", "post=later", 1)))
		from := len(e.engine.Calls())

		out, d := e.refresh(t, sites.StagingRefresh{Files: backup.RestoreFilesNone, Database: true, SnapshotID: string(snap.ID)})
		if d.Job.State != store.JobSucceeded {
			t.Fatalf("refresh job: %s %s\n%s", d.Job.State, d.Job.Error, jobLog(d))
		}
		if out.BackupJob != nil || out.Job.AfterJobID != "" || out.SnapshotID != string(snap.ID) {
			t.Errorf("a snapshot was given, but queued = %+v", out)
		}
		want := []string{"Backup " + e.stg.ID + " pre_restore", "Restore " + e.prod.ID + " " + string(snap.ID) + " " + e.stg.ID}
		if got := e.engineOps(from); !slices.Equal(got, want) {
			t.Errorf("engine calls = %q, want %q", got, want)
		}
		if got := e.data(e.stg.ID); !strings.Contains(got, "post=prod-new") || strings.Contains(got, "post=later") {
			t.Errorf("staging database = %q", got)
		}
		final := e.site(t, e.stg.ID)
		if final.StagingSnapshotID != string(snap.ID) || final.Status != store.SiteRunning {
			t.Errorf("staging site = %+v", final)
		}
		res := jobResult(t, d.Job)
		if _, ok := res["backup_id"]; ok || res["snapshot_id"] != string(snap.ID) {
			t.Errorf("result = %v", res)
		}
		e.requireSettingsKept(t)

		// A failed production site can't be backed up, but its snapshots can
		// still be copied.
		e.setStatus(t, e.prod.ID, store.SiteFailed)
		if _, err := e.svc.RefreshStaging(context.Background(), e.stg.ID, sites.StagingRefresh{
			Files: backup.RestoreFilesCode, SnapshotID: string(snap.ID),
		}); err != nil {
			t.Errorf("refreshing from a snapshot of a failed production site: %v", err)
		}
	})
}

func TestRefreshStagingCore(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name     string
		files    backup.RestoreFiles
		database bool
		wantCore string
	}{
		{"the database moves the core up", backup.RestoreFilesNone, true, "7.1.2"},
		{"files don't", backup.RestoreFilesAll, false, "6.9"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			storetest.Run(t, func(t *testing.T, db *store.DB) {
				e := newRefreshEnv(t, db)
				snap := e.snapshot(t, e.prod.ID)
				e.engine.cores[snap.ID] = "7.1"
				if _, err := store.NewSites(db).SetWPCoreVersion(ctx, e.stg.ID, "6.9"); err != nil {
					t.Fatal(err)
				}
				_, d := e.refresh(t, sites.StagingRefresh{Files: tc.files, Database: tc.database, SnapshotID: string(snap.ID)})
				if d.Job.State != store.JobSucceeded {
					t.Fatalf("refresh job: %s %s\n%s", d.Job.State, d.Job.Error, jobLog(d))
				}
				if got := e.site(t, e.stg.ID).WPCoreVersion; got != tc.wantCore {
					t.Errorf("core = %s, want %s", got, tc.wantCore)
				}
				// Moving the core re-renders the site from its own record.
				e.requireSettingsKept(t)
			})
		})
	}
}

// TestRefreshStagingFreshBackupTooNew: a fresh backup can turn out to need a
// core no image has, which the job finds out before it touches anything.
func TestRefreshStagingFreshBackupTooNew(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newRefreshEnv(t, db)
		e.engine.backupCore = map[string]string{e.prod.ID: "99.0"}
		from := len(e.engine.Calls())
		_, d := e.refresh(t, sites.StagingRefresh{Files: backup.RestoreFilesNone, Database: true})
		if d.Job.State != store.JobFailed {
			t.Fatalf("refresh job: %s %s\n%s", d.Job.State, d.Job.Error, jobLog(d))
		}
		if want := []string{"Backup " + e.prod.ID + " sync"}; !slices.Equal(e.engineOps(from), want) {
			t.Errorf("engine calls = %q, want %q", e.engineOps(from), want)
		}
		if got := e.site(t, e.stg.ID); got.Status != store.SiteRunning || got.Maintenance || e.data(e.stg.ID) != e.stgData {
			t.Errorf("staging site = %s, maintenance %v, database %q", got.Status, got.Maintenance, e.data(e.stg.ID))
		}
	})
}

// TestRefreshStagingFailedBackup: the awaited backup failed, so the job fails
// in its first step, and the staging site ends up as it was.
func TestRefreshStagingFailedBackup(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		for _, maintenance := range []bool{false, true} {
			e := newRefreshEnv(t, db)
			e.engine.Err = func(op, siteID string) error {
				if op == "Backup" && siteID == e.prod.ID {
					return backup.ErrSiteNotFound // permanent
				}
				return nil
			}
			if maintenance {
				if _, err := store.NewSites(db).SetMaintenance(ctx, e.stg.ID, true); err != nil {
					t.Fatal(err)
				}
			}
			before := e.site(t, e.stg.ID)
			engineFrom, provFrom := len(e.engine.Calls()), len(e.prov.Calls())
			e.wpCalls(e.stg.ID, "")
			flushes := e.cache.Flushes(e.stg.ID)

			out, d := e.refresh(t, sites.StagingRefresh{Files: backup.RestoreFilesAll, Database: true})
			if b := e.job(t, out.BackupJob.ID); b.Job.State != store.JobFailed {
				t.Fatalf("backup job: %s", b.Job.State)
			}
			if d.Job.State != store.JobFailed || !strings.Contains(d.Job.Error, jobs.ErrAwaitedJobFailed.Error()) {
				t.Fatalf("refresh job: %s %q\n%s", d.Job.State, d.Job.Error, jobLog(d))
			}
			// Only the first step ran.
			for _, st := range d.Steps[1:] {
				if st.State != store.StepPending {
					t.Errorf("step %s is %s", st.Name, st.State)
				}
			}

			got := e.site(t, e.stg.ID)
			if got.Status != store.SiteRunning || !strings.Contains(got.StatusMessage, "refresh failed before changing the site") {
				t.Errorf("staging site = %s %q", got.Status, got.StatusMessage)
			}
			if got.Maintenance != maintenance || got.WPCoreVersion != before.WPCoreVersion ||
				got.StagingSnapshotID != before.StagingSnapshotID || !got.StagingRefreshedAt.Equal(before.StagingRefreshedAt) {
				t.Errorf("staging site changed: %+v, was %+v", got, before)
			}
			if e.data(e.stg.ID) != e.stgData {
				t.Errorf("staging database changed: %q", e.data(e.stg.ID))
			}
			if slices.ContainsFunc(e.engine.Calls()[engineFrom:], func(c backuptest.Call) bool {
				return c.Op == "Restore" || c.SiteID == e.stg.ID
			}) {
				t.Errorf("engine calls = %q", e.engineOps(engineFrom))
			}
			if pre := e.preRestoreBackups(t, e.stg.ID); len(pre) != 0 {
				t.Errorf("pre-refresh backups = %+v", pre)
			}
			for op, n := range e.provOps(e.stg.ID, provFrom) {
				if op != "GetSite" {
					t.Errorf("provisioner %s called %d times on the staging site", op, n)
				}
			}
			if calls := e.wpCalls(e.stg.ID, ""); len(calls) != 0 {
				t.Errorf("WP-CLI calls = %q", calls)
			}
			if e.cache.Flushes(e.stg.ID) != flushes {
				t.Error("the object cache was flushed")
			}

			// It can be tried again once production's backups work.
			e.engine.Err = nil
			if _, d := e.refresh(t, sites.StagingRefresh{Files: backup.RestoreFilesNone, Database: true}); d.Job.State != store.JobSucceeded {
				t.Errorf("second refresh: %s %s", d.Job.State, d.Job.Error)
			}
		}
	})
}

// TestRefreshStagingRollsBack: the issue's second acceptance criterion. The
// pre-refresh backup comes back in full, whatever the scope.
func TestRefreshStagingRollsBack(t *testing.T) {
	transient := errors.New("connection reset by peer")
	fatal := &provisioner.ExecError{ExitCode: 255, Stderr: "PHP Fatal error: boom"}
	for _, tc := range []struct {
		name     string
		files    backup.RestoreFiles
		database bool
		inject   func(e *refreshEnv)
	}{
		{"database upgrade", backup.RestoreFilesNone, true, func(e *refreshEnv) {
			e.setFailWP(func(_ string, args []string) error {
				if slices.Equal(args, []string{"core", "update-db"}) {
					return fatal
				}
				return nil
			})
		}},
		{"URL rewrite", backup.RestoreFilesAll, true, func(e *refreshEnv) {
			e.setFailWP(func(_ string, args []string) error {
				if args[0] == "search-replace" {
					return fatal
				}
				return nil
			})
		}},
		{"plugin stack", backup.RestoreFilesCode, true, func(e *refreshEnv) {
			e.setFailWP(func(_ string, args []string) error {
				if args[0] == "plugin" && args[1] == "list" {
					return transient
				}
				return nil
			})
		}},
		{"the restore itself", backup.RestoreFilesCode, false, func(e *refreshEnv) {
			e.engine.restoreErr = func(_ int, snap backup.SnapshotID) error {
				if !strings.Contains(e.preSnapshot(), string(snap)) {
					return transient
				}
				return nil
			}
		}},
		{"the restart, on a files-only refresh", backup.RestoreFilesAll, false, func(e *refreshEnv) {
			e.prov.Err = func(op, siteID string) error {
				if op == "StopSite" && siteID == e.stg.ID && e.preSnapshot() != "" && !e.rolledBack() {
					return transient
				}
				return nil
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			storetest.Run(t, func(t *testing.T, db *store.DB) {
				e := newRefreshEnv(t, db)
				before := e.site(t, e.stg.ID)
				from := len(e.engine.Calls())
				tc.inject(e)

				_, d := e.refresh(t, sites.StagingRefresh{Files: tc.files, Database: tc.database})
				if d.Job.State != store.JobFailed {
					t.Fatalf("refresh job: %s %s\n%s", d.Job.State, d.Job.Error, jobLog(d))
				}
				pre := e.preRestoreBackups(t, e.stg.ID)
				if len(pre) != 1 || pre[0].Status != store.BackupSucceeded {
					t.Fatalf("pre-refresh backups = %+v", pre)
				}
				// The last restore is the pre-refresh snapshot, in full.
				var restores []backuptest.Call
				for _, c := range e.engine.Calls()[from:] {
					if c.Op == "Restore" {
						restores = append(restores, c)
					}
				}
				if len(restores) < 2 {
					t.Fatalf("restores = %+v", restores)
				}
				if want := (backup.RestoreOptions{Files: tc.files, Database: tc.database}); restores[0].Restore != want {
					t.Errorf("first restore's scope = %+v, want %+v", restores[0].Restore, want)
				}
				last := restores[len(restores)-1]
				if last.SiteID != e.stg.ID || last.TargetSiteID != e.stg.ID || string(last.Snapshot) != pre[0].SnapshotID || last.Restore != backup.FullRestore() {
					t.Errorf("rollback restore = %+v, want the pre-refresh snapshot %s in full", last, pre[0].SnapshotID)
				}
				if got := e.data(e.stg.ID); got != e.stgData {
					t.Errorf("staging database after the rollback = %q, want %q", got, e.stgData)
				}
				got := e.site(t, e.stg.ID)
				rt, _ := e.prov.Site(e.stg.ID)
				if got.Status != store.SiteRunning || !strings.Contains(got.StatusMessage, "refresh failed and the site was rolled back to its pre-refresh backup") ||
					!strings.Contains(got.StatusMessage, pre[0].ID) {
					t.Errorf("staging site after the rollback: %s %q", got.Status, got.StatusMessage)
				}
				if !got.Maintenance || !rt.Maintenance {
					t.Errorf("maintenance after the rollback: %v/%v, want on (§5.4 step 7)", got.Maintenance, rt.Maintenance)
				}
				// A failed refresh records nothing.
				if got.StagingSnapshotID != before.StagingSnapshotID || !got.StagingRefreshedAt.Equal(before.StagingRefreshedAt) {
					t.Errorf("refresh recorded: %s at %s, was %s at %s", got.StagingSnapshotID, got.StagingRefreshedAt,
						before.StagingSnapshotID, before.StagingRefreshedAt)
				}
				e.requireSettingsKept(t)
			})
		})
	}
}

// preSnapshot returns the snapshot ID of the staging site's pre-refresh backup,
// once it has one.
func (e *refreshEnv) preSnapshot() string {
	page, err := store.NewBackupRecords(e.db).List(context.Background(), store.BackupRecordFilter{SiteID: e.stg.ID})
	if err != nil {
		return ""
	}
	for _, r := range page.Records {
		if r.Kind == store.BackupPreRestore && r.SnapshotID != "" {
			return r.SnapshotID
		}
	}
	return ""
}

// rolledBack reports whether the pre-refresh snapshot was restored already.
func (e *refreshEnv) rolledBack() bool {
	pre := e.preSnapshot()
	return pre != "" && slices.ContainsFunc(e.engine.Calls(), func(c backuptest.Call) bool {
		return c.Op == "Restore" && string(c.Snapshot) == pre
	})
}

func TestRefreshStagingRefused(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newRefreshEnv(t, db)
		otherSnap := e.snapshot(t, e.productionSite(t, "Other").ID)
		newer := e.snapshot(t, e.prod.ID)
		e.engine.cores[newer.ID] = "99.0"
		jobsBefore := e.count(t, "SELECT COUNT(*) FROM jobs")
		good := sites.StagingRefresh{Files: backup.RestoreFilesAll, Database: true}

		field := func(field string) func(error) bool {
			return func(err error) bool {
				var ve *sites.ValidationError
				return errors.As(err, &ve) && slices.ContainsFunc(ve.Fields, func(f sites.FieldError) bool { return f.Field == field })
			}
		}
		is := func(target error) func(error) bool { return func(err error) bool { return errors.Is(err, target) } }
		for _, tc := range []struct {
			name   string
			siteID string
			req    sites.StagingRefresh
			setup  func() (cleanup func())
			want   func(error) bool
		}{
			{name: "a production site", siteID: e.prod.ID, req: good, want: field("/site_id")},
			{name: "a missing site", siteID: id.New(), req: good, want: is(sites.ErrNotFound)},
			{name: "a scope that copies nothing", siteID: e.stg.ID, req: sites.StagingRefresh{Files: backup.RestoreFilesNone}, want: field("/files")},
			{name: "an unknown files scope", siteID: e.stg.ID, req: sites.StagingRefresh{Files: "some", Database: true}, want: field("/files")},
			{name: "no files scope", siteID: e.stg.ID, req: sites.StagingRefresh{Database: true}, want: field("/files")},
			{
				name: "another site's snapshot", siteID: e.stg.ID,
				req:  sites.StagingRefresh{Files: backup.RestoreFilesAll, Database: true, SnapshotID: string(otherSnap.ID)},
				want: field("/snapshot_id"),
			},
			{
				name: "an unknown snapshot", siteID: e.stg.ID,
				req:  sites.StagingRefresh{Files: backup.RestoreFilesAll, SnapshotID: "snap-999"},
				want: field("/snapshot_id"),
			},
			{
				name: "a database newer than any image", siteID: e.stg.ID,
				req:  sites.StagingRefresh{Files: backup.RestoreFilesNone, Database: true, SnapshotID: string(newer.ID)},
				want: field("/snapshot_id"),
			},
			{
				name: "a staging site being updated", siteID: e.stg.ID, req: good,
				setup: func() func() {
					e.setStatus(t, e.stg.ID, store.SiteUpdating)
					return func() { e.setStatus(t, e.stg.ID, store.SiteRunning) }
				},
				want: is(sites.ErrBusy),
			},
			{
				name: "a stopped staging site", siteID: e.stg.ID, req: good,
				setup: func() func() {
					e.setStatus(t, e.stg.ID, store.SiteStopped)
					return func() { e.setStatus(t, e.stg.ID, store.SiteRunning) }
				},
				want: is(sites.ErrNotRunning),
			},
			{
				name: "a production site being updated, for a fresh backup", siteID: e.stg.ID, req: good,
				setup: func() func() {
					e.setStatus(t, e.prod.ID, store.SiteUpdating)
					return func() { e.setStatus(t, e.prod.ID, store.SiteRunning) }
				},
				want: is(sites.ErrBusy),
			},
			{
				name: "a failed production site, for a fresh backup", siteID: e.stg.ID, req: good,
				setup: func() func() {
					e.setStatus(t, e.prod.ID, store.SiteFailed)
					return func() { e.setStatus(t, e.prod.ID, store.SiteRunning) }
				},
				want: is(sites.ErrSourceFailed),
			},
		} {
			t.Run(tc.name, func(t *testing.T) {
				if tc.setup != nil {
					defer tc.setup()()
				}
				if _, err := e.svc.RefreshStaging(ctx, tc.siteID, tc.req); !tc.want(err) {
					t.Errorf("err = %v", err)
				}
			})
		}
		if n := e.count(t, "SELECT COUNT(*) FROM jobs"); n != jobsBefore {
			t.Errorf("%d jobs, want %d", n, jobsBefore)
		}
		if got := e.site(t, e.stg.ID); got.Status != store.SiteRunning {
			t.Errorf("staging site = %s", got.Status)
		}

		// Busy: a refresh is already queued, waiting for its backup.
		if _, err := e.svc.RefreshStaging(ctx, e.stg.ID, good); err != nil {
			t.Fatal(err)
		}
		if _, err := e.svc.RefreshStaging(ctx, e.stg.ID, good); !errors.Is(err, sites.ErrBusy) {
			t.Errorf("second refresh while one is queued = %v", err)
		}
	})
}

func TestRefreshStagingWithoutBackups(t *testing.T) {
	e := newEnv(t, storetest.Open(t, store.SQLite))
	if _, err := e.svc.RefreshStaging(context.Background(), id.New(), sites.StagingRefresh{}); !errors.Is(err, sites.ErrNoBackups) {
		t.Errorf("RefreshStaging without a backup engine = %v", err)
	}
	for _, h := range e.svc.Handlers() {
		if h.Type == sites.RefreshJobType {
			t.Error("the refresh job is registered without a backup engine")
		}
	}
}

// TestRestoreKeepsItsSteps guards the step names and order of site.restore,
// which queued jobs resume by, now that they are built by the shared pipeline.
// Its payload and scope stay the same too.
func TestRestoreKeepsItsSteps(t *testing.T) {
	want := []string{
		"prepare", "record_backup", "take_snapshot", "enable_maintenance", "update_core", "restore",
		"restart_site", "upgrade_database", "rewrite_urls", "purge_object_cache", "purge_page_cache",
		"disable_maintenance", "finish",
	}
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newRestoreEnv(t, db)
		var got []string
		for _, h := range e.svc.Handlers() {
			if h.Type == sites.RestoreJobType {
				for _, st := range h.Steps {
					got = append(got, st.Name)
				}
			}
		}
		if !slices.Equal(got, want) {
			t.Errorf("handler steps = %q, want %q", got, want)
		}

		site := e.runningSite(t, "Acme", "post=v1")
		snap := e.snapshot(t, site.ID)
		from := len(e.engine.Calls())
		d := e.restore(t, site.ID, sites.Restore{SnapshotID: string(snap.ID)})
		if d.Job.State != store.JobSucceeded {
			t.Fatalf("restore job: %s %s\n%s", d.Job.State, d.Job.Error, jobLog(d))
		}
		if got := stepNames(d); !slices.Equal(got, want) {
			t.Errorf("job steps = %q, want %q", got, want)
		}
		if strings.Contains(string(d.Job.Payload), "scope") {
			t.Errorf("payload = %s", d.Job.Payload)
		}
		for _, c := range e.engine.Calls()[from:] {
			if c.Op == "Restore" && c.Restore != backup.FullRestore() {
				t.Errorf("restore scope = %+v", c.Restore)
			}
		}
	})
}
