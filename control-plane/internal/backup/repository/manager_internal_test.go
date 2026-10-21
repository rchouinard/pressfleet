package repository

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/config"
)

// s3Manager returns a Manager whose S3 repositories live on a fake server,
// and the location of group's repository there.
func s3Manager(t *testing.T, objects map[string]string) (*Manager, *fakeS3, Repository) {
	t.Helper()
	f, c := newFakeS3(t, objects)
	m := &Manager{
		s3:   S3Config{AccessKeyID: testKeyID, SecretAccessKey: testSecret},
		http: c.http, log: slog.New(slog.DiscardHandler),
	}
	rec := Repository{ClientGroupID: "g1", Backend: BackendS3, Location: "s3:" + c.endpoint.String() + "/" + f.bucket + "/repo"}
	return m, f, rec
}

func TestS3RepositoryData(t *testing.T) {
	ctx := context.Background()
	objects := map[string]string{
		"repo/config": "c", "repo/keys/k": "k", "repo/data/00/a": "a", "repo/locks/l": "stale", "repo/": "",
		"repository-of-another-group/config": "x",
	}
	m, f, rec := s3Manager(t, objects)

	// Export copies everything but locks, which restic recreates.
	dir := filepath.Join(t.TempDir(), "export")
	if err := m.copyRepository(ctx, rec, dir); err != nil {
		t.Fatal(err)
	}
	var got []string
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, _ error) error {
		if rel, _ := filepath.Rel(dir, p); !d.IsDir() || rel == "locks" {
			got = append(got, filepath.ToSlash(rel))
		}
		return nil
	})
	slices.Sort(got)
	if want := []string{"config", "data/00/a", "keys/k", "locks"}; !slices.Equal(got, want) {
		t.Errorf("exported %v, want %v", got, want)
	}

	// removeData deletes the repository's objects only.
	if err := m.removeData(ctx, rec); err != nil {
		t.Fatal(err)
	}
	if len(f.objects) != 1 || f.objects["repository-of-another-group/config"] != "x" {
		t.Errorf("after removeData: %v, want only the other repository", f.objects)
	}
	// probe reports a missing repository itself, without restic, which
	// would retry a missing bucket for minutes.
	if err := m.probe(ctx, rec, "pw"); !errors.Is(err, errRepositoryMissing) {
		t.Errorf("probe after removeData: %v, want errRepositoryMissing", err)
	}
	if err := m.removeData(ctx, rec); err != nil {
		t.Errorf("removeData with nothing left: %v", err)
	}

	f.fail = http.StatusForbidden
	if err := m.probe(ctx, rec, "pw"); !errors.Is(err, errS3Status) {
		t.Errorf("probe when S3 refuses: %v, want errS3Status", err)
	}
	if err := m.removeData(ctx, rec); !errors.Is(err, errS3Status) {
		t.Errorf("removeData when S3 refuses: %v, want errS3Status", err)
	}
	if err := m.copyRepository(ctx, rec, filepath.Join(t.TempDir(), "x")); !errors.Is(err, errS3Status) {
		t.Errorf("copyRepository when S3 refuses: %v, want errS3Status", err)
	}

	m.s3.AccessKeyID = ""
	if err := m.removeData(ctx, rec); !errors.Is(err, ErrS3NotConfigured) {
		t.Errorf("removeData without credentials: %v, want ErrS3NotConfigured", err)
	}
}

