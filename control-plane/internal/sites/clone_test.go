package sites_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"

	"github.com/rchouinard/pressfleet/control-plane/internal/backup"
	"github.com/rchouinard/pressfleet/control-plane/internal/backup/backuptest"
	"github.com/rchouinard/pressfleet/control-plane/internal/backup/sitebackup"
	"github.com/rchouinard/pressfleet/control-plane/internal/jobs"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
	"github.com/rchouinard/pressfleet/control-plane/internal/secrets"
	"github.com/rchouinard/pressfleet/control-plane/internal/sitecron"
	"github.com/rchouinard/pressfleet/control-plane/internal/sites"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
)

// newCloneEnv is a restore env whose pool also runs site.backup jobs, which
// a clone waits for.
func newCloneEnv(t *testing.T, db *store.DB) *restoreEnv {
	t.Helper()
	var backups *sitebackup.Service
	e := newRestoreEnvWith(t, db, restoreManifest, func(o *sites.Options) { backups = o.Backups })
	if err := e.pool.Register(backups.Handler()); err != nil {
		t.Fatal(err)
	}
	return e
}

// productionSite provisions a running site whose content links to its own
// URL, as a real site's posts do.
func (e *restoreEnv) productionSite(t *testing.T, name string) store.Site {
	t.Helper()
	site := e.runningSite(t, name, "")
	u := "https://" + site.PreviewDomain
	e.dbs.SetData(site.ID, []byte(e.data(site.ID)+"post=hello\nlink="+u+"/about\njson="+strings.ReplaceAll(u, "/", `\/`)+"\n"))
	return site
}

func (e *restoreEnv) createStaging(t *testing.T, prodID string, req sites.NewStaging) sites.StagingCreated {
	t.Helper()
	out, err := e.svc.CreateStaging(context.Background(), prodID, req)
	if err != nil {
		t.Fatalf("CreateStaging: %v", err)
	}
	return out
}

