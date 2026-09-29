package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/id"
)

// DatabaseSessionStatus is the state of a temporary database session.
type DatabaseSessionStatus string

// Database session statuses. An active session becomes ended, expired, or
// failed, and never changes again.
const (
	DatabaseSessionActive  DatabaseSessionStatus = "active"
	DatabaseSessionEnded   DatabaseSessionStatus = "ended"
	DatabaseSessionExpired DatabaseSessionStatus = "expired"
	DatabaseSessionFailed  DatabaseSessionStatus = "failed"
)

// DatabaseSession is the record of a temporary database session (design
// doc §5.8): a MariaDB user, pft_<ID>, with access to one site's database
// until it expires or is ended. The password is never stored.
type DatabaseSession struct {
	ID     string
	SiteID string
	// UserID is the platform user who opened it; only they can tunnel to
	// the database with it.
	UserID string
	// Access is "read_only" or "read_write" (sitedb.Access).
	Access string
	Status DatabaseSessionStatus
	// CreatedAt is when it was opened and ExpiresAt when it stops working.
	CreatedAt time.Time
	ExpiresAt time.Time
	// EndedAt is when it stopped being active; zero while it is.
	EndedAt time.Time
	// EndedByUserID is who ended it through the API; empty otherwise.
	EndedByUserID string
	// EndReason says why it ended, e.g. "expired"; it never holds secrets.
	EndReason string
	// DroppedAt is when its MariaDB user was dropped and its connections
	// ended; zero until then.
	DroppedAt time.Time
}

// Active reports whether the session is active and unexpired at t.
func (s DatabaseSession) Active(t time.Time) bool {
	return s.Status == DatabaseSessionActive && t.Before(s.ExpiresAt)
}

// DatabaseSessions reads and writes database session records.
type DatabaseSessions struct {
	q Querier
}

// NewDatabaseSessions returns a DatabaseSessions that runs queries on q.
func NewDatabaseSessions(q Querier) DatabaseSessions {
	return DatabaseSessions{q: q}
}

const databaseSessionColumns = "id, site_id, user_id, access, status, created_at, expires_at, ended_at, ended_by_user_id, end_reason, dropped_at"

func scanDatabaseSession(s scanner) (DatabaseSession, error) {
	var (
		d              DatabaseSession
		ended, dropped sql.NullTime
		endedBy        sql.NullString
	)
	err := s.Scan(&d.ID, &d.SiteID, &d.UserID, &d.Access, &d.Status, &d.CreatedAt, &d.ExpiresAt, &ended, &endedBy, &d.EndReason, &dropped)
	d.EndedByUserID = endedBy.String
	if ended.Valid {
		d.EndedAt = ended.Time
	}
	if dropped.Valid {
		d.DroppedAt = dropped.Time
	}
	return d, err
}

// Create inserts d as an active session. d.ID may be set in advance;
// otherwise it is generated. CreatedAt is set to now unless given. It
// returns ErrConflict if a session with the ID exists.
func (r DatabaseSessions) Create(ctx context.Context, d *DatabaseSession) error {
	if d.SiteID == "" || d.UserID == "" || d.Access == "" || d.ExpiresAt.IsZero() {
		return errors.New("create database session: a site, user, access, and expiry are required")
	}
	created := DatabaseSession{ID: d.ID, SiteID: d.SiteID, UserID: d.UserID, Access: d.Access,
		Status: DatabaseSessionActive, CreatedAt: utc(d.CreatedAt), ExpiresAt: utc(d.ExpiresAt)}
	if created.ID == "" {
		created.ID = id.New()
	}
	if created.CreatedAt.IsZero() {
		created.CreatedAt = now()
	}
	_, err := r.q.ExecContext(ctx,
		"INSERT INTO database_sessions (id, site_id, user_id, access, status, created_at, expires_at) VALUES (?, ?, ?, ?, ?, ?, ?)",
		created.ID, created.SiteID, created.UserID, created.Access, string(created.Status), created.CreatedAt, created.ExpiresAt)
	if err != nil {
		return fmt.Errorf("create database session: %w", err)
	}
	*d = created
	return nil
}

// Get returns the session with the given ID, or ErrNotFound.
func (r DatabaseSessions) Get(ctx context.Context, sessionID string) (DatabaseSession, error) {
	d, err := scanDatabaseSession(r.q.QueryRowContext(ctx,
		"SELECT "+databaseSessionColumns+" FROM database_sessions WHERE id = ?", sessionID))
	if err != nil {
		return DatabaseSession{}, fmt.Errorf("get database session %s: %w", sessionID, err)
	}
	return d, nil
}