func TestRepositoryDataRefusals(t *testing.T) {
	ctx := context.Background()
	m := &Manager{layout: config.Layout{Root: t.TempDir()}, log: slog.New(slog.DiscardHandler)}
	victim := t.TempDir()
	if err := os.WriteFile(filepath.Join(victim, "keep"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A damaged record never makes removeData or copyRepository touch a
	// directory that isn't the group's.
	moved := Repository{ClientGroupID: "01m56e7dwt38qkqhzq535bw0r6", Backend: BackendLocal, Location: victim}
	if err := m.removeData(ctx, moved); err == nil || !strings.Contains(err.Error(), "was the data directory moved") {
		t.Errorf("removeData of a foreign directory: %v", err)
	}
	if _, err := os.Stat(filepath.Join(victim, "keep")); err != nil {
		t.Errorf("removeData touched the foreign directory: %v", err)
	}
	if err := m.copyRepository(ctx, moved, filepath.Join(t.TempDir(), "x")); err == nil {
		t.Error("copyRepository of a foreign directory succeeded")
	}
	for name, rec := range map[string]Repository{
		"unknown backend": {ClientGroupID: "g", Backend: "ftp", Location: "ftp://x"},
		"bad s3 location": {ClientGroupID: "g", Backend: BackendS3, Location: "s3:not a url"},
	} {
		if err := m.removeData(ctx, rec); err == nil {
			t.Errorf("removeData, %s: no error", name)
		}
		if err := m.copyRepository(ctx, rec, filepath.Join(t.TempDir(), "x")); err == nil {
			t.Errorf("copyRepository, %s: no error", name)
		}
		if err := m.probe(ctx, rec, "pw"); err == nil && rec.Backend == BackendS3 {
			t.Errorf("probe, %s: no error", name)
		}
	}
}

func TestChgrp(t *testing.T) {
	m := &Manager{gid: 4242, log: slog.New(slog.DiscardHandler)}
	if err := m.chgrp("p", func() error { return nil }); err != nil {
		t.Errorf("successful lchown: %v", err)
	}
	other := &fs.PathError{Op: "lchown", Path: "p", Err: syscall.ENOENT}
	if err := m.chgrp("p", func() error { return other }); !errors.Is(err, fs.ErrNotExist) || !strings.Contains(err.Error(), "group 4242") {
		t.Errorf("failed lchown: %v", err)
	}
	denied := &fs.PathError{Op: "lchown", Path: "p", Err: syscall.EPERM}
	err := m.chgrp("p", func() error { return denied })
	if os.Geteuid() == 0 {
		// Root may change any group, so a refusal is a real error.
		if !errors.Is(err, fs.ErrPermission) {
			t.Errorf("refused lchown as root: %v, want the error", err)
		}
	} else if err != nil || !m.warnedChgrp.Load() {
		t.Errorf("refused lchown as a user: %v, warned %v; want it skipped once", err, m.warnedChgrp.Load())
	}
}

func TestKeyedLock(t *testing.T) {
	k := &keyedLock{held: map[string]*heldLock{}}
	ctx := context.Background()
	unlockA, err := k.lock(ctx, "a")
	if err != nil {
		t.Fatal(err)
	}
	// Another key doesn't wait.
	unlockB, err := k.lock(ctx, "b")
	if err != nil {
		t.Fatal(err)
	}
	unlockB()

	// A waiter on a held key gives up with its context.
	waitCtx, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	if _, err := k.lock(waitCtx, "a"); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("lock of a held key: %v, want DeadlineExceeded", err)
	}
	// ... and a waiter that outlasts the holder gets the lock.
	got := make(chan error, 1)
	go func() {
		unlock, err := k.lock(ctx, "a")
		if err == nil {
			unlock()
		}
		got <- err
	}()
	unlockA()
	if err := <-got; err != nil {
		t.Errorf("waiter: %v", err)
	}
	if len(k.held) != 0 {
		t.Errorf("%d locks left in the map", len(k.held))
	}
}

func TestS3InterruptedAndDamagedInit(t *testing.T) {
	ctx := context.Background()
	m, f, rec := s3Manager(t, map[string]string{"repo/keys/k1": "k", "repo/data/00/": ""})

	// A key without a config, snapshots, or index is an interrupted init.
	if err := m.clearInterruptedInit(ctx, rec); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.objects["repo/keys/k1"]; ok {
		t.Error("the key of an interrupted init is still there")
	}
	f.objects["repo/keys/k1"], f.objects["repo/snapshots/s1"] = "k", "s"
	if err := m.clearInterruptedInit(ctx, rec); !errors.Is(err, ErrRepositoryUnfinished) {
		t.Errorf("clearInterruptedInit with snapshots: %v, want ErrRepositoryUnfinished", err)
	}
	if _, ok := f.objects["repo/keys/k1"]; !ok {
		t.Error("a key was removed from a repository with snapshots")
	}

	// Several keys and restic's complaint about them is the damage of
	// racing inits; one key is some other problem.
	cause := errors.New("restic cat: exit status 1: Fatal: config or key k1 is damaged: ciphertext verification failed")
	if err := m.diagnose(ctx, rec, cause); !errors.Is(err, cause) || errors.Is(err, ErrRepositoryDamaged) {
		t.Errorf("diagnose with one key: %v", err)
	}
	f.objects["repo/keys/k2"] = "k"
	err := m.diagnose(ctx, rec, cause)
	if !errors.Is(err, ErrRepositoryDamaged) || !strings.Contains(err.Error(), "k1, k2") {
		t.Errorf("diagnose with two keys: %v", err)
	}
	if err := m.diagnose(ctx, rec, errors.New("something else")); errors.Is(err, ErrRepositoryDamaged) {
		t.Errorf("diagnose of another error: %v", err)
	}
	if len(f.objects) != 4 {
		t.Errorf("diagnose changed the objects: %v", f.objects)
	}
}
