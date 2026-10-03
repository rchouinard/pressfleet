package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/id"
)

// BackupKind says what asked for a backup.
type BackupKind string

// Backup kinds.
const (
	// BackupManual is a backup a user asked for (POST /sites/{id}/backups).
	BackupManual BackupKind = "manual"
	// BackupScheduled is a backup taken on the site's backup schedule.
	BackupScheduled BackupKind = "scheduled"
	// BackupPreRestore is the snapshot a restore takes of its target site
	// first, so the restore can be undone (design doc §5.4).
	BackupPreRestore BackupKind = "pre_restore"
	// BackupPreUpdate is the snapshot a core update takes of its site
	// first, so a failed update can be reverted (design doc §5.5).
	BackupPreUpdate BackupKind = "pre_update"
)

// Valid reports whether k is a known kind.
func (k BackupKind) Valid() bool {
	switch k {
	case BackupManual, BackupScheduled, BackupPreRestore, BackupPreUpdate:
		return true
	}
	return false
}

// BackupStatus is the state of a backup.
type BackupStatus string

// Backup statuses. Running is the only one that changes.
const (
	BackupRunning   BackupStatus = "running"
	BackupSucceeded BackupStatus = "succeeded"
	BackupFailed    BackupStatus = "failed"
)

// BackupRecord is the platform's record of one backup of a site (design doc
// §4.3). The snapshot itself lives in the client group's Restic repository.
type BackupRecord struct {
	ID     string
	SiteID string
	// SiteName and SiteSlug are the site's when the backup began, so a
	// deleted site's backups can still be told apart; empty for backups
	// of deleted sites taken before they were recorded.
	SiteName      string
	SiteSlug      string
	ClientGroupID string
	Kind          BackupKind
	Status        BackupStatus
	// SnapshotID is the Restic snapshot's full ID; empty until the backup
	// succeeded.
	SnapshotID string
	// WPCoreVersion is the site's WordPress core version when the backup
	// began; a restore needs at least this version (§5.4).
	WPCoreVersion string
	// SizeBytes is the size of the files backed up, AddedBytes what the
	// backup added to the repository after deduplication and compression,
	// and FileCount the number of files. All three are zero until the backup
	// succeeded.
	SizeBytes  int64
	AddedBytes int64
	FileCount  int64
	// Message is why a backup failed, or a warning about one that succeeded
	// (files it couldn't read). It never holds secrets.
	Message string
	// JobID is the job that took the backup; empty once that job is gone.
	JobID string
	// CreatedAt is when the job began the backup, StartedAt when its latest
	// attempt at the snapshot began, and FinishedAt when it succeeded or
	// failed (zero while running).
	CreatedAt  time.Time
	StartedAt  time.Time
	FinishedAt time.Time
	// ExpiredAt is when the backup's snapshot was found to be gone from the
	// repository, normally removed by the retention policy (#54); zero
	// while it can be restored.
	ExpiredAt time.Time
}

// BackupResult is what a successful backup records.
type BackupResult struct {
	SnapshotID string
	SizeBytes  int64
	AddedBytes int64
	FileCount  int64
	// Warning, if set, is kept as the record's message.
	Warning string
}

// BackupRecords reads and writes backup records.
type BackupRecords struct {
	q Querier
}

// NewBackupRecords returns a BackupRecords that runs queries on q.
func NewBackupRecords(q Querier) BackupRecords {
	return BackupRecords{q: q}
}

const backupRecordColumns = "id, site_id, site_name, site_slug, client_group_id, kind, status, snapshot_id, wp_core_version, " +
	"size_bytes, added_bytes, file_count, message, job_id, created_at, started_at, finished_at, expired_at"

func scanBackupRecord(s scanner) (BackupRecord, error) {
	var (
		b                  BackupRecord
		snapshot, job      sql.NullString
		size, added, files sql.NullInt64
		finished, expired  sql.NullTime
	)
	err := s.Scan(&b.ID, &b.SiteID, &b.SiteName, &b.SiteSlug, &b.ClientGroupID, &b.Kind, &b.Status, &snapshot, &b.WPCoreVersion,
		&size, &added, &files, &b.Message, &job, &b.CreatedAt, &b.StartedAt, &finished, &expired)
	b.SnapshotID, b.JobID = snapshot.String, job.String
	b.SizeBytes, b.AddedBytes, b.FileCount = size.Int64, added.Int64, files.Int64
	if finished.Valid {
		b.FinishedAt = finished.Time
	}
	if expired.Valid {
		b.ExpiredAt = expired.Time
	}
	return b, err
}

