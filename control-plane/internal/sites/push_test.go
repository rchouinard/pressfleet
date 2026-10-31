package sites_test

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/backup"
	"github.com/rchouinard/pressfleet/control-plane/internal/backup/backuptest"
	"github.com/rchouinard/pressfleet/control-plane/internal/backup/sitebackup"
	"github.com/rchouinard/pressfleet/control-plane/internal/coreupdate"
	"github.com/rchouinard/pressfleet/control-plane/internal/id"
	"github.com/rchouinard/pressfleet/control-plane/internal/jobs"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
	"github.com/rchouinard/pressfleet/control-plane/internal/sites"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
)

// pushManifest has two major branches. A site can be on an older release of
// a branch than the manifest's, which is the one a rollout would move it to.
const pushManifest = `{
  "registry": "registry.example/pf",
  "php": [
    {"version": "8.4", "activeSupportEnds": "2026-12-31", "securitySupportEnds": "2028-12-31"}
  ],
  "wordpress": {
    "latest": {"version": "7.2.1", "sha1": "761b8101538f0631a0bfc4fba7bc4abeea92f81c", "published": "2026-01-01"},
    "pinned": [
      {"version": "7.1.4", "sha1": "1111111111111111111111111111111111111111", "published": "2025-06-01"}
    ]
  }
}`

// pushEnv has a production site and a staging site cloned from it, whose
// contents have since gone separate ways.
type pushEnv struct {
	*restoreEnv
	prod, stg store.Site
	// prodData is the production site's database before the push.
	prodData string
	// stgData is the staging site's.
	stgData string
}

func newPushEnv(t *testing.T, db *store.DB, opts ...func(*sites.Options)) *pushEnv {
	t.Helper()
	var backups *sitebackup.Service
	opts = append(opts, func(o *sites.Options) { backups = o.Backups })
	e := &pushEnv{restoreEnv: newRestoreEnvWith(t, db, pushManifest, opts...)}
	if err := e.pool.Register(backups.Handler()); err != nil {
		t.Fatal(err)
	}
	e.prod = e.productionSite(t, "Acme")
	mem := 384
	out := e.createStaging(t, e.prod.ID, sites.NewStaging{MemoryLimitMB: &mem})
	e.runJobs(t)
	if d := e.job(t, out.Job.ID); d.Job.State != store.JobSucceeded {
		t.Fatalf("clone job: %s %s\n%s", d.Job.State, d.Job.Error, jobLog(d))
	}
	e.stg = e.site(t, out.Site.ID)
	e.stgData = strings.Replace(e.data(e.stg.ID), "post=hello", "post=tested-on-staging", 1)
	e.dbs.SetData(e.stg.ID, []byte(e.stgData))
	e.prodData = strings.Replace(e.data(e.prod.ID), "post=hello", "post=recorded-on-production", 1)
	e.dbs.SetData(e.prod.ID, []byte(e.prodData))
	e.setSnapshotCore(e.stg.WPCoreVersion)
	return e
}

// setSnapshotCore sets the WordPress core the staging site's next snapshots
// say they were taken on, as the real engine's tags do.
func (e *pushEnv) setSnapshotCore(core string) {
	e.engine.mu.Lock()
	defer e.engine.mu.Unlock()
	if e.engine.backupCore == nil {
		e.engine.backupCore = map[string]string{}
	}
	e.engine.backupCore[e.stg.ID] = core
}

// setCores puts the production and staging sites on the given WordPress cores.
func (e *pushEnv) setCores(t *testing.T, prod, stg string) {
	t.Helper()
	sitesStore := store.NewSites(e.db)
	for siteID, core := range map[string]string{e.prod.ID: prod, e.stg.ID: stg} {
		if _, err := sitesStore.SetWPCoreVersion(context.Background(), siteID, core); err != nil {
			t.Fatal(err)
		}
	}
	e.prod.WPCoreVersion, e.stg.WPCoreVersion = prod, stg
	e.setSnapshotCore(stg)
}

// push queues a push and runs it, returning what was queued and the job's
// detail.
func (e *pushEnv) push(t *testing.T, req sites.StagingPush) (sites.StagingPushQueued, store.JobDetail) {
	t.Helper()
	out, err := e.svc.PushStaging(context.Background(), e.stg.ID, req)
	if err != nil {
		t.Fatalf("PushStaging: %v", err)
	}
	e.runJobs(t)
	return out, e.job(t, out.Job.ID)
}

// runBackupOnly runs the next job, which must be the push's sync backup, and
// leaves the push waiting to be leased.
func (e *pushEnv) runBackupOnly(t *testing.T, out sites.StagingPushQueued) {
	t.Helper()
	if ran, err := e.pool.RunNext(context.Background()); err != nil || !ran {
		t.Fatalf("RunNext = %v, %v", ran, err)
	}
	if b := e.job(t, out.BackupJob.ID); b.Job.State != store.JobSucceeded {
		t.Fatalf("backup job: %s %s", b.Job.State, b.Job.Error)
	}
	if p := e.job(t, out.Job.ID); p.Job.State != store.JobQueued {
		t.Fatalf("push job is %s, want it still queued", p.Job.State)
	}
}

// pushRewritten is the staging site's database as production should have it:
// with production's URLs.
func (e *pushEnv) pushRewritten(host string) string {
	from, to := "https://"+e.stg.PreviewDomain, "https://"+host
	return strings.ReplaceAll(strings.ReplaceAll(e.stgData, from, to),
		strings.ReplaceAll(from, "/", `\/`), strings.ReplaceAll(to, "/", `\/`))
}

func (e *pushEnv) provOps(siteID string, from int) map[string]int {
	out := map[string]int{}
	for _, c := range e.prov.Calls()[from:] {
		if c.SiteID == siteID {
			out[c.Op]++
		}
	}
	return out
}

// requireUntouched checks that nothing but the status message changed on the
// production site, for a push that failed before it did anything.
func (e *pushEnv) requireUntouched(t *testing.T, before store.Site, engineFrom, provFrom int, flushes int) {
	t.Helper()
	got := e.site(t, e.prod.ID)
	if got.Status != before.Status || !strings.Contains(got.StatusMessage, "push failed before changing the site") {
		t.Errorf("production site = %s %q", got.Status, got.StatusMessage)
	}
	if got.Maintenance != before.Maintenance || got.WPCoreVersion != before.WPCoreVersion {
		t.Errorf("production site changed: %+v, was %+v", got, before)
	}
	if e.data(e.prod.ID) != e.prodData {
		t.Errorf("production database changed: %q", e.data(e.prod.ID))
	}
	if slices.ContainsFunc(e.engine.Calls()[engineFrom:], func(c backuptest.Call) bool {
		return c.Op == "Restore" || c.SiteID == e.prod.ID
	}) {
		t.Errorf("engine calls = %q", e.engineOps(engineFrom))
	}
	if pre := e.preRestoreBackups(t, e.prod.ID); len(pre) != 0 {
		t.Errorf("pre-push backups = %+v", pre)
	}
	for op, n := range e.provOps(e.prod.ID, provFrom) {
		if op != "GetSite" {
			t.Errorf("provisioner %s called %d times on the production site", op, n)
		}
	}
	if calls := e.wpCalls(e.prod.ID, ""); len(calls) != 0 {
		t.Errorf("WP-CLI calls = %q", calls)
	}
	if e.cache.Flushes(e.prod.ID) != flushes {
		t.Error("the object cache was flushed")
	}
}

