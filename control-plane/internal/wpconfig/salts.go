package wpconfig

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/rchouinard/pressfleet/control-plane/internal/redact"
)

// Salts are a site's WordPress authentication keys and salts. They sign
// login cookies and nonces, so they are secrets: the provisioning job
// generates them once, stores them with secrets.Store (encoded with
// MarshalSalts), and every render of wp-config.php reads them back. Changing
// them logs every user out.
type Salts struct {
	AuthKey        redact.Secret
	SecureAuthKey  redact.Secret
	LoggedInKey    redact.Secret
	NonceKey       redact.Secret
	AuthSalt       redact.Secret
	SecureAuthSalt redact.Secret
	LoggedInSalt   redact.Secret
	NonceSalt      redact.Secret
}

// saltField pairs a salt's constant name with its field.
type saltField struct {
	name  string
	value *redact.Secret
}

// fields lists the salts in the order wp-config.php defines them. The
// constant names double as the keys of the stored JSON.
func (s *Salts) fields() []saltField {
	return []saltField{
		{"AUTH_KEY", &s.AuthKey},
		{"SECURE_AUTH_KEY", &s.SecureAuthKey},
		{"LOGGED_IN_KEY", &s.LoggedInKey},
		{"NONCE_KEY", &s.NonceKey},
		{"AUTH_SALT", &s.AuthSalt},
		{"SECURE_AUTH_SALT", &s.SecureAuthSalt},
		{"LOGGED_IN_SALT", &s.LoggedInSalt},
		{"NONCE_SALT", &s.NonceSalt},
	}
}

// SaltLength is the length of each generated key and salt, the same as
// WordPress's secret-key service and wp_generate_password(64, true, true).
const SaltLength = 64

// saltChars is the alphabet of wp_generate_password with special and extra
// special characters, which is what WordPress uses for keys and salts. It
// contains neither a quote nor a backslash, although Render escapes values
// regardless.
const saltChars = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789" +
	"!@#$%^&*()" + "-_ []{}<>~`+=,.;:/?|"

// GenerateSalts returns a fresh set of keys and salts from crypto/rand.
func GenerateSalts() Salts {
	var s Salts
	for _, f := range s.fields() {
		*f.value = redact.Secret(randomString(SaltLength))
	}
	return s
}

// randomString returns n characters drawn uniformly from saltChars.
func randomString(n int) string {
	// Rejection sampling: accept only bytes below the largest multiple of
	// the alphabet size, so the modulo doesn't favor early characters.
	limit := byte(256 - 256%len(saltChars))
	out := make([]byte, 0, n)
	buf := make([]byte, 2*n)
	for len(out) < n {
		_, _ = rand.Read(buf) // crypto/rand.Read never returns an error
		for _, b := range buf {
			if b < limit && len(out) < n {
				out = append(out, saltChars[int(b)%len(saltChars)])
			}
		}
	}
	return string(out)
}

// validate reports whether every salt is set.
func (s Salts) validate() error {
	for _, f := range s.fields() {
		if f.value.Reveal() == "" {
			return fmt.Errorf("salt %s is empty", f.name)
		}
	}
	return nil
}

// MarshalSalts encodes salts for storage as a secret: a JSON object keyed by
// the WordPress constant names, e.g. {"AUTH_KEY":"...",...}.
func MarshalSalts(s Salts) (redact.Secret, error) {
	if err := s.validate(); err != nil {
		return "", fmt.Errorf("encoding salts: %w", err)
	}
	m := make(map[string]string, 8)
	for _, f := range s.fields() {
		m[f.name] = f.value.Reveal()
	}
	b, err := json.Marshal(m)
	if err != nil {
		return "", fmt.Errorf("encoding salts: %w", err)
	}
	return redact.Secret(b), nil
}

// UnmarshalSalts decodes salts stored by MarshalSalts. Every salt must be
// present and non-empty, and unknown keys are rejected.
func UnmarshalSalts(secret redact.Secret) (Salts, error) {
	var m map[string]string
	if err := json.Unmarshal([]byte(secret.Reveal()), &m); err != nil {
		// Don't wrap: json errors can quote the input.
		return Salts{}, errors.New("decoding salts: invalid format")
	}
	var s Salts
	fields := s.fields()
	if len(m) != len(fields) {
		return Salts{}, fmt.Errorf("decoding salts: want %d keys, got %d", len(fields), len(m))
	}
	for _, f := range fields {
		v, ok := m[f.name]
		if !ok || v == "" {
			return Salts{}, fmt.Errorf("decoding salts: %s is missing", f.name)
		}
		*f.value = redact.Secret(v)
	}
	return s, nil
}
