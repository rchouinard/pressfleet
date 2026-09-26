package store_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
)

func groupNames(t *testing.T, db *store.DB) []string {
	t.Helper()
	list, err := store.NewClientGroups(db).List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, g := range list {
		names = append(names, g.Name)
	}
	return names
}

func TestRestore(t *testing.T) {
	ctx := context.Background()
	cfg := storetest.Config(t, store.SQLite)
	backups := filepath.Join(filepath.Dir(cfg.SQLitePath), "backups")

	db, err := store.Open(ctx, cfg, storetest.Logger())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Migrate(ctx, backups); err != nil {
		t.Fatal(err)
	}
	groups := store.NewClientGroups(db)
	if err := groups.Create(ctx, &store.ClientGroup{Name: "before"}); err != nil {
		t.Fatal(err)
	}
	backup, err := db.Backup(ctx, backups, "test")
	if err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(backup); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("backup %s: %v, %v", backup, info, err)
	}
	if err := groups.Create(ctx, &store.ClientGroup{Name: "after"}); err != nil {
		t.Fatal(err)
	}

	// The control plane still has the database open.
	if _, err := store.Restore(ctx, cfg, backup, backups, storetest.Logger()); !errors.Is(err, store.ErrInUse) {
		t.Fatalf("Restore while open: err = %v, want ErrInUse", err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	res, err := store.Restore(ctx, cfg, backup, backups, storetest.Logger())
	if err != nil {
		t.Fatal(err)
	}
	latest, _ := store.LatestVersion(store.SQLite)
	if res.SchemaVersion != latest || res.PreRestorePath == "" {
		t.Errorf("Restore = %+v", res)
	}

	db = storetest.OpenUnmigrated(t, cfg)
	if got := groupNames(t, db); len(got) != 1 || got[0] != "before" {
		t.Errorf("restored groups = %v, want [before]", got)
	}
	if v, err := db.SchemaVersion(ctx); err != nil || v != latest {
		t.Errorf("restored SchemaVersion = %d, %v", v, err)
	}

	// The replaced database was kept and can itself be restored.
	pre := storetest.OpenUnmigrated(t, store.Config{Driver: store.SQLite, SQLitePath: res.PreRestorePath})
	if got := groupNames(t, pre); len(got) != 2 {
		t.Errorf("pre-restore backup groups = %v, want both", got)
	}
}

func TestRestoreIntoMissingDatabase(t *testing.T) {
	ctx := context.Background()
	src := storetest.Open(t, store.SQLite)
	backup, err := src.Backup(ctx, t.TempDir(), "test")
	if err != nil {
		t.Fatal(err)
	}
	cfg := storetest.Config(t, store.SQLite)
	res, err := store.Restore(ctx, cfg, backup, t.TempDir(), storetest.Logger())
	if err != nil {
		t.Fatal(err)
	}
	if res.PreRestorePath != "" {
		t.Errorf("PreRestorePath = %q with no existing database", res.PreRestorePath)
	}
	if _, err := os.Stat(cfg.SQLitePath); err != nil {
		t.Error(err)
	}
}

func TestRestoreRejectsBadBackups(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	garbage := filepath.Join(dir, "garbage.db")
	if err := os.WriteFile(garbage, []byte("definitely not sqlite, but long enough to look like a header......"), 0o600); err != nil {
		t.Fatal(err)
	}

	empty := storetest.OpenUnmigrated(t, storetest.Config(t, store.SQLite))
	if _, err := empty.ExecContext(ctx, "CREATE TABLE unrelated (x INTEGER)"); err != nil {
		t.Fatal(err)
	}
	notPressFleet, err := empty.Backup(ctx, dir, "unrelated")
	if err != nil {
		t.Fatal(err)
	}

	future := storetest.Open(t, store.SQLite)
	if _, err := future.ExecContext(ctx, "INSERT INTO schema_migrations (version, name, applied_at) VALUES (9999, 'future', ?)", time.Now()); err != nil {
		t.Fatal(err)
	}
	tooNew, err := future.Backup(ctx, dir, "future")
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name, path string
		is         error
	}{
		{"missing", filepath.Join(dir, "nope.db"), os.ErrNotExist},
		{"garbage", garbage, nil},
		{"not a PressFleet database", notPressFleet, nil},
		{"schema too new", tooNew, store.ErrSchemaTooNew},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := storetest.Config(t, store.SQLite)
			_, err := store.Restore(ctx, cfg, tt.path, dir, storetest.Logger())
			if err == nil || (tt.is != nil && !errors.Is(err, tt.is)) {
				t.Fatalf("err = %v, want %v", err, tt.is)
			}
			if _, err := os.Stat(cfg.SQLitePath); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("rejected restore created the database: %v", err)
			}
		})
	}
}

func TestPostgresBackupUnsupported(t *testing.T) {
	ctx := context.Background()
	cfg := store.Config{Driver: store.Postgres, PostgresDSN: "postgres://localhost/x"}
	if _, err := store.Restore(ctx, cfg, "x.db", t.TempDir(), storetest.Logger()); !errors.Is(err, store.ErrUnsupported) {
		t.Errorf("Restore err = %v, want ErrUnsupported", err)
	}
	db := storetest.Open(t, store.Postgres)
	if _, err := db.Backup(ctx, t.TempDir(), "x"); !errors.Is(err, store.ErrUnsupported) {
		t.Errorf("Backup err = %v, want ErrUnsupported", err)
	}
}