// pushSteps are the site.push job's steps. Keep them stable: a queued job
// resumes by step name.
var pushSteps = []string{
	"prepare", sitebackup.StepRecord, sitebackup.StepSnapshot, "enable_maintenance", "update_core", "restore",
	"restart_site", "upgrade_database", "rewrite_urls", "configure_plugins", "purge_object_cache", "purge_page_cache",
	"disable_maintenance", "finish",
}

func TestPushStagingScopes(t *testing.T) {
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
				ctx := context.Background()
				e := newPushEnv(t, db)
				engineFrom, provFrom := len(e.engine.Calls()), len(e.prov.Calls())
				e.wpCalls(e.prod.ID, "")
				e.wpCalls(e.stg.ID, "")
				flushes := e.cache.Flushes(e.prod.ID)

				out, d := e.push(t, sites.StagingPush{Files: tc.files, Database: tc.database})
				if out.Site.ID != e.prod.ID || out.Site.Status != store.SiteUpdating {
					t.Errorf("queued site = %s %s", out.Site.ID, out.Site.Status)
				}
				if d.Job.State != store.JobSucceeded {
					t.Fatalf("push job: %s %s\n%s", d.Job.State, d.Job.Error, jobLog(d))
				}

				// The chain: a sync backup of the staging site under its lock
				// key, the push under production's, after it.
				if out.BackupJob.Type != sitebackup.JobType || out.BackupJob.TargetID != e.stg.ID ||
					out.BackupJob.LockKey != store.SiteLockKey(e.stg.ID) || !strings.Contains(string(out.BackupJob.Payload), `"sync"`) {
					t.Fatalf("backup job = %+v", out.BackupJob)
				}
				if out.Job.Type != sites.PushJobType || out.Job.TargetID != e.prod.ID ||
					out.Job.LockKey != store.SiteLockKey(e.prod.ID) || out.Job.AfterJobID != out.BackupJob.ID {
					t.Errorf("push job = %+v", out.Job)
				}

				// The engine: the staging site's backup, the undo point, and the
				// snapshot restored with exactly the scope.
				page, err := store.NewBackupRecords(db).List(ctx, store.BackupRecordFilter{SiteID: e.stg.ID})
				if err != nil {
					t.Fatal(err)
				}
				sync := page.Records[0]
				if sync.Kind != store.BackupSync || sync.Status != store.BackupSucceeded {
					t.Fatalf("staging site's newest backup = %+v", sync)
				}
				wantOps := []string{
					"Backup " + e.stg.ID + " sync",
					"Backup " + e.prod.ID + " pre_restore",
					"Restore " + e.stg.ID + " " + sync.SnapshotID + " " + e.prod.ID,
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
				pre := e.preRestoreBackups(t, e.prod.ID)
				if len(pre) != 1 || pre[0].Status != store.BackupSucceeded || pre[0].JobID != d.Job.ID {
					t.Fatalf("pre-push backups = %+v", pre)
				}

				calls := e.wpCalls(e.prod.ID, "")
				has := func(prefix string) bool {
					return slices.ContainsFunc(calls, func(c string) bool { return strings.HasPrefix(c, prefix) })
				}
				if tc.database {
					if got := e.data(e.prod.ID); got != e.pushRewritten(e.prod.PreviewDomain) || !strings.Contains(got, "post=tested-on-staging") ||
						strings.Contains(got, e.stg.PreviewDomain) {
						t.Errorf("production database = %q, want %q", got, e.pushRewritten(e.prod.PreviewDomain))
					}
					for _, prefix := range []string{"core update-db", "search-replace //" + e.stg.PreviewDomain + " //" + e.prod.PreviewDomain, "plugin list"} {
						if !has(prefix) {
							t.Errorf("no %q in WP-CLI calls %q", prefix, calls)
						}
					}
				} else {
					// The acceptance criterion: the database stays as it is, and
					// the only WP-CLI call reads which plugins the push left.
					if got := e.data(e.prod.ID); got != e.prodData {
						t.Errorf("production database changed: %q, want %q", got, e.prodData)
					}
					if want := []string{"plugin list --format=json --fields=name,status,title --skip-update-check"}; !slices.Equal(calls, want) {
						t.Errorf("WP-CLI calls on a push that keeps the database = %q, want %q", calls, want)
					}
				}
				if got := e.data(e.stg.ID); got != e.stgData {
					t.Errorf("staging database changed: %q", got)
				}

				// Restart for files; maintenance and purges for every scope.
				ops := e.provOps(e.prod.ID, provFrom)
				if got := ops["StopSite"] == 1 && ops["StartSite"] == 1; got != tc.restart {
					t.Errorf("restarted = %v, want %v (calls %v)", got, tc.restart, ops)
				}
				if ops["SetMaintenance"] != 2 || ops["PurgeCache"] != 1 {
					t.Errorf("provisioner calls = %v", ops)
				}
				if e.cache.Flushes(e.prod.ID) <= flushes {
					t.Error("the object cache wasn't flushed")
				}
				if got := stepNames(d); !slices.Equal(got, pushSteps) {
					t.Errorf("steps = %q, want %q", got, pushSteps)
				}

				final := e.site(t, e.prod.ID)
				if final.Status != store.SiteRunning || final.StatusMessage != "" || final.Maintenance {
					t.Errorf("production site after the push = %+v", final)
				}
				stg := e.site(t, e.stg.ID)
				if !stg.StagingPushedAt.Equal(testNow) || stg.Status != store.SiteRunning {
					t.Errorf("staging site after the push = %+v", stg)
				}
				if !stg.StagingRefreshedAt.Equal(e.stg.StagingRefreshedAt) || stg.StagingSnapshotID != e.stg.StagingSnapshotID {
					t.Errorf("the push changed the staging site's refresh record: %+v", stg)
				}

				res := jobResult(t, d.Job)
				if res["site_id"] != e.prod.ID || res["source_site_id"] != e.stg.ID || res["snapshot_id"] != sync.SnapshotID ||
					res["backup_id"] != sync.ID || res["pre_push_backup_id"] != pre[0].ID {
					t.Errorf("result = %v", res)
				}
				if _, ok := res["pre_restore_backup_id"]; ok {
					t.Errorf("result has two names for the undo point: %v", res)
				}
			})
		})
	}
}

// TestPushStagingToLiveSite: the snapshot's URLs name the staging preview
// domain, and after the push nothing does.
func TestPushStagingToLiveSite(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newPushEnv(t, db)
		const domain = "www.acme.example"
		if _, err := db.ExecContext(context.Background(), "UPDATE sites SET primary_domain = ? WHERE id = ?", domain, e.prod.ID); err != nil {
			t.Fatal(err)
		}
		_, d := e.push(t, sites.StagingPush{Files: backup.RestoreFilesNone, Database: true})
		if d.Job.State != store.JobSucceeded {
			t.Fatalf("push job: %s %s\n%s", d.Job.State, d.Job.Error, jobLog(d))
		}
		got := e.data(e.prod.ID)
		if got != e.pushRewritten(domain) || strings.Contains(got, e.stg.PreviewDomain) {
			t.Errorf("production database = %q, want %q", got, e.pushRewritten(domain))
		}
		for _, want := range []string{
			"home=https://" + domain, "siteurl=https://" + domain, "link=https://" + domain + "/about", `json=https:\/\/` + domain,
		} {
			if !strings.Contains(got, want) {
				t.Errorf("production database has no %q: %q", want, got)
			}
		}
	})
}

