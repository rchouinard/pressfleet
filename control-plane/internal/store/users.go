package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/id"
	"github.com/rchouinard/pressfleet/control-plane/internal/redact"
)

// Role is a user's platform-wide role (design doc §4.6).
type Role string

// Roles.
const (
	RoleAdmin    Role = "admin"
	RoleOperator Role = "operator"
	RoleViewer   Role = "viewer"
)

// Roles lists every role.
var Roles = []Role{RoleAdmin, RoleOperator, RoleViewer}

// Valid reports whether r is a known role.
func (r Role) Valid() bool {
	switch r {
	case RoleAdmin, RoleOperator, RoleViewer:
		return true
	}
	return false
}

// User is a person who can log in to the control plane.
type User struct {
	ID string
	// Email is normalized to lowercase by the caller; it is unique.
	Email string
	// DisplayName is optional; empty means the user hasn't set one.
	DisplayName string
	// PasswordHash is the encoded argon2id hash (package auth). It isn't
	// the password, but it would allow an offline guessing attack, so it is
	// kept out of logs like a secret.
	PasswordHash redact.Secret
	Role         Role
	// DisabledAt is set while the user is disabled.
	DisabledAt  *time.Time
	LastLoginAt *time.Time
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// Disabled reports whether the user is disabled.
func (u User) Disabled() bool { return u.DisabledAt != nil }

// Users reads and writes users.
type Users struct {
	q Querier
}

// NewUsers returns a Users that runs queries on q.
func NewUsers(q Querier) Users {
	return Users{q: q}
}

const userColumns = "id, email, display_name, password_hash, role, disabled_at, last_login_at, created_at, updated_at"

func scanUser(s scanner) (User, error) {
	var (
		u                 User
		disabled, lastLog sql.NullTime
	)
	err := s.Scan(&u.ID, &u.Email, &u.DisplayName, &u.PasswordHash, &u.Role, &disabled, &lastLog, &u.CreatedAt, &u.UpdatedAt)
	u.DisabledAt, u.LastLoginAt = timePtr(disabled), timePtr(lastLog)
	return u, err
}

func (u User) validate() error {
	switch {
	case u.Email == "":
		return errors.New("email is required")
	case u.PasswordHash == "":
		return errors.New("password hash is required")
	case !u.Role.Valid():
		return fmt.Errorf("invalid role %q", u.Role)
	}
	return nil
}

// Create inserts u, filling in its ID and, if zero, its timestamps. It
// returns ErrConflict if the email is taken.
func (r Users) Create(ctx context.Context, u *User) error {
	if err := u.validate(); err != nil {
		return fmt.Errorf("create user: %w", err)
	}
	created := *u
	created.ID = id.New()
	if created.CreatedAt.IsZero() {
		created.CreatedAt = now()
	}
	created.CreatedAt = utc(created.CreatedAt)
	created.UpdatedAt = created.CreatedAt
	created.DisabledAt, created.LastLoginAt = utcPtr(created.DisabledAt), utcPtr(created.LastLoginAt)
	_, err := r.q.ExecContext(ctx,
		"INSERT INTO users ("+userColumns+") VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)",
		created.ID, created.Email, created.DisplayName, created.PasswordHash.Reveal(), string(created.Role),
		created.DisabledAt, created.LastLoginAt, created.CreatedAt, created.UpdatedAt)
	if err != nil {
		return fmt.Errorf("create user: %w", err)
	}
	*u = created
	return nil
}

// Get returns the user with the given ID, or ErrNotFound.
func (r Users) Get(ctx context.Context, userID string) (User, error) {
	u, err := scanUser(r.q.QueryRowContext(ctx, "SELECT "+userColumns+" FROM users WHERE id = ?", userID))
	if err != nil {
		return User{}, fmt.Errorf("get user %s: %w", userID, err)
	}
	return u, nil
}

// GetByEmail returns the user with the given (normalized) email, or
// ErrNotFound.
func (r Users) GetByEmail(ctx context.Context, email string) (User, error) {
	u, err := scanUser(r.q.QueryRowContext(ctx, "SELECT "+userColumns+" FROM users WHERE email = ?", email))
	if err != nil {
		return User{}, fmt.Errorf("get user by email: %w", err)
	}
	return u, nil
}

// UserFilter selects users for List.
type UserFilter struct {
	// Role, if set, only returns users with this role.
	Role Role
	// After, if set, only returns users whose ID sorts after it (the
	// pagination cursor).
	After string
	// Limit caps the number of users returned; zero means no limit.
	Limit int
}

// List returns users in ID (creation) order.
func (r Users) List(ctx context.Context, f UserFilter) ([]User, error) {
	query := "SELECT " + userColumns + " FROM users WHERE id > ?"
	args := []any{f.After}
	if f.Role != "" {
		query += " AND role = ?"
		args = append(args, string(f.Role))
	}
	query += " ORDER BY id"
	if f.Limit > 0 {
		query += " LIMIT ?"
		args = append(args, f.Limit)
	}
	rows, err := r.q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list users: %w", err)
	}
	defer func() { _ = rows.Close() }()
	users := []User{}
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, fmt.Errorf("list users: %w", err)
		}
		users = append(users, u)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list users: %w", err)
	}
	return users, nil
}

