package repository_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/rchouinard/pressfleet/control-plane/internal/backup/repository"
	"github.com/rchouinard/pressfleet/control-plane/internal/backup/repository/repositorytest"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
	"github.com/rchouinard/pressfleet/control-plane/internal/redact"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
)

// failingRunner is a LocalRunner whose restic commands named in fail exit
// with status 1, as a lost connection or a full disk would make them.
type failingRunner struct {
	*repositorytest.LocalRunner
	fail atomic.Value // string: the restic subcommand that fails, or ""
}

func (r *failingRunner) RunRepositoryTask(ctx context.Context, repo string, task provisioner.Task) (*provisioner.TaskResult, error) {
	if sub, _ := r.fail.Load().(string); sub != "" && slices.Contains(task.Command, sub) {
		return nil, &provisioner.ExecError{ExitCode: 1, Stderr: "Fatal: unable to open repository: connection reset"}
	}
	return r.LocalRunner.RunRepositoryTask(ctx, repo, task)
}

// restoreState does to a group's repository what a state restore from a
// backup older than the repository does: its record and password are
// gone, and its files stay. It returns the password that opens them.
func restoreState(t *testing.T, f fixture, groupID string) redact.Secret {
	t.Helper()
	ctx := context.Background()
	rec, err := f.m.Get(ctx, groupID)
	if err != nil {
		t.Fatal(err)
	}
	old := f.password(t, rec)
	if _, err := f.db.ExecContext(ctx, "DELETE FROM backup_repositories WHERE client_group_id = ?", groupID); err != nil {
		t.Fatal(err)
	}
	if err := f.secrets.Delete(ctx, rec.SecretRef); err != nil {
		t.Fatal(err)
	}
	return old
}

// unopenable returns the repositories set aside in the backups directory.
func unopenable(t *testing.T, f fixture) []string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(f.layout.Root, "backups", "unopenable-*"))
	if err != nil {
		t.Fatal(err)
	}
	return matches
}

// snapshotCount counts the snapshots that password finds in repo.
func snapshotCount(t *testing.T, repo string, password redact.Secret) int {
	t.Helper()
	out, err := restic(t, []string{"RESTIC_REPOSITORY=" + repo, "RESTIC_PASSWORD=" + password.Reveal()}, "snapshots", "--json")
	if err != nil {
		t.Fatalf("listing snapshots of %s: %v", repo, err)
	}
	var snaps []json.RawMessage
	if err := json.Unmarshal(out, &snaps); err != nil {
		t.Fatal(err)
	}
	return len(snaps)
}

// After a state restore that predates a group's repository, the next
// backup sets the old repository aside, keeping its files and saying where,
// and uses a new one: whether the old one looks initialized (TaskAccess
// then runs no restic of its own before) or not, and through TaskAccess or
// Ensure (#277).
func TestSetAsideAfterStateRestore(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name             string
		looksInitialized bool
		ensure           bool
	}{
		{"TaskAccess, looks initialized", true, false},
		{"TaskAccess, looks uninitialized", false, false},
		{"Ensure", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			storetest.Run(t, func(t *testing.T, db *store.DB) {
				ctx := context.Background()
				f := newFixture(t, db, repository.Options{})
				groupID := f.group(t, "Acme")
				access, err := f.m.TaskAccess(ctx, groupID)
				if err != nil {
					t.Fatal(err)
				}
				backupFile(t, access, t.TempDir())
				oldPassword := restoreState(t, f, groupID)
				dir, _ := f.layout.BackupRepositoryDir(groupID)
				if !tc.looksInitialized {
					if err := os.Chmod(filepath.Join(dir, "config"), 0o400); err != nil {
						t.Fatal(err)
					}
				}

				if tc.ensure {
					_, err = f.m.Ensure(ctx, groupID)
				} else {
					_, err = f.m.TaskAccess(ctx, groupID)
				}
				if err != nil {
					t.Fatalf("after the restore: %v", err)
				}

				rec, err := f.m.Get(ctx, groupID)
				if err != nil {
					t.Fatal(err)
				}
				if rec.PasswordVerifiedAt.IsZero() || rec.Location != dir {
					t.Errorf("record = %+v, want it verified at %s", rec, dir)
				}
				if n := snapshotCount(t, dir, f.password(t, rec)); n != 0 {
					t.Errorf("new repository has %d snapshots, want 0", n)
				}
				aside := unopenable(t, f)
				if len(aside) != 1 || !strings.HasPrefix(filepath.Base(aside[0]), "unopenable-"+groupID+"-") {
					t.Fatalf("set aside: %v, want one unopenable-%s-<time>", aside, groupID)
				}
				if n := snapshotCount(t, aside[0], oldPassword); n != 1 {
					t.Errorf("set-aside repository has %d snapshots, want the 1 it had", n)
				}

				page, err := store.NewAuditEvents(db).List(ctx, store.AuditFilter{Action: repository.AuditActionSetAside})
				if err != nil {
					t.Fatal(err)
				}
				if len(page.Events) != 1 {
					t.Fatalf("audit events = %+v, want one", page.Events)
				}
				e := page.Events[0]
				if e.TargetID != groupID || e.Details["unopenable_location"] != aside[0] || e.Details["new_location"] != dir {
					t.Errorf("audit event = %+v", e)
				}

				// From now on, backups run no restic before their own.
				before := len(f.local.Calls())
				for range 2 {
					if _, err := f.m.TaskAccess(ctx, groupID); err != nil {
						t.Fatal(err)
					}
				}
				if n := len(f.local.Calls()) - before; n != 0 {
					t.Errorf("TaskAccess of the new repository ran %d tasks, want 0", n)
				}
			})
		})
	}
}