func TestCreateStagingFromFreshBackup(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newCloneEnv(t, db)
		prod := e.productionSite(t, "Acme")
		prodData := e.data(prod.ID)
		from := len(e.engine.Calls())

		out := e.createStaging(t, prod.ID, sites.NewStaging{})
		stg := out.Site

		// The staging site as allocated.
		if stg.ParentSiteID != prod.ID || stg.ClientGroupID != prod.ClientGroupID || stg.Status != store.SiteProvisioning ||
			stg.Name != "Acme (staging)" || stg.Slug != "acme-staging" || stg.PreviewDomain != "acme-staging."+testPreviewBase {
			t.Errorf("staging site = %+v", stg)
		}
		if stg.PHPVersion != prod.PHPVersion || stg.WPCoreVersion != prod.WPCoreVersion || stg.TablePrefix != prod.TablePrefix ||
			stg.AdminUsername != prod.AdminUsername || stg.AdminEmail != prod.AdminEmail || stg.CPULimit != prod.CPULimit ||
			stg.MemoryReservationMB != prod.MemoryReservationMB || stg.MemoryLimitMB != prod.MemoryLimitMB {
			t.Errorf("staging site doesn't take production's settings: %+v", stg)
		}
		if stg.AdminPasswordSecretRef != "" || stg.UID == prod.UID || stg.WPSaltsSecretRef == "" || stg.WPSaltsSecretRef == prod.WPSaltsSecretRef ||
			!stg.MailDisabled || !stg.PreviewProtection || stg.PreviewUsername != stg.Slug {
			t.Errorf("staging site's own settings = %+v", stg)
		}
		if out.Preview.Username != stg.Slug || bcrypt.CompareHashAndPassword([]byte(stg.PreviewPasswordHash), []byte(out.Preview.Password.Reveal())) != nil {
			t.Error("the preview credentials don't match the site's hash")
		}
		if stored, err := e.sec.Get(ctx, secrets.Ref(stg.PreviewPasswordSecretRef)); err != nil || stored != out.Preview.Password {
			t.Errorf("stored preview password: %v", err)
		}
		if sched, err := store.NewSites(db).BackupSchedule(ctx, stg.ID); err != nil || sched.Enabled {
			t.Errorf("backup schedule = %+v, %v; want off", sched, err)
		}
		if n := e.count(t, "SELECT COUNT(*) FROM schedules WHERE name = ?", sitebackup.ScheduleName(stg.ID)); n != 0 {
			t.Errorf("%d backup schedules", n)
		}
		if n := e.count(t, "SELECT COUNT(*) FROM schedules WHERE name = ?", sitecron.ScheduleName(stg.ID)); n != 1 {
			t.Errorf("%d cron schedules, want 1", n)
		}

		// The chain: a sync backup of production under its lock key, and
		// the clone under the staging site's, after it.
		if out.BackupJob == nil || out.BackupJob.Type != sitebackup.JobType || out.BackupJob.TargetID != prod.ID ||
			out.BackupJob.LockKey != store.SiteLockKey(prod.ID) || !strings.Contains(string(out.BackupJob.Payload), `"sync"`) {
			t.Fatalf("backup job = %+v", out.BackupJob)
		}
		if out.Job.Type != sites.CloneJobType || out.Job.TargetID != stg.ID || out.Job.LockKey != store.SiteLockKey(stg.ID) ||
			out.Job.AfterJobID != out.BackupJob.ID {
			t.Errorf("clone job = %+v", out.Job)
		}

		e.runJobs(t)
		if d := e.job(t, out.BackupJob.ID); d.Job.State != store.JobSucceeded {
			t.Fatalf("backup job: %s %s", d.Job.State, d.Job.Error)
		}
		d := e.job(t, out.Job.ID)
		if d.Job.State != store.JobSucceeded {
			t.Fatalf("clone job: %s %s\n%s", d.Job.State, d.Job.Error, jobLog(d))
		}
		page, err := store.NewBackupRecords(db).List(ctx, store.BackupRecordFilter{SiteID: prod.ID})
		if err != nil || len(page.Records) != 1 || page.Records[0].Kind != store.BackupSync || page.Records[0].Status != store.BackupSucceeded {
			t.Fatalf("production's backups = %+v, %v", page.Records, err)
		}
		rec := page.Records[0]
		want := []string{"Backup " + prod.ID + " sync", "Restore " + prod.ID + " " + rec.SnapshotID + " " + stg.ID}
		if got := e.engineOps(from); !slices.Equal(got, want) {
			t.Errorf("engine calls = %q, want %q", got, want)
		}

		// The staging site has production's content at its own URLs.
		stgURL, prodURL := "https://"+stg.PreviewDomain, "https://"+prod.PreviewDomain
		got := e.data(stg.ID)
		wantData := strings.ReplaceAll(strings.ReplaceAll(prodData, prodURL, stgURL),
			strings.ReplaceAll(prodURL, "/", `\/`), strings.ReplaceAll(stgURL, "/", `\/`))
		if got != wantData || strings.Contains(got, "//"+prod.PreviewDomain) {
			t.Errorf("staging database = %q, want %q", got, wantData)
		}
		if e.data(prod.ID) != prodData {
			t.Errorf("production's database changed: %q", e.data(prod.ID))
		}

		// It runs with its own credentials, rendered as staging.
		final := e.site(t, stg.ID)
		if final.Status != store.SiteRunning || final.StagingSnapshotID != rec.SnapshotID || final.StagingRefreshedAt.IsZero() {
			t.Errorf("staging site after the clone = %+v", final)
		}
		rt, ok := e.prov.Site(stg.ID)
		if !ok || rt.Config.Environment != provisioner.EnvironmentStaging || !rt.Config.DisableMail || rt.Config.PreviewAuth == nil ||
			rt.Config.TablePrefix != prod.TablePrefix || rt.Config.UID != stg.UID {
			t.Errorf("rendered staging site = %+v", rt.Config)
		}
		if !e.cache.HasUser(stg.ID) || !e.cache.HasUser(prod.ID) {
			t.Error("the staging site has no object cache user of its own")
		}
		if e.wp.state(stg.ID).dropin == "" {
			t.Error("configure_plugins didn't enable the object cache drop-in")
		}
		if p := e.site(t, prod.ID); p.Status != store.SiteRunning || p.Maintenance || !p.UpdatedAt.Equal(prod.UpdatedAt) {
			t.Errorf("production site changed: %+v", p)
		}

		// The result names the staging site, the snapshot, and the
		// preview login, without the password itself.
		var res map[string]any
		if err := json.Unmarshal(d.Job.Result, &res); err != nil {
			t.Fatal(err)
		}
		if res["site_id"] != stg.ID || res["snapshot_id"] != rec.SnapshotID || res["backup_id"] != rec.ID ||
			res["source_site_id"] != prod.ID || res["preview_username"] != stg.Slug {
			t.Errorf("result = %v", res)
		}
		if pw, _ := res["preview_password"].(map[string]any); pw["secret_ref"] != stg.PreviewPasswordSecretRef {
			t.Errorf("preview_password = %v", res["preview_password"])
		}
		if strings.Contains(jobLog(d), out.Preview.Password.Reveal()) {
			t.Error("the job leaks the preview password")
		}
	})
}

