package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/id"
)

// TOTP is a user's authenticator app (design doc §6.2). The secret itself is
// an encrypted secret (package secrets); only its ID is kept here.
type TOTP struct {
	UserID   string
	SecretID string
	// ConfirmedAt is nil until the user enters a valid code. Only a
	// confirmed TOTP counts as a second factor.
	ConfirmedAt *time.Time
	// LastUsedStep is the RFC 6238 time step of the last accepted code;
	// codes from it or earlier are refused, so none can be replayed.
	LastUsedStep int64
	CreatedAt    time.Time
}

// Confirmed reports whether the TOTP has been confirmed.
func (t TOTP) Confirmed() bool { return t.ConfirmedAt != nil }

// UserTOTPs reads and writes users' TOTPs, at most one per user.
type UserTOTPs struct {
	q Querier
}

// NewUserTOTPs returns a UserTOTPs that runs queries on q.
func NewUserTOTPs(q Querier) UserTOTPs {
	return UserTOTPs{q: q}
}

const totpColumns = "user_id, secret_id, confirmed_at, last_used_step, created_at"

func scanTOTP(s scanner) (TOTP, error) {
	var (
		t         TOTP
		confirmed sql.NullTime
	)
	err := s.Scan(&t.UserID, &t.SecretID, &confirmed, &t.LastUsedStep, &t.CreatedAt)
	t.ConfirmedAt = timePtr(confirmed)
	return t, err
}

// Create inserts t, filling in CreatedAt if zero. The secret must already be
// stored. It returns ErrConflict if the user already has a TOTP, and
// ErrReference if the user or the secret doesn't exist.
func (r UserTOTPs) Create(ctx context.Context, t *TOTP) error {
	if t.UserID == "" || t.SecretID == "" {
		return errors.New("create TOTP: user and secret are required")
	}
	created := *t
	if created.CreatedAt.IsZero() {
		created.CreatedAt = now()
	}
	created.CreatedAt, created.ConfirmedAt = utc(created.CreatedAt), utcPtr(created.ConfirmedAt)
	_, err := r.q.ExecContext(ctx,
		"INSERT INTO user_totp ("+totpColumns+") VALUES (?, ?, ?, ?, ?)",
		created.UserID, created.SecretID, created.ConfirmedAt, created.LastUsedStep, created.CreatedAt)
	if err != nil {
		return fmt.Errorf("create TOTP for user %s: %w", t.UserID, err)
	}
	*t = created
	return nil
}

// Get returns the user's TOTP, confirmed or not, or ErrNotFound.
func (r UserTOTPs) Get(ctx context.Context, userID string) (TOTP, error) {
	t, err := scanTOTP(r.q.QueryRowContext(ctx, "SELECT "+totpColumns+" FROM user_totp WHERE user_id = ?", userID))
	if err != nil {
		return TOTP{}, fmt.Errorf("get TOTP of user %s: %w", userID, err)
	}
	return t, nil
}

// Confirm activates the user's unconfirmed TOTP with the code of the given
// time step, which then can't be used again. It returns ErrNotFound if the
// user has no TOTP or it is already confirmed, so of two concurrent
// confirmations only one succeeds.
func (r UserTOTPs) Confirm(ctx context.Context, userID string, step int64, at time.Time) error {
	return execOne(ctx, r.q, "confirm TOTP of user "+userID,
		"UPDATE user_totp SET confirmed_at = ?, last_used_step = ? WHERE user_id = ? AND confirmed_at IS NULL",
		at, step, userID)
}

