package repository_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/backup/repository"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner/provisionertest"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
)

// fakeRestic is a repository task runner that acts like restic just enough
// for Ensure: `cat config` fails with exit 10 until `init`, which writes a
// config as the task's user would, readable by its owner alone.
func fakeRestic(t *testing.T) *provisionertest.Fake {
	t.Helper()
	fake := provisionertest.New()
	fake.RunRepositoryTaskFunc = func(_ string, task provisioner.Task) (*provisioner.TaskResult, error) {
		config := filepath.Join(task.RepositoryDir, "config")
		switch {
		case slices.Contains(task.Command, "init"):
			if err := os.MkdirAll(filepath.Join(task.RepositoryDir, "keys"), 0o700); err != nil {
				return nil, err
			}
			return &provisioner.TaskResult{}, os.WriteFile(config, []byte("c"), 0o600)
		case slices.Contains(task.Command, "cat"):
			if _, err := os.Stat(config); err != nil {
				return nil, &provisioner.ExecError{ExitCode: 10, Stderr: "Fatal: repository does not exist"}
			}
		}
		return &provisioner.TaskResult{}, nil
	}
	return fake
}

func repositoryTasks(fake *provisionertest.Fake) []provisionertest.Call {
	return slices.DeleteFunc(fake.Calls(), func(c provisionertest.Call) bool { return c.Op != "RunRepositoryTask" })
}

// restic init runs in a task as the repository group, so its files are
// owned by it with mode 0600 (#190): Ensure shares them with the group
// afterwards, and the next backup, which only looks at the config file,
// then runs no restic at all.
func TestEnsureInTaskSharesAndHotPathRunsNothing(t *testing.T) {
	t.Parallel()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		ctx := context.Background()
		fake := fakeRestic(t)
		f := newFixture(t, db, repository.Options{Tasks: fake})
		groupID := f.group(t, "Acme")

		access, err := f.m.TaskAccess(ctx, groupID)
		if err != nil {
			t.Fatal(err)
		}
		config := filepath.Join(access.RepositoryDir, "config")
		info, err := os.Stat(config)
		if err != nil {
			t.Fatal(err)
		}
		// 0600 from the task, plus the group bits shareLocal adds.
		if got := info.Mode().Perm(); got != 0o660 {
			t.Errorf("config mode after init = %o, want 660", got)
		}
		dirInfo, err := os.Stat(access.RepositoryDir)
		if err != nil || dirInfo.Mode()&os.ModeSetgid == 0 {
			t.Errorf("repository directory = %v, %v; want setgid", dirInfo, err)
		}
		var cmds []string
		for _, c := range repositoryTasks(fake) {
			cmds = append(cmds, c.Task.Command[2])
		}
		// The probes find no directory and so no repository without a task.
		if want := []string{"init"}; !slices.Equal(cmds, want) {
			t.Errorf("first TaskAccess ran %v, want %v", cmds, want)
		}

		before := len(repositoryTasks(fake))
		for range 3 {
			if _, err := f.m.TaskAccess(ctx, groupID); err != nil {
				t.Fatal(err)
			}
		}
		if n := len(repositoryTasks(fake)) - before; n != 0 {
			t.Errorf("TaskAccess of an initialized repository ran %d repository tasks, want 0", n)
		}

		// Ensure still verifies that the repository opens.
		if _, err := f.m.Ensure(ctx, groupID); err != nil {
			t.Fatal(err)
		}
		if n := len(repositoryTasks(fake)) - before; n != 1 {
			t.Errorf("Ensure ran %d repository tasks, want 1 (cat config)", n)
		}
	})
}

// restic's exit codes keep meaning what they did when restic ran in the
// control plane.
func TestResticExitCodes(t *testing.T) {
	t.Parallel()
	db := storetest.Open(t, store.SQLite)
	boom := errors.New("docker is down")
	for _, tc := range []struct {
		name string
		err  error
		want error
		text string
	}{
		{"locked", &provisioner.ExecError{ExitCode: 11, Stderr: "unable to create lock"}, repository.ErrLocked, "unable to create lock"},
		{"wrong password", &provisioner.ExecError{ExitCode: 12, Stderr: "wrong password or no key found"}, repository.ErrWrongPassword, "wrong password"},
		{"killed", &provisioner.ExecError{ExitCode: 137}, nil, "backup.task_memory_mb"},
		{"other", &provisioner.ExecError{ExitCode: 1, Stderr: "Fatal: boom"}, nil, "restic snapshots: exit status 1: Fatal: boom"},
		{"driver", fmt.Errorf("run task: %w", boom), boom, "running restic snapshots"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := fakeRestic(t)
			f := newFixture(t, db, repository.Options{Tasks: fake})
			groupID := f.group(t, tc.name)
			if _, err := f.m.Ensure(context.Background(), groupID); err != nil {
				t.Fatal(err)
			}
			fake.RunRepositoryTaskFunc = func(string, provisioner.Task) (*provisioner.TaskResult, error) { return nil, tc.err }
			_, err := f.m.Restic(context.Background(), groupID, "snapshots")
			if err == nil || !strings.Contains(err.Error(), tc.text) {
				t.Fatalf("err = %v, want it to say %q", err, tc.text)
			}
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Errorf("err = %v, want %v", err, tc.want)
			}
			var execErr *provisioner.ExecError
			if errors.As(err, &execErr) && tc.want != nil {
				t.Errorf("exit code errors are mapped to the repository's sentinels, got %v", err)
			}
		})
	}
}