// A repository whose record isn't marked (one from before the marker, with
// no succeeded backup) is opened once and then marked; nothing is moved.
func TestVerifyMarksRepositoryThatOpens(t *testing.T) {
	t.Parallel()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		ctx := context.Background()
		f := newFixture(t, db, repository.Options{})
		groupID := f.group(t, "Acme")
		if _, err := f.m.TaskAccess(ctx, groupID); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, "UPDATE backup_repositories SET password_verified_at = NULL"); err != nil {
			t.Fatal(err)
		}

		before := len(f.local.Calls())
		if _, err := f.m.TaskAccess(ctx, groupID); err != nil {
			t.Fatal(err)
		}
		calls := f.local.Calls()[before:]
		if len(calls) != 1 || !slices.Contains(calls[0].Task.Command, "cat") {
			t.Errorf("TaskAccess of an unmarked repository ran %d tasks, want 1 (cat config)", len(calls))
		}
		if rec, err := f.m.Get(ctx, groupID); err != nil || rec.PasswordVerifiedAt.IsZero() {
			t.Errorf("record = %+v, %v; want it marked", rec, err)
		}
		if aside := unopenable(t, f); len(aside) != 0 {
			t.Errorf("set aside %v", aside)
		}
		before = len(f.local.Calls())
		if _, err := f.m.TaskAccess(ctx, groupID); err != nil {
			t.Fatal(err)
		}
		if n := len(f.local.Calls()) - before; n != 0 {
			t.Errorf("TaskAccess of a marked repository ran %d tasks, want 0", n)
		}
	})
}

// Once the password has opened the repository, a wrong password is an
// error and moves nothing: TaskAccess hands out the repository without
// opening it, as before, and whatever opens it fails.
func TestWrongPasswordAfterVerified(t *testing.T) {
	t.Parallel()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		ctx := context.Background()
		f := newFixture(t, db, repository.Options{})
		groupID := f.group(t, "Acme")
		if _, err := f.m.TaskAccess(ctx, groupID); err != nil {
			t.Fatal(err)
		}
		dir, _ := f.layout.BackupRepositoryDir(groupID)
		if err := os.RemoveAll(dir); err != nil {
			t.Fatal(err)
		}
		foreign := []string{"RESTIC_REPOSITORY=" + dir, "RESTIC_PASSWORD=someone else's"}
		if _, err := restic(t, foreign, "init"); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(filepath.Join(dir, "config"), 0o440); err != nil {
			t.Fatal(err)
		}

		before := len(f.local.Calls())
		if _, err := f.m.TaskAccess(ctx, groupID); err != nil {
			t.Errorf("TaskAccess of a repository that looks initialized: %v", err)
		}
		if n := len(f.local.Calls()) - before; n != 0 {
			t.Errorf("TaskAccess ran %d tasks, want 0", n)
		}
		if _, err := f.m.Ensure(ctx, groupID); !errors.Is(err, repository.ErrWrongPassword) {
			t.Errorf("Ensure: err = %v, want ErrWrongPassword", err)
		}
		if err := os.Chmod(filepath.Join(dir, "config"), 0o400); err != nil {
			t.Fatal(err)
		}
		if _, err := f.m.TaskAccess(ctx, groupID); !errors.Is(err, repository.ErrWrongPassword) {
			t.Errorf("TaskAccess of a repository that doesn't look initialized: err = %v, want ErrWrongPassword", err)
		}
		if aside := unopenable(t, f); len(aside) != 0 {
			t.Errorf("set aside %v", aside)
		}
		if _, err := restic(t, foreign, "cat", "config"); err != nil {
			t.Errorf("the repository was changed: %v", err)
		}
	})
}

