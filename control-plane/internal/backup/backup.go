// Package backup defines the contract for creating and restoring site
// snapshots (design doc §4.4, "BackupEngine").
//
// Each client group has its own repository with its own encryption key;
// snapshots are tagged with the site ID and the site's WordPress core version
// (§5.3). Package backup/restic implements the Engine; package
// backup/sitebackup runs backups as jobs and keeps the platform's records of
// them.
package backup

import (
	"context"
	"errors"
	"time"
)

// Sentinel errors.
var (
	// ErrSnapshotNotFound is returned when a snapshot does not exist or does
	// not belong to the given site.
	ErrSnapshotNotFound = errors.New("backup: snapshot not found")
	// ErrSiteNotFound is returned when the platform knows nothing of the
	// site: it has no site record and no backup records.
	ErrSiteNotFound = errors.New("backup: site not found")
)

// SnapshotID identifies a snapshot within a client group repository.
type SnapshotID string

// Options describe one backup.
type Options struct {
	// Kind says what asked for the backup (store.BackupKind: manual,
	// scheduled, pre_restore). It is recorded on the snapshot.
	Kind string
	// BackupID is the platform's ID of the backup (store.BackupRecord.ID),
	// recorded on the snapshot. When set, Backup is idempotent: if the
	// repository already has a snapshot of the site with this ID, from an
	// attempt whose result was lost, Backup returns it instead of taking
	// another.
	BackupID string
}

// Snapshot describes a snapshot of a site.
type Snapshot struct {
	ID   SnapshotID
	Time time.Time
	// SiteID, WPCoreVersion, Kind, and BackupID are read back from the
	// snapshot's tags; they are empty for a snapshot that lacks the tag.
	SiteID        string
	WPCoreVersion string
	Kind          string
	BackupID      string
	// Tags are all of the snapshot's tags.
	Tags []string
	// SizeBytes is the size of the files backed up, AddedBytes what the
	// backup added to the repository after deduplication and compression,
	// and FileCount the number of files.
	SizeBytes  int64
	AddedBytes int64
	FileCount  int64
	// Warnings are problems that didn't stop the backup, such as files that
	// couldn't be read and are missing from the snapshot. Only Backup fills
	// them in.
	Warnings []string
}

// Engine creates and restores site snapshots.
type Engine interface {
	// Backup snapshots the site's wp-content and a database dump into its
	// client group repository. WordPress core and wp-config.php are not
	// included; both are reproducible from the site image and site record.
	// Callers must not back up the same site twice at once (site jobs
	// share a lock key).
	Backup(ctx context.Context, siteID string, opts Options) (Snapshot, error)
	// ListSnapshots returns the site's snapshots in its client group's
	// repository, oldest first; none if there are none. It returns
	// ErrSiteNotFound if the platform knows nothing of the site.
	ListSnapshots(ctx context.Context, siteID string) ([]Snapshot, error)
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
