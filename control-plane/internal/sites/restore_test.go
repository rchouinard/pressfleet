package sites_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/rchouinard/pressfleet/control-plane/internal/backup"
	"github.com/rchouinard/pressfleet/control-plane/internal/backup/backuptest"
	"github.com/rchouinard/pressfleet/control-plane/internal/backup/sitebackup"
	"github.com/rchouinard/pressfleet/control-plane/internal/catalog"
	"github.com/rchouinard/pressfleet/control-plane/internal/id"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
	"github.com/rchouinard/pressfleet/control-plane/internal/sitedb/sitedbtest"
	"github.com/rchouinard/pressfleet/control-plane/internal/sites"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
)

// restoreManifest adds an older WordPress release that sites can be on.
const restoreManifest = `{
  "registry": "registry.example/pf",
  "php": [
    {"version": "8.4", "activeSupportEnds": "2026-12-31", "securitySupportEnds": "2028-12-31"}
  ],
  "wordpress": {
    "latest": {"version": "7.1.2", "sha1": "761b8101538f0631a0bfc4fba7bc4abeea92f81c"},
    "pinned": [{"version": "6.9", "sha1": "0000000000000000000000000000000000000000"}]
  }
}`

// dataEngine is the fake backup engine with data: a snapshot holds the
// site's database contents (sitedbtest) when it was taken, and a restore
// puts them into the target's database. The database stands for the whole
// site: "key=value" lines, home and siteurl among them.
type dataEngine struct {
	*backuptest.Fake
	dbs *sitedbtest.Fake

	mu    sync.Mutex
	data  map[backup.SnapshotID][]byte
	cores map[backup.SnapshotID]string
	// backupCore, if set, is the WordPress core of the site's new snapshots.
	backupCore map[string]string
	// restoreErr, if set, is consulted on the nth Restore call (from 1).
	restoreErr func(n int, snap backup.SnapshotID) error
	restores   int
}