// A failure other than a wrong password moves nothing and is returned, and
// a retry, whose record is no longer new, still sets the repository aside.
// So does a retry after a crash between setting it aside and creating the
// new one.
func TestVerifyRetries(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		// fail is the restic command that fails on the first try.
		fail string
		// moved says whether the first try set the repository aside.
		moved bool
	}{
		{"opening fails", "cat", false},
		{"init fails after setting aside", "init", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			storetest.Run(t, func(t *testing.T, db *store.DB) {
				ctx := context.Background()
				runner := &failingRunner{LocalRunner: repositorytest.NewLocalRunner(t)}
				runner.fail.Store("")
				f := newFixture(t, db, repository.Options{Tasks: runner})
				groupID := f.group(t, "Acme")
				if _, err := f.m.TaskAccess(ctx, groupID); err != nil {
					t.Fatal(err)
				}
				oldPassword := restoreState(t, f, groupID)
				dir, _ := f.layout.BackupRepositoryDir(groupID)

				runner.fail.Store(tc.fail)
				_, err := f.m.TaskAccess(ctx, groupID)
				if err == nil || errors.Is(err, repository.ErrWrongPassword) || !strings.Contains(err.Error(), "connection reset") {
					t.Fatalf("first try: err = %v, want the task's failure", err)
				}
				rec, err := f.m.Get(ctx, groupID)
				if err != nil || !rec.PasswordVerifiedAt.IsZero() {
					t.Fatalf("record after the failure = %+v, %v; want it unmarked", rec, err)
				}
				if aside := unopenable(t, f); (len(aside) == 1) != tc.moved {
					t.Fatalf("set aside after the failure: %v, want moved %v", aside, tc.moved)
				}
				if !tc.moved {
					if _, err := restic(t, []string{"RESTIC_REPOSITORY=" + dir, "RESTIC_PASSWORD=" + oldPassword.Reveal()}, "cat", "config"); err != nil {
						t.Errorf("the repository was changed by the failed try: %v", err)
					}
				}

				runner.fail.Store("")
				if _, err := f.m.TaskAccess(ctx, groupID); err != nil {
					t.Fatalf("retry: %v", err)
				}
				rec, err = f.m.Get(ctx, groupID)
				if err != nil || rec.PasswordVerifiedAt.IsZero() {
					t.Errorf("record after the retry = %+v, %v; want it marked", rec, err)
				}
				aside := unopenable(t, f)
				if len(aside) != 1 {
					t.Fatalf("set aside after the retry: %v, want one", aside)
				}
				if _, err := restic(t, []string{"RESTIC_REPOSITORY=" + aside[0], "RESTIC_PASSWORD=" + oldPassword.Reveal()}, "cat", "config"); err != nil {
					t.Errorf("the set-aside repository doesn't open with its password: %v", err)
				}
				if _, err := restic(t, []string{"RESTIC_REPOSITORY=" + dir, "RESTIC_PASSWORD=" + f.password(t, rec).Reveal()}, "cat", "config"); err != nil {
					t.Errorf("the new repository doesn't open with the stored password: %v", err)
				}
			})
		})
	}
}

// Concurrent first backups after a state restore set the old repository
// aside once, and create one new repository with one key.
func TestSetAsideConcurrent(t *testing.T) {
	t.Parallel()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		ctx := context.Background()
		f := newFixture(t, db, repository.Options{})
		groupID := f.group(t, "Acme")
		if _, err := f.m.TaskAccess(ctx, groupID); err != nil {
			t.Fatal(err)
		}
		restoreState(t, f, groupID)

		const callers = 6
		errs := make([]error, callers)
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := range callers {
			wg.Go(func() {
				<-start
				_, errs[i] = f.m.TaskAccess(ctx, groupID)
			})
		}
		close(start)
		wg.Wait()
		for i, err := range errs {
			if err != nil {
				t.Errorf("TaskAccess #%d: %v", i, err)
			}
		}
		if aside := unopenable(t, f); len(aside) != 1 {
			t.Errorf("set aside %v, want one", aside)
		}
		rec, err := f.m.Get(ctx, groupID)
		if err != nil || rec.PasswordVerifiedAt.IsZero() {
			t.Fatalf("record = %+v, %v; want it verified", rec, err)
		}
		if keys := keyFiles(t, rec.Location); len(keys) != 1 {
			t.Errorf("%d key files %v, want 1", len(keys), keys)
		}
		env := []string{"RESTIC_REPOSITORY=" + rec.Location, "RESTIC_PASSWORD=" + f.password(t, rec).Reveal()}
		if _, err := restic(t, env, "cat", "config"); err != nil {
			t.Errorf("the new repository doesn't open: %v", err)
		}
	})
}
