package offboard_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/backup/offboard"
	"github.com/rchouinard/pressfleet/control-plane/internal/backup/repomaint"
	"github.com/rchouinard/pressfleet/control-plane/internal/backup/repository"
	"github.com/rchouinard/pressfleet/control-plane/internal/backup/repository/repositorytest"
	"github.com/rchouinard/pressfleet/control-plane/internal/config"
	"github.com/rchouinard/pressfleet/control-plane/internal/export"
	"github.com/rchouinard/pressfleet/control-plane/internal/id"
	"github.com/rchouinard/pressfleet/control-plane/internal/jobs"
	"github.com/rchouinard/pressfleet/control-plane/internal/secrets"
	"github.com/rchouinard/pressfleet/control-plane/internal/secrets/secretstest"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
)

type env struct {
	db    *store.DB
	repos *repository.Manager
	svc   *offboard.Service
	pool  *jobs.Pool
}

// newEnv runs the real repository manager, with restic (pinned in
// mise.toml) on local repositories, so the job is shown to remove what
// `pressfleet repository delete` does.
func newEnv(t *testing.T, db *store.DB) *env {
	t.Helper()
	repos, err := repository.New(repository.Options{
		DB: db, Secrets: secrets.NewStore(db, secretstest.Keyring(t, 1)), Layout: config.Layout{Root: t.TempDir()},
		Tasks: repositorytest.NewLocalRunner(t), Logger: storetest.Logger(),
		Created: repomaint.OnRepositoryCreated(repomaint.Timing{}), Deleted: repomaint.OnRepositoryDeleted,
	})
	if err != nil {
		t.Fatal(err)
	}
	svc, err := offboard.New(offboard.Options{DB: db, Repositories: repos, Logger: storetest.Logger()})
	if err != nil {
		t.Fatal(err)
	}
	pool := jobs.NewPool(db, jobs.Options{Logger: storetest.Logger(), Backoff: func(int) time.Duration { return 0 }})
	if err := pool.Register(svc.Handler()); err != nil {
		t.Fatal(err)
	}
	return &env{db: db, repos: repos, svc: svc, pool: pool}
}

// group creates a client group, with a repository holding the record of a
// deleted site's backup if withRepository.
func (e *env) group(t *testing.T, name string, withRepository bool) (string, string) {
	t.Helper()
	ctx := context.Background()
	g := store.ClientGroup{Name: name}
	if err := store.NewClientGroups(e.db).Create(ctx, &g); err != nil {
		t.Fatal(err)
	}
	if !withRepository {
		return g.ID, ""
	}
	r, err := e.repos.Ensure(ctx, g.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.NewBackupRecords(e.db).Create(ctx, &store.BackupRecord{SiteID: id.New(), ClientGroupID: g.ID, Kind: store.BackupManual}); err != nil {
		t.Fatal(err)
	}
	return g.ID, r.Location
}

func (e *env) addSite(t *testing.T, groupID string) string {
	t.Helper()
	siteID, now := id.New(), time.Now()
	if _, err := e.db.ExecContext(context.Background(), `INSERT INTO sites
		(id, client_group_id, name, slug, status, preview_domain, php_version, wp_core_version, uid, created_at, updated_at)
		VALUES (?, ?, 'Site', ?, 'running', ?, '8.4', '6.9.1', 20000, ?, ?)`,
		siteID, groupID, "site-"+siteID[:6], siteID[:6]+".preview.example", now, now); err != nil {
		t.Fatal(err)
	}
	return siteID
}

func (e *env) run(t *testing.T, jobID string) store.Job {
	t.Helper()
	ctx := context.Background()
	for range 20 {
		ran, err := e.pool.RunNext(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if !ran {
			break
		}
	}
	j, err := store.NewJobs(e.db).Get(ctx, jobID)
	if err != nil {
		t.Fatal(err)
	}
	return j
}

func (e *env) count(t *testing.T, query string, args ...any) int {
	t.Helper()
	var n int
	if err := e.db.QueryRowContext(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestDeleteRepository(t *testing.T) {
	t.Parallel()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		ctx := context.Background()
		e := newEnv(t, db)
		groupID, location := e.group(t, "Acme", true)
		if n := e.count(t, "SELECT COUNT(*) FROM schedules WHERE target_id = ?", groupID); n != 2 {
			t.Fatalf("maintenance schedules = %d, want 2", n)
		}

		job, err := e.svc.Enqueue(ctx, groupID, false, "")
		if err != nil {
			t.Fatal(err)
		}
		if job.Type != offboard.DeleteJobType || job.TargetType != store.JobTargetClientGroup || job.TargetID != groupID || job.LockKey != repomaint.LockKey(groupID) {
			t.Errorf("job = %+v", job)
		}
		if j := e.run(t, job.ID); j.State != store.JobSucceeded {
			t.Fatalf("job %s: %s", j.State, j.Error)
		}
		if _, err := os.Stat(location); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("repository directory: %v", err)
		}
		if _, err := e.repos.Get(ctx, groupID); !errors.Is(err, repository.ErrNoRepository) {
			t.Errorf("repository record: %v", err)
		}
		left := e.count(t, "SELECT COUNT(*) FROM backup_records WHERE client_group_id = ?", groupID) +
			e.count(t, "SELECT COUNT(*) FROM schedules WHERE target_id = ?", groupID) +
			e.count(t, "SELECT COUNT(*) FROM secrets")
		if left != 0 {
			t.Errorf("rows left behind: %d", left)
		}
		g, err := store.NewClientGroups(db).Get(ctx, groupID)
		if err != nil || g.BackupRepository != "" {
			t.Fatalf("group after the job: %+v, %v", g, err)
		}

		// Nothing left to queue for; a job that runs again anyway (a
		// retry, or one queued twice) finds nothing to do.
		if _, err := e.svc.Enqueue(ctx, groupID, false, ""); !errors.Is(err, offboard.ErrNoRepository) {
			t.Errorf("Enqueue again: %v, want ErrNoRepository", err)
		}
		again, err := jobs.Enqueue(ctx, db, jobs.Spec{Type: offboard.DeleteJobType, TargetType: store.JobTargetClientGroup, TargetID: groupID})
		if err != nil {
			t.Fatal(err)
		}
		if j := e.run(t, again.ID); j.State != store.JobSucceeded {
			t.Errorf("job run again: %s %s", j.State, j.Error)
		}
	})
}

func TestDeleteRepositoryAndGroup(t *testing.T) {
	t.Parallel()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		ctx := context.Background()
		e := newEnv(t, db)
		groupID, _ := e.group(t, "Acme", true)
		job, err := e.svc.Enqueue(ctx, groupID, true, "")
		if err != nil {
			t.Fatal(err)
		}
		if j := e.run(t, job.ID); j.State != store.JobSucceeded {
			t.Fatalf("job %s: %s", j.State, j.Error)
		}
		if _, err := store.NewClientGroups(db).Get(ctx, groupID); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("group after the job: %v, want ErrNotFound", err)
		}
		// Running again finds both gone.
		again, err := jobs.Enqueue(ctx, db, jobs.Spec{
			Type: offboard.DeleteJobType, TargetType: store.JobTargetClientGroup, TargetID: groupID, Payload: map[string]bool{"delete_client_group": true},
		})
		if err != nil {
			t.Fatal(err)
		}
		if j := e.run(t, again.ID); j.State != store.JobSucceeded {
			t.Errorf("job run again: %s %s", j.State, j.Error)
		}
	})
}