// TestPushStagingKeepsSettings: nothing about the production site's settings
// comes from the staging site or its snapshot, including when the push moves
// the core, which re-renders the site.
func TestPushStagingKeepsSettings(t *testing.T) {
	for _, tc := range []struct {
		name     string
		files    backup.RestoreFiles
		database bool
		moves    bool
	}{
		{"a database push that moves the core", backup.RestoreFilesAll, true, true},
		{"a code push", backup.RestoreFilesCode, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			storetest.Run(t, func(t *testing.T, db *store.DB) {
				ctx := context.Background()
				e := newPushEnv(t, db)
				e.setCores(t, "7.1.2", "7.1.4")
				if _, err := db.ExecContext(ctx, "UPDATE sites SET php_version = ? WHERE id = ?", "8.3", e.stg.ID); err != nil {
					t.Fatal(err)
				}
				before := e.site(t, e.prod.ID)
				rtBefore, _ := e.prov.Site(e.prod.ID)
				if before.MailDisabled || before.PreviewProtection || !e.stg.MailDisabled || !e.stg.PreviewProtection ||
					before.MemoryLimitMB == e.stg.MemoryLimitMB {
					t.Fatalf("the sites should differ: production %+v, staging %+v", before, e.stg)
				}

				_, d := e.push(t, sites.StagingPush{Files: tc.files, Database: tc.database})
				if d.Job.State != store.JobSucceeded {
					t.Fatalf("push job: %s %s\n%s", d.Job.State, d.Job.Error, jobLog(d))
				}

				got := e.site(t, e.prod.ID)
				wantCore := "7.1.2"
				if tc.moves {
					wantCore = "7.1.4"
				}
				if got.WPCoreVersion != wantCore {
					t.Errorf("core = %s, want %s", got.WPCoreVersion, wantCore)
				}
				// The record is the same, other than what a job records anyway.
				normalized := got
				normalized.WPCoreVersion, normalized.UpdatedAt = before.WPCoreVersion, before.UpdatedAt
				normalized.TemplateFingerprint = before.TemplateFingerprint
				if !reflect.DeepEqual(normalized, before) {
					t.Errorf("production site's record changed:\n got %+v\nwant %+v", normalized, before)
				}

				// So is what is rendered from it.
				rt, _ := e.prov.Site(e.prod.ID)
				if rt.Config.Environment != provisioner.EnvironmentProduction || rt.Config.DisableMail {
					t.Errorf("rendered production site: environment %q, mail disabled %v", rt.Config.Environment, rt.Config.DisableMail)
				}
				if tc.moves {
					if rt.Config.WPCoreVersion != wantCore {
						t.Errorf("rendered core = %s, want %s", rt.Config.WPCoreVersion, wantCore)
					}
					rt.Config.WPCoreVersion = rtBefore.Config.WPCoreVersion
				}
				// The job re-rendered it during maintenance mode; rt.Maintenance is the
				// live flag.
				rt.Config.PinnedImages, rt.Config.Maintenance = rtBefore.Config.PinnedImages, rtBefore.Config.Maintenance
				if !slices.Equal(rt.Config.Aliases, rtBefore.Config.Aliases) {
					t.Errorf("aliases = %v, want %v", rt.Config.Aliases, rtBefore.Config.Aliases)
				}
				rt.Config.Aliases = rtBefore.Config.Aliases
				if !reflect.DeepEqual(rt.Config, rtBefore.Config) {
					t.Errorf("rendered production site changed:\n got %+v\nwant %+v", rt.Config, rtBefore.Config)
				}
				if rt.Maintenance {
					t.Error("rendered production site is in maintenance mode")
				}

				// And the staging site's stayed its own.
				stg := e.site(t, e.stg.ID)
				rtStg, _ := e.prov.Site(e.stg.ID)
				if !stg.MailDisabled || !stg.PreviewProtection || rtStg.Config.Environment != provisioner.EnvironmentStaging || !rtStg.Config.DisableMail {
					t.Errorf("staging site = %+v, rendered %+v", stg, rtStg.Config)
				}
			})
		})
	}
}

// TestPushStagingKeepsMaintenanceMode: a production site that was already in
// maintenance mode stays in it, on success and on a failure before the restore.
func TestPushStagingKeepsMaintenanceMode(t *testing.T) {
	for _, tc := range []struct {
		name   string
		inject func(e *pushEnv)
		state  store.JobState
	}{
		{"a push that succeeds", func(*pushEnv) {}, store.JobSucceeded},
		{"a failed staging backup", func(e *pushEnv) {
			e.engine.Err = func(op, siteID string) error {
				if op == "Backup" && siteID == e.stg.ID {
					return backup.ErrSiteNotFound
				}
				return nil
			}
		}, store.JobFailed},
		{"a failed pre-push backup", func(e *pushEnv) {
			e.engine.Err = func(op, siteID string) error {
				if op == "Backup" && siteID == e.prod.ID {
					return backup.ErrSiteNotFound
				}
				return nil
			}
		}, store.JobFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			storetest.Run(t, func(t *testing.T, db *store.DB) {
				ctx := context.Background()
				e := newPushEnv(t, db)
				if _, err := store.NewSites(db).SetMaintenance(ctx, e.prod.ID, true); err != nil {
					t.Fatal(err)
				}
				if err := e.prov.SetMaintenance(ctx, e.prod.ID, true); err != nil {
					t.Fatal(err)
				}
				tc.inject(e)
				_, d := e.push(t, sites.StagingPush{Files: backup.RestoreFilesCode})
				if d.Job.State != tc.state {
					t.Fatalf("push job: %s %s\n%s", d.Job.State, d.Job.Error, jobLog(d))
				}
				rt, _ := e.prov.Site(e.prod.ID)
				if got := e.site(t, e.prod.ID); !got.Maintenance || !rt.Maintenance || got.Status != store.SiteRunning {
					t.Errorf("production site: status %s, maintenance %v, rendered %v", got.Status, got.Maintenance, rt.Maintenance)
				}
			})
		})
	}
}

// TestPushStagingFailedBackup: the issue's last test. The awaited backup
// failed, so the job fails in its first step, and production is as it was apart
// from its status message.
func TestPushStagingFailedBackup(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newPushEnv(t, db)
		e.engine.Err = func(op, siteID string) error {
			if op == "Backup" && siteID == e.stg.ID {
				return backup.ErrSiteNotFound // permanent
			}
			return nil
		}
		before := e.site(t, e.prod.ID)
		engineFrom, provFrom := len(e.engine.Calls()), len(e.prov.Calls())
		e.wpCalls(e.prod.ID, "")
		flushes := e.cache.Flushes(e.prod.ID)

		out, d := e.push(t, sites.StagingPush{Files: backup.RestoreFilesAll, Database: true})
		if b := e.job(t, out.BackupJob.ID); b.Job.State != store.JobFailed {
			t.Fatalf("backup job: %s", b.Job.State)
		}
		if d.Job.State != store.JobFailed || !strings.Contains(d.Job.Error, jobs.ErrAwaitedJobFailed.Error()) {
			t.Fatalf("push job: %s %q\n%s", d.Job.State, d.Job.Error, jobLog(d))
		}
		for _, st := range d.Steps[1:] {
			if st.State != store.StepPending {
				t.Errorf("step %s is %s", st.Name, st.State)
			}
		}
		e.requireUntouched(t, before, engineFrom, provFrom, flushes)
		if got := e.site(t, e.stg.ID); !got.StagingPushedAt.IsZero() {
			t.Errorf("a failed push was recorded: %+v", got)
		}

		// It can be tried again once the staging site's backups work.
		e.engine.Err = nil
		if _, d := e.push(t, sites.StagingPush{Files: backup.RestoreFilesNone, Database: true}); d.Job.State != store.JobSucceeded {
			t.Errorf("second push: %s %s", d.Job.State, d.Job.Error)
		}
	})
}

