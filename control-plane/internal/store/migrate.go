package store

import (
	"cmp"
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"slices"
	"strconv"
)

//go:embed migrations
var migrationFiles embed.FS

// Migration is one forward-only schema migration.
type Migration struct {
	Version int
	Name    string
	sql     string
}

var migrationName = regexp.MustCompile(`^(\d{4})_([a-z0-9_]+)\.sql$`)

// Migrations returns the migrations for a dialect in version order. Versions
// start at 1 and have no gaps.
func Migrations(d Dialect) ([]Migration, error) {
	dir := path.Join("migrations", string(d))
	entries, err := fs.ReadDir(migrationFiles, dir)
	if err != nil {
		return nil, fmt.Errorf("reading %s migrations: %w", d, err)
	}
	var ms []Migration
	for _, e := range entries {
		m := migrationName.FindStringSubmatch(e.Name())
		if m == nil {
			return nil, fmt.Errorf("migration %s/%s: name must look like 0001_description.sql", dir, e.Name())
		}
		version, _ := strconv.Atoi(m[1])
		body, err := fs.ReadFile(migrationFiles, path.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		ms = append(ms, Migration{Version: version, Name: m[2], sql: string(body)})
	}
	slices.SortFunc(ms, func(a, b Migration) int { return cmp.Compare(a.Version, b.Version) })
	for i, m := range ms {
		if m.Version != i+1 {
			return nil, fmt.Errorf("%s migrations: expected version %04d, found %04d", d, i+1, m.Version)
		}
	}
	return ms, nil
}

// LatestVersion is the newest schema version this binary knows.
func LatestVersion(d Dialect) (int, error) {
	ms, err := Migrations(d)
	if err != nil {
		return 0, err
	}
	return len(ms), nil
}

// SchemaVersion returns the version the database is migrated to, or 0 for a
// database that has never been migrated.
func (db *DB) SchemaVersion(ctx context.Context) (int, error) {
	return schemaVersion(ctx, db.querier)
}

func schemaVersion(ctx context.Context, q querier) (int, error) {
	exists, err := tableExists(ctx, q, "schema_migrations")
	if err != nil || !exists {
		return 0, err
	}
	var v sql.NullInt64
	if err := q.QueryRowContext(ctx, "SELECT MAX(version) FROM schema_migrations").Scan(&v); err != nil {
		return 0, fmt.Errorf("reading schema version: %w", err)
	}
	return int(v.Int64), nil
}

func tableExists(ctx context.Context, q querier, name string) (bool, error) {
	query := "SELECT COUNT(*) FROM sqlite_schema WHERE type = 'table' AND name = ?"
	if q.dialect == Postgres {
		// to_regclass resolves the name through search_path, like the queries do.
		query = "SELECT COUNT(*) FROM (SELECT to_regclass(?) AS t) r WHERE r.t IS NOT NULL"
	}
	var n int
	if err := q.QueryRowContext(ctx, query, name).Scan(&n); err != nil {
		return false, fmt.Errorf("checking for table %s: %w", name, err)
	}
	return n > 0, nil
}

// MigrateResult describes what Migrate did.
type MigrateResult struct {
	// From and To are the schema versions before and after.
	From, To int
	// BackupPath is the pre-migration backup, or empty if none was taken:
	// nothing was pending, the database was empty, or it is PostgreSQL.
	BackupPath string
}

// Migrate brings the schema up to date. If migrations are pending and the
// database already holds data, it first backs up an SQLite database into
// backupDir; for PostgreSQL it logs a reminder that backups are the
// operator's job. Each run with migrations pending, on either dialect,
// records itself in a MigrationMarker in backupDir. It returns
// ErrSchemaTooNew, changing nothing, if the database was migrated by a
// newer binary.
func (db *DB) Migrate(ctx context.Context, backupDir string) (MigrateResult, error) {
	ms, err := Migrations(db.dialect)
	if err != nil {
		return MigrateResult{}, err
	}
	return db.migrate(ctx, backupDir, ms)
}

// migrate applies ms, which tests can vary.
func (db *DB) migrate(ctx context.Context, backupDir string, ms []Migration) (MigrateResult, error) {
	var res MigrateResult
	latest := len(ms)

	// Migrate on a single connection so the PostgreSQL advisory lock, which
	// belongs to a session, covers every step.
	conn, err := db.db.Conn(ctx)
	if err != nil {
		return res, fmt.Errorf("migrating: %w", err)
	}
	defer func() { _ = conn.Close() }()
	q := querier{ex: conn, dialect: db.dialect}

	if db.dialect == Postgres {
		// Serializes concurrent control planes starting against one database.
		if _, err := q.ExecContext(ctx, "SELECT pg_advisory_lock(?)", migrationLockKey); err != nil {
			return res, fmt.Errorf("taking migration lock: %w", err)
		}
		defer func() {
			// Use a fresh context: the lock must be released even if ctx was canceled.
			_, _ = q.ExecContext(context.WithoutCancel(ctx), "SELECT pg_advisory_unlock(?)", migrationLockKey)
		}()
	}

	if res.From, err = schemaVersion(ctx, q); err != nil {
		return res, err
	}
	res.To = res.From
	if res.From > latest {
		return res, fmt.Errorf("%w: database is at schema version %d, this binary supports up to %d; "+
			"run a pressfleet release that supports this schema, or restore a backup taken before the upgrade",
			ErrSchemaTooNew, res.From, latest)
	}
	if res.From == latest {
		return res, nil
	}

	if err := db.backupBeforeMigrate(ctx, q, backupDir, res.From, latest, &res); err != nil {
		return res, err
	}

	// The marker must exist before anything changes: without one, the host
	// CLI would conclude that an upgrade didn't migrate.
	marker, err := newMarker(backupDir, MigrationMarker{
		Status:        MigrationRunning,
		StartedAt:     now(),
		Version:       db.version,
		Dialect:       db.dialect,
		FromSchema:    res.From,
		ToSchema:      latest,
		ReachedSchema: res.From,
		Backup:        res.BackupPath,
	})
	if err != nil {
		return res, err
	}
	err = db.applyMigrations(ctx, conn, q, ms[res.From:], &res)
	if merr := marker.finish(res.To, err); merr != nil {
		// A marker left running still tells the CLI a migration may have
		// run, so this isn't worth failing a successful migration for.
		db.log.Warn("recording the end of the migrations failed", "path", marker.path, "error", merr)
	}
	return res, err
}

func (db *DB) applyMigrations(ctx context.Context, conn *sql.Conn, q querier, ms []Migration, res *MigrateResult) error {
	if _, err := q.ExecContext(ctx, createSchemaMigrations[db.dialect]); err != nil {
		return fmt.Errorf("creating schema_migrations: %w", err)
	}
	for _, m := range ms {
		if err := applyMigration(ctx, conn, db.dialect, m); err != nil {
			return err
		}
		res.To = m.Version
		db.log.Info("applied database migration", "version", m.Version, "name", m.Name)
	}
	return nil
}

// migrationLockKey is an arbitrary constant identifying PressFleet's
// migration advisory lock.
const migrationLockKey int64 = 0x7072657373666c74 // "pressflt"

var createSchemaMigrations = map[Dialect]string{
	SQLite: `CREATE TABLE IF NOT EXISTS schema_migrations (
		version    INTEGER NOT NULL PRIMARY KEY,
		name       TEXT NOT NULL,
		applied_at TIMESTAMP NOT NULL)`,
	Postgres: `CREATE TABLE IF NOT EXISTS schema_migrations (
		version    INTEGER NOT NULL PRIMARY KEY,
		name       TEXT NOT NULL,
		applied_at TIMESTAMPTZ NOT NULL)`,
}

func (db *DB) backupBeforeMigrate(ctx context.Context, q querier, backupDir string, from, to int, res *MigrateResult) error {
	if db.dialect == Postgres {
		if from == 0 {
			return nil // nothing of PressFleet's to lose yet
		}
		db.log.Warn("applying database migrations to PostgreSQL without a backup; PostgreSQL backups are the operator's responsibility",
			"from_version", from, "to_version", to)
		return nil
	}
	var tables int
	if err := q.QueryRowContext(ctx, "SELECT COUNT(*) FROM sqlite_schema WHERE type = 'table'").Scan(&tables); err != nil {
		return fmt.Errorf("inspecting database: %w", err)
	}
	if tables == 0 {
		// A new, empty database has nothing to protect.
		return nil
	}
	p, err := backupSQLite(ctx, q, backupDir, fmt.Sprintf("schema%04d", from))
	if err != nil {
		return fmt.Errorf("backing up database before migrating: %w", err)
	}
	res.BackupPath = p
	db.log.Info("backed up database before migrating", "path", p, "schema_version", from)
	return nil
}

// applyMigration runs one migration and records it in the same transaction,
// so a failed migration leaves no trace. It is skipped if another process
// applied it first.
func applyMigration(ctx context.Context, conn *sql.Conn, d Dialect, m Migration) (err error) {
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("migration %04d: %w", m.Version, err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()
	q := querier{ex: tx, dialect: d}
	var n int
	if err := q.QueryRowContext(ctx, "SELECT COUNT(*) FROM schema_migrations WHERE version = ?", m.Version).Scan(&n); err != nil {
		return fmt.Errorf("migration %04d: %w", m.Version, err)
	}
	if n > 0 {
		return tx.Rollback()
	}
	// Run the file as-is: no placeholder rewriting, and no arguments so both
	// drivers accept several statements in one call.
	if _, err := tx.ExecContext(ctx, m.sql); err != nil {
		return fmt.Errorf("migration %04d_%s: %w", m.Version, m.Name, err)
	}
	if _, err := q.ExecContext(ctx, "INSERT INTO schema_migrations (version, name, applied_at) VALUES (?, ?, ?)",
		m.Version, m.Name, now()); err != nil {
		return fmt.Errorf("migration %04d: recording version: %w", m.Version, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migration %04d: %w", m.Version, err)
	}
	return nil
}