func (d *dataEngine) Backup(ctx context.Context, siteID string, opts backup.Options) (backup.Snapshot, error) {
	snap, err := d.Fake.Backup(ctx, siteID, opts)
	if err != nil {
		return snap, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.data[snap.ID]; !ok {
		d.data[snap.ID], _ = d.dbs.Data(siteID)
	}
	if core, ok := d.backupCore[siteID]; ok {
		d.cores[snap.ID] = core
	}
	return snap, nil
}

func (d *dataEngine) ListSnapshots(ctx context.Context, siteID string) ([]backup.Snapshot, error) {
	snaps, err := d.Fake.ListSnapshots(ctx, siteID)
	d.mu.Lock()
	defer d.mu.Unlock()
	for i := range snaps {
		snaps[i].WPCoreVersion = d.cores[snaps[i].ID]
	}
	return snaps, err
}

func (d *dataEngine) Restore(ctx context.Context, siteID string, snap backup.SnapshotID, target string, opts backup.RestoreOptions) error {
	d.mu.Lock()
	d.restores++
	n, fail := d.restores, d.restoreErr
	d.mu.Unlock()
	if err := d.Fake.Restore(ctx, siteID, snap, target, opts); err != nil {
		return err
	}
	if fail != nil {
		if err := fail(n, snap); err != nil {
			return err
		}
	}
	if !opts.Database {
		return nil
	}
	d.mu.Lock()
	data := d.data[snap]
	d.mu.Unlock()
	d.dbs.SetData(target, data)
	return nil
}

type restoreEnv struct {
	*env
	engine *dataEngine

	mu sync.Mutex
	// failWP, if set, is consulted for every WP-CLI call.
	failWP func(siteID string, args []string) error
	wpLog  map[string][]string
}

func newRestoreEnv(t *testing.T, db *store.DB) *restoreEnv {
	t.Helper()
	return newRestoreEnvWith(t, db, restoreManifest)
}

// newRestoreEnvWith is newRestoreEnv with another version manifest, after
// applying opts to the service's options.
func newRestoreEnvWith(t *testing.T, db *store.DB, manifest string, opts ...func(*sites.Options)) *restoreEnv {
	t.Helper()
	cat, err := catalog.Parse([]byte(manifest))
	if err != nil {
		t.Fatal(err)
	}
	re := &restoreEnv{wpLog: map[string][]string{}}
	re.env = newEnvWith(t, db, testPreviewBase, func(o *sites.Options) {
		o.Catalog = cat
		re.engine = &dataEngine{
			Fake: backuptest.New(), dbs: o.Databases.(*sitedbtest.Fake),
			data: map[backup.SnapshotID][]byte{}, cores: map[backup.SnapshotID]string{},
		}
		svc, err := sitebackup.New(sitebackup.Options{DB: db, Engine: re.engine, Logger: storetest.Logger()})
		if err != nil {
			t.Fatal(err)
		}
		o.Backups, o.BackupEngine = svc, re.engine
		for _, opt := range opts {
			opt(o)
		}
	})
	re.prov.WPCLI = re.wpcli
	return re
}

// wpcli simulates the WP-CLI commands of a restore on the site's database
// contents, and hands the rest to the plugin simulation.
func (e *restoreEnv) wpcli(siteID string, args []string) (string, error) {
	e.mu.Lock()
	fail := e.failWP
	e.wpLog[siteID] = append(e.wpLog[siteID], strings.Join(args, " "))
	e.mu.Unlock()
	if fail != nil {
		if err := fail(siteID, args); err != nil {
			return "", err
		}
	}
	data, _ := e.dbs.Data(siteID)
	switch {
	case len(args) == 3 && args[0] == "option" && args[1] == "get" && (args[2] == "home" || args[2] == "siteurl"):
		for line := range strings.Lines(string(data)) {
			if v, ok := strings.CutPrefix(strings.TrimSpace(line), args[2]+"="); ok {
				return v + "\n", nil
			}
		}
		return "", &provisioner.ExecError{ExitCode: 1, Stderr: "Error: no such option"}
	case len(args) == 4 && args[0] == "option" && args[1] == "update" && (args[2] == "home" || args[2] == "siteurl"):
		var out []string
		for line := range strings.Lines(string(data)) {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, args[2]+"=") {
				line = args[2] + "=" + args[3]
			}
			out = append(out, line)
		}
		e.dbs.SetData(siteID, []byte(strings.Join(out, "\n")+"\n"))
		return "Success", nil
	case len(args) > 2 && args[0] == "search-replace":
		n := strings.Count(string(data), args[1])
		e.dbs.SetData(siteID, []byte(strings.ReplaceAll(string(data), args[1], args[2])))
		return strings.Repeat("1", min(n, 1)), nil
	case slices.Equal(args, []string{"core", "update-db"}):
		return "Success: WordPress database already at latest db version.", nil
	}
	return e.wp.wpcli(siteID, args)
}

// wpCalls returns the site's WP-CLI calls that start with prefix, and
// forgets all of them.
func (e *restoreEnv) wpCalls(siteID, prefix string) []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []string
	for _, c := range e.wpLog[siteID] {
		if strings.HasPrefix(c, prefix) {
			out = append(out, c)
		}
	}
	delete(e.wpLog, siteID)
	return out
}

func (e *restoreEnv) setFailWP(f func(siteID string, args []string) error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.failWP = f
}

// runningSite provisions a site and gives it database contents naming its
// preview URL.
func (e *restoreEnv) runningSite(t *testing.T, name, content string) store.Site {
	t.Helper()
	site, job := e.create(t, name)
	e.runJobs(t)
	if d := e.job(t, job.ID); d.Job.State != store.JobSucceeded {
		t.Fatalf("provisioning %s: %s %s", name, d.Job.State, d.Job.Error)
	}
	site = e.site(t, site.ID)
	u := "https://" + site.PreviewDomain
	e.dbs.SetData(site.ID, []byte("home="+u+"\nsiteurl="+u+"\n"+content+"\n"))
	return site
}

func (e *restoreEnv) snapshot(t *testing.T, siteID string) backup.Snapshot {
	t.Helper()
	snap, err := e.engine.Backup(context.Background(), siteID, backup.Options{Kind: string(store.BackupManual)})
	if err != nil {
		t.Fatal(err)
	}
	return snap
}

