package auth

import (
	"context"
	"crypto/rsa"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"golang.org/x/crypto/ssh"

	"github.com/rchouinard/pressfleet/control-plane/internal/store"
)

// SSH keys (design doc §5.8). Users register SSH public keys on their
// account; the SFTP gateway identifies the user by the key alone, since the
// SSH username names the site. A key is therefore registered to one user at
// most, and it is looked up by its SHA-256 fingerprint.

// Sentinel errors for SSH keys.
var (
	// ErrInvalidSSHKey means a public key can't be parsed or is of a type
	// or size the gateway doesn't accept.
	ErrInvalidSSHKey = errors.New("invalid SSH public key")
	// ErrInvalidSSHKeyName means an SSH key name is too long.
	ErrInvalidSSHKeyName = errors.New("SSH key name must be at most 100 characters")
	// ErrSSHKeyTaken means the key is already registered, to this user or
	// another: a key must identify one user.
	ErrSSHKeyTaken = errors.New("this SSH key is already registered")
	// ErrSSHKeyNotFound means the user has no such SSH key.
	ErrSSHKeyNotFound = errors.New("SSH key not found")
)

// MinRSAKeyBits is the smallest RSA key accepted. 2048 bits is NIST's
// minimum through 2030; ssh-keygen has made 3072-bit keys by default since
// OpenSSH 8.0.
const MinRSAKeyBits = 2048

// maxSSHKeyNameLen matches the API's limit.
const maxSSHKeyNameLen = 100

// acceptedKeyTypes are the SSH public key algorithms users may register.
// DSA is obsolete (OpenSSH removed it) and certificates would need a CA the
// platform doesn't have.
var acceptedKeyTypes = map[string]bool{
	ssh.KeyAlgoED25519:    true,
	ssh.KeyAlgoSKED25519:  true,
	ssh.KeyAlgoECDSA256:   true,
	ssh.KeyAlgoECDSA384:   true,
	ssh.KeyAlgoECDSA521:   true,
	ssh.KeyAlgoSKECDSA256: true,
	ssh.KeyAlgoRSA:        true,
}

// ParseSSHPublicKey parses one public key in authorized_keys format
// ("ssh-ed25519 AAAA... comment"), as users paste it from their .pub file,
// and returns it with its comment. Options before the key type are not
// accepted: the platform decides what a key may do. It returns an error
// wrapping ErrInvalidSSHKey for anything else, including key types the
// gateway doesn't accept and RSA keys under MinRSAKeyBits.
func ParseSSHPublicKey(s string) (ssh.PublicKey, string, error) {
	s = strings.TrimSpace(s)
	if s == "" || strings.ContainsAny(s, "\r\n") {
		return nil, "", fmt.Errorf("%w: give exactly one key, as a single line from a .pub file", ErrInvalidSSHKey)
	}
	key, comment, options, rest, err := ssh.ParseAuthorizedKey([]byte(s))
	switch {
	case err != nil:
		return nil, "", fmt.Errorf("%w: expected a line like \"ssh-ed25519 AAAA... name@host\"", ErrInvalidSSHKey)
	case len(options) > 0:
		return nil, "", fmt.Errorf("%w: authorized_keys options are not accepted", ErrInvalidSSHKey)
	case len(rest) > 0:
		return nil, "", fmt.Errorf("%w: give exactly one key", ErrInvalidSSHKey)
	case !acceptedKeyTypes[key.Type()]:
		return nil, "", fmt.Errorf("%w: %s keys are not accepted; use an Ed25519 (recommended), ECDSA, or RSA key", ErrInvalidSSHKey, key.Type())
	}
	if ck, ok := key.(ssh.CryptoPublicKey); ok {
		if rk, ok := ck.CryptoPublicKey().(*rsa.PublicKey); ok && rk.N.BitLen() < MinRSAKeyBits {
			return nil, "", fmt.Errorf("%w: RSA keys must have at least %d bits, this one has %d", ErrInvalidSSHKey, MinRSAKeyBits, rk.N.BitLen())
		}
	}
	return key, strings.TrimSpace(comment), nil
}

// FormatSSHPublicKey returns key in authorized_keys format, without a
// comment or trailing newline: how the store keeps it.
func FormatSSHPublicKey(key ssh.PublicKey) string {
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key)))
}

// NewSSHKey describes an SSH key to register.
type NewSSHKey struct {
	// Name labels the key. Empty means the key's comment, or its type if it
	// has none.
	Name string
	// PublicKey is the key in authorized_keys format (ParseSSHPublicKey).
	PublicKey string
}