// UseStep records that a code of the given time step was accepted. It
// reports false, changing nothing, if a code of that step or a later one was
// already accepted or the user has no confirmed TOTP. The check and the
// update are one statement, so a code accepted by two requests at once
// works for only one of them.
func (r UserTOTPs) UseStep(ctx context.Context, userID string, step int64) (bool, error) {
	res, err := r.q.ExecContext(ctx,
		"UPDATE user_totp SET last_used_step = ? WHERE user_id = ? AND confirmed_at IS NOT NULL AND last_used_step < ?",
		step, userID, step)
	if err != nil {
		return false, fmt.Errorf("use TOTP step of user %s: %w", userID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("use TOTP step of user %s: %w", userID, err)
	}
	return n > 0, nil
}

// Delete removes the user's TOTP and its encrypted secret. It returns
// ErrNotFound if the user has none. Run it in a transaction ([DB.InTx]), so
// the two go together.
func (r UserTOTPs) Delete(ctx context.Context, userID string) error {
	n, err := deleteTOTP(ctx, r.q, "user_id = ?", userID)
	if err != nil {
		return fmt.Errorf("delete TOTP of user %s: %w", userID, err)
	}
	if n == 0 {
		return fmt.Errorf("delete TOTP of user %s: %w", userID, ErrNotFound)
	}
	return nil
}

// DeleteUnconfirmedBefore removes TOTPs that were never confirmed and were
// created before cutoff, with their secrets, and returns how many it
// removed. Run it in a transaction, like Delete.
func (r UserTOTPs) DeleteUnconfirmedBefore(ctx context.Context, cutoff time.Time) (int64, error) {
	n, err := deleteTOTP(ctx, r.q, "confirmed_at IS NULL AND created_at < ?", cutoff)
	if err != nil {
		return 0, fmt.Errorf("delete unconfirmed TOTPs: %w", err)
	}
	return n, nil
}

// deleteTOTP deletes the user_totp rows matching where and then their
// secrets, which the rows' foreign keys protect until the rows are gone. It
// returns how many rows it deleted.
func deleteTOTP(ctx context.Context, q Querier, where string, args ...any) (int64, error) {
	rows, err := q.QueryContext(ctx, "DELETE FROM user_totp WHERE "+where+" RETURNING secret_id", args...)
	if err != nil {
		return 0, err
	}
	var secretIDs []string
	for rows.Next() {
		var secretID string
		if err := rows.Scan(&secretID); err != nil {
			_ = rows.Close()
			return 0, err
		}
		secretIDs = append(secretIDs, secretID)
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	for _, secretID := range secretIDs {
		if _, err := q.ExecContext(ctx, "DELETE FROM secrets WHERE id = ?", secretID); err != nil {
			return 0, fmt.Errorf("delete TOTP secret %s: %w", secretID, err)
		}
	}
	return int64(len(secretIDs)), nil
}

// WebAuthnCredential is a security key or passkey registered by a user
// (design doc §6.2).
type WebAuthnCredential struct {
	ID     string
	UserID string
	// CredentialID is the authenticator's credential ID; it is unique.
	CredentialID []byte
	// PublicKey is the credential's COSE public key.
	PublicKey []byte
	// Algorithm is the COSE algorithm identifier, e.g. -7 for ES256.
	Algorithm int64
	SignCount uint32
	AAGUID    []byte
	// Transports are the transports the authenticator reported, such as
	// "usb" or "internal".
	Transports     []string
	UserVerified   bool
	BackupEligible bool
	BackupState    bool
	// RPID is the relying party ID the credential was registered for.
	RPID string
	// Name is the user's name for the credential.
	Name       string
	CreatedAt  time.Time
	LastUsedAt *time.Time
}

// WebAuthnCredentials reads and writes WebAuthn credentials.
type WebAuthnCredentials struct {
	q Querier
}

// NewWebAuthnCredentials returns a WebAuthnCredentials that runs queries on q.
func NewWebAuthnCredentials(q Querier) WebAuthnCredentials {
	return WebAuthnCredentials{q: q}
}

const webAuthnColumns = "id, user_id, credential_id, public_key, algorithm, sign_count, aaguid, transports, " +
	"user_verified, backup_eligible, backup_state, rp_id, name, created_at, last_used_at"

func scanWebAuthnCredential(s scanner) (WebAuthnCredential, error) {
	var (
		c          WebAuthnCredential
		signCount  int64
		transports string
		lastUsed   sql.NullTime
	)
	err := s.Scan(&c.ID, &c.UserID, &c.CredentialID, &c.PublicKey, &c.Algorithm, &signCount, &c.AAGUID, &transports,
		&c.UserVerified, &c.BackupEligible, &c.BackupState, &c.RPID, &c.Name, &c.CreatedAt, &lastUsed)
	if err != nil {
		return c, err
	}
	c.SignCount = uint32(signCount) //nolint:gosec // G115: written from a uint32
	c.LastUsedAt = timePtr(lastUsed)
	if err := json.Unmarshal([]byte(transports), &c.Transports); err != nil {
		return c, fmt.Errorf("decoding transports of WebAuthn credential %s: %w", c.ID, err)
	}
	return c, nil
}

// Create inserts c, filling in its ID and, if zero, CreatedAt. It returns
// ErrConflict if the credential ID is already registered, and ErrReference if
// the user doesn't exist.
// webAuthnCredentialIDs keeps credentials registered in the same millisecond
// in order, so ListForUser's creation order holds.
var webAuthnCredentialIDs id.Monotonic

func (r WebAuthnCredentials) Create(ctx context.Context, c *WebAuthnCredential) error {
	if c.UserID == "" || len(c.CredentialID) == 0 || len(c.PublicKey) == 0 || c.RPID == "" || c.Name == "" {
		return errors.New("create WebAuthn credential: user, credential ID, public key, relying party ID, and name are required")
	}
	created := *c
	if created.CreatedAt.IsZero() {
		created.CreatedAt = now()
	}
	created.ID = webAuthnCredentialIDs.NewAt(created.CreatedAt)
	created.CreatedAt, created.LastUsedAt = utc(created.CreatedAt), utcPtr(created.LastUsedAt)
	if created.Transports == nil {
		created.Transports = []string{}
	}
	if created.AAGUID == nil {
		created.AAGUID = []byte{}
	}
	transports, err := json.Marshal(created.Transports)
	if err != nil {
		return fmt.Errorf("create WebAuthn credential: %w", err)
	}
	_, err = r.q.ExecContext(ctx,
		"INSERT INTO webauthn_credentials ("+webAuthnColumns+") VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		created.ID, created.UserID, created.CredentialID, created.PublicKey, created.Algorithm, int64(created.SignCount),
		created.AAGUID, string(transports), created.UserVerified, created.BackupEligible, created.BackupState,
		created.RPID, created.Name, created.CreatedAt, created.LastUsedAt)
	if err != nil {
		return fmt.Errorf("create WebAuthn credential: %w", err)
	}
	*c = created
	return nil
}

// Get returns one of a user's credentials by its ID, or ErrNotFound.
func (r WebAuthnCredentials) Get(ctx context.Context, userID, credID string) (WebAuthnCredential, error) {
	c, err := scanWebAuthnCredential(r.q.QueryRowContext(ctx,
		"SELECT "+webAuthnColumns+" FROM webauthn_credentials WHERE id = ? AND user_id = ?", credID, userID))
	if err != nil {
		return WebAuthnCredential{}, fmt.Errorf("get WebAuthn credential %s: %w", credID, err)
	}
	return c, nil
}

// GetByCredentialID returns the credential with the given authenticator
// credential ID, whoever it belongs to, or ErrNotFound.
func (r WebAuthnCredentials) GetByCredentialID(ctx context.Context, credentialID []byte) (WebAuthnCredential, error) {
	c, err := scanWebAuthnCredential(r.q.QueryRowContext(ctx,
		"SELECT "+webAuthnColumns+" FROM webauthn_credentials WHERE credential_id = ?", credentialID))
	if err != nil {
		return WebAuthnCredential{}, fmt.Errorf("get WebAuthn credential by credential ID: %w", err)
	}
	return c, nil
}

// ListForUser returns a user's credentials in ID (creation) order.
func (r WebAuthnCredentials) ListForUser(ctx context.Context, userID string) ([]WebAuthnCredential, error) {
	rows, err := r.q.QueryContext(ctx,
		"SELECT "+webAuthnColumns+" FROM webauthn_credentials WHERE user_id = ? ORDER BY id", userID)
	if err != nil {
		return nil, fmt.Errorf("list WebAuthn credentials: %w", err)
	}
	defer func() { _ = rows.Close() }()
	creds := []WebAuthnCredential{}
	for rows.Next() {
		c, err := scanWebAuthnCredential(rows)
		if err != nil {
			return nil, fmt.Errorf("list WebAuthn credentials: %w", err)
		}
		creds = append(creds, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list WebAuthn credentials: %w", err)
	}
	return creds, nil
}

// Rename sets the name of one of a user's credentials. It returns
// ErrNotFound if the user has no such credential.
func (r WebAuthnCredentials) Rename(ctx context.Context, userID, credID, name string) error {
	if name == "" {
		return errors.New("rename WebAuthn credential: name is required")
	}
	return execOne(ctx, r.q, "rename WebAuthn credential "+credID,
		"UPDATE webauthn_credentials SET name = ? WHERE id = ? AND user_id = ?", name, credID, userID)
}

// RecordUse saves what a successful authentication with the credential
// reported: its signature counter and backup state, and when it was used.
// It returns ErrNotFound if the credential doesn't exist.
func (r WebAuthnCredentials) RecordUse(ctx context.Context, credID string, signCount uint32, backupState bool, at time.Time) error {
	return execOne(ctx, r.q, "record use of WebAuthn credential "+credID,
		"UPDATE webauthn_credentials SET sign_count = ?, backup_state = ?, last_used_at = ? WHERE id = ?",
		int64(signCount), backupState, at, credID)
}

// Delete removes one of a user's credentials. It returns ErrNotFound if the
// user has no such credential.
func (r WebAuthnCredentials) Delete(ctx context.Context, userID, credID string) error {
	return execOne(ctx, r.q, "delete WebAuthn credential "+credID,
		"DELETE FROM webauthn_credentials WHERE id = ? AND user_id = ?", credID, userID)
}

// DeleteAllForUser removes every credential of a user and returns how many
// it removed.
func (r WebAuthnCredentials) DeleteAllForUser(ctx context.Context, userID string) (int64, error) {
	res, err := r.q.ExecContext(ctx, "DELETE FROM webauthn_credentials WHERE user_id = ?", userID)
	if err != nil {
		return 0, fmt.Errorf("delete WebAuthn credentials of user %s: %w", userID, err)
	}
	return res.RowsAffected()
}

// CountBoundElsewhere counts the credentials bound to a relying party ID
// other than rpID, and the users who have them. Such credentials were
// registered before the dashboard domain changed and can't be used.
func (r WebAuthnCredentials) CountBoundElsewhere(ctx context.Context, rpID string) (credentials, users int, err error) {
	err = r.q.QueryRowContext(ctx,
		"SELECT COUNT(*), COUNT(DISTINCT user_id) FROM webauthn_credentials WHERE rp_id <> ?", rpID).Scan(&credentials, &users)
	if err != nil {
		return 0, 0, fmt.Errorf("count WebAuthn credentials bound to another relying party: %w", err)
	}
	return credentials, users, nil
}

// RecoveryCode is a single-use code that stands in for a second factor at
// login (design doc §6.2). Only a hash of the code is stored.
type RecoveryCode struct {
	ID     string
	UserID string
	// CodeHash identifies the code, like a session token's hash.
	CodeHash string
	// UsedAt is set once the code has been used.
	UsedAt    *time.Time
	CreatedAt time.Time
}

// RecoveryCodes reads and writes recovery codes.
type RecoveryCodes struct {
	q Querier
}

// NewRecoveryCodes returns a RecoveryCodes that runs queries on q.
func NewRecoveryCodes(q Querier) RecoveryCodes {
	return RecoveryCodes{q: q}
}

const recoveryCodeColumns = "id, user_id, code_hash, used_at, created_at"

func scanRecoveryCode(s scanner) (RecoveryCode, error) {
	var (
		c    RecoveryCode
		used sql.NullTime
	)
	err := s.Scan(&c.ID, &c.UserID, &c.CodeHash, &used, &c.CreatedAt)
	c.UsedAt = timePtr(used)
	return c, err
}

// Create inserts c, filling in its ID and, if zero, CreatedAt. It returns
// ErrConflict if the user already has a code with this hash, and
// ErrReference if the user doesn't exist.
func (r RecoveryCodes) Create(ctx context.Context, c *RecoveryCode) error {
	if c.UserID == "" || c.CodeHash == "" {
		return errors.New("create recovery code: user and code hash are required")
	}
	created := *c
	created.ID = id.New()
	if created.CreatedAt.IsZero() {
		created.CreatedAt = now()
	}
	created.CreatedAt, created.UsedAt = utc(created.CreatedAt), utcPtr(created.UsedAt)
	_, err := r.q.ExecContext(ctx,
		"INSERT INTO recovery_codes ("+recoveryCodeColumns+") VALUES (?, ?, ?, ?, ?)",
		created.ID, created.UserID, created.CodeHash, created.UsedAt, created.CreatedAt)
	if err != nil {
		return fmt.Errorf("create recovery code: %w", err)
	}
	*c = created
	return nil
}

// Replace replaces a user's recovery codes with new ones with the given
// hashes, and returns them. Run it in a transaction ([DB.InTx]), so the old
// set never outlives the new one.
func (r RecoveryCodes) Replace(ctx context.Context, userID string, codeHashes []string, at time.Time) ([]RecoveryCode, error) {
	if _, err := r.DeleteAllForUser(ctx, userID); err != nil {
		return nil, err
	}
	codes := make([]RecoveryCode, 0, len(codeHashes))
	for _, h := range codeHashes {
		c := RecoveryCode{UserID: userID, CodeHash: h, CreatedAt: at}
		if err := r.Create(ctx, &c); err != nil {
			return nil, err
		}
		codes = append(codes, c)
	}
	return codes, nil
}

// ListForUser returns a user's recovery codes, used ones included, in ID
// (creation) order.
func (r RecoveryCodes) ListForUser(ctx context.Context, userID string) ([]RecoveryCode, error) {
	rows, err := r.q.QueryContext(ctx,
		"SELECT "+recoveryCodeColumns+" FROM recovery_codes WHERE user_id = ? ORDER BY id", userID)
	if err != nil {
		return nil, fmt.Errorf("list recovery codes: %w", err)
	}
	defer func() { _ = rows.Close() }()
	codes := []RecoveryCode{}
	for rows.Next() {
		c, err := scanRecoveryCode(rows)
		if err != nil {
			return nil, fmt.Errorf("list recovery codes: %w", err)
		}
		codes = append(codes, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list recovery codes: %w", err)
	}
	return codes, nil
}

// CountUnused returns how many of a user's recovery codes are left.
func (r RecoveryCodes) CountUnused(ctx context.Context, userID string) (int, error) {
	var n int
	if err := r.q.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM recovery_codes WHERE user_id = ? AND used_at IS NULL", userID).Scan(&n); err != nil {
		return 0, fmt.Errorf("count recovery codes of user %s: %w", userID, err)
	}
	return n, nil
}

// Use marks the user's unused recovery code with the given hash as used. It
// returns ErrNotFound if there is no such code or it was already used. The
// check and the update are one statement, so a code submitted twice at once
// works only once.
func (r RecoveryCodes) Use(ctx context.Context, userID, codeHash string, at time.Time) error {
	return execOne(ctx, r.q, "use recovery code of user "+userID,
		"UPDATE recovery_codes SET used_at = ? WHERE user_id = ? AND code_hash = ? AND used_at IS NULL",
		at, userID, codeHash)
}

// DeleteAllForUser removes every recovery code of a user and returns how
// many it removed.
func (r RecoveryCodes) DeleteAllForUser(ctx context.Context, userID string) (int64, error) {
	res, err := r.q.ExecContext(ctx, "DELETE FROM recovery_codes WHERE user_id = ?", userID)
	if err != nil {
		return 0, fmt.Errorf("delete recovery codes of user %s: %w", userID, err)
	}
	return res.RowsAffected()
}