func (e *restoreEnv) data(siteID string) string {
	d, _ := e.dbs.Data(siteID)
	return string(d)
}

// restore queues a restore and runs it, returning the job's detail.
func (e *restoreEnv) restore(t *testing.T, siteID string, req sites.Restore) store.JobDetail {
	t.Helper()
	site, job, err := e.svc.Restore(context.Background(), siteID, req)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if site.Status != store.SiteUpdating || job.Type != sites.RestoreJobType || job.LockKey != store.SiteLockKey(siteID) {
		t.Errorf("Restore returned site %s, job %+v", site.Status, job)
	}
	e.runJobs(t)
	return e.job(t, job.ID)
}

// engineOps lists the backup engine's Backup and Restore calls from index
// from on, as "Backup site kind" and "Restore source snap target".
func (e *restoreEnv) engineOps(from int) []string {
	var out []string
	for _, c := range e.engine.Calls()[from:] {
		switch c.Op {
		case "Backup":
			out = append(out, "Backup "+c.SiteID+" "+c.Options.Kind)
		case "Restore":
			out = append(out, "Restore "+c.SiteID+" "+string(c.Snapshot)+" "+c.TargetSiteID)
		}
	}
	return out
}

func (e *restoreEnv) preRestoreBackups(t *testing.T, siteID string) []store.BackupRecord {
	t.Helper()
	page, err := store.NewBackupRecords(e.db).List(context.Background(), store.BackupRecordFilter{SiteID: siteID})
	if err != nil {
		t.Fatal(err)
	}
	var out []store.BackupRecord
	for _, r := range page.Records {
		if r.Kind == store.BackupPreRestore {
			out = append(out, r)
		}
	}
	return out
}

func jobResult(t *testing.T, j store.Job) map[string]string {
	t.Helper()
	var m map[string]string
	if err := json.Unmarshal(j.Result, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestRestoreSameSite(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newRestoreEnv(t, db)
		site := e.runningSite(t, "Acme", "post=v1")
		snap := e.snapshot(t, site.ID)
		e.dbs.SetData(site.ID, []byte(strings.Replace(e.data(site.ID), "post=v1", "post=v2", 1)))
		from := len(e.engine.Calls())
		e.wpCalls(site.ID, "")
		purges := e.cache.Flushes(site.ID)

		d := e.restore(t, site.ID, sites.Restore{SnapshotID: string(snap.ID)})
		if d.Job.State != store.JobSucceeded {
			t.Fatalf("restore job: %s %s\n%s", d.Job.State, d.Job.Error, jobLog(d))
		}
		if got := e.data(site.ID); !strings.Contains(got, "post=v1") || strings.Contains(got, "post=v2") {
			t.Errorf("database after the restore = %q", got)
		}
		// A pre-restore backup, then the restore; nothing else.
		pre := e.preRestoreBackups(t, site.ID)
		if len(pre) != 1 || pre[0].Status != store.BackupSucceeded || pre[0].JobID != d.Job.ID {
			t.Fatalf("pre-restore backups = %+v", pre)
		}
		want := []string{"Backup " + site.ID + " pre_restore", "Restore " + site.ID + " " + string(snap.ID) + " " + site.ID}
		if got := e.engineOps(from); !slices.Equal(got, want) {
			t.Errorf("engine calls = %q, want %q", got, want)
		}
		got := e.site(t, site.ID)
		if got.Status != store.SiteRunning || got.StatusMessage != "" || got.Maintenance {
			t.Errorf("site after the restore: %s %q, maintenance %v", got.Status, got.StatusMessage, got.Maintenance)
		}
		if rt, _ := e.prov.Site(site.ID); rt.Maintenance {
			t.Error("the site's domains are still on the maintenance page")
		}
		var maint int
		for _, c := range e.prov.Calls() {
			if c.Op == "SetMaintenance" && c.SiteID == site.ID {
				maint++
			}
		}
		if maint != 2 {
			t.Errorf("SetMaintenance called %d times, want on and off", maint)
		}
		// Same URLs: the database is upgraded, nothing is rewritten.
		calls := e.wpCalls(site.ID, "")
		if !slices.Contains(calls, "core update-db") || slices.ContainsFunc(calls, func(c string) bool { return strings.HasPrefix(c, "search-replace") }) {
			t.Errorf("WP-CLI calls = %q", calls)
		}
		if e.cache.Flushes(site.ID) <= purges {
			t.Error("the object cache wasn't flushed")
		}
		res := jobResult(t, d.Job)
		if res["snapshot_id"] != string(snap.ID) || res["source_site_id"] != site.ID || res["pre_restore_backup_id"] != pre[0].ID {
			t.Errorf("job result = %v", res)
		}

		// Restoring again works the same way.
		if d := e.restore(t, site.ID, sites.Restore{SnapshotID: string(snap.ID)}); d.Job.State != store.JobSucceeded {
			t.Errorf("second restore: %s %s", d.Job.State, d.Job.Error)
		}
	})
}

