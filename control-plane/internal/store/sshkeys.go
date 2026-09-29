package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/id"
)

// SSHKey is an SSH public key registered on a user's account, with which
// the user connects to the SFTP gateway (design doc §5.8). Package auth
// parses and validates keys; the store only keeps them.
type SSHKey struct {
	ID     string
	UserID string
	Name   string
	// PublicKey is the key in authorized_keys format ("<type> <base64>"),
	// without a comment.
	PublicKey string
	// Fingerprint is the key's SHA-256 fingerprint ("SHA256:<base64>"). It
	// identifies the key, and through it the user: it is unique.
	Fingerprint string
	CreatedAt   time.Time
	LastUsedAt  *time.Time
}

// SSHKeys reads and writes users' SSH keys.
type SSHKeys struct {
	q Querier
}

// NewSSHKeys returns an SSHKeys that runs queries on q.
func NewSSHKeys(q Querier) SSHKeys {
	return SSHKeys{q: q}
}

const sshKeyColumns = "id, user_id, name, public_key, fingerprint, created_at, last_used_at"

func scanSSHKey(s scanner) (SSHKey, error) {
	var (
		k        SSHKey
		lastUsed sql.NullTime
	)
	err := s.Scan(&k.ID, &k.UserID, &k.Name, &k.PublicKey, &k.Fingerprint, &k.CreatedAt, &lastUsed)
	k.LastUsedAt = timePtr(lastUsed)
	return k, err
}

// Create inserts k, filling in its ID and, if zero, CreatedAt. It returns
// ErrConflict if a user already has a key with the same fingerprint, and
// ErrReference if the user doesn't exist.
func (r SSHKeys) Create(ctx context.Context, k *SSHKey) error {
	if k.UserID == "" || k.Name == "" || k.PublicKey == "" || k.Fingerprint == "" {
		return errors.New("create SSH key: user, name, public key, and fingerprint are required")
	}
	created := *k
	created.ID = id.New()
	if created.CreatedAt.IsZero() {
		created.CreatedAt = now()
	}
	created.CreatedAt = utc(created.CreatedAt)
	created.LastUsedAt = utcPtr(created.LastUsedAt)
	_, err := r.q.ExecContext(ctx,
		"INSERT INTO ssh_keys ("+sshKeyColumns+") VALUES (?, ?, ?, ?, ?, ?, ?)",
		created.ID, created.UserID, created.Name, created.PublicKey, created.Fingerprint,
		created.CreatedAt, created.LastUsedAt)
	if err != nil {
		return fmt.Errorf("create SSH key: %w", err)
	}
	*k = created
	return nil
}

// Get returns the key with the given ID, or ErrNotFound.
func (r SSHKeys) Get(ctx context.Context, keyID string) (SSHKey, error) {
	k, err := scanSSHKey(r.q.QueryRowContext(ctx, "SELECT "+sshKeyColumns+" FROM ssh_keys WHERE id = ?", keyID))
	if err != nil {
		return SSHKey{}, fmt.Errorf("get SSH key %s: %w", keyID, err)
	}
	return k, nil
}

// GetByFingerprint returns the key with the given fingerprint, or
// ErrNotFound.
func (r SSHKeys) GetByFingerprint(ctx context.Context, fingerprint string) (SSHKey, error) {
	k, err := scanSSHKey(r.q.QueryRowContext(ctx, "SELECT "+sshKeyColumns+" FROM ssh_keys WHERE fingerprint = ?", fingerprint))
	if err != nil {
		return SSHKey{}, fmt.Errorf("get SSH key by fingerprint: %w", err)
	}
	return k, nil
}

// ListForUser returns a user's keys in ID (creation) order, starting after
// the cursor after and returning at most limit keys (zero means no limit).
func (r SSHKeys) ListForUser(ctx context.Context, userID, after string, limit int) ([]SSHKey, error) {
	query := "SELECT " + sshKeyColumns + " FROM ssh_keys WHERE user_id = ? AND id > ? ORDER BY id"
	args := []any{userID, after}
	if limit > 0 {
		query += " LIMIT ?"
		args = append(args, limit)
	}
	rows, err := r.q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list SSH keys: %w", err)
	}
	defer func() { _ = rows.Close() }()
	keys := []SSHKey{}
	for rows.Next() {
		k, err := scanSSHKey(rows)
		if err != nil {
			return nil, fmt.Errorf("list SSH keys: %w", err)
		}
		keys = append(keys, k)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list SSH keys: %w", err)
	}
	return keys, nil
}

// Delete removes one of a user's keys. It returns ErrNotFound if the user
// has no such key.
func (r SSHKeys) Delete(ctx context.Context, userID, keyID string) error {
	return execOne(ctx, r.q, "delete SSH key "+keyID,
		"DELETE FROM ssh_keys WHERE id = ? AND user_id = ?", keyID, userID)
}

// TouchLastUsed sets a key's LastUsedAt.
func (r SSHKeys) TouchLastUsed(ctx context.Context, keyID string, at time.Time) error {
	return execOne(ctx, r.q, "touch SSH key "+keyID,
		"UPDATE ssh_keys SET last_used_at = ? WHERE id = ?", at, keyID)
}