// Create inserts b as a running backup. b.ID may be set in advance (a job
// records the ID before creating the row, so a retry finds it); otherwise
// it is generated. CreatedAt and StartedAt are set to now. It returns
// ErrConflict if a record with the ID exists, and ErrReference if the client
// group doesn't.
func (r BackupRecords) Create(ctx context.Context, b *BackupRecord) error {
	if !b.Kind.Valid() {
		return fmt.Errorf("create backup record: invalid kind %q", b.Kind)
	}
	if b.SiteID == "" || b.ClientGroupID == "" {
		return errors.New("create backup record: site and client group are required")
	}
	created := *b
	if created.ID == "" {
		created.ID = id.New()
	}
	created.Status = BackupRunning
	created.SnapshotID, created.SizeBytes, created.AddedBytes, created.FileCount = "", 0, 0, 0
	created.Message, created.FinishedAt, created.ExpiredAt = "", time.Time{}, time.Time{}
	created.CreatedAt = now()
	created.StartedAt = created.CreatedAt
	_, err := r.q.ExecContext(ctx,
		"INSERT INTO backup_records (id, site_id, site_name, site_slug, client_group_id, kind, status, wp_core_version, job_id, "+
			"created_at, started_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		created.ID, created.SiteID, created.SiteName, created.SiteSlug, created.ClientGroupID, string(created.Kind), string(created.Status),
		created.WPCoreVersion, nullString(created.JobID), created.CreatedAt, created.StartedAt)
	if err != nil {
		return fmt.Errorf("create backup record: %w", err)
	}
	*b = created
	return nil
}

// Get returns the record with the given ID, or ErrNotFound.
func (r BackupRecords) Get(ctx context.Context, backupID string) (BackupRecord, error) {
	b, err := scanBackupRecord(r.q.QueryRowContext(ctx, "SELECT "+backupRecordColumns+" FROM backup_records WHERE id = ?", backupID))
	if err != nil {
		return BackupRecord{}, fmt.Errorf("get backup record %s: %w", backupID, err)
	}
	return b, nil
}

// BackupRecordFilter selects a page of backup records: a site's, or a
// client group's.
type BackupRecordFilter struct {
	SiteID        string
	ClientGroupID string
	// DeletedSites keeps the backups of sites that no longer exist.
	DeletedSites bool
	Status       BackupStatus
	// Before is a cursor: only records with a smaller ID (older ones) are
	// returned. Pass the previous page's NextCursor.
	Before string
	// Limit is the page size: default 50, at most 200.
	Limit int
}

// BackupRecordPage is one page of List results, newest first.
type BackupRecordPage struct {
	Records []BackupRecord
	// NextCursor is the Before value for the next page, or empty on the
	// last page.
	NextCursor string
}

// List returns the backup records of a site or a client group (one of
// them is required), newest first (IDs are ULIDs, so ID order is creation
// order), one page at a time.
func (r BackupRecords) List(ctx context.Context, f BackupRecordFilter) (BackupRecordPage, error) {
	if f.SiteID == "" && f.ClientGroupID == "" {
		return BackupRecordPage{}, errors.New("list backup records: a site or client group is required")
	}
	limit := f.Limit
	switch {
	case limit <= 0:
		limit = 50
	case limit > 200:
		limit = 200
	}
	query := "SELECT " + backupRecordColumns + " FROM backup_records WHERE 1 = 1"
	var args []any
	if f.SiteID != "" {
		query += " AND site_id = ?"
		args = append(args, f.SiteID)
	}
	if f.ClientGroupID != "" {
		query += " AND client_group_id = ?"
		args = append(args, f.ClientGroupID)
	}
	if f.DeletedSites {
		query += " AND NOT EXISTS (SELECT 1 FROM sites WHERE sites.id = backup_records.site_id)"
	}
	if f.Status != "" {
		query += " AND status = ?"
		args = append(args, string(f.Status))
	}
	if f.Before != "" {
		query += " AND id < ?"
		args = append(args, f.Before)
	}
	// One extra row says whether there is another page.
	query += " ORDER BY id DESC LIMIT ?"
	args = append(args, limit+1)
	rows, err := r.q.QueryContext(ctx, query, args...)
	if err != nil {
		return BackupRecordPage{}, fmt.Errorf("list backup records: %w", err)
	}
	defer func() { _ = rows.Close() }()
	page := BackupRecordPage{Records: []BackupRecord{}}
	for rows.Next() {
		b, err := scanBackupRecord(rows)
		if err != nil {
			return BackupRecordPage{}, fmt.Errorf("list backup records: %w", err)
		}
		page.Records = append(page.Records, b)
	}
	if err := rows.Err(); err != nil {
		return BackupRecordPage{}, fmt.Errorf("list backup records: %w", err)
	}
	if len(page.Records) > limit {
		page.Records = page.Records[:limit]
		page.NextCursor = page.Records[limit-1].ID
	}
	return page, nil
}

// SiteClientGroup returns the client group of the site's newest backup
// record, or ErrNotFound if the site has none. It locates the repository
// holding a deleted site's snapshots.
func (r BackupRecords) SiteClientGroup(ctx context.Context, siteID string) (string, error) {
	var group string
	err := r.q.QueryRowContext(ctx,
		"SELECT client_group_id FROM backup_records WHERE site_id = ? ORDER BY id DESC LIMIT 1", siteID).Scan(&group)
	if err != nil {
		return "", fmt.Errorf("client group of site %s's backups: %w", siteID, err)
	}
	return group, nil
}