func TestEnqueueRefusals(t *testing.T) {
	t.Parallel()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		ctx := context.Background()
		e := newEnv(t, db)
		bare, _ := e.group(t, "Bare", false)
		withSites, _ := e.group(t, "Sites", true)
		e.addSite(t, withSites)
		exporting, _ := e.group(t, "Exporting", true)
		exp, err := jobs.Enqueue(ctx, db, jobs.Spec{Type: export.BackupJobType, TargetType: store.JobTargetClientGroup, TargetID: exporting})
		if err != nil {
			t.Fatal(err)
		}
		for _, tc := range []struct {
			name, groupID string
			want          error
		}{
			{"unknown group", id.New(), offboard.ErrClientGroupNotFound},
			{"no repository", bare, offboard.ErrNoRepository},
			{"sites", withSites, offboard.ErrGroupHasSites},
			{"export in flight", exporting, offboard.ErrExportRunning},
		} {
			if _, err := e.svc.Enqueue(ctx, tc.groupID, true, ""); !errors.Is(err, tc.want) {
				t.Errorf("%s: err = %v, want %v", tc.name, err, tc.want)
			}
		}
		if _, err := e.svc.Enqueue(ctx, exporting, false, ""); err == nil || !strings.Contains(err.Error(), exp.ID) {
			t.Errorf("export in flight: err = %v, want one naming job %s", err, exp.ID)
		}
		if n := e.count(t, "SELECT COUNT(*) FROM jobs WHERE type = ?", offboard.DeleteJobType); n != 0 {
			t.Errorf("%d jobs queued despite the refusals", n)
		}
	})
}

// A site created after the job was queued stops it before anything is
// deleted, for good: retrying won't remove the site.
func TestDeleteRefusedWhenSiteAppears(t *testing.T) {
	t.Parallel()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		ctx := context.Background()
		e := newEnv(t, db)
		groupID, location := e.group(t, "Acme", true)
		job, err := e.svc.Enqueue(ctx, groupID, true, "")
		if err != nil {
			t.Fatal(err)
		}
		e.addSite(t, groupID)
		j := e.run(t, job.ID)
		if j.State != store.JobFailed || !strings.Contains(j.Error, "still has sites") || j.Attempts != 1 {
			t.Errorf("job: %s after %d attempt(s): %s", j.State, j.Attempts, j.Error)
		}
		if _, err := os.Stat(location); err != nil {
			t.Errorf("repository removed despite the refusal: %v", err)
		}
		if _, err := store.NewClientGroups(db).Get(ctx, groupID); err != nil {
			t.Errorf("group: %v", err)
		}
	})
}
