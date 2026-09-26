package auth

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"

	"github.com/rchouinard/pressfleet/control-plane/internal/redact"
)

// Password length policy, in characters. The maximum bounds hashing work.
const (
	MinPasswordLength = 12
	MaxPasswordLength = 256
)

// ErrPasswordPolicy means a new password doesn't meet the length policy.
var ErrPasswordPolicy = errors.New("password must be between 12 and 256 characters")

// CheckPasswordPolicy returns an error wrapping ErrPasswordPolicy if pw
// can't be used as a password.
func CheckPasswordPolicy(pw redact.Secret) error {
	n := utf8.RuneCountInString(pw.Reveal())
	if n < MinPasswordLength || n > MaxPasswordLength {
		return ErrPasswordPolicy
	}
	return nil
}

// Params are argon2id parameters.
type Params struct {
	// Memory is in KiB.
	Memory  uint32
	Time    uint32
	Threads uint8
	SaltLen uint32
	KeyLen  uint32
}

// DefaultParams are the parameters for new hashes (see the package
// documentation).
var DefaultParams = Params{Memory: 19 * 1024, Time: 2, Threads: 1, SaltLen: 16, KeyLen: 32}

// Limits on parameters read from stored hashes, so a corrupted hash can't
// make verification allocate unbounded memory.
const (
	maxMemory  = 1 << 20 // 1 GiB
	maxTime    = 64
	maxKeyLen  = 128
	maxSaltLen = 64
)

// maxConcurrentHashes bounds the memory used by concurrent logins.
const maxConcurrentHashes = 2

var errMalformedHash = errors.New("malformed password hash")

// hasher hashes and verifies passwords.
type hasher struct {
	params Params
	sem    chan struct{}
}

func newHasher(p Params) *hasher {
	return &hasher{params: p, sem: make(chan struct{}, maxConcurrentHashes)}
}

// key runs argon2id, waiting for a free slot.
func (h *hasher) key(ctx context.Context, pw, salt []byte, p Params) ([]byte, error) {
	select {
	case h.sem <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-h.sem }()
	return argon2.IDKey(pw, salt, p.Time, p.Memory, p.Threads, p.KeyLen), nil
}

// hash returns the encoded hash of pw with a new random salt.
func (h *hasher) hash(ctx context.Context, pw redact.Secret) (redact.Secret, error) {
	salt := make([]byte, h.params.SaltLen)
	_, _ = rand.Read(salt) // never fails (crypto/rand documentation)
	key, err := h.key(ctx, []byte(pw.Reveal()), salt, h.params)
	if err != nil {
		return "", err
	}
	return redact.Secret(encodeHash(h.params, salt, key)), nil
}

// verify reports whether pw matches the encoded hash, and whether the hash
// uses parameters other than the current ones and should be replaced.
func (h *hasher) verify(ctx context.Context, encoded, pw redact.Secret) (ok, rehash bool, err error) {
	p, salt, want, err := decodeHash(encoded.Reveal())
	if err != nil {
		return false, false, err
	}
	got, err := h.key(ctx, []byte(pw.Reveal()), salt, p)
	if err != nil {
		return false, false, err
	}
	ok = subtle.ConstantTimeCompare(got, want) == 1
	return ok, p != h.params, nil
}

var b64 = base64.RawStdEncoding

func encodeHash(p Params, salt, key []byte) string {
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, p.Memory, p.Time, p.Threads, b64.EncodeToString(salt), b64.EncodeToString(key))
}

func decodeHash(s string) (Params, []byte, []byte, error) {
	parts := strings.Split(s, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" {
		return Params{}, nil, nil, errMalformedHash
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return Params{}, nil, nil, errMalformedHash
	}
	var p Params
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &p.Memory, &p.Time, &p.Threads); err != nil {
		return Params{}, nil, nil, errMalformedHash
	}
	salt, err := b64.DecodeString(parts[4])
	if err != nil {
		return Params{}, nil, nil, errMalformedHash
	}
	key, err := b64.DecodeString(parts[5])
	if err != nil {
		return Params{}, nil, nil, errMalformedHash
	}
	p.SaltLen, p.KeyLen = uint32(len(salt)), uint32(len(key)) //nolint:gosec // G115: lengths are bounded below
	if p.Memory == 0 || p.Memory > maxMemory || p.Time == 0 || p.Time > maxTime || p.Threads == 0 ||
		len(salt) < 8 || len(salt) > maxSaltLen || len(key) < 16 || len(key) > maxKeyLen {
		return Params{}, nil, nil, errMalformedHash
	}
	return p, salt, key, nil
}
