package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/id"
)

// ExportKind is what an export holds.
type ExportKind string

// Export kinds.
const (
	// ExportDatabase is a site's database (design doc §5.8).
	ExportDatabase ExportKind = "database"
	// ExportSite is a live site's wp-content and database, in the files
	// site import takes (§5.7).
	ExportSite ExportKind = "site"
	// ExportSnapshot is the same two files, from a backup snapshot.
	ExportSnapshot ExportKind = "snapshot"
)

// Valid reports whether k is a known kind.
func (k ExportKind) Valid() bool {
	switch k {
	case ExportDatabase, ExportSite, ExportSnapshot:
		return true
	}
	return false
}

// ExportStatus is the state of an export.
type ExportStatus string

// Export statuses. A running export becomes ready or failed, and a ready
// one becomes expired.
const (
	ExportRunning ExportStatus = "running"
	ExportReady   ExportStatus = "ready"
	ExportFailed  ExportStatus = "failed"
	ExportExpired ExportStatus = "expired"
)

// ExportFile is one file of a ready export.
type ExportFile struct {
	// Name is the file's name, e.g. "wp-content.tar.gz".
	Name      string
	SizeBytes int64
}

// ExportSource describes what an export holds, for whoever imports it.
// Fields are empty when unknown.
type ExportSource struct {
	// SiteURL is the site's URL (its home option for a snapshot).
	SiteURL string
	// TablePrefix is the WordPress table prefix of the database.
	TablePrefix   string
	WPCoreVersion string
	PHPVersion    string
	// TakenAt is when the site's content was read: when the export began,
	// or when the snapshot was taken.
	TakenAt time.Time
}

// Export is the record of one export (design doc §5.7, §5.8): files that
// the user downloads before they expire.
type Export struct {
	ID   string
	Kind ExportKind
	// SiteID is the exported site, which may have been deleted since.
	// SiteSlug and ClientGroupID are the site's when the export began.
	SiteID        string
	SiteSlug      string
	ClientGroupID string
	Status        ExportStatus
	// SizeBytes is the size of all files; zero until the export is ready.
	SizeBytes int64
	// Message is why the export failed. It never holds secrets.
	Message string
	// JobID is the export job; empty once that job is gone.
	JobID string
	// BackupID and SnapshotID are the backup a snapshot export reads.
	BackupID   string
	SnapshotID string
	Source     ExportSource
	// Files are the export's files, by name; none until it is ready.
	Files []ExportFile
	// CreatedAt is when the job began the export, ReadyAt when the files
	// were complete, ExpiresAt when they stop being downloadable, and
	// ExpiredAt when they were deleted. All but CreatedAt are zero until
	// they happen.
	CreatedAt time.Time
	ReadyAt   time.Time
	ExpiresAt time.Time
	ExpiredAt time.Time
}

// File returns the export's file with the given name.
func (e Export) File(name string) (ExportFile, bool) {
	for _, f := range e.Files {
		if f.Name == name {
			return f, true
		}
	}
	return ExportFile{}, false
}

// Exports reads and writes export records.
type Exports struct {
	q Querier
}

// NewExports returns an Exports that runs queries on q.
func NewExports(q Querier) Exports {
	return Exports{q: q}
}

const exportColumns = "id, kind, site_id, site_slug, client_group_id, status, size_bytes, message, job_id, " +
	"backup_id, snapshot_id, site_url, table_prefix, wp_core_version, php_version, taken_at, " +
	"created_at, ready_at, expires_at, expired_at"

func scanExport(s scanner) (Export, error) {
	var (
		e                              Export
		size                           sql.NullInt64
		job                            sql.NullString
		taken, ready, expires, expired sql.NullTime
	)
	err := s.Scan(&e.ID, &e.Kind, &e.SiteID, &e.SiteSlug, &e.ClientGroupID, &e.Status, &size, &e.Message, &job,
		&e.BackupID, &e.SnapshotID, &e.Source.SiteURL, &e.Source.TablePrefix, &e.Source.WPCoreVersion, &e.Source.PHPVersion,
		&taken, &e.CreatedAt, &ready, &expires, &expired)
	e.SizeBytes, e.JobID = size.Int64, job.String
	for _, t := range []struct {
		src sql.NullTime
		dst *time.Time
	}{{taken, &e.Source.TakenAt}, {ready, &e.ReadyAt}, {expires, &e.ExpiresAt}, {expired, &e.ExpiredAt}} {
		if t.src.Valid {
			*t.dst = t.src.Time
		}
	}
	return e, err
}