// ListActive returns the site's sessions whose status is active, oldest
// first, including ones whose expiry has passed but that the sweep hasn't
// expired yet.
func (r DatabaseSessions) ListActive(ctx context.Context, siteID string) ([]DatabaseSession, error) {
	return r.list(ctx, "list active database sessions",
		"WHERE site_id = ? AND status = ? ORDER BY id", siteID, string(DatabaseSessionActive))
}

// ListActiveForUser returns the user's sessions on the site that are
// active and unexpired at t, oldest first.
func (r DatabaseSessions) ListActiveForUser(ctx context.Context, siteID, userID string, t time.Time) ([]DatabaseSession, error) {
	return r.list(ctx, "list active database sessions",
		"WHERE site_id = ? AND user_id = ? AND status = ? AND expires_at > ? ORDER BY id",
		siteID, userID, string(DatabaseSessionActive), utc(t))
}

// ActiveIDs returns the IDs of every session whose status is active.
func (r DatabaseSessions) ActiveIDs(ctx context.Context) (map[string]bool, error) {
	rows, err := r.q.QueryContext(ctx, "SELECT id FROM database_sessions WHERE status = ?", string(DatabaseSessionActive))
	if err != nil {
		return nil, fmt.Errorf("list active database sessions: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]bool{}
	for rows.Next() {
		var sid string
		if err := rows.Scan(&sid); err != nil {
			return nil, fmt.Errorf("list active database sessions: %w", err)
		}
		out[sid] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list active database sessions: %w", err)
	}
	return out, nil
}

// ListDue returns up to limit active sessions whose expiry is at or
// before t, oldest first.
func (r DatabaseSessions) ListDue(ctx context.Context, t time.Time, limit int) ([]DatabaseSession, error) {
	if limit <= 0 {
		limit = 100
	}
	return r.list(ctx, "list expired database sessions",
		"WHERE status = ? AND expires_at <= ? ORDER BY id LIMIT ?", string(DatabaseSessionActive), utc(t), limit)
}

// ListUndropped returns up to limit sessions that have ended but whose
// MariaDB user hasn't been dropped, oldest first.
func (r DatabaseSessions) ListUndropped(ctx context.Context, limit int) ([]DatabaseSession, error) {
	if limit <= 0 {
		limit = 100
	}
	return r.list(ctx, "list undropped database sessions",
		"WHERE dropped_at IS NULL AND status <> ? ORDER BY id LIMIT ?", string(DatabaseSessionActive), limit)
}

func (r DatabaseSessions) list(ctx context.Context, what, where string, args ...any) ([]DatabaseSession, error) {
	rows, err := r.q.QueryContext(ctx, "SELECT "+databaseSessionColumns+" FROM database_sessions "+where, args...)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	defer func() { _ = rows.Close() }()
	var out []DatabaseSession
	for rows.Next() {
		d, err := scanDatabaseSession(rows)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", what, err)
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	return out, nil
}

// End records that the session stopped being active at t, with status
// (ended, expired, or failed), a reason, and who ended it (empty for
// none). Only an active session changes; it reports whether this one did.
func (r DatabaseSessions) End(ctx context.Context, sessionID string, status DatabaseSessionStatus, reason, byUserID string, t time.Time) (bool, error) {
	if status == DatabaseSessionActive || status == "" {
		return false, fmt.Errorf("end database session %s: invalid status %q", sessionID, status)
	}
	return r.exec(ctx, "end database session "+sessionID,
		"UPDATE database_sessions SET status = ?, end_reason = ?, ended_by_user_id = ?, ended_at = ? WHERE id = ? AND status = ?",
		string(status), reason, nullString(byUserID), utc(t), sessionID, string(DatabaseSessionActive))
}

// MarkDropped records that the session's MariaDB user was dropped at t.
// Only an ended session changes; it reports whether this one did.
func (r DatabaseSessions) MarkDropped(ctx context.Context, sessionID string, t time.Time) (bool, error) {
	return r.exec(ctx, "mark database session dropped "+sessionID,
		"UPDATE database_sessions SET dropped_at = ? WHERE id = ? AND status <> ? AND dropped_at IS NULL",
		utc(t), sessionID, string(DatabaseSessionActive))
}

func (r DatabaseSessions) exec(ctx context.Context, what, query string, args ...any) (bool, error) {
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
