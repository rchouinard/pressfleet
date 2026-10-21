package secrets

import (
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/rchouinard/pressfleet/control-plane/internal/redact"
)

// KeySize is the length of a master key in bytes.
const KeySize = 32

// keyPrefix starts every encoded master key. The number is the format
// version; a future format gets a new prefix.
const keyPrefix = "pfmk1:"

// checksumSize is the length of the checksum appended to an encoded key.
const checksumSize = 4

// Labels for the values derived from a master key. Each purpose uses its own
// label, so the derived values are independent.
const (
	labelKeyID      = "pressfleet master key id v1"
	labelCheckValue = "pressfleet master key check v1"
	labelSecretsKey = "pressfleet secrets aes-256-gcm v1" //nolint:gosec // G101: an HKDF label, not a credential
	// labelStateBackup derives the password of the platform's state backup
	// repository, which can't be stored in the state database it backs up.
	labelStateBackup = "pressfleet state backup restic password v1"
)

// Sentinel errors for master keys.
var (
	// ErrNoMasterKey means no master key was configured.
	ErrNoMasterKey = errors.New("master key is not configured")
	// ErrInvalidMasterKey means the configured value isn't a usable master key.
	ErrInvalidMasterKey = errors.New("invalid master key")
	// ErrKeyMismatch means the master key isn't the one this installation's
	// secrets were encrypted with.
	ErrKeyMismatch = errors.New("master key does not match this installation")
)

// MasterKey is the platform master key. Like [redact.Secret], it never
// reveals itself when formatted, logged, or marshaled. The zero value is not
// a valid key.
type MasterKey struct {
	raw []byte
}

// GenerateMasterKey returns a new random master key.
func GenerateMasterKey() MasterKey {
	raw := make([]byte, KeySize)
	_, _ = rand.Read(raw) // crypto/rand.Read never returns an error
	return MasterKey{raw: raw}
}

// NewMasterKey returns the master key with the given bytes, for tests and
// tools that already hold raw key material.
func NewMasterKey(raw []byte) (MasterKey, error) {
	if len(raw) != KeySize {
		return MasterKey{}, fmt.Errorf("%w: must be %d bytes, got %d", ErrInvalidMasterKey, KeySize, len(raw))
	}
	return MasterKey{raw: append([]byte(nil), raw...)}, nil
}

// ParseMasterKey decodes an encoded master key, ignoring surrounding
// whitespace. Errors never include the value.
func ParseMasterKey(s redact.Secret) (MasterKey, error) {
	text := strings.TrimSpace(s.Reveal())
	if text == "" {
		return MasterKey{}, ErrNoMasterKey
	}
	encoded, ok := strings.CutPrefix(text, keyPrefix)
	if !ok {
		if strings.HasPrefix(text, "pfmk") {
			return MasterKey{}, fmt.Errorf("%w: unsupported format version (this pressfleet reads %q keys)", ErrInvalidMasterKey, keyPrefix)
		}
		return MasterKey{}, fmt.Errorf("%w: not a PressFleet master key (it should start with %q; generate one with `pressfleet key generate`)", ErrInvalidMasterKey, keyPrefix)
	}
	data, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil || len(data) != KeySize+checksumSize {
		return MasterKey{}, fmt.Errorf("%w: the key is garbled or truncated (check for a copy and paste error)", ErrInvalidMasterKey)
	}
	raw, sum := data[:KeySize], data[KeySize:]
	if subtle.ConstantTimeCompare(sum, checksum(raw)) != 1 {
		return MasterKey{}, fmt.Errorf("%w: the key is garbled (checksum mismatch; check for a copy and paste error)", ErrInvalidMasterKey)
	}
	return MasterKey{raw: append([]byte(nil), raw...)}, nil
}

// checksum detects typos in an encoded key. It is not a security measure.
func checksum(raw []byte) []byte {
	sum := sha256.Sum256(append([]byte(keyPrefix), raw...))
	return sum[:checksumSize]
}

// Encode returns the key in its text form, for showing to the operator once.
func (k MasterKey) Encode() redact.Secret {
	if !k.valid() {
		return ""
	}
	data := append(append([]byte(nil), k.raw...), checksum(k.raw)...)
	return redact.Secret(keyPrefix + base64.RawURLEncoding.EncodeToString(data))
}

func (k MasterKey) valid() bool { return len(k.raw) == KeySize }

// ID returns the key ID: a short fingerprint that identifies the key without
// revealing it. It is stored with every secret and safe to log.
func (k MasterKey) ID() string {
	return hex.EncodeToString(k.derive(labelKeyID)[:8])
}

// checkValue returns the value recorded in master_keys to recognize the key.
func (k MasterKey) checkValue() []byte {
	return k.derive(labelCheckValue)
}

// derive returns HMAC-SHA256(key, label).
func (k MasterKey) derive(label string) []byte {
	mac := hmac.New(sha256.New, k.raw)
	mac.Write([]byte(label))
	return mac.Sum(nil)
}

// secretsKey returns the AES-256 key for secrets.
func (k MasterKey) secretsKey() ([]byte, error) {
	return hkdf.Key(sha256.New, k.raw, nil, labelSecretsKey, 32)
}

// stateBackupPassword returns the restic password of the state backup
// repository: 256 bits derived from the key, base64url.
func (k MasterKey) stateBackupPassword() (redact.Secret, error) {
	b, err := hkdf.Key(sha256.New, k.raw, nil, labelStateBackup, 32)
	if err != nil {
		return "", err
	}
	return redact.Secret(base64.RawURLEncoding.EncodeToString(b)), nil
}

// String implements fmt.Stringer.
func (MasterKey) String() string { return redact.Placeholder }

// GoString implements fmt.GoStringer.
func (MasterKey) GoString() string { return redact.Placeholder }

// Format implements fmt.Formatter so that every verb prints the placeholder.
func (MasterKey) Format(f fmt.State, _ rune) {
	_, _ = f.Write([]byte(redact.Placeholder))
}

// LogValue implements slog.LogValuer.
func (MasterKey) LogValue() slog.Value { return slog.StringValue(redact.Placeholder) }

// MarshalText implements encoding.TextMarshaler, which also covers JSON.
func (MasterKey) MarshalText() ([]byte, error) { return []byte(redact.Placeholder), nil }