// AddSSHKey registers an SSH public key for a user. It returns an error
// wrapping ErrInvalidSSHKey for a key it doesn't accept, ErrSSHKeyTaken if
// the key is already registered (to anyone), and ErrUserNotFound if the
// user doesn't exist.
func (s *Service) AddSSHKey(ctx context.Context, userID string, nk NewSSHKey) (store.SSHKey, error) {
	key, comment, err := ParseSSHPublicKey(nk.PublicKey)
	if err != nil {
		return store.SSHKey{}, err
	}
	name := strings.TrimSpace(nk.Name)
	if name == "" {
		name = comment
	}
	if name == "" {
		name = key.Type()
	}
	if utf8.RuneCountInString(name) > maxSSHKeyNameLen || strings.ContainsAny(name, "\r\n") {
		if nk.Name != "" {
			return store.SSHKey{}, ErrInvalidSSHKeyName
		}
		// An overlong comment only names the key; it needn't be refused.
		name = truncate(strings.ReplaceAll(strings.ReplaceAll(name, "\r", " "), "\n", " "), maxSSHKeyNameLen)
	}
	k := store.SSHKey{
		UserID:      userID,
		Name:        name,
		PublicKey:   FormatSSHPublicKey(key),
		Fingerprint: ssh.FingerprintSHA256(key),
		CreatedAt:   s.now(),
	}
	switch err := store.NewSSHKeys(s.db).Create(ctx, &k); {
	case errors.Is(err, store.ErrConflict):
		return store.SSHKey{}, ErrSSHKeyTaken
	case errors.Is(err, store.ErrReference):
		return store.SSHKey{}, ErrUserNotFound
	case err != nil:
		return store.SSHKey{}, fmt.Errorf("add SSH key: %w", err)
	}
	s.log.Info("SSH key added", "user_id", userID, "ssh_key_id", k.ID, "fingerprint", k.Fingerprint)
	return k, nil
}

// ListSSHKeys lists a user's SSH keys, after the cursor after, at most
// limit of them (zero means no limit).
func (s *Service) ListSSHKeys(ctx context.Context, userID, after string, limit int) ([]store.SSHKey, error) {
	return store.NewSSHKeys(s.db).ListForUser(ctx, userID, after, limit)
}

// DeleteSSHKey removes one of a user's SSH keys, or returns
// ErrSSHKeyNotFound. Sessions using the key end within the gateway's
// recheck interval.
func (s *Service) DeleteSSHKey(ctx context.Context, userID, keyID string) error {
	err := store.NewSSHKeys(s.db).Delete(ctx, userID, keyID)
	if errors.Is(err, store.ErrNotFound) {
		return ErrSSHKeyNotFound
	} else if err != nil {
		return err
	}
	s.log.Info("SSH key deleted", "user_id", userID, "ssh_key_id", keyID)
	return nil
}

// AuthenticateSSHKey returns the principal for an SSH public key, and the
// registered key. It returns ErrUnauthenticated if no user has registered
// the key or its user is disabled. The principal carries the user's
// current role; authorization (package authz) decides what the key may
// reach.
//
// It has no side effects, because SSH clients offer public keys before
// proving they hold them: the gateway calls it for offers as well as for
// proven keys, and records a key's use (NoteSSHKeyUsed) only once a
// session starts.
func (s *Service) AuthenticateSSHKey(ctx context.Context, key ssh.PublicKey) (Principal, store.SSHKey, error) {
	k, err := store.NewSSHKeys(s.db).GetByFingerprint(ctx, ssh.FingerprintSHA256(key))
	if errors.Is(err, store.ErrNotFound) {
		return Principal{}, store.SSHKey{}, ErrUnauthenticated
	} else if err != nil {
		return Principal{}, store.SSHKey{}, fmt.Errorf("authenticate SSH key: %w", err)
	}
	// The fingerprint is a hash; make sure it is this very key.
	if k.PublicKey != FormatSSHPublicKey(key) {
		return Principal{}, store.SSHKey{}, ErrUnauthenticated
	}
	p, err := s.sshPrincipal(ctx, k)
	if err != nil {
		return Principal{}, store.SSHKey{}, err
	}
	return p, k, nil
}

// NoteSSHKeyUsed records that a key opened a session, for the key's
// LastUsedAt. Like API tokens, it writes at most once a minute per key.
// Failures are logged, not returned: they must not end the session.
func (s *Service) NoteSSHKeyUsed(ctx context.Context, k store.SSHKey) {
	now := s.now()
	if k.LastUsedAt != nil && now.Sub(*k.LastUsedAt) < touchInterval {
		return
	}
	if err := store.NewSSHKeys(s.db).TouchLastUsed(ctx, k.ID, now); err != nil && !errors.Is(err, store.ErrNotFound) {
		s.log.Warn("updating SSH key last used failed", "ssh_key_id", k.ID, "error", err)
	}
}

// RecheckSSHKey returns the current principal for a registered key, for a
// connection authenticated with it earlier: it returns ErrUnauthenticated
// once the key has been removed or its user disabled or deleted, and
// otherwise the user's current role.
func (s *Service) RecheckSSHKey(ctx context.Context, keyID string) (Principal, error) {
	k, err := store.NewSSHKeys(s.db).Get(ctx, keyID)
	if errors.Is(err, store.ErrNotFound) {
		return Principal{}, ErrUnauthenticated
	} else if err != nil {
		return Principal{}, fmt.Errorf("recheck SSH key: %w", err)
	}
	return s.sshPrincipal(ctx, k)
}

func (s *Service) sshPrincipal(ctx context.Context, k store.SSHKey) (Principal, error) {
	user, err := s.activeUser(ctx, k.UserID)
	if err != nil {
		return Principal{}, err
	}
	return Principal{UserID: user.ID, Email: user.Email, Role: user.Role, Method: MethodSSHKey, SSHKeyID: k.ID}, nil
}
