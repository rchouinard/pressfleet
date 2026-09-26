// Package backup defines the contract for creating and restoring site
// snapshots (design doc §4.4, "BackupEngine").
//
// Each client group has its own repository with its own encryption key;
// snapshots are tagged with the site ID and the site's WordPress core version
// (§5.3).
package backup

import (
	"context"
	"errors"
)

// ErrSnapshotNotFound is returned when a snapshot does not exist or does not
// belong to the given site.
var ErrSnapshotNotFound = errors.New("backup: snapshot not found")

// SnapshotID identifies a snapshot within a client group repository.
type SnapshotID string

// Engine creates and restores site snapshots.
type Engine interface {
	// Backup snapshots the site's wp-content and a database dump into its
	// client group repository. WordPress core and wp-config.php are not
	// included; both are reproducible from the site image and site record.
	Backup(ctx context.Context, siteID string) (SnapshotID, error)
	// ListSnapshots returns the site's snapshots, oldest first.
	ListSnapshots(ctx context.Context, siteID string) ([]SnapshotID, error)
	// Restore restores snapshot snap, taken of siteID, into targetSiteID.
	// The target may be siteID itself or a different site (e.g. a future
	// staging clone). Returns ErrSnapshotNotFound if snap does not belong to
	// siteID.
	Restore(ctx context.Context, siteID string, snap SnapshotID, targetSiteID string) error
	// Prune applies the retention policy to a client group repository.
	// Pruning takes an exclusive repository lock, so callers must serialize
	// it with other operations on the same repository.
	Prune(ctx context.Context, clientGroupID string) error
}
