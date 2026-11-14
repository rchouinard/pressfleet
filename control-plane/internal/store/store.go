package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/stdlib"
	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"

	"github.com/rchouinard/pressfleet/control-plane/internal/redact"
)

// Dialect identifies the database engine.
type Dialect string

// Supported dialects. The values match the database.driver setting.
const (
	SQLite   Dialect = "sqlite"
	Postgres Dialect = "postgres"
)

// Sentinel errors. Errors returned by this package wrap one of these where
// it applies, together with the underlying cause.
var (
	// ErrNotFound means no row matched.
	ErrNotFound = errors.New("not found")
	// ErrConflict means a unique constraint was violated.
	ErrConflict = errors.New("already exists")
	// ErrReference means a foreign key constraint was violated: the row
	// refers to something that doesn't exist, or something still refers to
	// the row being deleted.
	ErrReference = errors.New("invalid reference")
	// ErrRepositoryDeleting means the client group's backup repository is
	// being deleted, so no site can be created in the group (#351).
	ErrRepositoryDeleting = errors.New("the client group's backup repository is being deleted")
	// ErrSchemaTooNew means the database was migrated by a newer pressfleet.
	ErrSchemaTooNew = errors.New("database schema is newer than this pressfleet binary supports")
	// ErrInUse means another process has the SQLite database open.
	ErrInUse = errors.New("database is in use by another pressfleet process")
	// ErrUnsupported means the operation isn't available for the dialect.
	ErrUnsupported = errors.New("not supported for this database driver")
)

// Config selects and locates the database.
type Config struct {
	Driver Dialect
	// SQLitePath is the database file for the sqlite driver.
	SQLitePath string
	// PostgresDSN is the connection string for the postgres driver.
	PostgresDSN redact.Secret
	// Version is the release of the running binary, which Migrate records
	// in its migration markers.
	Version string
}

// DB is an open state database. It is a Querier outside any transaction.
type DB struct {
	querier
	db      *sql.DB
	log     *slog.Logger
	unlock  func() error
	version string
}

// Open connects to the database. It does not migrate; call Migrate.
func Open(ctx context.Context, cfg Config, log *slog.Logger) (*DB, error) {
	var (
		sqldb  *sql.DB
		unlock = func() error { return nil }
		err    error
	)
	switch cfg.Driver {
	case SQLite:
		if unlock, err = lockSQLite(cfg.SQLitePath, false); err != nil {
			return nil, err
		}
		if err = createPrivate(cfg.SQLitePath); err == nil {
			sqldb, err = openSQLite(cfg.SQLitePath, false)
		}
	case Postgres:
		sqldb, err = openPostgres(cfg.PostgresDSN)
	default:
		return nil, fmt.Errorf("unknown database driver %q", cfg.Driver)
	}
	if err == nil {
		err = sqldb.PingContext(ctx)
	}
	if err != nil {
		if sqldb != nil {
			_ = sqldb.Close()
		}
		_ = unlock()
		return nil, fmt.Errorf("opening %s database: %w", cfg.Driver, err)
	}
	return &DB{
		querier: querier{ex: sqldb, dialect: cfg.Driver},
		db:      sqldb,
		log:     log,
		unlock:  unlock,
		version: cfg.Version,
	}, nil
}

// sqliteDSN returns the modernc.org/sqlite connection string for path.
//
// Every connection gets: foreign key enforcement (off by default in SQLite),
// WAL so readers don't block the writer, a busy timeout instead of immediate
// SQLITE_BUSY errors, IMMEDIATE transactions so a transaction that will write
// takes the write lock up front rather than failing on upgrade, and UTC
// timestamps in SQLite's own text format so its date functions understand
// them. readOnly is for inspecting backup files.
func sqliteDSN(path string, readOnly bool) string {
	q := "_pragma=foreign_keys(1)&_pragma=busy_timeout(10000)&_txlock=immediate&_time_format=sqlite&_timezone=UTC"
	if readOnly {
		// immutable=1 reads the file without locking it or creating -wal
		// and -shm files next to it; only used for backups nobody writes.
		q += "&mode=ro&immutable=1"
	} else {
		q += "&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)"
	}
	return "file:" + path + "?" + q
}

func openSQLite(path string, readOnly bool) (*sql.DB, error) {
	if path == "" {
		return nil, errors.New("sqlite path is empty")
	}
	if strings.ContainsAny(path, "?#%") {
		return nil, fmt.Errorf("sqlite path %q must not contain ?, #, or %%", path)
	}
	return sql.Open("sqlite", sqliteDSN(path, readOnly))
}

// createPrivate creates an empty database file readable only by its owner,
// if it doesn't exist. SQLite gives its -wal and -shm files the same mode.
// An empty file is a valid empty database.
func createPrivate(path string) error {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600) //nolint:gosec // G304: the path comes from operator configuration
	if err != nil {
		return err
	}
	return f.Close()
}

func openPostgres(dsn redact.Secret) (*sql.DB, error) {
	cfg, err := pgx.ParseConfig(dsn.Reveal())
	if err != nil {
		// pgx errors can echo the connection string, including the password.
		return nil, errors.New("invalid postgres DSN")
	}
	return stdlib.OpenDB(*cfg, stdlib.OptionAfterConnect(func(_ context.Context, c *pgx.Conn) error {
		// Return timestamptz values in UTC, matching SQLite.
		c.TypeMap().RegisterType(&pgtype.Type{
			Name:  "timestamptz",
			OID:   pgtype.TimestamptzOID,
			Codec: &pgtype.TimestamptzCodec{ScanLocation: time.UTC},
		})
		return nil
	})), nil
}

