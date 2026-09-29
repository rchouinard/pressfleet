package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/id"
)

// DatabaseExportStatus is the state of a database export.
type DatabaseExportStatus string

// Database export statuses. A running export becomes ready or failed, and
// a ready one becomes expired.
const (
	DatabaseExportRunning DatabaseExportStatus = "running"
	DatabaseExportReady   DatabaseExportStatus = "ready"
	DatabaseExportFailed  DatabaseExportStatus = "failed"
	DatabaseExportExpired DatabaseExportStatus = "expired"
)

// DatabaseExport is the record of one export of a site's database (design
// doc §5.8): a compressed dump that the user downloads before it expires.
type DatabaseExport struct {
	ID     string
	SiteID string
	Status DatabaseExportStatus
	// SizeBytes is the size of the compressed dump; zero until it is ready.
	SizeBytes int64
	// Message is why the export failed. It never holds secrets.
	Message string
	// JobID is the export job; empty once that job is gone.
	JobID string
	// CreatedAt is when the job began the export, ReadyAt when the file
	// was complete, ExpiresAt when it stops being downloadable, and
	// ExpiredAt when its file was deleted. All but CreatedAt are zero until
	// they happen.
	CreatedAt time.Time
	ReadyAt   time.Time
	ExpiresAt time.Time
	ExpiredAt time.Time
}

// DatabaseExports reads and writes database export records.
type DatabaseExports struct {
	q Querier
}

// NewDatabaseExports returns a DatabaseExports that runs queries on q.
func NewDatabaseExports(q Querier) DatabaseExports {
	return DatabaseExports{q: q}
}

const databaseExportColumns = "id, site_id, status, size_bytes, message, job_id, created_at, ready_at, expires_at, expired_at"

func scanDatabaseExport(s scanner) (DatabaseExport, error) {
	var (
		e                       DatabaseExport
		size                    sql.NullInt64
		job                     sql.NullString
		ready, expires, expired sql.NullTime
	)
	err := s.Scan(&e.ID, &e.SiteID, &e.Status, &size, &e.Message, &job, &e.CreatedAt, &ready, &expires, &expired)
	e.SizeBytes, e.JobID = size.Int64, job.String
	if ready.Valid {
		e.ReadyAt = ready.Time
	}
	if expires.Valid {
		e.ExpiresAt = expires.Time
	}
	if expired.Valid {
		e.ExpiredAt = expired.Time
	}
	return e, err
}

// Create inserts e as a running export. e.ID may be set in advance (a job
// records the ID before creating the row, so a retry finds it); otherwise
// it is generated. CreatedAt is set to now. It returns ErrConflict if an
// export with the ID exists.
func (r DatabaseExports) Create(ctx context.Context, e *DatabaseExport) error {
	if e.SiteID == "" {
		return errors.New("create database export: a site is required")
	}
	created := DatabaseExport{ID: e.ID, SiteID: e.SiteID, JobID: e.JobID, Status: DatabaseExportRunning, CreatedAt: now()}
	if created.ID == "" {
		created.ID = id.New()
	}
	_, err := r.q.ExecContext(ctx,
		"INSERT INTO database_exports (id, site_id, status, job_id, created_at) VALUES (?, ?, ?, ?, ?)",
		created.ID, created.SiteID, string(created.Status), nullString(created.JobID), created.CreatedAt)
	if err != nil {
		return fmt.Errorf("create database export: %w", err)
	}
	*e = created
	return nil
}

// Get returns the export with the given ID, or ErrNotFound.
func (r DatabaseExports) Get(ctx context.Context, exportID string) (DatabaseExport, error) {
	e, err := scanDatabaseExport(r.q.QueryRowContext(ctx,
		"SELECT "+databaseExportColumns+" FROM database_exports WHERE id = ?", exportID))
	if err != nil {
		return DatabaseExport{}, fmt.Errorf("get database export %s: %w", exportID, err)
	}
	return e, nil
}

// Ready records that the export's file is complete, sizeBytes long, and
// downloadable until expiresAt. Only a running export changes; it reports
// whether this one did.
func (r DatabaseExports) Ready(ctx context.Context, exportID string, sizeBytes int64, expiresAt time.Time) (bool, error) {
	return r.exec(ctx, "finish database export "+exportID,
		"UPDATE database_exports SET status = ?, size_bytes = ?, ready_at = ?, expires_at = ? WHERE id = ? AND status = ?",
		string(DatabaseExportReady), sizeBytes, now(), utc(expiresAt), exportID, string(DatabaseExportRunning))
}

// Fail records that the export failed with message, which must not hold
// secrets. Only a running export changes; it reports whether this one did.
func (r DatabaseExports) Fail(ctx context.Context, exportID, message string) (bool, error) {
	return r.exec(ctx, "fail database export "+exportID,
		"UPDATE database_exports SET status = ?, message = ? WHERE id = ? AND status = ?",
		string(DatabaseExportFailed), message, exportID, string(DatabaseExportRunning))
}

// Expire records that the export's file was deleted. Only a ready export
// changes; it reports whether this one did.
func (r DatabaseExports) Expire(ctx context.Context, exportID string) (bool, error) {
	return r.exec(ctx, "expire database export "+exportID,
		"UPDATE database_exports SET status = ?, expired_at = ? WHERE id = ? AND status = ?",
		string(DatabaseExportExpired), now(), exportID, string(DatabaseExportReady))
}

// ListExpired returns up to limit ready exports whose expiry is at or
// before t, oldest first.
func (r DatabaseExports) ListExpired(ctx context.Context, t time.Time, limit int) ([]DatabaseExport, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := r.q.QueryContext(ctx,
		"SELECT "+databaseExportColumns+" FROM database_exports WHERE status = ? AND expires_at <= ? ORDER BY id LIMIT ?",
		string(DatabaseExportReady), utc(t), limit)
	if err != nil {
		return nil, fmt.Errorf("list expired database exports: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []DatabaseExport
	for rows.Next() {
		e, err := scanDatabaseExport(rows)
		if err != nil {
			return nil, fmt.Errorf("list expired database exports: %w", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list expired database exports: %w", err)
	}
	return out, nil
}

func (r DatabaseExports) exec(ctx context.Context, what, query string, args ...any) (bool, error) {
	res, err := r.q.ExecContext(ctx, query, args...)
	if err != nil {
		return false, fmt.Errorf("%s: %w", what, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("%s: %w", what, err)
	}
	return n > 0, nil
}
