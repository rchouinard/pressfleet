package statebackup_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/backup"
	"github.com/rchouinard/pressfleet/control-plane/internal/backup/repository"
	"github.com/rchouinard/pressfleet/control-plane/internal/backup/repository/repositorytest"
	"github.com/rchouinard/pressfleet/control-plane/internal/backup/statebackup"
	"github.com/rchouinard/pressfleet/control-plane/internal/config"
	"github.com/rchouinard/pressfleet/control-plane/internal/jobs"
	"github.com/rchouinard/pressfleet/control-plane/internal/secrets"
	"github.com/rchouinard/pressfleet/control-plane/internal/secrets/secretstest"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
)

// newRepo returns a repository manager for a data directory, whose state
// repository opens with the master key's derived password.
func newRepo(t *testing.T, db *store.DB, layout config.Layout, keys *secrets.Keyring) *repository.Manager {
	t.Helper()
	m, err := repository.New(repository.Options{
		DB: db, Secrets: secrets.NewStore(db, keys), Layout: layout, Tasks: repositorytest.NewLocalRunner(t),
		StatePassword: keys.StateBackupPassword(), Logger: storetest.Logger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func groupCount(t *testing.T, cfg store.Config) int {
	t.Helper()
	db := storetest.OpenUnmigrated(t, cfg)
	var n int
	if err := db.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM client_groups").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestBackupAndRestore(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		keys := secretstest.Keyring(t, 1)
		layout := config.Layout{Root: t.TempDir()}
		repo := newRepo(t, db, layout, keys)
		svc, err := statebackup.New(statebackup.Options{
			DB: db, Repository: repo, Retention: backup.Retention{KeepLast: 2},
			TmpDir: layout.TmpDir(), Version: "1.2.3", Logger: storetest.Logger(),
		})
		if err != nil {
			t.Fatal(err)
		}
		if snaps, err := statebackup.List(ctx, repo); err != nil || len(snaps) != 0 {
			t.Fatalf("List before any backup = %v, %v", snaps, err)
		}
		if _, err := statebackup.Find(ctx, repo, "latest"); !errors.Is(err, statebackup.ErrNoBackups) {
			t.Fatalf("Find before any backup = %v", err)
		}

		groups := store.NewClientGroups(db)
		var taken []statebackup.Snapshot
		for _, name := range []string{"first", "second", "third"} {
			if err := groups.Create(ctx, &store.ClientGroup{Name: name}); err != nil {
				t.Fatal(err)
			}
			snap, err := svc.Backup(ctx)
			if err != nil {
				t.Fatal(err)
			}
			taken = append(taken, snap)
		}
		latest, _ := store.LatestVersion(db.Dialect())
		if s := taken[0]; s.ID == "" || s.SchemaVersion != latest || s.Dialect != string(db.Dialect()) || s.SizeBytes == 0 {
			t.Errorf("snapshot = %+v", s)
		}
		if p := repo.StateLocation(); p != filepath.Join(layout.Root, "backups", "state") {
			t.Errorf("StateLocation = %s", p)
		}

		n, err := svc.Forget(ctx)
		if err != nil || n != 1 {
			t.Fatalf("Forget = %d, %v; want 1 forgotten", n, err)
		}
		snaps, err := statebackup.List(ctx, repo)
		if err != nil || len(snaps) != 2 || snaps[1].ID != taken[2].ID {
			t.Fatalf("List = %+v, %v", snaps, err)
		}
		if s := snaps[1]; s.SchemaVersion != latest || s.Dialect != string(db.Dialect()) || s.Version != "1.2.3" {
			t.Errorf("listed snapshot = %+v", s)
		}
		if got, err := statebackup.Find(ctx, repo, taken[1].ID[:8]); err != nil || got.ID != taken[1].ID {
			t.Errorf("Find by short ID = %+v, %v", got, err)
		}
		if _, err := statebackup.Find(ctx, repo, taken[0].ID); !errors.Is(err, statebackup.ErrSnapshotNotFound) {
			t.Errorf("Find of a forgotten backup = %v", err)
		}

		// A new host: a fresh database of either dialect, and only the
		// master key and the data directory's backups.
		for _, to := range storetest.Dialects {
			t.Run("to_"+string(to), func(t *testing.T) {
				cfg := storetest.Config(t, to)
				snap, err := statebackup.Find(ctx, repo, "")
				if err != nil {
					t.Fatal(err)
				}
				checked := false
				res, err := statebackup.Restore(ctx, repo, snap, statebackup.RestoreOptions{
					Store: cfg, BackupDir: t.TempDir(), Logger: storetest.Logger(),
					Check: func(context.Context, store.Querier) error {
						checked = true
						return nil
					},
				})
				if err != nil {
					t.Fatal(err)
				}
				if !checked || res.Dump.SchemaVersion != latest {
					t.Errorf("checked = %v, result = %+v", checked, res)
				}
				if n := groupCount(t, cfg); n != 3 {
					t.Errorf("%d client groups restored, want 3", n)
				}
			})
		}

		// A failed check leaves the database alone.
		cfg := storetest.Config(t, db.Dialect())
		errNo := errors.New("no")
		_, err = statebackup.Restore(ctx, repo, snaps[0], statebackup.RestoreOptions{
			Store: cfg, BackupDir: t.TempDir(), Logger: storetest.Logger(),
			Check: func(context.Context, store.Querier) error { return errNo },
		})
		if !errors.Is(err, errNo) {
			t.Fatalf("Restore with a failing check = %v", err)
		}

		// Another master key doesn't open the repository.
		other := newRepo(t, db, layout, secretstest.Keyring(t, 2))
		if _, err := statebackup.List(ctx, other); !errors.Is(err, repository.ErrWrongPassword) {
			t.Errorf("List with another master key = %v, want ErrWrongPassword", err)
		}
	})
}

// Restoring a backup ID that isn't in the repository fails before the
// database changes.
func TestRestoreMissingSnapshot(t *testing.T) {
	ctx := context.Background()
	db := storetest.Open(t, store.SQLite)
	keys := secretstest.Keyring(t, 1)
	layout := config.Layout{Root: t.TempDir()}
	repo := newRepo(t, db, layout, keys)
	svc, err := statebackup.New(statebackup.Options{DB: db, Repository: repo, TmpDir: layout.TmpDir(), Logger: storetest.Logger()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Backup(ctx); err != nil {
		t.Fatal(err)
	}
	cfg := storetest.Config(t, store.SQLite)
	_, err = statebackup.Restore(ctx, repo, statebackup.Snapshot{ID: "0123456789abcdef"}, statebackup.RestoreOptions{
		Store: cfg, BackupDir: t.TempDir(), Logger: storetest.Logger(),
	})
	if err == nil {
		t.Fatal("Restore of a missing snapshot succeeded")
	}
}

func TestJobAndSchedule(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		keys := secretstest.Keyring(t, 1)
		layout := config.Layout{Root: t.TempDir()}
		repo := newRepo(t, db, layout, keys)
		svc, err := statebackup.New(statebackup.Options{
			DB: db, Repository: repo, Retention: backup.Retention{KeepLast: 1}, TmpDir: layout.TmpDir(), Logger: storetest.Logger(),
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := statebackup.EnsureSchedule(ctx, db, time.Hour); err != nil {
			t.Fatal(err)
		}
		sch, err := store.NewSchedules(db).Get(ctx, statebackup.ScheduleName)
		if err != nil || sch.JobType != statebackup.JobType {
			t.Fatalf("schedule = %+v, %v", sch, err)
		}
		pool := jobs.NewPool(db, jobs.Options{Logger: storetest.Logger(), Secrets: secrets.NewStore(db, keys)})
		if err := pool.Register(svc.Handler()); err != nil {
			t.Fatal(err)
		}
		for range 2 {
			job, err := statebackup.Enqueue(ctx, db)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := pool.RunNext(ctx); err != nil {
				t.Fatal(err)
			}
			d, err := store.NewJobs(db).Detail(ctx, job.ID)
			if err != nil {
				t.Fatal(err)
			}
			if d.Job.State != store.JobSucceeded {
				t.Fatalf("job %s: %s", d.Job.State, d.Job.Error)
			}
		}
		snaps, err := statebackup.List(ctx, repo)
		if err != nil || len(snaps) != 1 {
			t.Fatalf("after two jobs keeping the last 1: %+v, %v", snaps, err)
		}

		// The backup was taken while its job ran; the restore settles it.
		cfg := storetest.Config(t, db.Dialect())
		var running, settled int
		_, err = statebackup.Restore(ctx, repo, snaps[0], statebackup.RestoreOptions{
			Store: cfg, BackupDir: t.TempDir(), Logger: storetest.Logger(),
			Check: func(ctx context.Context, q store.Querier) error {
				if err := q.QueryRowContext(ctx, "SELECT COUNT(*) FROM jobs WHERE type = ? AND state = ?",
					statebackup.JobType, store.JobRunning).Scan(&running); err != nil {
					return err
				}
				if err := statebackup.SettleRestored(ctx, q, time.Now()); err != nil {
					return err
				}
				return q.QueryRowContext(ctx, "SELECT COUNT(*) FROM jobs WHERE type = ? AND state = ?",
					statebackup.JobType, store.JobSucceeded).Scan(&settled)
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		if running != 1 || settled != 2 {
			t.Errorf("restored backup jobs: %d running before settling, %d succeeded after; want 1 and 2", running, settled)
		}
	})
}
