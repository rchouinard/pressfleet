package repomaint_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/backup"
	"github.com/rchouinard/pressfleet/control-plane/internal/backup/backuptest"
	"github.com/rchouinard/pressfleet/control-plane/internal/backup/repomaint"
	"github.com/rchouinard/pressfleet/control-plane/internal/backup/repository"
	"github.com/rchouinard/pressfleet/control-plane/internal/backup/sitebackup"
	"github.com/rchouinard/pressfleet/control-plane/internal/config"
	"github.com/rchouinard/pressfleet/control-plane/internal/jobs"
	"github.com/rchouinard/pressfleet/control-plane/internal/secrets"
	"github.com/rchouinard/pressfleet/control-plane/internal/secrets/secretstest"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
)

type env struct {
	db      *store.DB
	engine  *backuptest.Fake
	backups *sitebackup.Service
	svc     *repomaint.Service
	pool    *jobs.Pool
	group   string
}

func newEnv(t *testing.T, db *store.DB) *env {
	t.Helper()
	e := &env{db: db, engine: backuptest.New()}
	var err error
	e.backups, err = sitebackup.New(sitebackup.Options{DB: db, Engine: e.engine, Logger: storetest.Logger()})
	if err != nil {
		t.Fatal(err)
	}
	e.svc, err = repomaint.New(repomaint.Options{DB: db, Engine: e.engine, Logger: storetest.Logger()})
	if err != nil {
		t.Fatal(err)
	}
	e.pool = jobs.NewPool(db, jobs.Options{Logger: storetest.Logger(), Backoff: func(int) time.Duration { return 0 }})
	for _, h := range append(e.svc.Handlers(), e.backups.Handler()) {
		if err := e.pool.Register(h); err != nil {
			t.Fatal(err)
		}
	}
	e.group = newGroup(t, db, "Acme")
	return e
}

func newGroup(t *testing.T, db *store.DB, name string) string {
	t.Helper()
	g := store.ClientGroup{Name: name}
	if err := store.NewClientGroups(db).Create(context.Background(), &g); err != nil {
		t.Fatal(err)
	}
	return g.ID
}

func (e *env) site(t *testing.T, slug string) store.Site {
	t.Helper()
	ctx := context.Background()
	uid, err := store.AllocateSiteUID(ctx, e.db)
	if err != nil {
		t.Fatal(err)
	}
	s := store.Site{
		ClientGroupID: e.group, Name: slug, Slug: slug, Status: store.SiteRunning, PreviewDomain: slug + ".preview.test",
		PHPVersion: "8.4", WPCoreVersion: "6.9.1", UID: uid, WPSaltsSecretRef: "salts",
	}
	if err := store.NewSites(e.db).Create(ctx, &s); err != nil {
		t.Fatal(err)
	}
	e.engine.SetGroup(s.ID, e.group)
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

func (e *env) enqueue(t *testing.T, jobType string) store.Job {
	t.Helper()
	j, err := repomaint.Enqueue(context.Background(), e.db, jobType, e.group, "")
	if err != nil {
		t.Fatal(err)
	}
	return j
}

// backUp takes a manual backup of the site through its job.
func (e *env) backUp(t *testing.T, siteID string) store.BackupRecord {
	t.Helper()
	job, err := e.backups.Backup(context.Background(), siteID, "")
	if err != nil {
		t.Fatal(err)
	}
	e.runJobs(t)
	var backupID string
	if found, err := jobResult(e.job(t, job.ID), "backup_id", &backupID); err != nil || !found {
		t.Fatalf("backup job result: %v, %v", found, err)
	}
	return e.record(t, backupID)
}

func (e *env) record(t *testing.T, backupID string) store.BackupRecord {
	t.Helper()
	rec, err := store.NewBackupRecords(e.db).Get(context.Background(), backupID)
	if err != nil {
		t.Fatal(err)
	}
	return rec
}

func jobResult(j store.Job, key string, v *string) (bool, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(j.Result, &m); err != nil {
		return false, err
	}
	raw, ok := m[key]
	if !ok {
		return false, nil
	}
	return true, json.Unmarshal(raw, v)
}

// result returns the job's result, with each value as JSON text.
func result(t *testing.T, j store.Job) map[string]string {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(j.Result, &m); err != nil {
		t.Fatalf("job result %s: %v", j.Result, err)
	}
	out := map[string]string{}
	for k, v := range m {
		out[k] = string(v)
	}
	return out
}

func TestPruneJob(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		e.engine.KeepLast = 1
		site, other := e.site(t, "acme"), e.site(t, "acme-shop")
		first, second, last := e.backUp(t, site.ID), e.backUp(t, site.ID), e.backUp(t, site.ID)
		otherRec := e.backUp(t, other.ID)

		job := e.enqueue(t, repomaint.PruneJobType)
		if job.LockKey != "repo:"+e.group || job.TargetType != store.JobTargetClientGroup || job.TargetID != e.group {
			t.Errorf("job = %+v", job)
		}
		e.runJobs(t)
		job = e.job(t, job.ID)
		if job.State != store.JobSucceeded {
			t.Fatalf("prune job = %s: %s", job.State, job.Error)
		}
		if result(t, job)["forgotten_snapshots"] != "2" || result(t, job)["expired_backups"] != "2" {
			t.Errorf("result = %s", job.Result)
		}
		for _, tt := range []struct {
			rec     store.BackupRecord
			expired bool
		}{{first, true}, {second, true}, {last, false}, {otherRec, false}} {
			if got := e.record(t, tt.rec.ID); got.ExpiredAt.IsZero() == tt.expired {
				t.Errorf("backup %s (snapshot %s) expired at %v, want expired %v", got.ID, got.SnapshotID, got.ExpiredAt, tt.expired)
			}
		}

		// Again: nothing more to remove or expire.
		job = e.enqueue(t, repomaint.PruneJobType)
		e.runJobs(t)
		if job = e.job(t, job.ID); job.State != store.JobSucceeded || result(t, job)["expired_backups"] != "0" {
			t.Errorf("second prune = %s %s: %s", job.State, job.Result, job.Error)
		}
	})
}