// TestPushStagingRollsBack: the issue's third acceptance criterion. Production's
// pre-push backup comes back in full, whatever the scope, and it stays in
// maintenance mode with the error.
func TestPushStagingRollsBack(t *testing.T) {
	transient := errors.New("connection reset by peer")
	fatal := &provisioner.ExecError{ExitCode: 255, Stderr: "PHP Fatal error: boom\nStack trace:\n#0 long"}
	failWP := func(match func(args []string) bool, err error) func(*pushEnv) {
		return func(e *pushEnv) {
			e.setFailWP(func(siteID string, args []string) error {
				if siteID == e.prod.ID && match(args) {
					return err
				}
				return nil
			})
		}
	}
	for _, tc := range []struct {
		name     string
		files    backup.RestoreFiles
		database bool
		inject   func(e *pushEnv)
	}{
		{"database upgrade", backup.RestoreFilesNone, true, failWP(func(a []string) bool { return slices.Equal(a, []string{"core", "update-db"}) }, fatal)},
		{"URL rewrite", backup.RestoreFilesAll, true, failWP(func(a []string) bool { return a[0] == "search-replace" }, fatal)},
		{"plugin stack", backup.RestoreFilesCode, true, failWP(func(a []string) bool { return a[0] == "plugin" && a[1] == "list" }, transient)},
		{"code WordPress can't load", backup.RestoreFilesCode, false, failWP(func(a []string) bool { return a[0] == "plugin" && a[1] == "list" }, fatal)},
		{"the restore itself", backup.RestoreFilesCode, false, func(e *pushEnv) {
			e.engine.restoreErr = func(_ int, snap backup.SnapshotID) error {
				if !strings.Contains(e.prePushSnapshot(), string(snap)) {
					return transient
				}
				return nil
			}
		}},
		{"the staging site's snapshot vanishing", backup.RestoreFilesCode, true, func(e *pushEnv) {
			e.engine.restoreErr = func(_ int, snap backup.SnapshotID) error {
				if !strings.Contains(e.prePushSnapshot(), string(snap)) {
					return backup.ErrSnapshotNotFound
				}
				return nil
			}
		}},
		{"the restart", backup.RestoreFilesAll, false, func(e *pushEnv) {
			e.prov.Err = func(op, siteID string) error {
				if op == "StopSite" && siteID == e.prod.ID && e.prePushSnapshot() != "" && !e.pushRolledBack() {
					return transient
				}
				return nil
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			storetest.Run(t, func(t *testing.T, db *store.DB) {
				e := newPushEnv(t, db)
				from := len(e.engine.Calls())
				tc.inject(e)

				_, d := e.push(t, sites.StagingPush{Files: tc.files, Database: tc.database})
				if d.Job.State != store.JobFailed {
					t.Fatalf("push job: %s %s\n%s", d.Job.State, d.Job.Error, jobLog(d))
				}
				if strings.Contains(d.Job.Error, "Stack trace") {
					t.Errorf("job error keeps the stack trace: %q", d.Job.Error)
				}
				pre := e.preRestoreBackups(t, e.prod.ID)
				if len(pre) != 1 || pre[0].Status != store.BackupSucceeded {
					t.Fatalf("pre-push backups = %+v", pre)
				}
				if res := jobResult(t, d.Job); res["pre_push_backup_id"] != pre[0].ID {
					t.Errorf("the failed push's result = %v, want it to name the pre-push backup %s", res, pre[0].ID)
				}
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
				if last.SiteID != e.prod.ID || last.TargetSiteID != e.prod.ID || string(last.Snapshot) != pre[0].SnapshotID || last.Restore != backup.FullRestore() {
					t.Errorf("rollback restore = %+v, want the pre-push snapshot %s in full", last, pre[0].SnapshotID)
				}
				if got := e.data(e.prod.ID); got != e.prodData {
					t.Errorf("production database after the rollback = %q, want %q", got, e.prodData)
				}
				got := e.site(t, e.prod.ID)
				rt, _ := e.prov.Site(e.prod.ID)
				if got.Status != store.SiteRunning || !strings.Contains(got.StatusMessage, "push failed and the site was rolled back to its pre-push backup") ||
					!strings.Contains(got.StatusMessage, pre[0].ID) {
					t.Errorf("production site after the rollback: %s %q", got.Status, got.StatusMessage)
				}
				if !got.Maintenance || !rt.Maintenance {
					t.Errorf("maintenance after the rollback: %v/%v, want on (§5.4 step 7)", got.Maintenance, rt.Maintenance)
				}
				if rt.Config.Environment != provisioner.EnvironmentProduction || rt.Config.DisableMail {
					t.Errorf("rendered production site after the rollback = %+v", rt.Config)
				}
				if stg := e.site(t, e.stg.ID); !stg.StagingPushedAt.IsZero() {
					t.Errorf("a failed push was recorded: %+v", stg)
				}
			})
		})
	}
}

// TestPushStagingRollsBackCore: a core the push moved production to is put back
// after the rollback, the way a restore does it, and stays if the rollback
// couldn't restore the database that needs the older core.
func TestPushStagingRollsBackCore(t *testing.T) {
	fatal := &provisioner.ExecError{ExitCode: 255, Stderr: "PHP Fatal error: boom"}
	for _, tc := range []struct {
		name         string
		rollbackFail bool
		wantCore     string
		wantStatus   store.SiteStatus
		wantMessage  string
	}{
		{"rolled back", false, "7.1.2", store.SiteRunning, "push failed and the site was rolled back"},
		{"rollback failed", true, "7.1.4", store.SiteFailed, "rolling back to the pre-push backup failed too"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			storetest.Run(t, func(t *testing.T, db *store.DB) {
				e := newPushEnv(t, db)
				e.setCores(t, "7.1.2", "7.1.4")
				e.setFailWP(func(siteID string, args []string) error {
					if siteID == e.prod.ID && slices.Equal(args, []string{"core", "update-db"}) {
						return fatal
					}
					return nil
				})
				if tc.rollbackFail {
					e.engine.restoreErr = func(_ int, snap backup.SnapshotID) error {
						if strings.Contains(e.prePushSnapshot(), string(snap)) {
							return backup.ErrSnapshotNotFound
						}
						return nil
					}
				}
				_, d := e.push(t, sites.StagingPush{Files: backup.RestoreFilesAll, Database: true})
				if d.Job.State != store.JobFailed {
					t.Fatalf("push job: %s %s\n%s", d.Job.State, d.Job.Error, jobLog(d))
				}
				got := e.site(t, e.prod.ID)
				rt, _ := e.prov.Site(e.prod.ID)
				if got.WPCoreVersion != tc.wantCore || rt.Config.WPCoreVersion != tc.wantCore {
					t.Errorf("core = %s, rendered %s, want %s", got.WPCoreVersion, rt.Config.WPCoreVersion, tc.wantCore)
				}
				if got.Status != tc.wantStatus || !strings.Contains(got.StatusMessage, tc.wantMessage) || !got.Maintenance {
					t.Errorf("production site = %s, %q, maintenance %v", got.Status, got.StatusMessage, got.Maintenance)
				}
			})
		})
	}
}

