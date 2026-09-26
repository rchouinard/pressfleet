package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/id"
)

// TokenScope limits what an API token may do, on top of its owner's role
// (design doc §4.2, §4.6).
type TokenScope string

// Token scopes.
const (
	// ScopeFull allows everything the owner's role allows.
	ScopeFull TokenScope = "full"
	// ScopeReadOnly allows only read actions.
	ScopeReadOnly TokenScope = "read-only"
)

// Valid reports whether s is a known scope.
func (s TokenScope) Valid() bool {
	return s == ScopeFull || s == ScopeReadOnly
}

// APIToken is a bearer token for automation. Only a hash of the token is
// stored; the plaintext is shown once, when it is created.
type APIToken struct {
	ID     string
	UserID string
	Name   string
	// TokenHash identifies the token; see package auth.
	TokenHash string
	// Prefix is the start of the plaintext token, for display.
	Prefix     string
	Scope      TokenScope
	CreatedAt  time.Time
	LastUsedAt *time.Time
	// ExpiresAt is nil for tokens that don't expire.
	ExpiresAt *time.Time
	RevokedAt *time.Time
}

// APITokens reads and writes API tokens.
type APITokens struct {
	q Querier
}

// NewAPITokens returns an APITokens that runs queries on q.
func NewAPITokens(q Querier) APITokens {
	return APITokens{q: q}
}

const apiTokenColumns = "id, user_id, name, token_hash, prefix, scope, created_at, last_used_at, expires_at, revoked_at" //nolint:gosec // G101: column names, not a credential

func scanAPIToken(s scanner) (APIToken, error) {
	var (
		t                         APIToken
		lastUsed, expires, revoke sql.NullTime
	)
	err := s.Scan(&t.ID, &t.UserID, &t.Name, &t.TokenHash, &t.Prefix, &t.Scope, &t.CreatedAt, &lastUsed, &expires, &revoke)
	t.LastUsedAt, t.ExpiresAt, t.RevokedAt = timePtr(lastUsed), timePtr(expires), timePtr(revoke)
	return t, err
}

// Create inserts t, filling in its ID and, if zero, CreatedAt. It returns
// ErrReference if the user doesn't exist.
func (r APITokens) Create(ctx context.Context, t *APIToken) error {
	switch {
	case t.UserID == "" || t.Name == "" || t.TokenHash == "":
		return errors.New("create API token: user, name, and token hash are required")
	case !t.Scope.Valid():
		return fmt.Errorf("create API token: invalid scope %q", t.Scope)
	}
	created := *t
	created.ID = id.New()
	if created.CreatedAt.IsZero() {
		created.CreatedAt = now()
	}
	created.CreatedAt = utc(created.CreatedAt)
	created.LastUsedAt, created.ExpiresAt, created.RevokedAt = utcPtr(created.LastUsedAt), utcPtr(created.ExpiresAt), utcPtr(created.RevokedAt)
	_, err := r.q.ExecContext(ctx,
		"INSERT INTO api_tokens ("+apiTokenColumns+") VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		created.ID, created.UserID, created.Name, created.TokenHash, created.Prefix, string(created.Scope),
		created.CreatedAt, created.LastUsedAt, created.ExpiresAt, created.RevokedAt)
	if err != nil {
		return fmt.Errorf("create API token: %w", err)
	}
	*t = created
	return nil
}

// GetByTokenHash returns the token with the given hash, or ErrNotFound. It
// doesn't check expiry or revocation.
func (r APITokens) GetByTokenHash(ctx context.Context, tokenHash string) (APIToken, error) {
	t, err := scanAPIToken(r.q.QueryRowContext(ctx, "SELECT "+apiTokenColumns+" FROM api_tokens WHERE token_hash = ?", tokenHash))
	if err != nil {
		return APIToken{}, fmt.Errorf("get API token: %w", err)
	}
	return t, nil
}

// ListActiveForUser returns a user's tokens that haven't been revoked
// (expired ones included), in ID (creation) order, starting after the
// cursor after and returning at most limit tokens (zero means no limit).
func (r APITokens) ListActiveForUser(ctx context.Context, userID, after string, limit int) ([]APIToken, error) {
	query := "SELECT " + apiTokenColumns + " FROM api_tokens WHERE user_id = ? AND revoked_at IS NULL AND id > ? ORDER BY id"
	args := []any{userID, after}
	if limit > 0 {
		query += " LIMIT ?"
		args = append(args, limit)
	}
	rows, err := r.q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list API tokens: %w", err)
	}
	defer func() { _ = rows.Close() }()
	tokens := []APIToken{}
	for rows.Next() {
		t, err := scanAPIToken(rows)
		if err != nil {
			return nil, fmt.Errorf("list API tokens: %w", err)
		}
		tokens = append(tokens, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list API tokens: %w", err)
	}
	return tokens, nil
}

// Revoke revokes one of a user's tokens. It returns ErrNotFound if the user
// has no such token or it is already revoked. Revoked tokens are kept, so
// audit events that refer to them still resolve.
func (r APITokens) Revoke(ctx context.Context, userID, tokenID string, at time.Time) error {
	return execOne(ctx, r.q, "revoke API token "+tokenID,
		"UPDATE api_tokens SET revoked_at = ? WHERE id = ? AND user_id = ? AND revoked_at IS NULL", at, tokenID, userID)
}

// RevokeAllForUser revokes every token of a user and returns how many it
// revoked.
func (r APITokens) RevokeAllForUser(ctx context.Context, userID string, at time.Time) (int64, error) {
	res, err := r.q.ExecContext(ctx, "UPDATE api_tokens SET revoked_at = ? WHERE user_id = ? AND revoked_at IS NULL", at, userID)
	if err != nil {
		return 0, fmt.Errorf("revoke API tokens of user %s: %w", userID, err)
	}
	return res.RowsAffected()
}

// TouchLastUsed sets a token's LastUsedAt.
func (r APITokens) TouchLastUsed(ctx context.Context, tokenID string, at time.Time) error {
	return execOne(ctx, r.q, "touch API token "+tokenID,
		"UPDATE api_tokens SET last_used_at = ? WHERE id = ?", at, tokenID)
}