// Ping checks that the database is reachable. It is used as a readiness
// check.
func (db *DB) Ping(ctx context.Context) error {
	return db.db.PingContext(ctx)
}

// Close closes the database and releases the SQLite lock.
func (db *DB) Close() error {
	return errors.Join(db.db.Close(), db.unlock())
}

// InTx runs fn in a transaction, committing if it returns nil and rolling
// back otherwise, including when fn panics.
func (db *DB) InTx(ctx context.Context, fn func(q Querier) error) error {
	tx, err := db.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning transaction: %w", mapError(err))
	}
	// Unconditional, so a panic in fn (which the HTTP server and the job
	// pool recover) doesn't leave the transaction holding SQLite's write
	// lock until its context ends, which for some callers is never. After
	// Commit it is a no-op.
	defer func() { _ = tx.Rollback() }()
	if err := fn(querier{ex: tx, dialect: db.dialect}); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing transaction: %w", mapError(err))
	}
	return nil
}

// Querier runs queries on a database or transaction. Queries use ?
// placeholders, which are rewritten for the dialect; time.Time arguments are
// normalized to UTC with microsecond precision; and errors are mapped to this
// package's sentinel errors.
type Querier interface {
	Dialect() Dialect
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *Row
}

// Row is the result of QueryRowContext.
type Row struct {
	row *sql.Row
}

// Scan copies the row into dest. It returns ErrNotFound if there was no row.
func (r *Row) Scan(dest ...any) error {
	return mapError(r.row.Scan(dest...))
}

// execer is implemented by *sql.DB, *sql.Tx, and *sql.Conn.
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

type querier struct {
	ex      execer
	dialect Dialect
}

func (q querier) Dialect() Dialect { return q.dialect }

func (q querier) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	res, err := q.ex.ExecContext(ctx, q.rebind(query), normalizeArgs(args)...)
	return res, mapError(err)
}

func (q querier) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	rows, err := q.ex.QueryContext(ctx, q.rebind(query), normalizeArgs(args)...)
	return rows, mapError(err)
}

func (q querier) QueryRowContext(ctx context.Context, query string, args ...any) *Row {
	return &Row{row: q.ex.QueryRowContext(ctx, q.rebind(query), normalizeArgs(args)...)}
}

func (q querier) rebind(query string) string {
	if q.dialect != Postgres {
		return query
	}
	return rebindDollar(query)
}

// rebindDollar rewrites ? placeholders to $1, $2, ..., leaving ? inside
// quoted strings, quoted identifiers, and comments alone.
func rebindDollar(query string) string {
	var b strings.Builder
	b.Grow(len(query) + 8)
	n := 0
	for i := 0; i < len(query); i++ {
		c := query[i]
		switch {
		case c == '\'' || c == '"':
			end := strings.IndexByte(query[i+1:], c)
			if end < 0 {
				b.WriteString(query[i:])
				return b.String()
			}
			b.WriteString(query[i : i+end+2])
			i += end + 1
		case c == '-' && i+1 < len(query) && query[i+1] == '-':
			end := strings.IndexByte(query[i:], '\n')
			if end < 0 {
				b.WriteString(query[i:])
				return b.String()
			}
			b.WriteString(query[i : i+end])
			i += end - 1
		case c == '?':
			n++
			b.WriteByte('$')
			b.WriteString(strconv.Itoa(n))
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// normalizeArgs converts time arguments to UTC at microsecond precision
// (PostgreSQL's), so a value reads back the same on both dialects.
// It copies rather than modifying the caller's slice.
func normalizeArgs(args []any) []any {
	args = append([]any(nil), args...)
	for i, a := range args {
		switch v := a.(type) {
		case time.Time:
			args[i] = utc(v)
		case *time.Time:
			if v != nil {
				args[i] = utc(*v)
			}
		case sql.NullTime:
			if v.Valid {
				args[i] = utc(v.Time)
			}
		}
	}
	return args
}

func utc(t time.Time) time.Time {
	return t.UTC().Truncate(time.Microsecond)
}

// now returns the current time as stored in the database.
func now() time.Time { return utc(time.Now()) }

// mapError wraps driver errors in this package's sentinel errors.
func mapError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	var sqliteErr *sqlite.Error
	if errors.As(err, &sqliteErr) {
		switch sqliteErr.Code() {
		case sqlite3.SQLITE_CONSTRAINT_UNIQUE, sqlite3.SQLITE_CONSTRAINT_PRIMARYKEY:
			return fmt.Errorf("%w: %w", ErrConflict, err)
		case sqlite3.SQLITE_CONSTRAINT_FOREIGNKEY:
			return fmt.Errorf("%w: %w", ErrReference, err)
		case sqlite3.SQLITE_CONSTRAINT_TRIGGER:
			// ON DELETE RESTRICT is enforced like a trigger and reported
			// with this code.
			if strings.Contains(err.Error(), "FOREIGN KEY constraint failed") {
				return fmt.Errorf("%w: %w", ErrReference, err)
			}
		}
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "23505": // unique_violation
			return fmt.Errorf("%w: %w", ErrConflict, err)
		case "23503", "23001": // foreign_key_violation, restrict_violation (ON DELETE RESTRICT)
			return fmt.Errorf("%w: %w", ErrReference, err)
		}
	}
	return err
}