// prePushSnapshot returns the snapshot ID of production's pre-push backup, once
// it has one.
func (e *pushEnv) prePushSnapshot() string {
	page, err := store.NewBackupRecords(e.db).List(context.Background(), store.BackupRecordFilter{SiteID: e.prod.ID})
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

func (e *pushEnv) pushRolledBack() bool {
	pre := e.prePushSnapshot()
	return pre != "" && slices.ContainsFunc(e.engine.Calls(), func(c backuptest.Call) bool {
		return c.Op == "Restore" && string(c.Snapshot) == pre
	})
}

// TestPushStagingCoreGuard: what a database push may do to production's core,
// as its core policy decides. Refused at request time, nothing is queued.
func TestPushStagingCoreGuard(t *testing.T) {
	since := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	hold := store.CorePolicy{Hold: true, HoldSince: since}
	approve72 := store.CorePolicy{ApprovedBranch: "7.2"}
	auto := func(o *sites.Options) {
		o.CoreUpdates.Policy = coreupdate.Settings{MajorAutoApproveAfter: 30 * 24 * time.Hour}
	}
	for _, tc := range []struct {
		name          string
		prod, stg     string
		site, group   store.CorePolicy
		opts          []func(*sites.Options)
		files         backup.RestoreFiles
		database      bool
		refused       string // what the detail says; empty means allowed
		wantProdCore  string
		wantProdMoved bool
	}{
		{name: "a newer release of the same branch moves production up", prod: "7.1.2", stg: "7.1.4", database: true, wantProdCore: "7.1.4"},
		{name: "same branch, held", prod: "7.1.2", stg: "7.1.4", site: hold, database: true, wantProdCore: "7.1.4"},
		{name: "the same release", prod: "7.1.4", stg: "7.1.4", database: true, wantProdCore: "7.1.4"},
		{name: "an older core stays on production's", prod: "7.2.1", stg: "7.1.2", database: true, wantProdCore: "7.2.1"},
		{name: "a newer branch, not approved", prod: "7.1.4", stg: "7.2.1", database: true, refused: "approve branch 7.2"},
		{name: "a newer branch, held", prod: "7.1.4", stg: "7.2.1", site: hold, database: true, refused: "holds it on branch 7.1"},
		{name: "a newer branch, its group held", prod: "7.1.4", stg: "7.2.1", group: hold, database: true, refused: "holds it on branch 7.1"},
		{name: "a newer branch, held, its group approving", prod: "7.1.4", stg: "7.2.1", site: hold, group: approve72, database: true, refused: "holds it on branch 7.1"},
		{name: "a newer branch, approved", prod: "7.1.4", stg: "7.2.1", site: approve72, database: true, wantProdCore: "7.2.1"},
		{name: "a newer branch, its group approving", prod: "7.1.4", stg: "7.2.1", group: approve72, database: true, wantProdCore: "7.2.1"},
		{name: "a newer branch, approved, its group holding", prod: "7.1.4", stg: "7.2.1", site: approve72, group: hold, database: true, wantProdCore: "7.2.1"},
		{name: "a newer branch, approved only up to its own", prod: "7.1.4", stg: "7.2.1", site: store.CorePolicy{ApprovedBranch: "7.1"}, database: true, refused: "approve branch 7.2"},
		{name: "a newer branch, approved automatically", prod: "7.1.4", stg: "7.2.1", opts: []func(*sites.Options){auto}, database: true, wantProdCore: "7.2.1"},
		{name: "a newer branch, held despite automatic approval", prod: "7.1.4", stg: "7.2.1", site: hold, opts: []func(*sites.Options){auto}, database: true, refused: "holds it on branch 7.1"},
		// update_core takes the oldest image with at least the snapshot's core.
		{name: "a release whose branch has no image at least as new", prod: "7.1.2", stg: "7.1.9", database: true, refused: "approve branch 7.2"},
		{name: "a core no image has", prod: "7.1.2", stg: "99.0", site: approve72, database: true, refused: "newer than any site image"},
		{name: "code doesn't move the core", prod: "7.1.4", stg: "7.2.1", site: hold, files: backup.RestoreFilesCode, wantProdCore: "7.1.4"},
		{name: "files don't move the core", prod: "7.1.4", stg: "7.2.1", files: backup.RestoreFilesAll, wantProdCore: "7.1.4"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			storetest.Run(t, func(t *testing.T, db *store.DB) {
				ctx := context.Background()
				e := newPushEnv(t, db, tc.opts...)
				e.setCores(t, tc.prod, tc.stg)
				policies := store.NewCorePolicies(db)
				if err := policies.SetSite(ctx, e.prod.ID, tc.site); err != nil {
					t.Fatal(err)
				}
				if err := policies.SetClientGroup(ctx, e.prod.ClientGroupID, tc.group); err != nil {
					t.Fatal(err)
				}
				files := tc.files
				if files == "" {
					files = backup.RestoreFilesNone
				}
				req := sites.StagingPush{Files: files, Database: tc.database}
				if tc.refused != "" {
					jobsBefore := e.count(t, "SELECT COUNT(*) FROM jobs")
					_, err := e.svc.PushStaging(ctx, e.stg.ID, req)
					var ve *sites.ValidationError
					if !errors.As(err, &ve) || len(ve.Fields) != 1 || ve.Fields[0].Field != "/database" || !strings.Contains(ve.Fields[0].Detail, tc.refused) {
						t.Fatalf("err = %v, want a validation error on /database saying %q", err, tc.refused)
					}
					if n := e.count(t, "SELECT COUNT(*) FROM jobs"); n != jobsBefore {
						t.Errorf("%d jobs, want %d", n, jobsBefore)
					}
					if got := e.site(t, e.prod.ID); got.Status != store.SiteRunning {
						t.Errorf("production site = %s", got.Status)
					}
					return
				}
				_, d := e.push(t, req)
				if d.Job.State != store.JobSucceeded {
					t.Fatalf("push job: %s %s\n%s", d.Job.State, d.Job.Error, jobLog(d))
				}
				if got := e.site(t, e.prod.ID).WPCoreVersion; got != tc.wantProdCore {
					t.Errorf("production core = %s, want %s", got, tc.wantProdCore)
				}
			})
		})
	}
}