func TestCreateStagingFromExistingSnapshot(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newCloneEnv(t, db)
		prod := e.productionSite(t, "Acme")
		snap := e.snapshot(t, prod.ID)
		// Production moves on; the clone gets the snapshot's content.
		e.dbs.SetData(prod.ID, []byte(strings.Replace(e.data(prod.ID), "post=hello", "post=later", 1)))
		from := len(e.engine.Calls())

		out := e.createStaging(t, prod.ID, sites.NewStaging{
			Name: "  QA  ", SnapshotID: string(snap.ID),
			MemoryLimitMB: ptr(1024),
		})
		if out.BackupJob != nil || out.Job.AfterJobID != "" {
			t.Errorf("a snapshot was given, but jobs = %+v, %+v", out.BackupJob, out.Job)
		}
		if out.Site.Name != "QA" || out.Site.MemoryLimitMB != 1024 || out.Site.CPULimit != prod.CPULimit {
			t.Errorf("staging site = %+v", out.Site)
		}
		e.runJobs(t)
		if d := e.job(t, out.Job.ID); d.Job.State != store.JobSucceeded {
			t.Fatalf("clone job: %s %s\n%s", d.Job.State, d.Job.Error, jobLog(d))
		}
		want := []string{"Restore " + prod.ID + " " + string(snap.ID) + " " + out.Site.ID}
		if got := e.engineOps(from); !slices.Equal(got, want) {
			t.Errorf("engine calls = %q, want %q", got, want)
		}
		if got := e.data(out.Site.ID); !strings.Contains(got, "post=hello") {
			t.Errorf("staging database = %q", got)
		}
		if got := e.site(t, out.Site.ID); got.Status != store.SiteRunning || got.StagingSnapshotID != string(snap.ID) {
			t.Errorf("staging site = %+v", got)
		}
	})
}

// requireRolledBack checks that a failed clone left the staging site
// failed, with nothing behind it, and production as it was.
func (e *restoreEnv) requireRolledBack(t *testing.T, out sites.StagingCreated, prod store.Site, prodData string) store.JobDetail {
	t.Helper()
	ctx := context.Background()
	d := e.job(t, out.Job.ID)
	if d.Job.State != store.JobFailed {
		t.Fatalf("clone job: %s %s\n%s", d.Job.State, d.Job.Error, jobLog(d))
	}
	stg := e.site(t, out.Site.ID)
	if stg.Status != store.SiteFailed || !strings.HasPrefix(stg.StatusMessage, "creating the staging site failed: ") {
		t.Errorf("staging site = %s %q", stg.Status, stg.StatusMessage)
	}
	if stg.WPSaltsSecretRef != "" || stg.PreviewPasswordSecretRef != "" {
		t.Errorf("staging site keeps secret references: %+v", stg)
	}
	for _, ref := range []string{out.Site.WPSaltsSecretRef, out.Site.PreviewPasswordSecretRef} {
		if _, err := e.sec.Get(ctx, secrets.Ref(ref)); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("secret %s: %v, want deleted", ref, err)
		}
	}
	if _, ok := e.prov.Site(stg.ID); ok {
		t.Error("the staging site's containers are left")
	}
	if _, ok := e.dbs.Data(stg.ID); ok {
		t.Error("the staging site's database is left")
	}
	if e.cache.HasUser(stg.ID) {
		t.Error("the staging site's object cache user is left")
	}
	if n := e.count(t, "SELECT COUNT(*) FROM schedules WHERE name LIKE ?", "%"+stg.ID+"%"); n != 0 {
		t.Errorf("%d schedules left", n)
	}
	if p := e.site(t, prod.ID); p.Status != store.SiteRunning || p.StatusMessage != "" {
		t.Errorf("production site = %s %q", p.Status, p.StatusMessage)
	}
	if e.data(prod.ID) != prodData {
		t.Errorf("production's database changed: %q", e.data(prod.ID))
	}
	if _, ok := e.prov.Site(prod.ID); !ok || !e.cache.HasUser(prod.ID) {
		t.Error("production's resources are gone")
	}
	return d
}