// Create inserts e as a running export without files. e.ID may be set in
// advance (a job records the ID before creating the row, so a retry finds
// it); otherwise it is generated. CreatedAt is set to now. It returns
// ErrConflict if an export with the ID exists.
func (r Exports) Create(ctx context.Context, e *Export) error {
	if !e.Kind.Valid() {
		return fmt.Errorf("create export: invalid kind %q", e.Kind)
	}
	if e.SiteID == "" {
		return errors.New("create export: a site is required")
	}
	created := *e
	if created.ID == "" {
		created.ID = id.New()
	}
	created.Status, created.SizeBytes, created.Message, created.Files = ExportRunning, 0, "", nil
	created.CreatedAt, created.ReadyAt, created.ExpiresAt, created.ExpiredAt = now(), time.Time{}, time.Time{}, time.Time{}
	src := created.Source
	_, err := r.q.ExecContext(ctx,
		"INSERT INTO exports (id, kind, site_id, site_slug, client_group_id, status, job_id, backup_id, snapshot_id, "+
			"site_url, table_prefix, wp_core_version, php_version, taken_at, created_at) "+
			"VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		created.ID, string(created.Kind), created.SiteID, created.SiteSlug, created.ClientGroupID, string(created.Status),
		nullString(created.JobID), created.BackupID, created.SnapshotID,
		src.SiteURL, src.TablePrefix, src.WPCoreVersion, src.PHPVersion, nullTime(utc(src.TakenAt)), created.CreatedAt)
	if err != nil {
		return fmt.Errorf("create export: %w", err)
	}
	*e = created
	return nil
}

// Get returns the export with the given ID, with its files, or
// ErrNotFound.
func (r Exports) Get(ctx context.Context, exportID string) (Export, error) {
	e, err := scanExport(r.q.QueryRowContext(ctx, "SELECT "+exportColumns+" FROM exports WHERE id = ?", exportID))
	if err != nil {
		return Export{}, fmt.Errorf("get export %s: %w", exportID, err)
	}
	if err := r.loadFiles(ctx, []*Export{&e}); err != nil {
		return Export{}, err
	}
	return e, nil
}

// loadFiles fills in the exports' files.
func (r Exports) loadFiles(ctx context.Context, exports []*Export) error {
	for _, e := range exports {
		e.Files = []ExportFile{}
		rows, err := r.q.QueryContext(ctx, "SELECT name, size_bytes FROM export_files WHERE export_id = ? ORDER BY name", e.ID)
		if err != nil {
			return fmt.Errorf("files of export %s: %w", e.ID, err)
		}
		for rows.Next() {
			var f ExportFile
			if err := rows.Scan(&f.Name, &f.SizeBytes); err != nil {
				_ = rows.Close()
				return fmt.Errorf("files of export %s: %w", e.ID, err)
			}
			e.Files = append(e.Files, f)
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			return fmt.Errorf("files of export %s: %w", e.ID, err)
		}
	}
	return nil
}

// Ready records that the export's files are complete and downloadable
// until expiresAt, together with what they hold. Only a running export
// changes; it reports whether this one did. Pass a transaction's Querier:
// the files are inserted after the export is updated.
func (r Exports) Ready(ctx context.Context, exportID string, files []ExportFile, src ExportSource, expiresAt time.Time) (bool, error) {
	if len(files) == 0 {
		return false, fmt.Errorf("finish export %s: no files", exportID)
	}
	var total int64
	for _, f := range files {
		total += f.SizeBytes
	}
	changed, err := r.exec(ctx, "finish export "+exportID,
		"UPDATE exports SET status = ?, size_bytes = ?, site_url = ?, table_prefix = ?, wp_core_version = ?, php_version = ?, "+
			"taken_at = ?, ready_at = ?, expires_at = ? WHERE id = ? AND status = ?",
		string(ExportReady), total, src.SiteURL, src.TablePrefix, src.WPCoreVersion, src.PHPVersion, nullTime(utc(src.TakenAt)),
		now(), utc(expiresAt), exportID, string(ExportRunning))
	if err != nil || !changed {
		return false, err
	}
	for _, f := range files {
		if _, err := r.q.ExecContext(ctx, "INSERT INTO export_files (export_id, name, size_bytes) VALUES (?, ?, ?)",
			exportID, f.Name, f.SizeBytes); err != nil {
			return false, fmt.Errorf("finish export %s: %w", exportID, err)
		}
	}
	return true, nil
}

