// Package storetest provides state databases for tests, on both dialects.
//
// SQLite tests always run. PostgreSQL tests run when PRESSFLEET_TEST_POSTGRES_DSN
// names a server the tests may create schemas on, for example
//
//	PRESSFLEET_TEST_POSTGRES_DSN=postgres://postgres:test@localhost:55432/postgres?sslmode=disable
//
// and are skipped otherwise. Each test gets its own PostgreSQL schema, dropped
// when the test ends.
package storetest

import (
	"context"
	"database/sql"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" driver used to create test schemas

	"github.com/rchouinard/pressfleet/control-plane/internal/id"
	"github.com/rchouinard/pressfleet/control-plane/internal/redact"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
)

// EnvPostgresDSN names the environment variable that enables PostgreSQL tests.
const EnvPostgresDSN = "PRESSFLEET_TEST_POSTGRES_DSN"

// Dialects lists every supported dialect.
var Dialects = []store.Dialect{store.SQLite, store.Postgres}

// Run runs fn as a subtest for each dialect, with a fresh migrated database.
// Use it for every test that touches the database.
func Run(t *testing.T, fn func(t *testing.T, db *store.DB)) {
	t.Helper()
	for _, d := range Dialects {
		t.Run(string(d), func(t *testing.T) {
			fn(t, Open(t, d))
		})
	}
}

// Open returns a fresh, migrated database, closed when the test ends. It
// skips the test if the dialect isn't available.
func Open(t testing.TB, d store.Dialect) *store.DB {
	t.Helper()
	db := OpenUnmigrated(t, Config(t, d))
	if _, err := db.Migrate(context.Background(), filepath.Join(t.TempDir(), "backups")); err != nil {
		t.Fatalf("migrating %s test database: %v", d, err)
	}
	return db
}

// OpenUnmigrated opens the database described by cfg without migrating it,
// closing it when the test ends.
func OpenUnmigrated(t testing.TB, cfg store.Config) *store.DB {
	t.Helper()
	db, err := store.Open(context.Background(), cfg, Logger())
	if err != nil {
		t.Fatalf("opening %s test database: %v", cfg.Driver, err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// Config returns the configuration for a new, empty database without opening
// it, for tests of opening, migrating, and restoring. It skips the test if
// the dialect isn't available.
func Config(t testing.TB, d store.Dialect) store.Config {
	t.Helper()
	switch d {
	case store.SQLite:
		return store.Config{Driver: store.SQLite, SQLitePath: filepath.Join(t.TempDir(), "state", "pressfleet.db")}
	case store.Postgres:
		return store.Config{Driver: store.Postgres, PostgresDSN: redact.Secret(postgresSchema(t))}
	default:
		t.Fatalf("unknown dialect %q", d)
		return store.Config{}
	}
}

// Logger discards log output.
func Logger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

// postgresSchema creates a schema for the test and returns a DSN that uses it.
func postgresSchema(t testing.TB) string {
	t.Helper()
	dsn := os.Getenv(EnvPostgresDSN)
	if dsn == "" {
		t.Skipf("PostgreSQL tests skipped: set %s to run them", EnvPostgresDSN)
	}
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("connecting to %s: %v", EnvPostgresDSN, err)
	}
	t.Cleanup(func() { _ = admin.Close() })

	schema := "pf_test_" + id.New()
	ctx := context.Background()
	if _, err := admin.ExecContext(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatalf("creating test schema: %v", err)
	}
	t.Cleanup(func() {
		if _, err := admin.ExecContext(context.Background(), "DROP SCHEMA "+schema+" CASCADE"); err != nil {
			t.Errorf("dropping test schema: %v", err)
		}
	})
	return withSearchPath(t, dsn, schema)
}

// withSearchPath adds a search_path runtime parameter to a URL or
// keyword/value DSN.
func withSearchPath(t testing.TB, dsn, schema string) string {
	t.Helper()
	if !strings.HasPrefix(dsn, "postgres://") && !strings.HasPrefix(dsn, "postgresql://") {
		return dsn + " search_path=" + schema
	}
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parsing %s: invalid URL", EnvPostgresDSN)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	return u.String()
}
