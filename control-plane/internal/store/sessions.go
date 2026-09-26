package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/id"
)

// Session is a logged-in browser session. Only a hash of the session token
// is stored; the token itself lives in the user's cookie.
type Session struct {
	ID        string
	UserID    string
	TokenHash string
	SourceIP  string
	UserAgent string
	CreatedAt time.Time
	// LastSeenAt drives the idle timeout. It is updated at most about once
	// a minute, not on every request.
	LastSeenAt time.Time
	// ExpiresAt is the absolute expiry, however active the session is.
	ExpiresAt time.Time
}

// Sessions reads and writes sessions.
type Sessions struct {
	q Querier
}

// NewSessions returns a Sessions that runs queries on q.
func NewSessions(q Querier) Sessions {
	return Sessions{q: q}
}

const sessionColumns = "id, user_id, token_hash, source_ip, user_agent, created_at, last_seen_at, expires_at"

func scanSession(s scanner) (Session, error) {
	var ss Session
	err := s.Scan(&ss.ID, &ss.UserID, &ss.TokenHash, &ss.SourceIP, &ss.UserAgent, &ss.CreatedAt, &ss.LastSeenAt, &ss.ExpiresAt)
	return ss, err
}

// Create inserts s, filling in its ID. CreatedAt, LastSeenAt, and ExpiresAt
// must be set. It returns ErrReference if the user doesn't exist.
func (r Sessions) Create(ctx context.Context, s *Session) error {
	if s.UserID == "" || s.TokenHash == "" || s.CreatedAt.IsZero() || s.ExpiresAt.IsZero() {
		return errors.New("create session: user, token hash, and times are required")
	}
	created := *s
	created.ID = id.New()
	created.CreatedAt, created.ExpiresAt = utc(created.CreatedAt), utc(created.ExpiresAt)
	if created.LastSeenAt.IsZero() {
		created.LastSeenAt = created.CreatedAt
	}
	created.LastSeenAt = utc(created.LastSeenAt)
	_, err := r.q.ExecContext(ctx,
		"INSERT INTO sessions ("+sessionColumns+") VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
		created.ID, created.UserID, created.TokenHash, created.SourceIP, created.UserAgent,
		created.CreatedAt, created.LastSeenAt, created.ExpiresAt)
	if err != nil {
		return fmt.Errorf("create session: %w", err)
	}
	*s = created
	return nil
}

// GetByTokenHash returns the session with the given token hash, or
// ErrNotFound. It doesn't check expiry.
func (r Sessions) GetByTokenHash(ctx context.Context, tokenHash string) (Session, error) {
	s, err := scanSession(r.q.QueryRowContext(ctx, "SELECT "+sessionColumns+" FROM sessions WHERE token_hash = ?", tokenHash))
	if err != nil {
		return Session{}, fmt.Errorf("get session: %w", err)
	}
	return s, nil
}

// Touch sets a session's LastSeenAt.
func (r Sessions) Touch(ctx context.Context, sessionID string, at time.Time) error {
	return execOne(ctx, r.q, "touch session "+sessionID,
		"UPDATE sessions SET last_seen_at = ? WHERE id = ?", at, sessionID)
}

// Delete removes a session. It returns ErrNotFound if it doesn't exist.
func (r Sessions) Delete(ctx context.Context, sessionID string) error {
	return execOne(ctx, r.q, "delete session "+sessionID, "DELETE FROM sessions WHERE id = ?", sessionID)
}

// DeleteForUser removes every session of a user except keepID (which may be
// empty) and returns how many it removed.
func (r Sessions) DeleteForUser(ctx context.Context, userID, keepID string) (int64, error) {
	res, err := r.q.ExecContext(ctx, "DELETE FROM sessions WHERE user_id = ? AND id <> ?", userID, keepID)
	if err != nil {
		return 0, fmt.Errorf("delete sessions of user %s: %w", userID, err)
	}
	return res.RowsAffected()
}

// DeleteExpired removes sessions past their absolute expiry (before now)
// or idle since before idleCutoff, and returns how many it removed.
func (r Sessions) DeleteExpired(ctx context.Context, now, idleCutoff time.Time) (int64, error) {
	res, err := r.q.ExecContext(ctx, "DELETE FROM sessions WHERE expires_at <= ? OR last_seen_at <= ?", now, idleCutoff)
	if err != nil {
		return 0, fmt.Errorf("delete expired sessions: %w", err)
	}
	return res.RowsAffected()
}
