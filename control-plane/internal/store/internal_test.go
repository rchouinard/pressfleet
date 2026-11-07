package store

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRebindDollar(t *testing.T) {
	tests := []struct{ in, want string }{
		{"SELECT 1", "SELECT 1"},
		{"SELECT * FROM t WHERE a = ? AND b = ?", "SELECT * FROM t WHERE a = $1 AND b = $2"},
		{"INSERT INTO t VALUES (?, '?', \"?\", ?)", "INSERT INTO t VALUES ($1, '?', \"?\", $2)"},
		{"SELECT 'it''s ?' , ?", "SELECT 'it''s ?' , $1"},
		{"SELECT ? -- what?\nFROM t WHERE x = ?", "SELECT $1 -- what?\nFROM t WHERE x = $2"},
		{"SELECT ? -- trailing?", "SELECT $1 -- trailing?"},
		{"SELECT 'unterminated ?", "SELECT 'unterminated ?"},
		{"SELECT a - ? FROM t", "SELECT a - $1 FROM t"},
	}
	for _, tt := range tests {
		if got := rebindDollar(tt.in); got != tt.want {
			t.Errorf("rebindDollar(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestNormalizeArgs(t *testing.T) {
	ts := time.Date(2026, 9, 26, 8, 0, 0, 123456789, time.FixedZone("EDT", -4*3600))
	in := []any{ts, &ts, "x"}
	out := normalizeArgs(in)
	want := time.Date(2026, 9, 26, 12, 0, 0, 123456000, time.UTC)
	for i := range 2 {
		got, ok := out[i].(time.Time)
		if !ok || got != want {
			t.Errorf("out[%d] = %#v, want %v", i, out[i], want)
		}
	}
	if in[0] != ts {
		t.Error("normalizeArgs modified the caller's slice")
	}
}

// TestBackupBeforeUpgrade migrates a database with data from one schema
// version to the next and checks that the backup holds the old state.
func TestBackupBeforeUpgrade(t *testing.T) {
	ctx := context.Background()
	all, err := Migrations(SQLite)
	if err != nil {
		t.Fatal(err)
	}
	next := Migration{Version: len(all) + 1, Name: "test_next", sql: "CREATE TABLE test_next (x INTEGER)"}

	dir := t.TempDir()
	cfg := Config{Driver: SQLite, SQLitePath: filepath.Join(dir, "pressfleet.db")}
	db, err := Open(ctx, cfg, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	backups := filepath.Join(dir, "backups")

	res, err := db.migrate(ctx, backups, all)
	if err != nil {
		t.Fatal(err)
	}
	if res.BackupPath != "" {
		t.Errorf("backed up an empty database: %s", res.BackupPath)
	}
	g := &ClientGroup{Name: "Acme"}
	if err := NewClientGroups(db).Create(ctx, g); err != nil {
		t.Fatal(err)
	}

	res, err = db.migrate(ctx, backups, append(all, next))
	if err != nil {
		t.Fatal(err)
	}
	if res.From != len(all) || res.To != next.Version {
		t.Errorf("result = %+v", res)
	}
	if !strings.HasPrefix(res.BackupPath, backups+string(filepath.Separator)) ||
		!strings.HasSuffix(res.BackupPath, fmt.Sprintf("-schema%04d.db", len(all))) {
		t.Fatalf("BackupPath = %q", res.BackupPath)
	}

	version, err := checkBackup(ctx, res.BackupPath)
	if err != nil || version != len(all) {
		t.Fatalf("checkBackup = %d, %v; want %d", version, err, len(all))
	}
	bdb, err := openSQLite(res.BackupPath, true)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = bdb.Close() }()
	bq := querier{ex: bdb, dialect: SQLite}
	if got, err := NewClientGroups(bq).Get(ctx, g.ID); err != nil || got.Name != "Acme" {
		t.Errorf("backup missing client group: %+v, %v", got, err)
	}
	if exists, err := tableExists(ctx, bq, "test_next"); err != nil || exists {
		t.Errorf("backup contains the new migration's table (exists=%v, err=%v)", exists, err)
	}
	if exists, err := tableExists(ctx, db.querier, "test_next"); err != nil || !exists {
		t.Errorf("migration not applied (exists=%v, err=%v)", exists, err)
	}
}

func TestFloorDiv(t *testing.T) {
	tests := []struct{ a, b, want int64 }{
		{7, 2, 3}, {6, 2, 3}, {0, 3, 0},
		{-7, 2, -4}, {-6, 2, -3}, {-1, 3, -1},
		{7, -2, -4}, {-7, -2, 3},
	}
	for _, tt := range tests {
		if got := floorDiv(tt.a, tt.b); got != tt.want {
			t.Errorf("floorDiv(%d, %d) = %d, want %d", tt.a, tt.b, got, tt.want)
		}
	}
}

// TestTestMigration checks the hook the e2emigration build tag sets
// (migrate_e2e.go): its migration comes after the embedded ones, in both
// dialects, and applies like any other.
func TestTestMigration(t *testing.T) {
	ctx := context.Background()
	embedded := map[Dialect]int{}
	for _, d := range []Dialect{SQLite, Postgres} {
		ms, err := Migrations(d)
		if err != nil {
			t.Fatal(err)
		}
		embedded[d] = len(ms)
	}
	old := testMigration
	testMigration = "SELECT 1"
	t.Cleanup(func() { testMigration = old })
	for _, d := range []Dialect{SQLite, Postgres} {
		ms, err := Migrations(d)
		if err != nil {
			t.Fatal(err)
		}
		if last := ms[len(ms)-1]; len(ms) != embedded[d]+1 || last.Version != len(ms) || last.Name != "e2e_noop" {
			t.Errorf("%s: the last of %d migrations is %+v, want e2e_noop after %d", d, len(ms), last, embedded[d])
		}
	}

	db, err := Open(ctx, Config{Driver: SQLite, SQLitePath: filepath.Join(t.TempDir(), "pressfleet.db")}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	res, err := db.Migrate(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if res.To != embedded[SQLite]+1 {
		t.Errorf("migrated to schema version %d, want %d", res.To, embedded[SQLite]+1)
	}
}
