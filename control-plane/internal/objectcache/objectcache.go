// Package objectcache defines the contract for managing each site's access
// to the shared object cache, a Redis-protocol server (design doc §5.1 step
// 4, §6). Package objectcache/redis implements it.
package objectcache

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/rchouinard/pressfleet/control-plane/internal/redact"
)

// Manager manages per-site object cache users.
//
// Jobs retry failed steps (§5), so CreateUser and DeleteUser must be
// idempotent.
type Manager interface {
	// CreateUser gives the site its cache user, restricted to its own
	// database and key prefix, stores the generated credentials, and
	// returns a reference to them. If the site already has a user it
	// converges the server to the stored credentials and returns the
	// existing reference.
	CreateUser(ctx context.Context, siteID string) (secretRef string, err error)
	// DeleteUser removes the site's user, its cached data, and its stored
	// credentials. Deleting a user that does not exist succeeds.
	DeleteUser(ctx context.Context, siteID string) error
	// FlushCache empties the site's cache and nothing else, whatever the
	// site's own configuration says. A site without a user has nothing
	// cached, so flushing it succeeds.
	FlushCache(ctx context.Context, siteID string) error
}

// KeyPrefix returns the prefix every key of the site must start with, as
// enforced by its ACL user: the site ID and a colon. The Redis Object Cache
// plugin builds keys as WP_REDIS_PREFIX + "<table prefix>:<group>:<key>",
// so WP_REDIS_PREFIX must be exactly this, colon included.
func KeyPrefix(siteID string) string { return siteID + ":" }

// Credentials are a site's object cache connection details. The secret that
// CreateUser's secretRef points to holds them, encoded with
// MarshalCredentials. The server's address is platform configuration, not
// part of the credentials.
type Credentials struct {
	// Username and Password authenticate with AUTH <username> <password>.
	Username string
	Password redact.Secret
	// Database is the site's logical database (SELECT). It is never 0, so
	// clients that only select a non-zero database still select it.
	Database int
	// KeyPrefix is KeyPrefix(site ID).
	KeyPrefix string
}

// credentialsJSON is the stored form; Password is revealed only here.
type credentialsJSON struct {
	Username  string `json:"username"`
	Password  string `json:"password"`
	Database  int    `json:"database"`
	KeyPrefix string `json:"key_prefix"`
}

// MarshalCredentials encodes credentials for storage as a secret.
func MarshalCredentials(c Credentials) (redact.Secret, error) {
	b, err := json.Marshal(credentialsJSON{Username: c.Username, Password: c.Password.Reveal(), Database: c.Database, KeyPrefix: c.KeyPrefix})
	if err != nil {
		return "", fmt.Errorf("encoding object cache credentials: %w", err)
	}
	return redact.Secret(b), nil
}

// UnmarshalCredentials decodes credentials stored by MarshalCredentials.
func UnmarshalCredentials(s redact.Secret) (Credentials, error) {
	var c credentialsJSON
	if err := json.Unmarshal([]byte(s.Reveal()), &c); err != nil {
		// Don't wrap: json errors can quote the input.
		return Credentials{}, errors.New("decoding object cache credentials: invalid format")
	}
	if c.Username == "" || c.Password == "" || c.Database <= 0 || c.KeyPrefix == "" {
		return Credentials{}, errors.New("decoding object cache credentials: missing field")
	}
	return Credentials{Username: c.Username, Password: redact.Secret(c.Password), Database: c.Database, KeyPrefix: c.KeyPrefix}, nil
}