func TestCreateStagingFailedBackup(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newCloneEnv(t, db)
		prod := e.productionSite(t, "Acme")
		prodData := e.data(prod.ID)
		e.engine.Err = func(op, siteID string) error {
			if op == "Backup" && siteID == prod.ID {
				return backup.ErrSiteNotFound // permanent
			}
			return nil
		}
		out := e.createStaging(t, prod.ID, sites.NewStaging{})
		e.runJobs(t)

		if b := e.job(t, out.BackupJob.ID); b.Job.State != store.JobFailed {
			t.Fatalf("backup job: %s", b.Job.State)
		}
		d := e.requireRolledBack(t, out, prod, prodData)
		if !strings.Contains(d.Job.Error, jobs.ErrAwaitedJobFailed.Error()) {
			t.Errorf("clone job error = %q", d.Job.Error)
		}
		// Nothing was created: prepare failed before the database.
		for _, c := range e.dbs.Calls() {
			if c.SiteID == out.Site.ID && c.Op == "CreateDatabase" {
				t.Error("the clone created a database after the backup failed")
			}
		}
		if slices.ContainsFunc(e.engine.Calls(), func(c backuptest.Call) bool { return c.Op == "Restore" }) {
			t.Error("the clone restored something after the backup failed")
		}
	})
}

func TestCreateStagingRollsBack(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newCloneEnv(t, db)
		prod := e.productionSite(t, "Acme")
		prodData := e.data(prod.ID)
		out := e.createStaging(t, prod.ID, sites.NewStaging{})
		// WordPress can't load the copy: a permanent failure after the
		// stack started and the data was restored.
		e.setFailWP(func(siteID string, args []string) error {
			if siteID == out.Site.ID && slices.Equal(args, []string{"core", "update-db"}) {
				return &provisioner.ExecError{ExitCode: 255, Stderr: "PHP Fatal error: boom"}
			}
			return nil
		})
		e.runJobs(t)
		d := e.requireRolledBack(t, out, prod, prodData)
		if !strings.Contains(d.Job.Error, "boom") {
			t.Errorf("clone job error = %q", d.Job.Error)
		}
		// The sync backup is a backup like any other, and stays.
		if b := e.job(t, out.BackupJob.ID); b.Job.State != store.JobSucceeded {
			t.Errorf("backup job: %s", b.Job.State)
		}

		// The failed staging site can be deleted, and then production.
		ctx := context.Background()
		job, err := e.svc.Delete(ctx, out.Site.ID, "", sites.DeleteOptions{})
		if err != nil {
			t.Fatal(err)
		}
		e.runJobs(t)
		if d := e.job(t, job.ID); d.Job.State != store.JobSucceeded {
			t.Fatalf("delete job: %s %s", d.Job.State, d.Job.Error)
		}
		if _, err := e.svc.Delete(ctx, prod.ID, "", sites.DeleteOptions{}); err != nil {
			t.Errorf("deleting production: %v", err)
		}
	})
}

func TestCreateStagingRefused(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newCloneEnv(t, db)
		prod := e.productionSite(t, "Acme")
		other := e.productionSite(t, "Other")
		otherSnap := e.snapshot(t, other.ID)
		stg := e.stagingOf(t, e.productionSite(t, "Linked"), prod, true)
		sitesBefore := e.count(t, "SELECT COUNT(*) FROM sites")
		jobsBefore := e.count(t, "SELECT COUNT(*) FROM jobs")

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
			req    sites.NewStaging
			setup  func()
			want   func(error) bool
		}{
			{"a staging site as the parent", stg.ID, sites.NewStaging{}, nil, field("/site_id")},
			{"a missing site", "01j9zq3v6t8k4m2n5p7r9s1x3y", sites.NewStaging{}, nil, is(sites.ErrNotFound)},
			{"another site's snapshot", prod.ID, sites.NewStaging{SnapshotID: string(otherSnap.ID)}, nil, field("/snapshot_id")},
			{"a blank name", prod.ID, sites.NewStaging{Name: "   "}, nil, field("/name")},
			{"too little memory", prod.ID, sites.NewStaging{MemoryLimitMB: ptr(16)}, nil, field("/resources/memory_limit_mb")},
			{"a parent being deleted", prod.ID, sites.NewStaging{}, func() { e.setStatus(t, prod.ID, store.SiteDeleting) }, is(sites.ErrBusy)},
			{"a parent being updated", prod.ID, sites.NewStaging{}, func() { e.setStatus(t, prod.ID, store.SiteUpdating) }, is(sites.ErrBusy)},
			{"a failed parent", prod.ID, sites.NewStaging{}, func() { e.setStatus(t, prod.ID, store.SiteFailed) }, is(sites.ErrSourceFailed)},
		} {
			t.Run(tc.name, func(t *testing.T) {
				if tc.setup != nil {
					tc.setup()
					defer e.setStatus(t, prod.ID, store.SiteRunning)
				}
				_, err := e.svc.CreateStaging(ctx, tc.siteID, tc.req)
				if !tc.want(err) {
					t.Errorf("err = %v", err)
				}
			})
		}
		if n := e.count(t, "SELECT COUNT(*) FROM sites"); n != sitesBefore {
			t.Errorf("%d sites, want %d", n, sitesBefore)
		}
		if n := e.count(t, "SELECT COUNT(*) FROM jobs"); n != jobsBefore {
			t.Errorf("%d jobs, want %d", n, jobsBefore)
		}
	})
}

