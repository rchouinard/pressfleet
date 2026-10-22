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
	// ErrCrossGroup is returned when a snapshot would be restored into a
	// site of another client group. Each group has its own repository and
	// key, and a restore task reaches one repository only, so the MVP
	// restores within a group.
	ErrCrossGroup = errors.New("backup: the snapshot is in another client group's repository; restores stay within a client group")
	// ErrNoRepository is returned by group-level operations (Prune, Check,
	// GroupSnapshots) when the client group has no repository.
	ErrNoRepository = errors.New("backup: the client group has no backup repository")
	// ErrRepositoryDamaged is returned by Check when the repository's
	// integrity check found errors. The repository needs an operator: it
	// must be repaired (restic repair) before its backups can be trusted.
	ErrRepositoryDamaged = errors.New("backup: the backup repository is damaged")
)

// SnapshotID identifies a snapshot within a client group repository.
type SnapshotID string

// Options describe one backup.
type Options struct {
	// Kind says what asked for the backup (store.BackupKind: manual,
	// scheduled, pre_restore, pre_update). It is recorded on the snapshot.
	Kind string
	// BackupID is the platform's ID of the backup (store.BackupRecord.ID),
	// recorded on the snapshot. When set and Retry is true, Backup is
	// idempotent: if the repository already has a snapshot of the site with
	// this ID, from an attempt whose result was lost, Backup returns it
	// instead of taking another.
	BackupID string
	// Retry says that an earlier attempt to take this backup may have got as
	// far as creating the snapshot. Only then does Backup look for it, which
	// costs a restic task; the first attempt can't have been preceded by
	// one.
	Retry bool
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
	// Paths are the paths the snapshot holds, as restic lists them.
	Paths []string
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

// Retention is a retention policy: which of each site's snapshots Prune
// keeps. A snapshot is kept if any rule keeps it; each site's snapshots
// are counted separately. The zero policy keeps everything.
type Retention struct {
	// KeepLast keeps the newest n snapshots.
	KeepLast int
	// KeepHourly, KeepDaily, KeepWeekly, KeepMonthly, and KeepYearly keep
	// the newest snapshot of each of the last n hours, days, weeks, months,
	// and years that have one.
	KeepHourly, KeepDaily, KeepWeekly, KeepMonthly, KeepYearly int
}

// KeepsEverything reports whether the policy has no rules, so Prune
// removes nothing.
func (r Retention) KeepsEverything() bool { return r == Retention{} }

// PruneResult is what Prune or ForgetSite did.
type PruneResult struct {
	// Forgotten are the snapshots removed: for Prune those the retention
	// policy doesn't keep, for ForgetSite all of the site's.
	Forgotten []SnapshotID
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
	// Restore restores snapshot snap, taken of siteID, into targetSiteID:
	// it replaces the target's wp-content with the snapshot's, removing
	// files the snapshot doesn't have, and its database with the
	// snapshot's dump. The target may be siteID itself or a different site
	// of the same client group (e.g. a future staging clone). It changes
	// nothing else: maintenance mode, URLs, and caches are the caller's.
	// Restoring again converges on the same state. Returns
	// ErrSnapshotNotFound if snap does not belong to siteID, ErrSiteNotFound
	// if the target doesn't exist, and ErrCrossGroup if the target is in
	// another client group.
	Restore(ctx context.Context, siteID string, snap SnapshotID, targetSiteID string) error
	// Prune applies the engine's retention policy to a client group
	// repository: it removes the snapshots the policy doesn't keep, then
	// the data that only they used. Running it again converges on the
	// same state. It takes an exclusive repository lock, so callers must
	// not run it alongside Check on the same repository; backups wait for
	// it. Returns ErrNoRepository if the group has no repository.
	Prune(ctx context.Context, clientGroupID string) (PruneResult, error)
	// Check verifies a client group repository's integrity, reading back
	// part of its data. It returns ErrRepositoryDamaged if it found errors,
	// and ErrNoRepository if the group has no repository. Like Prune, it
	// takes an exclusive lock.
	Check(ctx context.Context, clientGroupID string) error
	// GroupSnapshots returns every snapshot in a client group's
	// repository, of any site, oldest first; none if the group has no
	// repository.
	GroupSnapshots(ctx context.Context, clientGroupID string) ([]Snapshot, error)
	// ForgetSite removes every snapshot of the site from its client
	// group's repository, then the data that only they used (restic
	// prune), e.g. when the site is deleted together with its backups.
	// Other sites' snapshots in the repository are untouched. Running it
	// again converges on the same state: it prunes even when nothing is
	// left to forget, in case an earlier attempt forgot snapshots and
	// failed before pruning. Like Prune it takes an exclusive repository
	// lock, waiting for a prune or check that holds one. A group without
	// a repository has nothing to forget. It returns ErrSiteNotFound if
	// the platform knows nothing of the site.
	ForgetSite(ctx context.Context, siteID string) (PruneResult, error)
}
