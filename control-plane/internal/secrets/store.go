package secrets

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/id"
	"github.com/rchouinard/pressfleet/control-plane/internal/redact"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
)

// Ref is an opaque reference to a stored secret. It is what the rest of the
// control plane keeps instead of plaintext, e.g. provisioner.SiteConfig's
// DBSecretRef.
type Ref string

// Kind says what a secret is for. It is authenticated with the ciphertext,
// so a secret can't be passed off as another kind.
type Kind string

// Kinds of secrets stored by the control plane (design doc §6).
const (
	KindSiteDBPassword    Kind = "site-db-password"
	KindSiteRedisPassword Kind = "site-redis-password"
	KindResticRepoKey     Kind = "restic-repository-key"
	KindDNSCredentials    Kind = "dns-provider-credentials" //nolint:gosec // G101: a kind name, not a credential
	KindS3Credentials     Kind = "s3-credentials"
	KindSiteWPSalts       Kind = "site-wp-salts" // wpconfig.MarshalSalts
	// KindSiteWPAdminPassword is a site's generated WordPress admin
	// password, kept until the API hands it out once.
	KindSiteWPAdminPassword Kind = "site-wp-admin-password" //nolint:gosec // G101: a kind name, not a credential
	// KindSitePreviewPassword is a site's generated preview domain basic
	// auth password (§5.1), kept for the uptime checks that send it.
	KindSitePreviewPassword Kind = "site-preview-password"
	// KindAlertChannel is the credentials of an alert channel (#76): an
	// SMTP password, a webhook URL, a webhook signing secret.
	KindAlertChannel Kind = "alert-channel-credentials"
	// KindPlatformLoginKey is the Ed25519 private key (its 32-byte seed,
	// base64) that signs dashboard login tokens (#128, §5.9).
	KindPlatformLoginKey Kind = "platform-login-key"
	// KindTOTPSecret is a user's TOTP secret (§6.2), its raw bytes base32
	// encoded. It can't be hashed, since checking a code needs it.
	KindTOTPSecret Kind = "totp-secret"
)

