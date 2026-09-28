package cli_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rchouinard/pressfleet/control-plane/internal/backup/repomaint"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
)

// createGroup adds a client group to the SQLite state database in dataDir.
func createGroup(t *testing.T, dataDir, name string) string {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(ctx, store.Config{Driver: store.SQLite, SQLitePath: filepath.Join(dataDir, "state", "pressfleet.db")}, storetest.Logger())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Migrate(ctx, filepath.Join(dataDir, "state", "backups")); err != nil {
		t.Fatal(err)
	}
	g := store.ClientGroup{Name: name}
	if err := store.NewClientGroups(db).Create(ctx, &g); err != nil {
		t.Fatal(err)
	}
	return g.ID
}

func TestRepositoryCommands(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	groupID := createGroup(t, dataDir, "Acme")
	repoCmd := func(args ...string) result {
		t.Helper()
		return run(t, ctx, append(append([]string{"repository"}, args...), "--data-dir", dataDir), nil)
	}

	r := repoCmd("create", groupID)
	repoDir := filepath.Join(dataDir, "backups", "repositories", groupID)
	if r.code != 0 || !strings.Contains(r.stdout, repoDir) {
		t.Fatalf("create: exit %d, stdout %s, stderr %s", r.code, r.stdout, r.stderr)
	}
	if _, err := os.Stat(filepath.Join(repoDir, "config")); err != nil {
		t.Errorf("repository not initialized: %v", err)
	}

	// The new repository's maintenance is scheduled, and can be queued
	// by hand; the control plane runs the jobs.
	db, err := store.Open(ctx, store.Config{Driver: store.SQLite, SQLitePath: filepath.Join(dataDir, "state", "pressfleet.db")}, storetest.Logger())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	for _, name := range []string{repomaint.PruneScheduleName(groupID), repomaint.CheckScheduleName(groupID)} {
		if _, err := store.NewSchedules(db).Get(ctx, name); err != nil {
			t.Errorf("schedule %s: %v", name, err)
		}
	}
	for _, tt := range []struct{ cmd, jobType string }{{"prune", repomaint.PruneJobType}, {"check", repomaint.CheckJobType}} {
		r := repoCmd(tt.cmd, groupID)
		if r.code != 0 || !strings.Contains(r.stdout, "Queued "+tt.jobType+" job") {
			t.Fatalf("%s: exit %d, stdout %s, stderr %s", tt.cmd, r.code, r.stdout, r.stderr)
		}
		page, err := store.NewJobs(db).List(ctx, store.JobFilter{Types: []string{tt.jobType}})
		if err != nil || len(page.Jobs) != 1 || page.Jobs[0].TargetID != groupID || page.Jobs[0].LockKey != "repo:"+groupID {
			t.Errorf("%s jobs = %+v, %v", tt.jobType, page, err)
		}
	}
	if r := repoCmd("prune", createGroup(t, dataDir, "Globex")); r.code == 0 || !strings.Contains(r.stderr, "no backup repository") {
		t.Errorf("prune of a group without a repository: exit %d, stderr %s", r.code, r.stderr)
	}

	dest := filepath.Join(dataDir, "tmp", "acme-export")
	if r := repoCmd("export", groupID, dest); r.code != 0 || !strings.Contains(r.stdout, filepath.Join(dest, "password.txt")) {
		t.Fatalf("export: exit %d, stdout %s, stderr %s", r.code, r.stdout, r.stderr)
	}
	pw, err := os.ReadFile(filepath.Join(dest, "password.txt"))
	if err != nil || len(strings.TrimSpace(string(pw))) < 40 {
		t.Errorf("exported password: %v", err)
	}
	if strings.Contains(r.stdout+r.stderr, strings.TrimSpace(string(pw))) {
		t.Error("the repository password was printed or logged")
	}

	if r := repoCmd("delete", groupID); r.code == 0 || !strings.Contains(r.stderr, "--yes") {
		t.Errorf("delete without --yes: exit %d, stderr %s", r.code, r.stderr)
	}
	if r := repoCmd("delete", groupID, "--yes"); r.code != 0 {
		t.Fatalf("delete: exit %d, stderr %s", r.code, r.stderr)
	}
	if _, err := os.Stat(repoDir); !os.IsNotExist(err) {
		t.Errorf("repository directory after delete: %v", err)
	}
	for _, name := range []string{repomaint.PruneScheduleName(groupID), repomaint.CheckScheduleName(groupID)} {
		if _, err := store.NewSchedules(db).Get(ctx, name); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("schedule %s after delete: %v", name, err)
		}
	}
	if r := repoCmd("create", "not-an-id"); r.code == 0 {
		t.Error("create with an invalid ID succeeded")
	}
}
