// Package sitedb defines the contract for managing per-site databases and
// users on the shared MariaDB server (design doc §4.4, "DatabaseManager").
//
// This is distinct from the control plane's own state database.
package sitedb

import (
	"context"
	"errors"
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
}