// TestPushStagingCoreGuardAtPrepare: the snapshot is what counts. What changed
// while the job waited fails it in prepare, before it touches production.
func TestPushStagingCoreGuardAtPrepare(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(t *testing.T, e *pushEnv)
		want   string
	}{
		{"the staging site moved to a newer branch", func(_ *testing.T, e *pushEnv) { e.setSnapshotCore("7.2.1") }, "approve branch 7.2"},
		{"the snapshot doesn't say", func(_ *testing.T, e *pushEnv) { e.setSnapshotCore("") }, "doesn't say which WordPress core"},
		{"no image has its core any more", func(_ *testing.T, e *pushEnv) { e.setSnapshotCore("99.0") }, "newer than any site image"},
		{"production was held on its branch", func(t *testing.T, e *pushEnv) {
			e.setSnapshotCore("7.2.1")
			if err := store.NewCorePolicies(e.db).SetSite(context.Background(), e.prod.ID, store.CorePolicy{Hold: true, HoldSince: testNow}); err != nil {
				t.Fatal(err)
			}
		}, "holds it on branch 7.1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			storetest.Run(t, func(t *testing.T, db *store.DB) {
				ctx := context.Background()
				e := newPushEnv(t, db)
				e.setCores(t, "7.1.4", "7.1.4")
				if err := store.NewCorePolicies(db).SetSite(ctx, e.prod.ID, store.CorePolicy{}); err != nil {
					t.Fatal(err)
				}
				before := e.site(t, e.prod.ID)
				engineFrom, provFrom := len(e.engine.Calls()), len(e.prov.Calls())
				e.wpCalls(e.prod.ID, "")
				flushes := e.cache.Flushes(e.prod.ID)

				out, err := e.svc.PushStaging(ctx, e.stg.ID, sites.StagingPush{Files: backup.RestoreFilesAll, Database: true})
				if err != nil {
					t.Fatal(err)
				}
				tc.change(t, e)
				e.runJobs(t)
				d := e.job(t, out.Job.ID)
				if d.Job.State != store.JobFailed || !strings.Contains(d.Job.Error, tc.want) {
					t.Fatalf("push job: %s %q, want it failed saying %q", d.Job.State, d.Job.Error, tc.want)
				}
				e.requireUntouched(t, before, engineFrom, provFrom, flushes)
				if got := e.site(t, e.prod.ID); !strings.Contains(got.StatusMessage, tc.want) {
					t.Errorf("status message = %q", got.StatusMessage)
				}
			})
		})
	}

	// A push that keeps the database doesn't care.
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newPushEnv(t, db)
		e.setCores(t, "7.1.4", "7.1.4")
		e.setSnapshotCore("7.2.1")
		_, d := e.push(t, sites.StagingPush{Files: backup.RestoreFilesCode})
		if d.Job.State != store.JobSucceeded {
			t.Errorf("code push: %s %s", d.Job.State, d.Job.Error)
		}
		if got := e.site(t, e.prod.ID).WPCoreVersion; got != "7.1.4" {
			t.Errorf("production core = %s", got)
		}
	})
}

// TestPushStagingSameBranchMovesUpFromSnapshot: a newer release of the same
// branch that only the snapshot knows about moves production up too.
func TestPushStagingSameBranchMovesUpFromSnapshot(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newPushEnv(t, db)
		e.setCores(t, "7.1.2", "7.1.2")
		out, err := e.svc.PushStaging(context.Background(), e.stg.ID, sites.StagingPush{Files: backup.RestoreFilesNone, Database: true})
		if err != nil {
			t.Fatal(err)
		}
		e.setSnapshotCore("7.1.4")
		e.runJobs(t)
		if d := e.job(t, out.Job.ID); d.Job.State != store.JobSucceeded {
			t.Fatalf("push job: %s %s\n%s", d.Job.State, d.Job.Error, jobLog(d))
		}
		if got := e.site(t, e.prod.ID).WPCoreVersion; got != "7.1.4" {
			t.Errorf("production core = %s, want 7.1.4", got)
		}
		if calls := e.wpCalls(e.prod.ID, "core update-db"); len(calls) == 0 {
			t.Error("the database wasn't upgraded")
		}
	})
}

// TestPushStagingStagingSiteChanges: what happens to the staging site between
// the request and the job's prepare step.
func TestPushStagingStagingSiteChanges(t *testing.T) {
	ctx := context.Background()
	deleteRow := func(t *testing.T, e *pushEnv) {
		t.Helper()
		if _, err := store.NewSites(e.db).Delete(ctx, e.stg.ID); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name string
		// before runs before the sync backup, after between it and the push.
		before, after func(t *testing.T, e *pushEnv)
		// backupFails says the backup job itself fails.
		backupFails bool
		// want is the job's error; empty means the push succeeds.
		want string
	}{
		{
			name: "a deletion requested before the backup ran",
			before: func(t *testing.T, e *pushEnv) {
				if _, err := e.svc.Delete(ctx, e.stg.ID, "", sites.DeleteOptions{}); err != nil {
					t.Fatal(err)
				}
			},
			backupFails: true, want: "is being deleted",
		},
		{
			name: "a deletion requested after the backup ran",
			after: func(t *testing.T, e *pushEnv) {
				if _, err := e.svc.Delete(ctx, e.stg.ID, "", sites.DeleteOptions{}); err != nil {
					t.Fatal(err)
				}
			},
			want: "is being deleted",
		},
		{name: "deleted, keeping its backups, after the backup ran", after: deleteRow, want: "was deleted while the push waited"},
		{
			name: "deleted with its backups after the backup ran",
			after: func(t *testing.T, e *pushEnv) {
				if _, err := e.engine.ForgetSite(ctx, e.stg.ID); err != nil {
					t.Fatal(err)
				}
				deleteRow(t, e)
			},
			want: "was deleted while the push waited",
		},
		{
			name: "its snapshot removed by retention",
			after: func(t *testing.T, e *pushEnv) {
				if _, err := e.engine.ForgetSite(ctx, e.stg.ID); err != nil {
					t.Fatal(err)
				}
			},
			want: backup.ErrSnapshotNotFound.Error(),
		},
		{
			name:  "marked failed after the backup ran",
			after: func(t *testing.T, e *pushEnv) { e.setStatus(t, e.stg.ID, store.SiteFailed) },
			want:  "was marked failed while the push waited",
		},
		{
			// The snapshot is complete, so these don't matter.
			name:  "stopped after the backup ran",
			after: func(t *testing.T, e *pushEnv) { e.setStatus(t, e.stg.ID, store.SiteStopped) },
		},
		{
			name:  "being updated after the backup ran",
			after: func(t *testing.T, e *pushEnv) { e.setStatus(t, e.stg.ID, store.SiteUpdating) },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			storetest.Run(t, func(t *testing.T, db *store.DB) {
				e := newPushEnv(t, db)
				before := e.site(t, e.prod.ID)
				engineFrom, provFrom := len(e.engine.Calls()), len(e.prov.Calls())
				e.wpCalls(e.prod.ID, "")
				flushes := e.cache.Flushes(e.prod.ID)

				out, err := e.svc.PushStaging(ctx, e.stg.ID, sites.StagingPush{Files: backup.RestoreFilesAll, Database: true})
				if err != nil {
					t.Fatal(err)
				}
				if tc.before != nil {
					tc.before(t, e)
				}
				if !tc.backupFails {
					e.runBackupOnly(t, out)
				}
				if tc.after != nil {
					tc.after(t, e)
				}
				e.runJobs(t)
				d := e.job(t, out.Job.ID)
				if tc.want == "" {
					if d.Job.State != store.JobSucceeded {
						t.Fatalf("push job: %s %s\n%s", d.Job.State, d.Job.Error, jobLog(d))
					}
					if got := e.data(e.prod.ID); got != e.pushRewritten(e.prod.PreviewDomain) {
						t.Errorf("production database = %q", got)
					}
					return
				}
				if d.Job.State != store.JobFailed || !strings.Contains(d.Job.Error, tc.want) {
					t.Fatalf("push job: %s %q, want it failed saying %q", d.Job.State, d.Job.Error, tc.want)
				}
				e.requireUntouched(t, before, engineFrom, provFrom, flushes)
			})
		})
	}
}