func TestRestoreIntoAnotherSite(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newRestoreEnv(t, db)
		source := e.runningSite(t, "Acme", "post=from-acme")
		target := e.runningSite(t, "Acme Staging", "post=staging")
		snap := e.snapshot(t, source.ID)
		e.wpCalls(target.ID, "")

		d := e.restore(t, target.ID, sites.Restore{SnapshotID: string(snap.ID), SourceSiteID: source.ID})
		if d.Job.State != store.JobSucceeded {
			t.Fatalf("restore job: %s %s\n%s", d.Job.State, d.Job.Error, jobLog(d))
		}
		got := e.data(target.ID)
		wantURL := "https://" + target.PreviewDomain
		if !strings.Contains(got, "post=from-acme") || !strings.Contains(got, "home="+wantURL) ||
			!strings.Contains(got, "siteurl="+wantURL) || strings.Contains(got, source.PreviewDomain) {
			t.Errorf("target database = %q", got)
		}
		wantSR := []string{
			"search-replace //" + source.PreviewDomain + " //" + target.PreviewDomain + " --all-tables-with-prefix --skip-columns=guid --format=count",
			`search-replace \/\/` + source.PreviewDomain + ` \/\/` + target.PreviewDomain + " --all-tables-with-prefix --skip-columns=guid --format=count",
		}
		if sr := e.wpCalls(target.ID, "search-replace"); !slices.Equal(sr, wantSR) {
			t.Errorf("search-replace calls = %q\nwant %q", sr, wantSR)
		}
		// The pre-restore backup is of the target; the source is untouched.
		if pre := e.preRestoreBackups(t, target.ID); len(pre) != 1 {
			t.Errorf("target's pre-restore backups = %+v", pre)
		}
		if pre := e.preRestoreBackups(t, source.ID); len(pre) != 0 {
			t.Errorf("source's pre-restore backups = %+v", pre)
		}
		if !strings.Contains(e.data(source.ID), "home=https://"+source.PreviewDomain) {
			t.Errorf("source database changed: %q", e.data(source.ID))
		}
	})
}

