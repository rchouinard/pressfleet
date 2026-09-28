package sitebackup_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

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

type env struct {
	db     *store.DB
	engine *backuptest.Fake
	svc    *sitebackup.Service
	pool   *jobs.Pool
	group  string
}

func newEnv(t *testing.T, db *store.DB) *env {
	t.Helper()
	e := &env{db: db, engine: backuptest.New()}
	var err error
	e.svc, err = sitebackup.New(sitebackup.Options{DB: db, Engine: e.engine, Logger: storetest.Logger()})
	if err != nil {
		t.Fatal(err)
	}
	e.pool = jobs.NewPool(db, jobs.Options{Logger: storetest.Logger(), Backoff: func(int) time.Duration { return 0 }})
	if err := e.pool.Register(e.svc.Handler()); err != nil {
		t.Fatal(err)
	}
	g := store.ClientGroup{Name: "Acme"}
	if err := store.NewClientGroups(db).Create(context.Background(), &g); err != nil {
		t.Fatal(err)
	}
	e.group = g.ID
	return e
}

func (e *env) site(t *testing.T, slug string, status store.SiteStatus) store.Site {
	t.Helper()
	ctx := context.Background()
	uid, err := store.AllocateSiteUID(ctx, e.db)
	if err != nil {
		t.Fatal(err)
	}
	s := store.Site{
		ClientGroupID: e.group, Name: slug, Slug: slug, Status: status, PreviewDomain: slug + ".preview.test",
		PHPVersion: "8.4", WPCoreVersion: "6.9.1", UID: uid, WPSaltsSecretRef: "salts",
	}
	if err := store.NewSites(e.db).Create(ctx, &s); err != nil {
		t.Fatal(err)
	}
	return s
}

func (e *env) runJobs(t *testing.T) {
	t.Helper()
	for range 100 {
		ran, err := e.pool.RunNext(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if !ran {
			return
		}
	}
	t.Fatal("jobs never finished")
}

func (e *env) job(t *testing.T, jobID string) store.Job {
	t.Helper()
	j, err := store.NewJobs(e.db).Get(context.Background(), jobID)
	if err != nil {
		t.Fatal(err)
	}
	return j
}

func (e *env) records(t *testing.T, siteID string) []store.BackupRecord {
	t.Helper()
	page, err := store.NewBackupRecords(e.db).List(context.Background(), store.BackupRecordFilter{SiteID: siteID})
	if err != nil {
		t.Fatal(err)
	}
	return page.Records
}

func (e *env) backups() []backuptest.Call {
	var calls []backuptest.Call
	for _, c := range e.engine.Calls() {
		if c.Op == "Backup" {
			calls = append(calls, c)
		}
	}
	return calls
}

func TestManualBackup(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		site := e.site(t, "acme", store.SiteRunning)
		stopped := e.site(t, "acme-stopped", store.SiteStopped)

		job, err := e.svc.Backup(ctx, site.ID, "")
		if err != nil {
			t.Fatal(err)
		}
		var payload sitebackup.JobPayload
		if err := json.Unmarshal(job.Payload, &payload); err != nil {
			t.Fatal(err)
		}
		if job.Type != sitebackup.JobType || job.TargetType != "site" || job.TargetID != site.ID ||
			job.LockKey != sites.LockKey(site.ID) || payload.Kind != store.BackupManual || job.State != store.JobQueued {
			t.Errorf("job = %+v, payload %+v", job, payload)
		}
		if _, err := e.svc.Backup(ctx, stopped.ID, ""); err != nil {
			t.Errorf("backing up a stopped site: %v", err)
		}
		e.runJobs(t)

		if got := e.job(t, job.ID); got.State != store.JobSucceeded {
			t.Fatalf("job %s: %s", got.State, got.Error)
		}
		recs := e.records(t, site.ID)
		if len(recs) != 1 {
			t.Fatalf("records = %+v", recs)
		}
		rec := recs[0]
		if rec.Status != store.BackupSucceeded || rec.Kind != store.BackupManual || rec.SnapshotID == "" || rec.ClientGroupID != e.group ||
			rec.WPCoreVersion != "6.9.1" || rec.SizeBytes != 1024 || rec.AddedBytes != 512 || rec.FileCount != 3 || rec.JobID != job.ID ||
			rec.FinishedAt.IsZero() || rec.Message != "" {
			t.Errorf("record = %+v", rec)
		}
		calls := e.backups()
		if len(calls) != 2 || calls[0].SiteID != site.ID || calls[0].Options != (backup.Options{Kind: "manual", BackupID: rec.ID}) {
			t.Errorf("engine calls = %+v", calls)
		}
		var result map[string]string
		if err := json.Unmarshal(e.job(t, job.ID).Result, &result); err != nil {
			t.Fatal(err)
		}
		if result["site_id"] != site.ID || result["backup_id"] != rec.ID || result["snapshot_id"] != rec.SnapshotID {
			t.Errorf("job result = %v", result)
		}

		page, err := e.svc.List(ctx, store.BackupRecordFilter{SiteID: site.ID})
		if err != nil || len(page.Records) != 1 || page.Records[0].ID != rec.ID {
			t.Errorf("List = %+v, %v", page, err)
		}
		if _, err := e.svc.List(ctx, store.BackupRecordFilter{SiteID: id.New()}); !errors.Is(err, sitebackup.ErrSiteNotFound) {
			t.Errorf("List of a missing site: %v", err)
		}
	})
}

