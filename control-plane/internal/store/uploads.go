package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/id"
)

// UploadPurpose is the kind of job an upload is for. It decides which
// permission the upload needs (design doc §4.6), and a job only takes an
// upload made for it.
type UploadPurpose string

// Upload purposes.
const (
	// UploadSiteImport is a wp-content archive or SQL dump for a site
	// import (design doc §5.7) into a client group.
	UploadSiteImport UploadPurpose = "site_import"
	// UploadSiteDeploy is a theme or plugin package to deploy to a site
	// (design doc §5.8).
	UploadSiteDeploy UploadPurpose = "site_deploy"
)

// Valid reports whether p is a known purpose.
func (p UploadPurpose) Valid() bool {
	return p == UploadSiteImport || p == UploadSiteDeploy
}

// UploadStatus is where an upload is in its lifecycle.
type UploadStatus string

// Upload statuses.
const (
	// UploadReceiving uploads take chunks.
	UploadReceiving UploadStatus = "uploading"
	// UploadComplete uploads have all their bytes and a verified checksum,
	// and wait for a job to claim them.
	UploadComplete UploadStatus = "complete"
	// UploadInUse uploads are claimed by a job; they are removed when the
	// job ends.
	UploadInUse UploadStatus = "in_use"
)

// Upload is a large file sent to the control plane in chunks (#111). Its
// content lives under the data directory (package uploads); this is its
// record.
type Upload struct {
	ID      string
	Purpose UploadPurpose
	// ClientGroupID is the group an import will create its site in, or the
	// deploy target's group.
	ClientGroupID string
	// SiteID is the deploy target; empty for imports.
	SiteID          string
	CreatedByUserID string
	// Filename is the client's name for the file, for display only.
	Filename string
	// SizeBytes is the declared size, ReceivedBytes how much has arrived.
	SizeBytes     int64
	ReceivedBytes int64
	// SHA256 is the declared checksum in lowercase hex, or empty until the
	// client declares it.
	SHA256 string
	// HashState is the marshaled SHA-256 state after ReceivedBytes; nil
	// before the first chunk.
	HashState []byte
	Status    UploadStatus
	// JobID is the job that claimed the upload; empty unless in use, and
	// empty again if that job is deleted.
	JobID     string
	CreatedAt time.Time
	UpdatedAt time.Time
	// ExpiresAt is when the upload counts as abandoned; zero while in use.
	ExpiresAt time.Time
}

// Uploads reads and writes upload records.
type Uploads struct {
	q Querier
}

// NewUploads returns an Uploads that runs queries on q.
func NewUploads(q Querier) Uploads {
	return Uploads{q: q}
}

const uploadColumns = "id, purpose, client_group_id, site_id, created_by_user_id, filename, size_bytes, received_bytes, " +
	"sha256, hash_state, status, job_id, created_at, updated_at, expires_at"

func scanUpload(s scanner) (Upload, error) {
	var (
		u         Upload
		site, job sql.NullString
		hashState []byte
		expiresAt sql.NullTime
	)
	err := s.Scan(&u.ID, &u.Purpose, &u.ClientGroupID, &site, &u.CreatedByUserID, &u.Filename, &u.SizeBytes, &u.ReceivedBytes,
		&u.SHA256, &hashState, &u.Status, &job, &u.CreatedAt, &u.UpdatedAt, &expiresAt)
	u.SiteID, u.JobID = site.String, job.String
	if len(hashState) > 0 {
		u.HashState = hashState
	}
	if expiresAt.Valid {
		u.ExpiresAt = expiresAt.Time
	}
	return u, err
}

// Create inserts u as a new upload that is receiving data, generating its
// ID unless set. ReceivedBytes, HashState, and JobID start empty;
// CreatedAt and UpdatedAt are at, and ExpiresAt must be set. It returns
// ErrReference if the client group, site, or user doesn't exist.
func (r Uploads) Create(ctx context.Context, u *Upload, at time.Time) error {
	switch {
	case !u.Purpose.Valid():
		return fmt.Errorf("create upload: invalid purpose %q", u.Purpose)
	case u.ClientGroupID == "" || u.CreatedByUserID == "":
		return errors.New("create upload: client group and user are required")
	case u.SizeBytes <= 0:
		return fmt.Errorf("create upload: invalid size %d", u.SizeBytes)
	case u.ExpiresAt.IsZero():
		return errors.New("create upload: an expiry is required")
	}
	created := *u
	if created.ID == "" {
		created.ID = id.New()
	}
	created.ReceivedBytes, created.HashState, created.JobID = 0, nil, ""
	created.Status = UploadReceiving
	created.CreatedAt, created.UpdatedAt, created.ExpiresAt = utc(at), utc(at), utc(created.ExpiresAt)
	_, err := r.q.ExecContext(ctx,
		"INSERT INTO uploads (id, purpose, client_group_id, site_id, created_by_user_id, filename, size_bytes, sha256, "+
			"status, created_at, updated_at, expires_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		created.ID, string(created.Purpose), created.ClientGroupID, nullString(created.SiteID), created.CreatedByUserID,
		created.Filename, created.SizeBytes, created.SHA256, string(created.Status),
		created.CreatedAt, created.UpdatedAt, created.ExpiresAt)
	if err != nil {
		return fmt.Errorf("create upload: %w", err)
	}
	*u = created
	return nil
}