func TestRestoreValidation(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newRestoreEnv(t, db)
		site := e.runningSite(t, "Acme", "post=v1")
		other := e.runningSite(t, "Acme Shop", "post=shop")
		snap := e.snapshot(t, site.ID)
		otherSnap := e.snapshot(t, other.ID)
		newer := e.snapshot(t, site.ID)
		e.engine.cores[newer.ID] = "8.0"

		// A site of another client group.
		g := &store.ClientGroup{Name: "Globex " + id.New()}
		if err := store.NewClientGroups(db).Create(ctx, g); err != nil {
			t.Fatal(err)
		}
		stranger, _, err := e.svc.Create(ctx, sites.NewSite{ClientGroupID: g.ID, Name: "Globex", AdminEmail: "x@example.com"})
		if err != nil {
			t.Fatal(err)
		}
		e.runJobs(t)
		strangerSnap := e.snapshot(t, stranger.ID)

		for _, tt := range []struct {
			name  string
			req   sites.Restore
			field string
		}{
			{"unknown snapshot", sites.Restore{SnapshotID: "snap-999"}, "/snapshot_id"},
			{"too short", sites.Restore{SnapshotID: "snap"}, "/snapshot_id"},
			{"another site's snapshot", sites.Restore{SnapshotID: string(otherSnap.ID)}, "/snapshot_id"},
			{"wrong source", sites.Restore{SnapshotID: string(snap.ID), SourceSiteID: other.ID}, "/snapshot_id"},
			{"unknown source", sites.Restore{SnapshotID: string(snap.ID), SourceSiteID: id.New()}, "/source_site_id"},
			{"invalid source", sites.Restore{SnapshotID: string(snap.ID), SourceSiteID: "../x"}, "/source_site_id"},
			{"another client group", sites.Restore{SnapshotID: string(strangerSnap.ID), SourceSiteID: stranger.ID}, "/source_site_id"},
			{"core too new", sites.Restore{SnapshotID: string(newer.ID)}, "/snapshot_id"},
		} {
			_, _, err := e.svc.Restore(ctx, site.ID, tt.req)
			var ve *sites.ValidationError
			if !errors.As(err, &ve) || len(ve.Fields) != 1 || ve.Fields[0].Field != tt.field {
				t.Errorf("%s: Restore = %v, want a validation error on %s", tt.name, err, tt.field)
			}
		}
		if _, _, err := e.svc.Restore(ctx, id.New(), sites.Restore{SnapshotID: string(snap.ID)}); !errors.Is(err, sites.ErrNotFound) {
			t.Errorf("Restore of a missing site = %v", err)
		}

		// A stopped site can't be restored: the job needs WP-CLI.
		if _, err := e.svc.Stop(ctx, other.ID, ""); err != nil {
			t.Fatal(err)
		}
		e.runJobs(t)
		if _, _, err := e.svc.Restore(ctx, other.ID, sites.Restore{SnapshotID: string(otherSnap.ID)}); !errors.Is(err, sites.ErrNotRunning) {
			t.Errorf("Restore of a stopped site = %v", err)
		}
		// Nothing changed and no job was queued.
		if got := e.site(t, site.ID); got.Status != store.SiteRunning || got.Maintenance {
			t.Errorf("site after refused restores: %s, maintenance %v", got.Status, got.Maintenance)
		}
		if n := e.count(t, "SELECT COUNT(*) FROM jobs WHERE type = ?", sites.RestoreJobType); n != 0 {
			t.Errorf("%d restore jobs queued", n)
		}

		// Busy: a restore is already queued.
		if _, _, err := e.svc.Restore(ctx, site.ID, sites.Restore{SnapshotID: string(snap.ID)}); err != nil {
			t.Fatal(err)
		}
		if _, _, err := e.svc.Restore(ctx, site.ID, sites.Restore{SnapshotID: string(snap.ID)}); !errors.Is(err, sites.ErrBusy) {
			t.Errorf("second Restore while one is queued = %v", err)
		}
	})
}

func TestRestoreCoreUpdate(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		for _, fail := range []bool{false, true} {
			e := newRestoreEnv(t, db)
			site := e.runningSite(t, "Acme", "post=v1")
			snap := e.snapshot(t, site.ID)
			e.engine.cores[snap.ID] = "7.1"
			// The site is on an older core than the snapshot.
			if _, err := store.NewSites(db).SetWPCoreVersion(ctx, site.ID, "6.9"); err != nil {
				t.Fatal(err)
			}
			e.dbs.SetData(site.ID, []byte(strings.Replace(e.data(site.ID), "post=v1", "post=v2", 1)))
			if fail {
				e.setFailWP(func(_ string, args []string) error {
					if slices.Equal(args, []string{"core", "update-db"}) {
						return &provisioner.ExecError{ExitCode: 255, Stderr: "PHP Fatal error"}
					}
					return nil
				})
			}

			d := e.restore(t, site.ID, sites.Restore{SnapshotID: string(snap.ID)})
			rt, _ := e.prov.Site(site.ID)
			got := e.site(t, site.ID)
			if !fail {
				if d.Job.State != store.JobSucceeded {
					t.Fatalf("restore job: %s %s\n%s", d.Job.State, d.Job.Error, jobLog(d))
				}
				// The oldest image with at least the snapshot's core.
				if got.WPCoreVersion != "7.1.2" || rt.Config.WPCoreVersion != "7.1.2" {
					t.Errorf("core after the restore: record %s, runtime %s; want 7.1.2", got.WPCoreVersion, rt.Config.WPCoreVersion)
				}
				continue
			}
			// The rollback restores the pre-restore data, then the core.
			if d.Job.State != store.JobFailed {
				t.Fatalf("restore job: %s %s\n%s", d.Job.State, d.Job.Error, jobLog(d))
			}
			if got.WPCoreVersion != "6.9" || rt.Config.WPCoreVersion != "6.9" {
				t.Errorf("core after the rollback: record %s, runtime %s; want 6.9", got.WPCoreVersion, rt.Config.WPCoreVersion)
			}
			if !strings.Contains(e.data(site.ID), "post=v2") {
				t.Errorf("database after the rollback = %q", e.data(site.ID))
			}
			if got.Status != store.SiteRunning || !got.Maintenance || !rt.Maintenance {
				t.Errorf("site after the rollback: %s, maintenance %v/%v", got.Status, got.Maintenance, rt.Maintenance)
			}
		}
	})
}