// Kinds are validated by shape rather than against the list above, so a new
// kind of secret needs no change here.
var kindPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,63}$`)

// ErrInvalidKind means a kind isn't well formed.
var ErrInvalidKind = errors.New("invalid secret kind")

func (k Kind) validate() error {
	if !kindPattern.MatchString(string(k)) {
		return fmt.Errorf("%w %q: use up to 64 lowercase letters, digits, and hyphens, starting with a letter", ErrInvalidKind, k)
	}
	return nil
}

// Store reads and writes encrypted secrets. Like the store repositories it
// wraps a Querier, so the same code runs on a *store.DB or inside
// store.DB.InTx.
type Store struct {
	q    store.Querier
	keys *Keyring
}

// NewStore returns a Store that runs queries on q and encrypts with keys.
func NewStore(q store.Querier, keys *Keyring) Store {
	return Store{q: q, keys: keys}
}

// With returns a copy of s that runs queries on q, typically the Querier of
// a transaction.
func (s Store) With(q store.Querier) Store {
	return Store{q: q, keys: s.keys}
}

// Put encrypts and stores a new secret, returning its reference.
func (s Store) Put(ctx context.Context, kind Kind, plaintext redact.Secret) (Ref, error) {
	if err := kind.validate(); err != nil {
		return "", fmt.Errorf("put secret: %w", err)
	}
	secretID := id.New()
	keyID, ciphertext := s.keys.seal(secretID, kind, plaintext)
	now := time.Now()
	_, err := s.q.ExecContext(ctx,
		"INSERT INTO secrets (id, kind, key_id, ciphertext, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)",
		secretID, string(kind), keyID, ciphertext, now, now)
	if err != nil {
		return "", fmt.Errorf("put %s secret: %w", kind, err)
	}
	return Ref(secretID), nil
}

// Get decrypts a secret. It returns store.ErrNotFound if it doesn't exist,
// ErrUnknownKey if it was encrypted with a key that isn't loaded, and
// ErrDecrypt if the stored row fails authentication.
func (s Store) Get(ctx context.Context, ref Ref) (redact.Secret, error) {
	var (
		kind, keyID string
		ciphertext  []byte
	)
	err := s.q.QueryRowContext(ctx, "SELECT kind, key_id, ciphertext FROM secrets WHERE id = ?", string(ref)).
		Scan(&kind, &keyID, &ciphertext)
	if err != nil {
		return "", fmt.Errorf("get secret %s: %w", ref, err)
	}
	plain, err := s.keys.open(string(ref), Kind(kind), keyID, ciphertext)
	if err != nil {
		return "", fmt.Errorf("get secret %s: %w", ref, err)
	}
	return plain, nil
}

// Update replaces a secret's value, keeping its reference and kind. It
// re-encrypts with the current key. It returns store.ErrNotFound if the
// secret doesn't exist.
func (s Store) Update(ctx context.Context, ref Ref, plaintext redact.Secret) error {
	var kind string
	if err := s.q.QueryRowContext(ctx, "SELECT kind FROM secrets WHERE id = ?", string(ref)).Scan(&kind); err != nil {
		return fmt.Errorf("update secret %s: %w", ref, err)
	}
	keyID, ciphertext := s.keys.seal(string(ref), Kind(kind), plaintext)
	res, err := s.q.ExecContext(ctx,
		"UPDATE secrets SET key_id = ?, ciphertext = ?, updated_at = ? WHERE id = ?",
		keyID, ciphertext, time.Now(), string(ref))
	if err != nil {
		return fmt.Errorf("update secret %s: %w", ref, err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return fmt.Errorf("update secret %s: %w", ref, err)
	} else if n == 0 {
		// Deleted between the two statements.
		return fmt.Errorf("update secret %s: %w", ref, store.ErrNotFound)
	}
	return nil
}

// Delete removes a secret. Deleting a secret that doesn't exist succeeds, so
// job steps that clean up can safely run again (design doc §5).
func (s Store) Delete(ctx context.Context, ref Ref) error {
	if _, err := s.q.ExecContext(ctx, "DELETE FROM secrets WHERE id = ?", string(ref)); err != nil {
		return fmt.Errorf("delete secret %s: %w", ref, err)
	}
	return nil
}

// Verify checks that the keyring's current key is this installation's
// master key. The first time a key is used with a database, it is recorded
// as the current key and firstUse is true. A different key fails with
// ErrKeyMismatch.
func Verify(ctx context.Context, db *store.DB, keys *Keyring) (firstUse bool, err error) {
	err = db.InTx(ctx, func(q store.Querier) error {
		var e error
		firstUse, e = verify(ctx, q, keys.current)
		return e
	})
	if errors.Is(err, store.ErrConflict) {
		// Another control plane registered a key at the same moment; compare
		// against the key it recorded.
		err = db.InTx(ctx, func(q store.Querier) error {
			var e error
			firstUse, e = verify(ctx, q, keys.current)
			return e
		})
	}
	return firstUse, err
}

// VerifyIn is Verify inside a transaction the caller holds, e.g. to check a
// state database loaded from a backup before committing it.
func VerifyIn(ctx context.Context, q store.Querier, keys *Keyring) (firstUse bool, err error) {
	return verify(ctx, q, keys.current)
}

func verify(ctx context.Context, q store.Querier, k *keyEntry) (firstUse bool, err error) {
	var (
		currentID string
		check     []byte
	)
	err = q.QueryRowContext(ctx, "SELECT id, check_value FROM master_keys WHERE is_current = ?", true).
		Scan(&currentID, &check)
	switch {
	case err == nil:
		if currentID != k.id || subtle.ConstantTimeCompare(check, k.checkValue) != 1 {
			return false, fmt.Errorf("%w: the configured key has ID %s, but this installation uses %s; supply the master key saved when PressFleet was installed",
				ErrKeyMismatch, k.id, currentID)
		}
		return false, nil
	case !errors.Is(err, store.ErrNotFound):
		return false, fmt.Errorf("reading master key check value: %w", err)
	}

	// No key yet. Secrets without a recorded key would mean the check value
	// was lost; refuse rather than adopt a key that may not match them.
	var n int
	if err := q.QueryRowContext(ctx, "SELECT COUNT(*) FROM secrets").Scan(&n); err != nil {
		return false, fmt.Errorf("counting secrets: %w", err)
	}
	if n > 0 {
		return false, fmt.Errorf("%w: the database holds %d secrets but no master key check value", ErrKeyMismatch, n)
	}
	if _, err := q.ExecContext(ctx,
		"INSERT INTO master_keys (id, check_value, is_current, created_at) VALUES (?, ?, ?, ?)",
		k.id, k.checkValue, true, time.Now()); err != nil {
		return false, fmt.Errorf("recording master key check value: %w", err)
	}
	return true, nil
}