// Get returns the upload with the given ID, or ErrNotFound.
func (r Uploads) Get(ctx context.Context, uploadID string) (Upload, error) {
	u, err := scanUpload(r.q.QueryRowContext(ctx, "SELECT "+uploadColumns+" FROM uploads WHERE id = ?", uploadID))
	if err != nil {
		return Upload{}, fmt.Errorf("get upload %s: %w", uploadID, err)
	}
	return u, nil
}

// Advance records that the bytes from offset from up to to have been
// written, with the hash state after them, and moves the expiry. Only an
// upload that is receiving and has exactly from bytes changes; it reports
// whether this one did.
func (r Uploads) Advance(ctx context.Context, uploadID string, from, to int64, hashState []byte, expiresAt, at time.Time) (bool, error) {
	return r.exec(ctx, "advance upload "+uploadID,
		"UPDATE uploads SET received_bytes = ?, hash_state = ?, expires_at = ?, updated_at = ? "+
			"WHERE id = ? AND status = ? AND received_bytes = ?",
		to, hashState, expiresAt, at, uploadID, string(UploadReceiving), from)
}

// Complete records that a receiving upload with all its bytes has the
// checksum sha256 (verified by the caller), and moves the expiry. It
// reports whether the upload changed.
func (r Uploads) Complete(ctx context.Context, uploadID, sha256 string, expiresAt, at time.Time) (bool, error) {
	return r.exec(ctx, "complete upload "+uploadID,
		"UPDATE uploads SET status = ?, sha256 = ?, expires_at = ?, updated_at = ? "+
			"WHERE id = ? AND status = ? AND received_bytes = size_bytes",
		string(UploadComplete), sha256, expiresAt, at, uploadID, string(UploadReceiving))
}

// Claim marks a complete upload as in use by the job, which never
// expires. It reports whether the upload changed. It returns ErrReference
// if the job doesn't exist.
func (r Uploads) Claim(ctx context.Context, uploadID, jobID string, at time.Time) (bool, error) {
	return r.exec(ctx, "claim upload "+uploadID,
		"UPDATE uploads SET status = ?, job_id = ?, expires_at = NULL, updated_at = ? WHERE id = ? AND status = ?",
		string(UploadInUse), jobID, at, uploadID, string(UploadComplete))
}

// Delete deletes the upload's record, reporting whether it existed.
func (r Uploads) Delete(ctx context.Context, uploadID string) (bool, error) {
	return r.exec(ctx, "delete upload "+uploadID, "DELETE FROM uploads WHERE id = ?", uploadID)
}

// DeleteUnclaimed deletes the upload's record unless a job has claimed
// it, reporting whether it did.
func (r Uploads) DeleteUnclaimed(ctx context.Context, uploadID string) (bool, error) {
	return r.exec(ctx, "delete upload "+uploadID, "DELETE FROM uploads WHERE id = ? AND status <> ?",
		uploadID, string(UploadInUse))
}

// DeleteExpired deletes the upload's record if no job has claimed it and
// it expired before t, reporting whether it did. A chunk that arrived
// since the caller found the upload expired keeps it.
func (r Uploads) DeleteExpired(ctx context.Context, uploadID string, t time.Time) (bool, error) {
	return r.exec(ctx, "delete expired upload "+uploadID, "DELETE FROM uploads WHERE id = ? AND status <> ? AND expires_at < ?",
		uploadID, string(UploadInUse), t)
}

// DeleteClaimed deletes the upload's record if the job has claimed it,
// reporting whether it did.
func (r Uploads) DeleteClaimed(ctx context.Context, uploadID, jobID string) (bool, error) {
	return r.exec(ctx, "delete upload "+uploadID, "DELETE FROM uploads WHERE id = ? AND status = ? AND job_id = ?",
		uploadID, string(UploadInUse), jobID)
}

// Expired returns the uploads that aren't in use and expired before t.
func (r Uploads) Expired(ctx context.Context, t time.Time) ([]Upload, error) {
	return r.list(ctx, "list expired uploads",
		"SELECT "+uploadColumns+" FROM uploads WHERE status <> ? AND expires_at < ? ORDER BY id",
		string(UploadInUse), t)
}

// Finished returns the uploads in use by a job that has reached a final
// state, or whose job is gone.
func (r Uploads) Finished(ctx context.Context) ([]Upload, error) {
	return r.list(ctx, "list uploads of finished jobs",
		"SELECT "+uploadColumns+" FROM uploads WHERE status = ? AND "+
			"(job_id IS NULL OR job_id IN (SELECT id FROM jobs WHERE state IN (?, ?, ?))) ORDER BY id",
		string(UploadInUse), string(JobSucceeded), string(JobFailed), string(JobRollbackFailed))
}

// Exists reports whether the upload has a record, for finding files
// without one.
func (r Uploads) Exists(ctx context.Context, uploadID string) (bool, error) {
	var n int
	if err := r.q.QueryRowContext(ctx, "SELECT COUNT(*) FROM uploads WHERE id = ?", uploadID).Scan(&n); err != nil {
		return false, fmt.Errorf("check upload %s: %w", uploadID, err)
	}
	return n > 0, nil
}

func (r Uploads) list(ctx context.Context, what, query string, args ...any) ([]Upload, error) {
	rows, err := r.q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	defer func() { _ = rows.Close() }()
	var out []Upload
	for rows.Next() {
		u, err := scanUpload(rows)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", what, err)
		}
		out = append(out, u)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	return out, nil
}

func (r Uploads) exec(ctx context.Context, what, query string, args ...any) (bool, error) {
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