// TestRestoreRollback injects a failure at each step of the job and checks
// what the rollback leaves: before the restore, the site as it was; after,
// its pre-restore data, in maintenance mode.
func TestRestoreRollback(t *testing.T) {
	transient := errors.New("connection reset by peer")
	tests := []struct {
		name string
		// inject sets up the failure.
		inject func(e *restoreEnv, siteID string)
		// touched is whether the site's data was restored (and so rolled
		// back, and left in maintenance mode).
		touched bool
		msg     string
	}{
		{
			name: "pre-restore backup",
			inject: func(e *restoreEnv, _ string) {
				e.engine.Err = func(op, _ string) error {
					if op == "Backup" {
						return transient
					}
					return nil
				}
			},
			msg: "restore failed before changing the site",
		},
		{
			name: "enable maintenance",
			inject: func(e *restoreEnv, _ string) {
				e.prov.Err = func(op, _ string) error {
					if op == "SetMaintenance" {
						return transient
					}
					return nil
				}
			},
			msg: "restore failed before changing the site",
		},
		{
			name: "restore",
			inject: func(e *restoreEnv, _ string) {
				e.engine.restoreErr = func(_ int, snap backup.SnapshotID) error {
					if snap == "snap-1" {
						return transient
					}
					return nil
				}
			},
			touched: true,
			msg:     "restore failed and the site was rolled back to its pre-restore backup",
		},
		{
			name: "upgrade database",
			inject: func(e *restoreEnv, _ string) {
				e.setFailWP(func(_ string, args []string) error {
					if args[0] == "core" && args[1] == "update-db" {
						return &provisioner.ExecError{ExitCode: 255, Stderr: "PHP Fatal error: boom\nStack trace:\n" + strings.Repeat("#0 frame\n", 1000)}
					}
					return nil
				})
			},
			touched: true,
			msg:     "restore failed and the site was rolled back",
		},
		{
			name: "rewrite URLs",
			inject: func(e *restoreEnv, _ string) {
				e.setFailWP(func(_ string, args []string) error {
					if args[0] == "option" && args[1] == "get" {
						return transient
					}
					return nil
				})
			},
			touched: true,
			msg:     "restore failed and the site was rolled back",
		},
		{
			name: "purge object cache",
			inject: func(e *restoreEnv, _ string) {
				var n int
				e.cache.Err = func(op, _ string) error {
					if op != "FlushCache" {
						return nil
					}
					// The steps before flush twice; fail from the purge on.
					if n++; n > 2 {
						return transient
					}
					return nil
				}
			},
			touched: true,
			msg:     "restore failed and the site was rolled back",
		},
		{
			name: "purge page cache",
			inject: func(e *restoreEnv, _ string) {
				e.prov.Err = func(op, _ string) error {
					if op == "PurgeCache" {
						return transient
					}
					return nil
				}
			},
			touched: true,
			msg:     "restore failed and the site was rolled back",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			storetest.Run(t, func(t *testing.T, db *store.DB) {
				e := newRestoreEnv(t, db)
				site := e.runningSite(t, "Acme", "post=v1")
				snap := e.snapshot(t, site.ID) // snap-1
				e.dbs.SetData(site.ID, []byte(strings.Replace(e.data(site.ID), "post=v1", "post=v2", 1)))
				from := len(e.engine.Calls())
				tt.inject(e, site.ID)

				d := e.restore(t, site.ID, sites.Restore{SnapshotID: string(snap.ID)})
				if d.Job.State != store.JobFailed {
					t.Fatalf("restore job: %s %s\n%s", d.Job.State, d.Job.Error, jobLog(d))
				}
				// WP-CLI's stack traces stay out of the job's error.
				if len(d.Job.Error) > 1000 || strings.Contains(d.Job.Error, "Stack trace") {
					t.Errorf("job error (%d bytes) = %.300q", len(d.Job.Error), d.Job.Error)
				}
				// The site's data is as it was before the restore.
				if got := e.data(site.ID); !strings.Contains(got, "post=v2") {
					t.Errorf("database after the rollback = %q", got)
				}
				got := e.site(t, site.ID)
				rt, _ := e.prov.Site(site.ID)
				if got.Status != store.SiteRunning || !strings.Contains(got.StatusMessage, tt.msg) ||
					!strings.Contains(got.StatusMessage, d.Job.Error) {
					t.Errorf("site after the rollback: %s %q; want running, %q", got.Status, got.StatusMessage, tt.msg)
				}
				if got.Maintenance != tt.touched {
					t.Errorf("maintenance flag after the rollback = %v, want %v", got.Maintenance, tt.touched)
				}
				if tt.name != "enable maintenance" && rt.Maintenance != tt.touched {
					t.Errorf("maintenance page after the rollback = %v, want %v", rt.Maintenance, tt.touched)
				}
				ops := e.engineOps(from)
				pre := e.preRestoreBackups(t, site.ID)
				if tt.name == "pre-restore backup" {
					if len(pre) != 1 || pre[0].Status != store.BackupFailed || slices.ContainsFunc(ops, func(o string) bool { return strings.HasPrefix(o, "Restore") }) {
						t.Errorf("pre-restore backups %+v, engine calls %q", pre, ops)
					}
					return
				}
				if len(pre) != 1 || pre[0].Status != store.BackupSucceeded {
					t.Fatalf("pre-restore backups = %+v", pre)
				}
				rollback := "Restore " + site.ID + " " + pre[0].SnapshotID + " " + site.ID
				if slices.Contains(ops, rollback) != tt.touched {
					t.Errorf("engine calls = %q; rollback %q expected: %v", ops, rollback, tt.touched)
				}
				if n := len(slices.DeleteFunc(slices.Clone(ops), func(o string) bool { return !strings.HasPrefix(o, "Backup") })); n != 1 {
					t.Errorf("%d backups taken, want one pre-restore backup: %q", n, ops)
				}
			})
		})
	}
}