// Update saves u's email, display name, password hash, role, and disabled state, and sets
// UpdatedAt to at. It returns ErrNotFound if u doesn't exist and ErrConflict
// if the new email is taken.
func (r Users) Update(ctx context.Context, u *User, at time.Time) error {
	if err := u.validate(); err != nil {
		return fmt.Errorf("update user %s: %w", u.ID, err)
	}
	at = utc(at)
	disabled := utcPtr(u.DisabledAt)
	var (
		createdAt time.Time
		lastLogin sql.NullTime
	)
	err := r.q.QueryRowContext(ctx,
		"UPDATE users SET email = ?, display_name = ?, password_hash = ?, role = ?, disabled_at = ?, updated_at = ? WHERE id = ? RETURNING created_at, last_login_at",
		u.Email, u.DisplayName, u.PasswordHash.Reveal(), string(u.Role), disabled, at, u.ID).Scan(&createdAt, &lastLogin)
	if err != nil {
		return fmt.Errorf("update user %s: %w", u.ID, err)
	}
	u.DisabledAt, u.CreatedAt, u.UpdatedAt, u.LastLoginAt = disabled, createdAt, at, timePtr(lastLogin)
	return nil
}

// LockActiveAdmins returns the IDs of the enabled admins. Inside a
// transaction on PostgreSQL it locks their rows (SELECT ... FOR UPDATE)
// until the transaction ends, so two transactions that each remove a
// different admin can't both see the other one and leave none: the second
// waits, then sees the first one's change. SQLite needs no row locks,
// because the store's transactions take the database write lock when they
// begin, so they already run one at a time.
func (r Users) LockActiveAdmins(ctx context.Context) ([]string, error) {
	query := "SELECT id FROM users WHERE role = ? AND disabled_at IS NULL ORDER BY id"
	if r.q.Dialect() == Postgres {
		query += " FOR UPDATE"
	}
	rows, err := r.q.QueryContext(ctx, query, string(RoleAdmin))
	if err != nil {
		return nil, fmt.Errorf("lock admins: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("lock admins: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("lock admins: %w", err)
	}
	return ids, nil
}

// SetLastLogin records a successful login. It doesn't change UpdatedAt,
// which tracks changes to the user's settings.
func (r Users) SetLastLogin(ctx context.Context, userID string, at time.Time) error {
	return execOne(ctx, r.q, "set last login for user "+userID,
		"UPDATE users SET last_login_at = ? WHERE id = ?", at, userID)
}

// Delete removes a user together with their sessions, API tokens, and second
// factors, including the encrypted TOTP secret. It returns ErrNotFound if the
// user doesn't exist. Run it in a transaction ([DB.InTx]), so the user and
// their TOTP secret go together.
func (r Users) Delete(ctx context.Context, userID string) error {
	// The cascade would remove user_totp and with it the only reference to
	// the secret, so the secret goes first.
	if _, err := deleteTOTP(ctx, r.q, "user_id = ?", userID); err != nil {
		return fmt.Errorf("delete user %s: %w", userID, err)
	}
	return execOne(ctx, r.q, "delete user "+userID, "DELETE FROM users WHERE id = ?", userID)
}

// WebAuthnUserHandleSize is the length in bytes of a WebAuthn user handle,
// the most the specification allows.
const WebAuthnUserHandleSize = 64

// WebAuthnUserHandle returns the user's WebAuthn user handle, or nil if
// they haven't registered a credential yet. It returns ErrNotFound if the
// user doesn't exist.
func (r Users) WebAuthnUserHandle(ctx context.Context, userID string) ([]byte, error) {
	var handle []byte
	if err := r.q.QueryRowContext(ctx, "SELECT webauthn_user_handle FROM users WHERE id = ?", userID).Scan(&handle); err != nil {
		return nil, fmt.Errorf("get WebAuthn user handle of user %s: %w", userID, err)
	}
	return handle, nil
}

// EnsureWebAuthnUserHandle returns the user's WebAuthn user handle,
// generating a random one first if they don't have one. Credentials carry
// the handle instead of the user ID, as the specification recommends. It
// returns ErrNotFound if the user doesn't exist.
func (r Users) EnsureWebAuthnUserHandle(ctx context.Context, userID string) ([]byte, error) {
	handle := make([]byte, WebAuthnUserHandleSize)
	_, _ = rand.Read(handle) // never fails
	// If two registrations race, the first handle stays and both read it.
	if _, err := r.q.ExecContext(ctx,
		"UPDATE users SET webauthn_user_handle = ? WHERE id = ? AND webauthn_user_handle IS NULL", handle, userID); err != nil {
		return nil, fmt.Errorf("set WebAuthn user handle of user %s: %w", userID, err)
	}
	return r.WebAuthnUserHandle(ctx, userID)
}

// GetByWebAuthnUserHandle returns the user with the given WebAuthn user
// handle, or ErrNotFound.
func (r Users) GetByWebAuthnUserHandle(ctx context.Context, handle []byte) (User, error) {
	if len(handle) == 0 {
		return User{}, fmt.Errorf("get user by WebAuthn user handle: %w", ErrNotFound)
	}
	u, err := scanUser(r.q.QueryRowContext(ctx, "SELECT "+userColumns+" FROM users WHERE webauthn_user_handle = ?", handle))
	if err != nil {
		return User{}, fmt.Errorf("get user by WebAuthn user handle: %w", err)
	}
	return u, nil
}

// HasSecondFactor reports whether the user has a confirmed TOTP or at least
// one WebAuthn credential. Recovery codes don't count: they stand in for a
// second factor but aren't one (§6.2).
func (r Users) HasSecondFactor(ctx context.Context, userID string) (bool, error) {
	var n int
	err := r.q.QueryRowContext(ctx,
		"SELECT (SELECT COUNT(*) FROM user_totp WHERE user_id = ? AND confirmed_at IS NOT NULL) + "+
			"(SELECT COUNT(*) FROM webauthn_credentials WHERE user_id = ?)", userID, userID).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("check second factors of user %s: %w", userID, err)
	}
	return n > 0, nil
}

// execOne runs a statement that must affect exactly one row, returning
// ErrNotFound if it affected none.
func execOne(ctx context.Context, q Querier, what, query string, args ...any) error {
	res, err := q.ExecContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	if n == 0 {
		return fmt.Errorf("%s: %w", what, ErrNotFound)
	}
	return nil
}

// timePtr converts a nullable scanned time to a pointer.
func timePtr(t sql.NullTime) *time.Time {
	if !t.Valid {
		return nil
	}
	v := t.Time
	return &v
}

// utcPtr normalizes a nullable time like the database does, so the value a
// caller holds matches what reads back.
func utcPtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	v := utc(*t)
	return &v
}
