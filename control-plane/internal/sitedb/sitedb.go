// Package sitedb defines the contract for managing per-site databases and
// users on the shared MariaDB server (design doc §4.4, "DatabaseManager").
//
// This is distinct from the control plane's own state database.
package sitedb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/rchouinard/pressfleet/control-plane/internal/redact"
)

// ErrDatabaseNotFound is returned when an operation targets a site that has
// no database.
var ErrDatabaseNotFound = errors.New("sitedb: database not found")

// Manager manages per-site databases and users on the shared MariaDB server.
//
// Jobs retry failed steps (§5), so CreateDatabase and DropDatabase must be
// idempotent.
type Manager interface {
	// CreateDatabase creates the site's database and a user whose grants are
	// limited to it, stores the generated credentials, and returns a
	// reference to them. If the database already exists it returns the
	// existing reference.
	CreateDatabase(ctx context.Context, siteID string) (secretRef string, err error)
	// DropDatabase drops the site's database and user. Dropping a database
	// that does not exist succeeds.
	DropDatabase(ctx context.Context, siteID string) error
	// Dump writes a logical dump of the site's database to destPath.
	// Returns ErrDatabaseNotFound if the site has no database.
	Dump(ctx context.Context, siteID string, destPath string) error
	// Import replaces the site's database contents with the dump at srcPath.
	// Returns ErrDatabaseNotFound if the site has no database.
	Import(ctx context.Context, siteID string, srcPath string) error

	// CreateTemporaryUser creates the temporary user of a database session
	// (design doc §5.8), named after the session, with a new random
	// password and access to the site's database only, and returns its
	// credentials. Calling it again for the same session sets a new
	// password.
	CreateTemporaryUser(ctx context.Context, siteID, sessionID string, access Access) (Credentials, error)
	// DropTemporaryUser drops the session's temporary user and ends its
	// open connections. Dropping a user that doesn't exist succeeds.
	DropTemporaryUser(ctx context.Context, sessionID string) error
	// ListTemporaryUsers returns the sessions whose temporary users exist
	// on the server, for dropping those left behind.
	ListTemporaryUsers(ctx context.Context) (sessionIDs []string, err error)
}

// Access is what a temporary user may do with the site's database.
type Access string

// Accesses of temporary users.
const (
	// AccessReadOnly may read tables and views, nothing else.
	AccessReadOnly Access = "read_only"
	// AccessReadWrite has the site's own privileges on its database.
	AccessReadWrite Access = "read_write"
)

// Valid reports whether a is a known access.
func (a Access) Valid() bool { return a == AccessReadOnly || a == AccessReadWrite }

// Credentials are a site's database connection details. The secret that
// CreateDatabase's secretRef points to holds them, encoded with
// MarshalCredentials. The database host is platform configuration, not part
// of the credentials.
type Credentials struct {
	Database string        `json:"database"`
	Username string        `json:"username"`
	Password redact.Secret `json:"-"`
}

// credentialsJSON is the stored form; Password is revealed only here.
type credentialsJSON struct {
	Database string `json:"database"`
	Username string `json:"username"`
	Password string `json:"password"`
}

// MarshalCredentials encodes credentials for storage as a secret.
func MarshalCredentials(c Credentials) (redact.Secret, error) {
	b, err := json.Marshal(credentialsJSON{Database: c.Database, Username: c.Username, Password: c.Password.Reveal()})
	if err != nil {
		return "", fmt.Errorf("encoding database credentials: %w", err)
	}
	return redact.Secret(b), nil
}

// UnmarshalCredentials decodes credentials stored by MarshalCredentials.
func UnmarshalCredentials(s redact.Secret) (Credentials, error) {
	var c credentialsJSON
	if err := json.Unmarshal([]byte(s.Reveal()), &c); err != nil {
		// Don't wrap: json errors can quote the input.
		return Credentials{}, errors.New("decoding database credentials: invalid format")
	}
	if c.Database == "" || c.Username == "" || c.Password == "" {
		return Credentials{}, errors.New("decoding database credentials: missing field")
	}
	return Credentials{Database: c.Database, Username: c.Username, Password: redact.Secret(c.Password)}, nil
}