// TestRestoreRollbackFails checks a restore whose rollback can't restore
// the pre-restore snapshot either: the site is marked failed, stays in
// maintenance mode, and its status message names the pre-restore backup.
func TestRestoreRollbackFails(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newRestoreEnv(t, db)
		site := e.runningSite(t, "Acme", "post=v1")
		snap := e.snapshot(t, site.ID)
		e.setFailWP(func(_ string, args []string) error {
			if args[0] == "core" && args[1] == "update-db" {
				return &provisioner.ExecError{ExitCode: 255, Stderr: "PHP Fatal error"}
			}
			return nil
		})
		e.engine.restoreErr = func(n int, _ backup.SnapshotID) error {
			if n > 1 {
				return errors.New("repository unreachable")
			}
			return nil
		}
		d := e.restore(t, site.ID, sites.Restore{SnapshotID: string(snap.ID)})
		// The rollback gave up on restoring the data but finished, so the
		// job failed rather than rollback_failed and the site is marked.
		if d.Job.State != store.JobFailed {
			t.Fatalf("restore job: %s %s\n%s", d.Job.State, d.Job.Error, jobLog(d))
		}
		pre := e.preRestoreBackups(t, site.ID)
		got := e.site(t, site.ID)
		if len(pre) != 1 || got.Status != store.SiteFailed || !got.Maintenance ||
			!strings.Contains(got.StatusMessage, "rolling back to the pre-restore backup failed too") ||
			!strings.Contains(got.StatusMessage, pre[0].ID) {
			t.Errorf("site after a failed rollback: %s %q, maintenance %v; pre-restore backups %+v", got.Status, got.StatusMessage, got.Maintenance, pre)
		}
	})
}

