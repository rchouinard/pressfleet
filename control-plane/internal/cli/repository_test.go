package cli_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
	if r := repoCmd("create", "not-an-id"); r.code == 0 {
		t.Error("create with an invalid ID succeeded")
	}
}