func TestBackupRefused(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		if _, err := e.svc.Backup(ctx, id.New(), ""); !errors.Is(err, sitebackup.ErrSiteNotFound) {
			t.Errorf("missing site: %v", err)
		}
		for _, status := range []store.SiteStatus{store.SiteProvisioning, store.SiteDeleting} {
			s := e.site(t, "site-"+string(status), status)
			if _, err := e.svc.Backup(ctx, s.ID, ""); !errors.Is(err, sitebackup.ErrNotBackupable) {
				t.Errorf("%s site: %v", status, err)
			}
		}
		failed := e.site(t, "never", store.SiteFailed)
		if _, err := db.ExecContext(ctx, "UPDATE sites SET wp_salts_secret_ref = '' WHERE id = ?", failed.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := e.svc.Backup(ctx, failed.ID, ""); !errors.Is(err, sitebackup.ErrNotBackupable) {
			t.Errorf("never provisioned site: %v", err)
		}
		// Updating sites can be backed up: the job waits for the update.
		updating := e.site(t, "updating", store.SiteUpdating)
		if _, err := e.svc.Backup(ctx, updating.ID, ""); err != nil {
			t.Errorf("updating site: %v", err)
		}
		if _, err := sitebackup.Enqueue(ctx, db, updating.ID, "hourly", ""); err == nil {
			t.Error("Enqueue accepted an unknown kind")
		}
	})
}

func TestScheduledBackupRetriesAndSucceeds(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		site := e.site(t, "acme", store.SiteRunning)
		var mu sync.Mutex
		failures := 1
		e.engine.Err = func(op, _ string) error {
			mu.Lock()
			defer mu.Unlock()
			if op == "Backup" && failures > 0 {
				failures--
				return errors.New("repository is locked")
			}
			return nil
		}
		job, err := sitebackup.Enqueue(ctx, db, site.ID, store.BackupScheduled, "")
		if err != nil {
			t.Fatal(err)
		}
		e.runJobs(t)
		if got := e.job(t, job.ID); got.State != store.JobSucceeded || got.Attempts != 2 {
			t.Fatalf("job = %s after %d attempts: %s", got.State, got.Attempts, got.Error)
		}
		recs := e.records(t, site.ID)
		if len(recs) != 1 || recs[0].Status != store.BackupSucceeded || recs[0].Kind != store.BackupScheduled {
			t.Fatalf("records = %+v", recs)
		}
		// Both attempts used the one record's ID, so the engine could find
		// the first attempt's snapshot had it existed.
		calls := e.backups()
		if len(calls) != 2 || calls[0].Options.BackupID != recs[0].ID || calls[1].Options.BackupID != recs[0].ID {
			t.Errorf("engine calls = %+v", calls)
		}
	})
}