// TestRestoreAfterCompletion checks that a job failing after the restore
// finished (turning maintenance mode off) keeps the restored site.
func TestRestoreAfterCompletion(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newRestoreEnv(t, db)
		site := e.runningSite(t, "Acme", "post=v1")
		snap := e.snapshot(t, site.ID)
		e.dbs.SetData(site.ID, []byte(strings.Replace(e.data(site.ID), "post=v1", "post=v2", 1)))
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
		if d.Job.State != store.JobFailed {
			t.Fatalf("restore job: %s %s", d.Job.State, d.Job.Error)
		}
		if got := e.data(site.ID); !strings.Contains(got, "post=v1") {
			t.Errorf("database = %q, want the restored one", got)
		}
		got := e.site(t, site.ID)
		if got.Status != store.SiteRunning || !strings.Contains(got.StatusMessage, "the restore finished") {
			t.Errorf("site: %s %q", got.Status, got.StatusMessage)
		}
	})
}

// TestRestoreRetries checks that retried steps don't repeat work: a
// transient failure of the restore and of WP-CLI are retried, and the job
// takes one pre-restore backup.
func TestRestoreRetries(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newRestoreEnv(t, db)
		site := e.runningSite(t, "Acme", "post=v1")
		snap := e.snapshot(t, site.ID)
		e.dbs.SetData(site.ID, []byte(strings.Replace(e.data(site.ID), "post=v1", "post=v2", 1)))
		from := len(e.engine.Calls())
		e.engine.restoreErr = func(n int, _ backup.SnapshotID) error {
			if n == 1 {
				return errors.New("repository locked")
			}
			return nil
		}
		var wpFails int
		e.setFailWP(func(_ string, args []string) error {
			if args[0] == "search-replace" || (args[0] == "option" && args[1] == "get" && wpFails < 1) {
				wpFails++
				return errors.New("docker exec: connection reset")
			}
			return nil
		})
		d := e.restore(t, site.ID, sites.Restore{SnapshotID: string(snap.ID)})
		if d.Job.State != store.JobSucceeded {
			t.Fatalf("restore job: %s %s\n%s", d.Job.State, d.Job.Error, jobLog(d))
		}
		if d.Job.Attempts < 3 {
			t.Errorf("attempts = %d, want the retries", d.Job.Attempts)
		}
		ops := e.engineOps(from)
		want := []string{
			"Backup " + site.ID + " pre_restore",
			"Restore " + site.ID + " " + string(snap.ID) + " " + site.ID,
			"Restore " + site.ID + " " + string(snap.ID) + " " + site.ID,
		}
		if !slices.Equal(ops, want) {
			t.Errorf("engine calls = %q, want %q", ops, want)
		}
		if pre := e.preRestoreBackups(t, site.ID); len(pre) != 1 || pre[0].Status != store.BackupSucceeded {
			t.Errorf("pre-restore backups = %+v", pre)
		}
		if got := e.data(site.ID); !strings.Contains(got, "post=v1") {
			t.Errorf("database = %q", got)
		}
	})
}

func TestRestoreWithoutBackups(t *testing.T) {
	e := newEnv(t, storetest.Open(t, store.SQLite))
	if _, _, err := e.svc.Restore(context.Background(), id.New(), sites.Restore{SnapshotID: "abcdef012345"}); !errors.Is(err, sites.ErrNoBackups) {
		t.Errorf("Restore without a backup engine = %v", err)
	}
	for _, h := range e.svc.Handlers() {
		if h.Type == sites.RestoreJobType {
			t.Error("the restore job is registered without a backup engine")
		}
	}
}