// Start records that an attempt at the backup began now. Only a running
// backup changes; it reports whether this one did.
func (r BackupRecords) Start(ctx context.Context, backupID string) (bool, error) {
	return r.exec(ctx, "start backup "+backupID,
		"UPDATE backup_records SET started_at = ? WHERE id = ? AND status = ?", now(), backupID, string(BackupRunning))
}

// Succeed records that the backup succeeded. Only a running backup changes;
// it reports whether this one did, so a retried step can tell that an
// earlier attempt already finished it.
func (r BackupRecords) Succeed(ctx context.Context, backupID string, res BackupResult) (bool, error) {
	if res.SnapshotID == "" {
		return false, fmt.Errorf("finish backup %s: a snapshot ID is required", backupID)
	}
	return r.exec(ctx, "finish backup "+backupID,
		"UPDATE backup_records SET status = ?, snapshot_id = ?, size_bytes = ?, added_bytes = ?, file_count = ?, "+
			"message = ?, finished_at = ? WHERE id = ? AND status = ?",
		string(BackupSucceeded), res.SnapshotID, res.SizeBytes, res.AddedBytes, res.FileCount,
		res.Warning, now(), backupID, string(BackupRunning))
}

// Fail records that the backup failed with message, which must not hold
// secrets. Only a running backup changes, so a restore that rolls back
// after its pre-restore backup succeeded leaves that backup alone; it
// reports whether this one changed.
func (r BackupRecords) Fail(ctx context.Context, backupID, message string) (bool, error) {
	return r.exec(ctx, "fail backup "+backupID,
		"UPDATE backup_records SET status = ?, message = ?, finished_at = ? WHERE id = ? AND status = ?",
		string(BackupFailed), message, now(), backupID, string(BackupRunning))
}

// Restorable returns the group's succeeded backups whose snapshots are not
// known to be gone (not expired) and that finished before t, oldest first.
// Retention compares them with a listing of the repository started at t: a
// backup that finished before the listing began has its snapshot in it
// unless the snapshot was removed.
func (r BackupRecords) Restorable(ctx context.Context, groupID string, finishedBefore time.Time) ([]BackupRecord, error) {
	rows, err := r.q.QueryContext(ctx,
		"SELECT "+backupRecordColumns+" FROM backup_records WHERE client_group_id = ? AND status = ? "+
			"AND expired_at IS NULL AND finished_at < ? ORDER BY id",
		groupID, string(BackupSucceeded), finishedBefore)
	if err != nil {
		return nil, fmt.Errorf("list restorable backups of client group %s: %w", groupID, err)
	}
	defer func() { _ = rows.Close() }()
	var out []BackupRecord
	for rows.Next() {
		b, err := scanBackupRecord(rows)
		if err != nil {
			return nil, fmt.Errorf("list restorable backups of client group %s: %w", groupID, err)
		}
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list restorable backups of client group %s: %w", groupID, err)
	}
	return out, nil
}

// expireBatch bounds the IDs in one UPDATE, well below SQLite's limit on
// bound parameters.
const expireBatch = 500

// Expire records that the snapshots of the given backups are gone from
// the repository. Only succeeded backups that aren't expired yet change,
// so running it again changes nothing; it returns how many changed.
func (r BackupRecords) Expire(ctx context.Context, backupIDs []string) (int, error) {
	at := now()
	total := 0
	for batch := range slices.Chunk(backupIDs, expireBatch) {
		args := []any{at, string(BackupSucceeded)}
		for _, backupID := range batch {
			args = append(args, backupID)
		}
		res, err := r.q.ExecContext(ctx,
			"UPDATE backup_records SET expired_at = ? WHERE status = ? AND expired_at IS NULL AND id IN (?"+
				strings.Repeat(", ?", len(batch)-1)+")", args...)
		if err != nil {
			return total, fmt.Errorf("expire backups: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return total, fmt.Errorf("expire backups: %w", err)
		}
		total += int(n)
	}
	return total, nil
}

// DeleteForClientGroup deletes every record of the group's backups, e.g.
// with its repository. It returns how many it deleted.
func (r BackupRecords) DeleteForClientGroup(ctx context.Context, groupID string) (int, error) {
	res, err := r.q.ExecContext(ctx, "DELETE FROM backup_records WHERE client_group_id = ?", groupID)
	if err != nil {
		return 0, fmt.Errorf("delete backup records of client group %s: %w", groupID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("delete backup records of client group %s: %w", groupID, err)
	}
	return int(n), nil
}

// DeleteForSite deletes every record of the site's backups, e.g. when the
// site is deleted together with its snapshots. It returns how many it
// deleted; deleting again deletes none.
func (r BackupRecords) DeleteForSite(ctx context.Context, siteID string) (int, error) {
	res, err := r.q.ExecContext(ctx, "DELETE FROM backup_records WHERE site_id = ?", siteID)
	if err != nil {
		return 0, fmt.Errorf("delete backup records of site %s: %w", siteID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("delete backup records of site %s: %w", siteID, err)
	}
	return int(n), nil
}

func (r BackupRecords) exec(ctx context.Context, what, query string, args ...any) (bool, error) {
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