// TestCreateStagingAndDeleteParent covers the guard between creating a
// staging site and deleting its production site, in both orders.
func TestCreateStagingAndDeleteParent(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newCloneEnv(t, db)
		prod := e.productionSite(t, "Acme")
		out := e.createStaging(t, prod.ID, sites.NewStaging{})
		if _, err := e.svc.Delete(ctx, prod.ID, "", sites.DeleteOptions{}); !errors.Is(err, sites.ErrHasStagingSites) {
			t.Errorf("deleting production with a staging site being created: %v", err)
		}
		e.runJobs(t)
		if got := e.site(t, out.Site.ID); got.Status != store.SiteRunning {
			t.Fatalf("staging site = %s %q", got.Status, got.StatusMessage)
		}

		other := e.productionSite(t, "Other")
		if _, err := e.svc.Delete(ctx, other.ID, "", sites.DeleteOptions{}); err != nil {
			t.Fatal(err)
		}
		if _, err := e.svc.CreateStaging(ctx, other.ID, sites.NewStaging{}); !errors.Is(err, sites.ErrBusy) {
			t.Errorf("creating a staging site of a site being deleted: %v", err)
		}
		if children, err := store.NewSites(db).Children(ctx, other.ID); err != nil || len(children) != 0 {
			t.Errorf("staging sites of the deleted site = %v, %v", children, err)
		}
	})
}

func TestCreateStagingSlugs(t *testing.T) {
	// Slugify stops this name at 39 characters ("abc-" * 9 + "abc"); its
	// first 32 end in a hyphen, which the staging slug drops.
	hyphens := strings.Repeat("abc-", 10)
	for _, tc := range []struct {
		name          string
		wantProd      string
		wantStaging   []string
		wantStageName string
	}{
		{"Acme", "acme", []string{"acme-staging", "acme-staging-2", "acme-staging-3"}, "Acme (staging)"},
		{hyphens, strings.TrimSuffix(hyphens, "-"),
			[]string{strings.Repeat("abc-", 7) + "abc-staging", strings.Repeat("abc-", 7) + "abc-staging-2"},
			hyphens + " (staging)"},
		{strings.Repeat("x", 100), strings.Repeat("x", 40),
			[]string{strings.Repeat("x", 32) + "-staging"}, strings.Repeat("x", 90) + " (staging)"},
	} {
		t.Run(tc.wantProd[:4], func(t *testing.T) {
			storetest.Run(t, func(t *testing.T, db *store.DB) {
				e := newCloneEnv(t, db)
				prod := e.productionSite(t, tc.name)
				if !strings.HasPrefix(prod.Slug, tc.wantProd) {
					t.Fatalf("production slug = %q", prod.Slug)
				}
				for i, want := range tc.wantStaging {
					out := e.createStaging(t, prod.ID, sites.NewStaging{})
					if out.Site.Slug != want || len(out.Site.Slug) > 63 || out.Site.PreviewDomain != want+"."+testPreviewBase {
						t.Errorf("staging site %d: slug %q (%d characters), want %q", i+1, out.Site.Slug, len(out.Site.Slug), want)
					}
					if out.Site.Name != tc.wantStageName || len([]rune(out.Site.Name)) > 100 {
						t.Errorf("staging site %d: name %q, want %q", i+1, out.Site.Name, tc.wantStageName)
					}
				}
			})
		})
	}
}