func TestPruneJobRetriesUntilRecordsExpire(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		e.engine.KeepLast = 1
		site := e.site(t, "acme")
		first, _ := e.backUp(t, site.ID), e.backUp(t, site.ID)

		// Listing the repository fails once, after the prune removed the
		// snapshot: the retry finds the record by comparing with the
		// repository, although the prune removes nothing more.
		failures := 1
		e.engine.Err = func(op, _ string) error {
			if op == "GroupSnapshots" && failures > 0 {
				failures--
				return errors.New("listing failed")
			}
			return nil
		}
		job := e.enqueue(t, repomaint.PruneJobType)
		e.runJobs(t)
		if job = e.job(t, job.ID); job.State != store.JobSucceeded || job.Attempts != 2 {
			t.Fatalf("prune job = %s after %d attempts: %s", job.State, job.Attempts, job.Error)
		}
		if e.record(t, first.ID).ExpiredAt.IsZero() {
			t.Error("the removed snapshot's backup isn't expired")
		}
	})
}

func TestCheckJob(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		job := e.enqueue(t, repomaint.CheckJobType)
		e.runJobs(t)
		if job = e.job(t, job.ID); job.State != store.JobSucceeded {
			t.Errorf("check job = %s: %s", job.State, job.Error)
		}

		// Damage fails the job at once; other errors are retried.
		e.engine.Err = func(op, _ string) error {
			if op == "Check" {
				return fmt.Errorf("%w: pack 1234 is damaged", backup.ErrRepositoryDamaged)
			}
			return nil
		}
		job = e.enqueue(t, repomaint.CheckJobType)
		e.runJobs(t)
		if job = e.job(t, job.ID); job.State != store.JobFailed || job.Attempts != 1 || !strings.Contains(job.Error, "pack 1234") {
			t.Errorf("check of a damaged repository = %s after %d attempts: %s", job.State, job.Attempts, job.Error)
		}
		failures := 1
		e.engine.Err = func(op, _ string) error {
			if op == "Check" && failures > 0 {
				failures--
				return errors.New("connection reset")
			}
			return nil
		}
		job = e.enqueue(t, repomaint.CheckJobType)
		e.runJobs(t)
		if job = e.job(t, job.ID); job.State != store.JobSucceeded || job.Attempts != 2 {
			t.Errorf("check after a transient error = %s after %d attempts: %s", job.State, job.Attempts, job.Error)
		}
	})
}

func TestJobsWithoutRepository(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		if err := repomaint.EnsureSchedules(ctx, db, e.group, repomaint.Timing{}); err != nil {
			t.Fatal(err)
		}
		e.engine.Err = func(string, string) error { return fmt.Errorf("%w (deleted)", backup.ErrNoRepository) }
		for _, typ := range []string{repomaint.PruneJobType, repomaint.CheckJobType} {
			job := e.enqueue(t, typ)
			e.runJobs(t)
			if job = e.job(t, job.ID); job.State != store.JobSucceeded {
				t.Errorf("%s without a repository = %s: %s", typ, job.State, job.Error)
			}
		}
		// The schedules went away.
		for _, name := range []string{repomaint.PruneScheduleName(e.group), repomaint.CheckScheduleName(e.group)} {
			if _, err := store.NewSchedules(db).Get(ctx, name); !errors.Is(err, store.ErrNotFound) {
				t.Errorf("schedule %s: %v, want ErrNotFound", name, err)
			}
		}
		if _, err := repomaint.Enqueue(ctx, db, "backup.nothing", e.group, ""); err == nil {
			t.Error("Enqueue accepted an unknown job type")
		}
	})
}