func TestFailedBackup(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name      string
		err       error
		permanent bool
	}{
		{"transient", errors.New("restic backup failed (exit status 1): Fatal: unable to open repository"), false},
		{"site runtime gone", fmt.Errorf("back up site: %w", provisioner.ErrSiteNotFound), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			storetest.Run(t, func(t *testing.T, db *store.DB) {
				e := newEnv(t, db)
				site := e.site(t, "acme", store.SiteRunning)
				e.engine.Err = func(op, _ string) error {
					if op == "Backup" {
						return tt.err
					}
					return nil
				}
				job, err := e.svc.Backup(ctx, site.ID, "")
				if err != nil {
					t.Fatal(err)
				}
				e.runJobs(t)
				got := e.job(t, job.ID)
				if got.State != store.JobFailed {
					t.Fatalf("job = %s: %s", got.State, got.Error)
				}
				wantCalls := store.DefaultMaxAttempts
				if tt.permanent {
					wantCalls = 1
				}
				if n := len(e.backups()); n != wantCalls {
					t.Errorf("engine called %d times, want %d", n, wantCalls)
				}
				recs := e.records(t, site.ID)
				if len(recs) != 1 || recs[0].Status != store.BackupFailed || recs[0].SnapshotID != "" ||
					!strings.Contains(recs[0].Message, tt.err.Error()) || recs[0].FinishedAt.IsZero() {
					t.Errorf("records = %+v", recs)
				}
			})
		})
	}
}

func TestBackupOfDeletedSite(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		site := e.site(t, "acme", store.SiteRunning)
		job, err := sitebackup.Enqueue(ctx, db, site.ID, store.BackupManual, "")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.NewSites(db).Delete(ctx, site.ID); err != nil {
			t.Fatal(err)
		}
		e.runJobs(t)
		if got := e.job(t, job.ID); got.State != store.JobFailed || !strings.Contains(got.Error, "site not found") || got.Attempts != 1 {
			t.Errorf("job = %s after %d attempts: %s", got.State, got.Attempts, got.Error)
		}
		if recs := e.records(t, site.ID); len(recs) != 0 {
			t.Errorf("records = %+v", recs)
		}
	})
}

// A job that takes a backup as one of its steps, like a restore's
// pre-restore snapshot, and then fails: its backup stays.
func TestStepsInAnotherJob(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		var (
			seen    store.BackupRecord
			seenErr error
		)
		restore := jobs.Handler{Type: "test.restore", Steps: append(e.svc.Steps(store.BackupPreRestore),
			jobs.Step{Name: "restore", Do: func(ctx context.Context, r *jobs.Run) error {
				var found bool
				seen, found, seenErr = e.svc.JobBackup(ctx, r)
				if !found && seenErr == nil {
					seenErr = errors.New("no backup")
				}
				return jobs.Permanent(errors.New("restore failed"))
			}})}
		if err := e.pool.Register(restore); err != nil {
			t.Fatal(err)
		}

		site := e.site(t, "acme", store.SiteRunning)
		job, err := jobs.Enqueue(ctx, db, jobs.Spec{Type: "test.restore", TargetType: "site", TargetID: site.ID, LockKey: sitebackup.LockKey(site.ID)})
		if err != nil {
			t.Fatal(err)
		}
		e.runJobs(t)
		if got := e.job(t, job.ID); got.State != store.JobFailed {
			t.Fatalf("job = %s: %s", got.State, got.Error)
		}
		if seenErr != nil {
			t.Fatal(seenErr)
		}
		recs := e.records(t, site.ID)
		if len(recs) != 1 || recs[0].ID != seen.ID || recs[0].Status != store.BackupSucceeded || recs[0].Kind != store.BackupPreRestore ||
			recs[0].SnapshotID != seen.SnapshotID || seen.SnapshotID == "" {
			t.Errorf("records = %+v; the restore saw %+v", recs, seen)
		}
	})
}

func TestNewRequiresDependencies(t *testing.T) {
	if _, err := sitebackup.New(sitebackup.Options{}); err == nil {
		t.Error("New accepted no dependencies")
	}
}