// TestPushStagingDeletedWhileRunning: a staging site deleted once the job is
// under way doesn't stop it. The URLs still come out right, and finish has
// nowhere to record the push.
func TestPushStagingDeletedWhileRunning(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		ctx := context.Background()
		e := newPushEnv(t, db)
		e.setFailWP(func(siteID string, args []string) error {
			if siteID == e.prod.ID && slices.Equal(args, []string{"core", "update-db"}) {
				if _, err := store.NewSites(db).Delete(ctx, e.stg.ID); err != nil {
					t.Error(err)
				}
			}
			return nil
		})
		_, d := e.push(t, sites.StagingPush{Files: backup.RestoreFilesAll, Database: true})
		if d.Job.State != store.JobSucceeded {
			t.Fatalf("push job: %s %s\n%s", d.Job.State, d.Job.Error, jobLog(d))
		}
		if !strings.Contains(jobLog(d), "no longer exists") {
			t.Errorf("job log doesn't say so:\n%s", jobLog(d))
		}
		if got := e.data(e.prod.ID); got != e.pushRewritten(e.prod.PreviewDomain) {
			t.Errorf("production database = %q", got)
		}
		if got := e.site(t, e.prod.ID); got.Status != store.SiteRunning || got.Maintenance {
			t.Errorf("production site = %+v", got)
		}
		if res := jobResult(t, d.Job); res["pre_push_backup_id"] == "" {
			t.Errorf("result = %v", res)
		}
	})
}

// TestPushStagingStoppedStaging: a stopped staging site can be backed up, so it
// can be pushed.
func TestPushStagingStoppedStaging(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newPushEnv(t, db)
		e.setStatus(t, e.stg.ID, store.SiteStopped)
		_, d := e.push(t, sites.StagingPush{Files: backup.RestoreFilesAll, Database: true})
		if d.Job.State != store.JobSucceeded {
			t.Fatalf("push job: %s %s\n%s", d.Job.State, d.Job.Error, jobLog(d))
		}
		if got := e.site(t, e.stg.ID).Status; got != store.SiteStopped {
			t.Errorf("staging site = %s, want it still stopped", got)
		}
	})
}

// TestPushStagingWithoutDatabaseRepairsPlugins: the pushed plugins directory
// can lack what the platform depends on. Production gets it back without
// anything that writes to its database.
func TestPushStagingWithoutDatabaseRepairsPlugins(t *testing.T) {
	const installPrefix = "plugin install " + sites.BundledPluginDir + "/"
	for _, tc := range []struct {
		name       string
		files      backup.RestoreFiles
		remove     func(s *wpSite)
		unbundled  string
		wantCalls  []string
		wantDropIn string
	}{
		{
			name: "Nginx Helper gone", files: backup.RestoreFilesCode,
			remove:     func(s *wpSite) { delete(s.plugins, sites.PluginNginxHelper) },
			wantCalls:  []string{installPrefix + sites.PluginNginxHelper + ".zip"},
			wantDropIn: redisDropIn,
		},
		{
			name: "both gone", files: backup.RestoreFilesCode,
			remove: func(s *wpSite) {
				delete(s.plugins, sites.PluginNginxHelper)
				delete(s.plugins, sites.PluginRedisCache)
			},
			wantCalls: []string{
				installPrefix + sites.PluginNginxHelper + ".zip",
				installPrefix + sites.PluginRedisCache + ".zip",
			},
			wantDropIn: redisDropIn,
		},
		{
			name: "the drop-in gone with all of wp-content", files: backup.RestoreFilesAll,
			remove:     func(s *wpSite) { s.dropin = "" },
			wantCalls:  []string{"redis enable"},
			wantDropIn: redisDropIn,
		},
		{
			name: "an image that doesn't bundle it", files: backup.RestoreFilesCode,
			remove:     func(s *wpSite) { delete(s.plugins, sites.PluginNginxHelper) },
			unbundled:  sites.PluginNginxHelper,
			wantDropIn: redisDropIn,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			storetest.Run(t, func(t *testing.T, db *store.DB) {
				e := newPushEnv(t, db)
				if tc.unbundled != "" {
					e.wp.unbundled[tc.unbundled] = true
				}
				optionsBefore := e.wp.state(e.prod.ID).options
				capsBefore := e.wp.state(e.prod.ID).caps
				autoBefore := e.wp.state(e.prod.ID).autoUpdates
				e.wpCalls(e.prod.ID, "")
				removed := false
				e.engine.restoreErr = func(int, backup.SnapshotID) error {
					if !removed {
						e.wp.edit(e.prod.ID, tc.remove)
						removed = true
					}
					return nil
				}

				_, d := e.push(t, sites.StagingPush{Files: tc.files})
				if d.Job.State != store.JobSucceeded {
					t.Fatalf("push job: %s %s\n%s", d.Job.State, d.Job.Error, jobLog(d))
				}
				if got := e.data(e.prod.ID); got != e.prodData {
					t.Errorf("production database changed: %q", got)
				}
				var got []string
				for _, c := range e.wpCalls(e.prod.ID, "") {
					if strings.HasPrefix(c, "plugin list") || strings.HasPrefix(c, "eval ") {
						continue
					}
					got = append(got, c)
				}
				if !slices.Equal(got, tc.wantCalls) {
					t.Errorf("WP-CLI calls that change something = %q, want %q", got, tc.wantCalls)
				}
				st := e.wp.state(e.prod.ID)
				if tc.unbundled == "" {
					for _, slug := range []string{sites.PluginNginxHelper, sites.PluginRedisCache} {
						if _, ok := st.plugins[slug]; !ok {
							t.Errorf("%s is missing after the push: %v", slug, st.plugins)
						}
					}
				}
				if st.dropin != tc.wantDropIn {
					t.Errorf("drop-in = %q, want %q", st.dropin, tc.wantDropIn)
				}
				if !reflect.DeepEqual(st.options, optionsBefore) || !slices.Equal(st.caps, capsBefore) || !slices.Equal(st.autoUpdates, autoBefore) {
					t.Errorf("the push changed WordPress's state: %+v", st)
				}
			})
		})
	}
}

// TestPushStagingWithDatabaseConvergesPlugins: the dump brings the staging
// site's plugin state, so a database push converges the stack.
func TestPushStagingWithDatabaseConvergesPlugins(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newPushEnv(t, db)
		removed := false
		e.engine.restoreErr = func(int, backup.SnapshotID) error {
			if !removed {
				e.wp.edit(e.prod.ID, func(s *wpSite) {
					s.plugins[sites.PluginRedisCache] = "inactive"
					delete(s.plugins, sites.PluginNginxHelper)
					s.dropin = ""
				})
				removed = true
			}
			return nil
		}
		_, d := e.push(t, sites.StagingPush{Files: backup.RestoreFilesAll, Database: true})
		if d.Job.State != store.JobSucceeded {
			t.Fatalf("push job: %s %s\n%s", d.Job.State, d.Job.Error, jobLog(d))
		}
		st := e.wp.state(e.prod.ID)
		if st.plugins[sites.PluginRedisCache] != "active" || st.plugins[sites.PluginNginxHelper] != "active" || st.dropin != redisDropIn {
			t.Errorf("default stack after the push = %v, drop-in %q", st.plugins, st.dropin)
		}
	})
}

