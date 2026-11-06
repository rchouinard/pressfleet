package store_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
)

// markers reads the migration markers in dir, oldest first, and fails on
// anything else there but backups.
func markers(t *testing.T, dir string) []store.MigrationMarker {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var ms []store.MigrationMarker
	for _, e := range entries { // ReadDir sorts by name, which is by time
		name := e.Name()
		if strings.HasSuffix(name, ".db") {
			continue
		}
		at, ok := store.MarkerTime(name)
		if !ok {
			t.Errorf("unexpected file %s in the backups directory", name)
			continue
		}
		info, err := e.Info()
		if err != nil {
			t.Fatal(err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("%s has mode %o, want 600", name, perm)
		}
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		m, err := store.ParseMigrationMarker(data)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !m.StartedAt.Truncate(time.Millisecond).Equal(at) {
			t.Errorf("%s: started at %s, not the name's time", name, m.StartedAt)
		}
		ms = append(ms, m)
	}
	return ms
}

func TestMigrationMarkers(t *testing.T) {
	ctx := context.Background()
	for _, d := range storetest.Dialects {
		t.Run(string(d), func(t *testing.T) {
			cfg := storetest.Config(t, d)
			cfg.Version = "v1.2.3"
			db := storetest.OpenUnmigrated(t, cfg)
			latest, err := store.LatestVersion(d)
			if err != nil {
				t.Fatal(err)
			}
			// Data to protect, so SQLite backs up before migrating.
			if _, err := db.ExecContext(ctx, "CREATE TABLE legacy (x INTEGER)"); err != nil {
				t.Fatal(err)
			}
			dir := filepath.Join(t.TempDir(), "backups")
			before := time.Now()

			res, err := db.Migrate(ctx, dir)
			if err != nil {
				t.Fatal(err)
			}
			ms := markers(t, dir)
			if len(ms) != 1 {
				t.Fatalf("%d markers after migrating, want 1", len(ms))
			}
			m := ms[0]
			if m.Status != store.MigrationSucceeded || m.Version != "v1.2.3" || m.Dialect != d ||
				m.FromSchema != 0 || m.ToSchema != latest || m.ReachedSchema != latest || m.Error != "" ||
				m.Backup != res.BackupPath || !m.Changed() {
				t.Errorf("marker = %+v, result %+v", m, res)
			}
			if (d == store.SQLite) != (m.Backup != "") {
				t.Errorf("marker backup = %q for %s", m.Backup, d)
			}
			if m.StartedAt.Before(before.Add(-time.Second)) || m.FinishedAt.Before(m.StartedAt) {
				t.Errorf("marker times %s to %s, migration began %s", m.StartedAt, m.FinishedAt, before)
			}

			// Nothing pending: no new marker.
			if _, err := db.Migrate(ctx, dir); err != nil {
				t.Fatal(err)
			}
			if got := len(markers(t, dir)); got != 1 {
				t.Fatalf("%d markers after a run with nothing pending, want 1", got)
			}

			// A run that fails at its second migration.
			res, err = db.MigrateWithExtra(ctx, dir, "CREATE TABLE test_extra_ok (x INTEGER)", "THIS IS NOT SQL")
			if err == nil {
				t.Fatal("a broken migration succeeded")
			}
			ms = markers(t, dir)
			if len(ms) != 2 {
				t.Fatalf("%d markers after a failed run, want 2", len(ms))
			}
			m = ms[1]
			if m.Status != store.MigrationFailed || m.FromSchema != latest || m.ToSchema != latest+2 ||
				m.ReachedSchema != latest+1 || !strings.Contains(m.Error, "test_extra") ||
				m.Backup != res.BackupPath || m.FinishedAt.IsZero() || !m.Changed() {
				t.Errorf("failed run's marker = %+v, result %+v", m, res)
			}
			if (d == store.SQLite) != (m.Backup != "") {
				t.Errorf("failed run's marker backup = %q for %s", m.Backup, d)
			}
		})
	}
}

// Without a marker the CLI would take a migration for none, so Migrate
// changes nothing if it can't write one.
func TestMigrateWithoutMarker(t *testing.T) {
	ctx := context.Background()
	for _, d := range storetest.Dialects {
		t.Run(string(d), func(t *testing.T) {
			db := storetest.OpenUnmigrated(t, storetest.Config(t, d))
			notDir := filepath.Join(t.TempDir(), "file")
			if err := os.WriteFile(notDir, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Migrate(ctx, notDir); err == nil || !strings.Contains(err.Error(), "migration marker") {
				t.Fatalf("Migrate error = %v, want one about the marker", err)
			}
			if v, err := db.SchemaVersion(ctx); err != nil || v != 0 {
				t.Errorf("SchemaVersion = %d, %v; want 0", v, err)
			}
		})
	}
}

func TestMigrationMarkerChanged(t *testing.T) {
	tests := []struct {
		m    store.MigrationMarker
		want bool
	}{
		{store.MigrationMarker{Status: store.MigrationSucceeded, FromSchema: 3, ReachedSchema: 5}, true},
		{store.MigrationMarker{Status: store.MigrationFailed, FromSchema: 3, ReachedSchema: 4}, true},
		{store.MigrationMarker{Status: store.MigrationFailed, FromSchema: 3, ReachedSchema: 3}, false},
		{store.MigrationMarker{Status: store.MigrationRunning, FromSchema: 3, ReachedSchema: 3}, true},
	}
	for _, tt := range tests {
		if got := tt.m.Changed(); got != tt.want {
			t.Errorf("%+v: Changed = %t, want %t", tt.m, got, tt.want)
		}
	}
}

func TestParseMigrationMarker(t *testing.T) {
	for _, bad := range []string{"", "{", `{"status":"done"}`} {
		if _, err := store.ParseMigrationMarker([]byte(bad)); err == nil {
			t.Errorf("ParseMigrationMarker(%q) succeeded", bad)
		}
	}
}
