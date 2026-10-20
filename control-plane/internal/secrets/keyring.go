package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"

	"github.com/rchouinard/pressfleet/control-plane/internal/redact"
)

// formatAESGCM marks a ciphertext sealed with AES-256-GCM and a random
// 96-bit nonce prepended. A new algorithm gets a new format byte.
const formatAESGCM byte = 1

// aadLabel starts the additional authenticated data of every secret.
const aadLabel = "pressfleet secret"

// Sentinel errors for decryption.
var (
	// ErrDecrypt means a stored secret failed authentication: its ciphertext
	// was modified, copied from another row, or its kind was changed.
	ErrDecrypt = errors.New("secret failed to decrypt (tampered with or moved between rows)")
	// ErrUnknownKey means a secret was encrypted with a master key that
	// isn't loaded.
	ErrUnknownKey = errors.New("secret is encrypted with a master key that is not loaded")
)

// Keyring holds the master keys secrets are encrypted with. New secrets are
// sealed with the current key; existing ones are opened with the key named
// by their key ID. Today it holds only the current key; master key rotation
// will add the retired keys.
type Keyring struct {
	current *keyEntry
	byID    map[string]*keyEntry
}

type keyEntry struct {
	id         string
	checkValue []byte
	aead       cipher.AEAD
	// stateBackup is the state backup repository's password.
	stateBackup redact.Secret
}

// NewKeyring returns a keyring whose current key is current.
func NewKeyring(current MasterKey) (*Keyring, error) {
	e, err := newKeyEntry(current)
	if err != nil {
		return nil, err
	}
	return &Keyring{current: e, byID: map[string]*keyEntry{e.id: e}}, nil
}

func newKeyEntry(k MasterKey) (*keyEntry, error) {
	if !k.valid() {
		return nil, fmt.Errorf("%w: empty key", ErrInvalidMasterKey)
	}
	key, err := k.secretsKey()
	if err != nil {
		return nil, fmt.Errorf("deriving secrets key: %w", err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCMWithRandomNonce(block)
	if err != nil {
		return nil, err
	}
	stateBackup, err := k.stateBackupPassword()
	if err != nil {
		return nil, fmt.Errorf("deriving the state backup password: %w", err)
	}
	return &keyEntry{id: k.ID(), checkValue: k.checkValue(), aead: aead, stateBackup: stateBackup}, nil
}

// KeyID returns the current key's ID.
func (r *Keyring) KeyID() string { return r.current.id }

// StateBackupPassword returns the password of the platform's state backup
// repository (package backup/statebackup), derived from the current master
// key. The repository holds the state database, so its password can't be
// one of the secrets stored there: a new host has only the master key.
// Master key rotation will have to re-key that repository too.
func (r *Keyring) StateBackupPassword() redact.Secret { return r.current.stateBackup }

// seal encrypts plaintext for the secret row (secretID, kind) with the
// current key, returning the key ID to store with the ciphertext.
func (r *Keyring) seal(secretID string, kind Kind, plaintext redact.Secret) (keyID string, ciphertext []byte) {
	e := r.current
	out := []byte{formatAESGCM}
	// The AEAD from NewGCMWithRandomNonce requires a nil nonce and generates
	// a random one itself.
	out = e.aead.Seal(out, nil, []byte(plaintext.Reveal()), aad(formatAESGCM, e.id, secretID, kind)) //nolint:gosec // G407: see above
	return e.id, out
}

// open decrypts a ciphertext stored in row (secretID, kind, keyID).
func (r *Keyring) open(secretID string, kind Kind, keyID string, ciphertext []byte) (redact.Secret, error) {
	e, ok := r.byID[keyID]
	if !ok {
		return "", fmt.Errorf("%w (key ID %q; this process uses %s)", ErrUnknownKey, keyID, r.current.id)
	}
	if len(ciphertext) < 1 || ciphertext[0] != formatAESGCM {
		return "", fmt.Errorf("%w: unknown ciphertext format", ErrDecrypt)
	}
	plain, err := e.aead.Open(nil, nil, ciphertext[1:], aad(ciphertext[0], keyID, secretID, kind))
	if err != nil {
		return "", ErrDecrypt
	}
	return redact.Secret(plain), nil
}

// aad builds the additional authenticated data that binds a ciphertext to
// its row. Fields are length-prefixed so no two rows produce the same bytes.
func aad(format byte, keyID, secretID string, kind Kind) []byte {
	b := append([]byte(aadLabel), 0, format)
	for _, f := range []string{keyID, secretID, string(kind)} {
		b = binary.BigEndian.AppendUint16(b, uint16(len(f))) //nolint:gosec // G115: IDs and kinds are short
		b = append(b, f...)
	}
	return b
}

// Format implements fmt.Formatter so that a keyring never prints key
// material.
func (r *Keyring) Format(f fmt.State, _ rune) {
	_, _ = fmt.Fprintf(f, "Keyring{current: %s}", r.current.id)
}

// LogValue implements slog.LogValuer.
func (r *Keyring) LogValue() slog.Value {
	return slog.GroupValue(slog.String("current_key_id", r.current.id))
}