func TestPushStagingRefused(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newPushEnv(t, db)
		jobsBefore := e.count(t, "SELECT COUNT(*) FROM jobs")
		good := sites.StagingPush{Files: backup.RestoreFilesAll, Database: true}

		field := func(field string) func(error) bool {
			return func(err error) bool {
				var ve *sites.ValidationError
				return errors.As(err, &ve) && slices.ContainsFunc(ve.Fields, func(f sites.FieldError) bool { return f.Field == field })
			}
		}
		is := func(target error) func(error) bool { return func(err error) bool { return errors.Is(err, target) } }
		status := func(siteID string, s store.SiteStatus) func() func() {
			return func() func() {
				e.setStatus(t, siteID, s)
				return func() { e.setStatus(t, siteID, store.SiteRunning) }
			}
		}
		for _, tc := range []struct {
			name   string
			siteID string
			req    sites.StagingPush
			setup  func() func()
			want   func(error) bool
		}{
			{name: "a production site", siteID: e.prod.ID, req: good, want: field("/site_id")},
			{name: "a missing site", siteID: id.New(), req: good, want: is(sites.ErrNotFound)},
			{name: "a scope that copies nothing", siteID: e.stg.ID, req: sites.StagingPush{Files: backup.RestoreFilesNone}, want: field("/files")},
			{name: "an unknown files scope", siteID: e.stg.ID, req: sites.StagingPush{Files: "some", Database: true}, want: field("/files")},
			{name: "no files scope", siteID: e.stg.ID, req: sites.StagingPush{Database: true}, want: field("/files")},
			{name: "a staging site being updated", siteID: e.stg.ID, req: good, setup: status(e.stg.ID, store.SiteUpdating), want: is(sites.ErrBusy)},
			{name: "a staging site being provisioned", siteID: e.stg.ID, req: good, setup: status(e.stg.ID, store.SiteProvisioning), want: is(sites.ErrBusy)},
			{name: "a staging site being deleted", siteID: e.stg.ID, req: good, setup: status(e.stg.ID, store.SiteDeleting), want: is(sites.ErrBusy)},
			{name: "a failed staging site", siteID: e.stg.ID, req: good, setup: status(e.stg.ID, store.SiteFailed), want: is(sites.ErrStagingFailed)},
			{name: "a production site being updated", siteID: e.stg.ID, req: good, setup: status(e.prod.ID, store.SiteUpdating), want: is(sites.ErrBusy)},
			{name: "a stopped production site", siteID: e.stg.ID, req: good, setup: status(e.prod.ID, store.SiteStopped), want: is(sites.ErrNotRunning)},
			{name: "a failed production site", siteID: e.stg.ID, req: good, setup: status(e.prod.ID, store.SiteFailed), want: is(sites.ErrNotRunning)},
		} {
			t.Run(tc.name, func(t *testing.T) {
				if tc.setup != nil {
					defer tc.setup()()
				}
				if _, err := e.svc.PushStaging(ctx, tc.siteID, tc.req); !tc.want(err) {
					t.Errorf("err = %v", err)
				}
			})
		}
		if n := e.count(t, "SELECT COUNT(*) FROM jobs"); n != jobsBefore {
			t.Errorf("%d jobs, want %d", n, jobsBefore)
		}
		if got := e.site(t, e.prod.ID); got.Status != store.SiteRunning {
			t.Errorf("production site = %s", got.Status)
		}

		// A push is queued, waiting for its backup: not a second one, from this
		// staging site or another, and no fresh refresh.
		mem := 384
		other := e.createStaging(t, e.prod.ID, sites.NewStaging{MemoryLimitMB: &mem})
		e.runJobs(t)
		if _, err := e.svc.PushStaging(ctx, e.stg.ID, good); err != nil {
			t.Fatal(err)
		}
		if _, err := e.svc.PushStaging(ctx, e.stg.ID, good); !errors.Is(err, sites.ErrBusy) {
			t.Errorf("second push while one is queued = %v", err)
		}
		if _, err := e.svc.PushStaging(ctx, other.Site.ID, good); !errors.Is(err, sites.ErrBusy) {
			t.Errorf("another staging site's push while one is queued = %v", err)
		}
		if _, err := e.svc.RefreshStaging(ctx, e.stg.ID, sites.StagingRefresh{Files: backup.RestoreFilesAll, Database: true}); !errors.Is(err, sites.ErrBusy) {
			t.Errorf("fresh refresh while a push is queued = %v", err)
		}
	})
}

// TestPushStagingAndRefresh: a push and a refresh of the same staging site
// don't interleave.
func TestPushStagingAndRefresh(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newPushEnv(t, db)

		// A refresh is queued: the staging site is updating, so no push.
		refresh, err := e.svc.RefreshStaging(ctx, e.stg.ID, sites.StagingRefresh{Files: backup.RestoreFilesCode})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := e.svc.PushStaging(ctx, e.stg.ID, sites.StagingPush{Files: backup.RestoreFilesCode}); !errors.Is(err, sites.ErrBusy) {
			t.Errorf("push while a refresh is queued = %v", err)
		}
		e.runJobs(t)
		if d := e.job(t, refresh.Job.ID); d.Job.State != store.JobSucceeded {
			t.Fatalf("refresh job: %s %s", d.Job.State, d.Job.Error)
		}

		// A refresh from a named snapshot requested after the push queues behind
		// its sync backup, so the push gets the staging site's content from
		// before that refresh.
		snap := e.snapshot(t, e.prod.ID)
		from := len(e.engine.Calls())
		push, err := e.svc.PushStaging(ctx, e.stg.ID, sites.StagingPush{Files: backup.RestoreFilesNone, Database: true})
		if err != nil {
			t.Fatal(err)
		}
		late, err := e.svc.RefreshStaging(ctx, e.stg.ID, sites.StagingRefresh{Files: backup.RestoreFilesNone, Database: true, SnapshotID: string(snap.ID)})
		if err != nil {
			t.Fatal(err)
		}
		e.runJobs(t)
		for _, j := range []string{push.Job.ID, late.Job.ID} {
			if d := e.job(t, j); d.Job.State != store.JobSucceeded {
				t.Fatalf("job %s: %s %s\n%s", d.Job.Type, d.Job.State, d.Job.Error, jobLog(d))
			}
		}
		ops := e.engineOps(from)
		sync := slices.Index(ops, "Backup "+e.stg.ID+" sync")
		pre := slices.Index(ops, "Backup "+e.stg.ID+" pre_restore")
		if sync < 0 || pre < sync {
			t.Errorf("engine calls = %q: the sync backup should come before the refresh", ops)
		}
		if got := e.data(e.prod.ID); got != e.pushRewritten(e.prod.PreviewDomain) {
			t.Errorf("production database = %q, want the staging site's from before the refresh", got)
		}
	})
}

func TestPushStagingWithoutBackups(t *testing.T) {
	e := newEnv(t, storetest.Open(t, store.SQLite))
	if _, err := e.svc.PushStaging(context.Background(), id.New(), sites.StagingPush{}); !errors.Is(err, sites.ErrNoBackups) {
		t.Errorf("PushStaging without a backup engine = %v", err)
	}
	for _, h := range e.svc.Handlers() {
		if h.Type == sites.PushJobType {
			t.Error("the push job is registered without a backup engine")
		}
	}
}