// A state repository that doesn't exist is ErrNoRepository: restic's exit
// code 10, or no directory to mount.
func TestStateResticMissing(t *testing.T) {
	t.Parallel()
	db := storetest.Open(t, store.SQLite)
	fake := fakeRestic(t)
	f := newFixture(t, db, repository.Options{Tasks: fake, StatePassword: "state-password"})
	var out bytes.Buffer
	if err := f.m.StateRestic(context.Background(), nil, &out, "snapshots"); !errors.Is(err, repository.ErrNoRepository) {
		t.Errorf("StateRestic with no repository directory = %v, want ErrNoRepository", err)
	}
	if n := len(repositoryTasks(fake)); n != 0 {
		t.Errorf("ran %d tasks for a repository that has no directory", n)
	}

	if err := f.m.EnsureState(context.Background()); err != nil {
		t.Fatal(err)
	}
	fake.RunRepositoryTaskFunc = func(string, provisioner.Task) (*provisioner.TaskResult, error) {
		return nil, &provisioner.ExecError{ExitCode: 10, Stderr: "Fatal: repository does not exist"}
	}
	if err := f.m.StateRestic(context.Background(), nil, &out, "snapshots"); !errors.Is(err, repository.ErrNoRepository) {
		t.Errorf("StateRestic with exit 10 = %v, want ErrNoRepository", err)
	}
	for _, c := range repositoryTasks(fake) {
		if c.Repository != provisioner.StateRepository {
			t.Errorf("state task ran for repository %q", c.Repository)
		}
	}
	info, err := os.Stat(filepath.Join(f.layout.StateBackupRepositoryDir(), "config"))
	if err != nil || info.Mode().Perm()&0o060 != 0o060 {
		t.Errorf("state repository config = %v, %v; want shared with the repository group", info, err)
	}
}

// Standard input reaches a task as a file in its transfer directory, which
// is removed afterwards.
func TestStateResticStdin(t *testing.T) {
	t.Parallel()
	db := storetest.Open(t, store.SQLite)
	fake := fakeRestic(t)
	f := newFixture(t, db, repository.Options{Tasks: fake, StatePassword: "state-password"})
	ctx := context.Background()
	if err := f.m.EnsureState(ctx); err != nil {
		t.Fatal(err)
	}
	var staged string
	inner := fake.RunRepositoryTaskFunc
	fake.RunRepositoryTaskFunc = func(r string, task provisioner.Task) (*provisioner.TaskResult, error) {
		if task.TransferDir != "" {
			b, err := os.ReadFile(filepath.Join(task.TransferDir, "stdin"))
			if err != nil {
				return nil, err
			}
			staged = string(b)
			if rel, err := filepath.Rel(f.layout.TmpDir(), task.TransferDir); err != nil || !filepath.IsLocal(rel) {
				return nil, fmt.Errorf("transfer directory %s is not below %s", task.TransferDir, f.layout.TmpDir())
			}
			want := []string{"sh", "-c", `exec "$@" < /transfer/stdin`, "sh", "restic", "--no-cache", "backup", "--stdin"}
			if !slices.Equal(task.Command, want) {
				return nil, fmt.Errorf("command = %q, want %q", task.Command, want)
			}
		}
		return inner(r, task)
	}
	var out bytes.Buffer
	if err := f.m.StateRestic(ctx, strings.NewReader("the dump"), &out, "backup", "--stdin"); err != nil {
		t.Fatal(err)
	}
	if staged != "the dump" {
		t.Errorf("task found standard input %q", staged)
	}
	if entries, _ := os.ReadDir(f.layout.TmpDir()); len(entries) != 0 {
		t.Errorf("transfer directories left in tmp/: %v", entries)
	}
}

// The Manager never runs more repository tasks at once than MaxTasks, and a
// caller waiting for a slot gives up with its context.
func TestMaxTasks(t *testing.T) {
	t.Parallel()
	db := storetest.Open(t, store.SQLite)
	fake := fakeRestic(t)
	f := newFixture(t, db, repository.Options{Tasks: fake, MaxTasks: 2})
	ctx := context.Background()
	groupID := f.group(t, "Acme")
	if _, err := f.m.Ensure(ctx, groupID); err != nil {
		t.Fatal(err)
	}

	var running, peak atomic.Int32
	release := make(chan struct{})
	fake.RunRepositoryTaskFunc = func(string, provisioner.Task) (*provisioner.TaskResult, error) {
		n := running.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		<-release
		running.Add(-1)
		return &provisioner.TaskResult{}, nil
	}
	var wg sync.WaitGroup
	for range 6 {
		wg.Go(func() {
			if _, err := f.m.Restic(ctx, groupID, "snapshots"); err != nil {
				t.Error(err)
			}
		})
	}
	deadline := time.Now().Add(10 * time.Second)
	for running.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond) // time for a third to start, were it allowed to

	waiting, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	if _, err := f.m.Restic(waiting, groupID, "snapshots"); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Restic with no free slot = %v, want the context's deadline", err)
	}
	close(release)
	wg.Wait()
	if p := peak.Load(); p != 2 {
		t.Errorf("peak concurrent repository tasks = %d, want 2", p)
	}
}