func TestSchedules(t *testing.T) {
	ctx := context.Background()
	restic, err := exec.LookPath("restic")
	if err != nil {
		t.Fatal("restic is not on PATH; run the tests with `mise exec -- make test` after `mise install`")
	}
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		timing := repomaint.Timing{
			Window:        sitebackup.Window{Start: 22 * time.Hour, Length: 4 * time.Hour},
			CheckInterval: 3 * 24 * time.Hour,
		}
		repos, err := repository.New(repository.Options{
			DB: db, Secrets: secrets.NewStore(db, secretstest.Keyring(t, 1)), Layout: config.Layout{Root: t.TempDir()},
			Restic: restic, Created: repomaint.OnRepositoryCreated(timing), Logger: storetest.Logger(),
		})
		if err != nil {
			t.Fatal(err)
		}
		acme, globex := newGroup(t, db, "Acme"), newGroup(t, db, "Globex")

		// A new repository gets its schedules with it.
		if _, err := repos.Ensure(ctx, acme); err != nil {
			t.Fatal(err)
		}
		schedules := store.NewSchedules(db)
		prune, err := schedules.Get(ctx, repomaint.PruneScheduleName(acme))
		if err != nil {
			t.Fatal(err)
		}
		check, err := schedules.Get(ctx, repomaint.CheckScheduleName(acme))
		if err != nil {
			t.Fatal(err)
		}
		// The window closes at 02:00 UTC; maintenance starts within two
		// hours of it, and checks on one of the interval's days.
		if prune.JobType != repomaint.PruneJobType || prune.TargetID != acme || prune.LockKey != "repo:"+acme ||
			prune.Interval != 24*time.Hour || prune.Offset != 2*time.Hour || prune.Stagger != repomaint.MaxSpread ||
			prune.MissedRunPolicy != store.MissedCoalesce {
			t.Errorf("prune schedule = %+v", prune)
		}
		if check.JobType != repomaint.CheckJobType || check.LockKey != prune.LockKey || check.Interval != timing.CheckInterval ||
			check.Offset%(24*time.Hour) != 2*time.Hour || check.Offset >= timing.CheckInterval || check.Stagger != repomaint.MaxSpread {
			t.Errorf("check schedule = %+v", check)
		}
		for _, sch := range []store.Schedule{prune, check} {
			at := sch.NextRunAt.UTC()
			if since := at.Sub(at.Truncate(24 * time.Hour)); since < 2*time.Hour || since >= 4*time.Hour {
				t.Errorf("%s next runs at %s, not between 02:00 and 04:00 UTC", sch.Name, at)
			}
		}

		// Reconciling keeps them, adds missing ones, applies a new timing,
		// and removes those of groups without a repository.
		if err := repomaint.RemoveSchedules(ctx, db, acme); err != nil {
			t.Fatal(err)
		}
		if err := repomaint.EnsureSchedules(ctx, db, globex, timing); err != nil {
			t.Fatal(err)
		}
		timing.Window.Start = 0
		if err := repomaint.ReconcileSchedules(ctx, db, timing); err != nil {
			t.Fatal(err)
		}
		if prune, err = schedules.Get(ctx, repomaint.PruneScheduleName(acme)); err != nil || prune.Offset != 4*time.Hour {
			t.Errorf("reconciled prune schedule = %+v, %v", prune, err)
		}
		if _, err := schedules.Get(ctx, repomaint.CheckScheduleName(acme)); err != nil {
			t.Errorf("reconciled check schedule: %v", err)
		}
		for _, name := range []string{repomaint.PruneScheduleName(globex), repomaint.CheckScheduleName(globex)} {
			if _, err := schedules.Get(ctx, name); !errors.Is(err, store.ErrNotFound) {
				t.Errorf("schedule %s of a group without a repository: %v", name, err)
			}
		}
	})
}

func TestScheduleSpread(t *testing.T) {
	ctx := context.Background()
	db := storetest.Open(t, store.SQLite)
	// A window covering the whole day leaves no gap: maintenance still
	// spreads over MaxSpread. A window leaving one hour spreads over it.
	for _, tt := range []struct {
		length, spread time.Duration
	}{{24 * time.Hour, repomaint.MaxSpread}, {23 * time.Hour, time.Hour}, {time.Hour, repomaint.MaxSpread}} {
		group := newGroup(t, db, "g"+tt.length.String())
		timing := repomaint.Timing{Window: sitebackup.Window{Start: 2 * time.Hour, Length: tt.length}}
		if err := repomaint.EnsureSchedules(ctx, db, group, timing); err != nil {
			t.Fatal(err)
		}
		sch, err := store.NewSchedules(db).Get(ctx, repomaint.PruneScheduleName(group))
		if err != nil || sch.Stagger != tt.spread {
			t.Errorf("window of %s: stagger %s, %v; want %s", tt.length, sch.Stagger, err, tt.spread)
		}
	}
	if _, err := repomaint.New(repomaint.Options{DB: db, Engine: backuptest.New(), Timing: repomaint.Timing{CheckInterval: 36 * time.Hour}}); err == nil {
		t.Error("New accepted a check interval of a day and a half")
	}
}