// Fail records that the export failed with message, which must not hold
// secrets. Only a running export changes; it reports whether this one did.
func (r Exports) Fail(ctx context.Context, exportID, message string) (bool, error) {
	return r.exec(ctx, "fail export "+exportID,
		"UPDATE exports SET status = ?, message = ? WHERE id = ? AND status = ?",
		string(ExportFailed), message, exportID, string(ExportRunning))
}

// Expire records that the export's files were deleted. Only a ready export
// changes; it reports whether this one did. The file records stay, as the
// history of what the export held.
func (r Exports) Expire(ctx context.Context, exportID string) (bool, error) {
	return r.exec(ctx, "expire export "+exportID,
		"UPDATE exports SET status = ?, expired_at = ? WHERE id = ? AND status = ?",
		string(ExportExpired), now(), exportID, string(ExportReady))
}

// ListExpired returns up to limit ready exports whose expiry is at or
// before t, oldest first, without their files.
func (r Exports) ListExpired(ctx context.Context, t time.Time, limit int) ([]Export, error) {
	if limit <= 0 {
		limit = 100
	}
	return r.query(ctx, "list expired exports",
		"SELECT "+exportColumns+" FROM exports WHERE status = ? AND expires_at <= ? ORDER BY id LIMIT ?",
		string(ExportReady), utc(t), limit)
}

// ExportFilter selects a page of exports. SiteID or ClientGroupID is
// required.
type ExportFilter struct {
	SiteID        string
	ClientGroupID string
	// Kinds and Statuses, if set, keep exports of one of them.
	Kinds    []ExportKind
	Statuses []ExportStatus
	// Before is a cursor: only exports with a smaller ID (older ones) are
	// returned. Pass the previous page's NextCursor.
	Before string
	// Limit is the page size: default 50, at most 200.
	Limit int
}

// ExportPage is one page of List results, newest first.
type ExportPage struct {
	Exports []Export
	// NextCursor is the Before value for the next page, or empty on the
	// last page.
	NextCursor string
}

// List returns exports, with their files, newest first, one page at a
// time.
func (r Exports) List(ctx context.Context, f ExportFilter) (ExportPage, error) {
	if f.SiteID == "" && f.ClientGroupID == "" {
		return ExportPage{}, errors.New("list exports: a site or client group is required")
	}
	limit := f.Limit
	switch {
	case limit <= 0:
		limit = 50
	case limit > 200:
		limit = 200
	}
	query := "SELECT " + exportColumns + " FROM exports WHERE 1 = 1"
	var args []any
	if f.SiteID != "" {
		query += " AND site_id = ?"
		args = append(args, f.SiteID)
	}
	if f.ClientGroupID != "" {
		query += " AND client_group_id = ?"
		args = append(args, f.ClientGroupID)
	}
	query, args = inList(query, "kind", f.Kinds, args)
	query, args = inList(query, "status", f.Statuses, args)
	if f.Before != "" {
		query += " AND id < ?"
		args = append(args, f.Before)
	}
	// One extra row says whether there is another page.
	query += " ORDER BY id DESC LIMIT ?"
	args = append(args, limit+1)
	list, err := r.query(ctx, "list exports", query, args...)
	if err != nil {
		return ExportPage{}, err
	}
	page := ExportPage{Exports: list}
	if len(page.Exports) > limit {
		page.Exports = page.Exports[:limit]
		page.NextCursor = page.Exports[limit-1].ID
	}
	ptrs := make([]*Export, len(page.Exports))
	for i := range page.Exports {
		ptrs[i] = &page.Exports[i]
	}
	if err := r.loadFiles(ctx, ptrs); err != nil {
		return ExportPage{}, err
	}
	return page, nil
}

// inList adds "AND column IN (...)" for a non-empty list of values.
func inList[T ~string](query, column string, values []T, args []any) (string, []any) {
	if len(values) == 0 {
		return query, args
	}
	query += " AND " + column + " IN (?"
	args = append(args, string(values[0]))
	for _, v := range values[1:] {
		query += ", ?"
		args = append(args, string(v))
	}
	return query + ")", args
}

func (r Exports) query(ctx context.Context, what, query string, args ...any) ([]Export, error) {
	rows, err := r.q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	defer func() { _ = rows.Close() }()
	out := []Export{}
	for rows.Next() {
		e, err := scanExport(rows)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", what, err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	return out, nil
}

func (r Exports) exec(ctx context.Context, what, query string, args ...any) (bool, error) {
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
