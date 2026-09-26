package cli_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
)

// seedStateDB creates a migrated SQLite state database in dataDir with one
// client group, runs extra, and closes it.
func seedStateDB(t *testing.T, dataDir string, extra func(db *store.DB)) {
	t.Helper()
	ctx := context.Background()
	cfg := store.Config{Driver: store.SQLite, SQLitePath: filepath.Join(dataDir, "state", "pressfleet.db")}
	db, err := store.Open(ctx, cfg, storetest.Logger())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Migrate(ctx, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if err := store.NewClientGroups(db).Create(ctx, &store.ClientGroup{Name: "Acme"}); err != nil {
		t.Fatal(err)
	}
	if extra != nil {
		extra(db)
	}
}

func TestServeRefusesNewerSchema(t *testing.T) {
	dataDir := t.TempDir()
	seedStateDB(t, dataDir, func(db *store.DB) {
		if _, err := db.ExecContext(context.Background(),
			"INSERT INTO schema_migrations (version, name, applied_at) VALUES (9999, 'future', ?)", time.Now()); err != nil {
			t.Fatal(err)
		}
	})
	r := run(t, context.Background(), []string{"serve", "--data-dir", dataDir}, nil)
	if r.code != 1 || !strings.Contains(r.stderr, "database schema is newer than this pressfleet binary supports") {
		t.Errorf("exit %d, stderr: %s", r.code, r.stderr)
	}
}

func TestStateRestore(t *testing.T) {
	dataDir := t.TempDir()
	backupDir := filepath.Join(dataDir, "state", "backups")
	var backup string
	seedStateDB(t, dataDir, func(db *store.DB) {
		var err error
		if backup, err = db.Backup(context.Background(), backupDir, "test"); err != nil {
			t.Fatal(err)
		}
	})

	// A bare file name is found in the backups directory.
	r := run(t, context.Background(), []string{"state", "restore", filepath.Base(backup), "--data-dir", dataDir}, nil)
	if r.code != 0 {
		t.Fatalf("exit %d, stderr: %s", r.code, r.stderr)
	}
	for _, want := range []string{"Restored " + backup, "The previous database was saved as " + backupDir} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("stdout missing %q:\n%s", want, r.stdout)
		}
	}

	r = run(t, context.Background(), []string{"state", "restore", "missing.db", "--data-dir", dataDir}, nil)
	if r.code != 1 || !strings.Contains(r.stderr, "no such file") {
		t.Errorf("missing backup: exit %d, stderr: %s", r.code, r.stderr)
	}

	r = run(t, context.Background(), []string{"state", "restore", backup, "--data-dir", dataDir},
		map[string]string{"PRESSFLEET_DATABASE_DRIVER": "postgres", "PRESSFLEET_DATABASE_POSTGRES_DSN": "postgres://db/pf"})
	if r.code != 1 || !strings.Contains(r.stderr, "not supported for this database driver") {
		t.Errorf("postgres: exit %d, stderr: %s", r.code, r.stderr)
	}
}
