package restic

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/backup"
	"github.com/rchouinard/pressfleet/control-plane/internal/id"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
)

// ErrRestoreFailed is wrapped by errors from a restic restore that ran but
// failed, or that left something out.
var ErrRestoreFailed = errors.New("restic restore failed")

// snapshotIDPattern is what Restore accepts as a snapshot ID: restic's full
// ID or a prefix of at least 8 hex digits, the short form restic shows.
var snapshotIDPattern = regexp.MustCompile(`^[0-9a-f]{8,64}$`)

// Restore implements backup.Engine; see the package documentation.
func (e *Engine) Restore(ctx context.Context, siteID string, snap backup.SnapshotID, targetSiteID string) error {
	if !id.Valid(siteID) || !id.Valid(targetSiteID) {
		return fmt.Errorf("restore snapshot %s of site %q into site %q: invalid site ID", snap, siteID, targetSiteID)
	}
	if !snapshotIDPattern.MatchString(string(snap)) {
		return fmt.Errorf("restore snapshot %q of site %s: %w", snap, siteID, backup.ErrSnapshotNotFound)
	}
	what := fmt.Sprintf("restore snapshot %s of site %s into site %s", shortID(snap), siteID, targetSiteID)
	target, err := store.NewSites(e.db).Get(ctx, targetSiteID)
	if errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("%s: %w", what, backup.ErrSiteNotFound)
	}
	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	groupID, err := e.siteGroup(ctx, siteID)
	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	if groupID != target.ClientGroupID {
		return fmt.Errorf("%s: %w", what, backup.ErrCrossGroup)
	}
	access, err := e.repos.TaskAccess(ctx, groupID)
	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	full, err := e.resolveSnapshot(ctx, groupID, siteID, snap)
	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}

	start := time.Now()
	// Named after the target and snapshot, so a retry replaces what a
	// crashed attempt left behind.
	transfer, err := e.newTransferDir("restore-", targetSiteID, shortID(full))
	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	defer func() {
		if err := os.RemoveAll(transfer); err != nil {
			e.log.ErrorContext(ctx, "removing a restore transfer directory failed", "site_id", targetSiteID, "dir", transfer, "error", err)
		}
	}()
	task := provisioner.Task{
		Env:           access.Env,
		SecretEnv:     access.SecretEnv,
		RepositoryDir: access.RepositoryDir,
		BackupNetwork: access.BackupNetwork,
		Timeout:       e.timeout,
	}

	// The dump first: it changes nothing, so a snapshot without one fails
	// before the site is touched.
	dumpTask := task
	dumpTask.Command = restoreDumpCommand(full)
	dumpTask.TransferDir = transfer
	if _, err := restoreResult(e.tasks.RunTask(ctx, targetSiteID, dumpTask)); err != nil {
		return fmt.Errorf("%s: extracting the database dump: %w", what, err)
	}
	dump := filepath.Join(transfer, DatabaseDumpName)
	if fi, err := os.Stat(dump); err != nil || !fi.Mode().IsRegular() {
		return fmt.Errorf("%s: %w: the snapshot has no database dump", what, ErrRestoreFailed)
	}

	filesTask := task
	filesTask.Command = restoreFilesCommand(full)
	filesTask.WPContent = provisioner.MountReadWrite
	summary, err := restoreResult(e.tasks.RunTask(ctx, targetSiteID, filesTask))
	if err != nil {
		return fmt.Errorf("%s: restoring wp-content: %w", what, err)
	}
	if err := e.dbs.Import(ctx, targetSiteID, dump); err != nil {
		return fmt.Errorf("%s: importing the database: %w", what, err)
	}
	e.log.InfoContext(ctx, "snapshot restored", "site_id", siteID, "target_site_id", targetSiteID, "client_group_id", groupID,
		"snapshot_id", full, "files_restored", summary.FilesRestored, "files_deleted", summary.FilesDeleted, "duration", time.Since(start))
	return nil
}

// resolveSnapshot returns the full ID of the site's snapshot snap, which
// may be a unique prefix, or ErrSnapshotNotFound.
func (e *Engine) resolveSnapshot(ctx context.Context, groupID, siteID string, snap backup.SnapshotID) (backup.SnapshotID, error) {
	snaps, err := e.snapshots(ctx, groupID, TagSite+siteID)
	if err != nil {
		return "", fmt.Errorf("listing the site's snapshots: %w", err)
	}
	var found []backup.SnapshotID
	for _, s := range snaps {
		if s.ID == snap {
			return s.ID, nil
		}
		if strings.HasPrefix(string(s.ID), string(snap)) {
			found = append(found, s.ID)
		}
	}
	if len(found) != 1 {
		return "", fmt.Errorf("snapshot %s of site %s: %w", snap, siteID, backup.ErrSnapshotNotFound)
	}
	return found[0], nil
}

// restoreDumpCommand extracts the snapshot's database dump into the
// transfer directory.
func restoreDumpCommand(snap backup.SnapshotID) []string {
	return []string{
		"restic", "restore", "--json", "--quiet", "--retry-lock", LockWait.String(),
		string(snap) + ":" + provisioner.TaskTransferPath,
		"--target", provisioner.TaskTransferPath,
		"--include", "/" + DatabaseDumpName,
	}
}

// restoreFilesCommand replaces wp-content with the snapshot's. Restoring
// the snapshot's wp-content directory (the snapshot:subfolder form) into
// the mounted volume, with --delete, leaves exactly the snapshot's files:
// files added since are removed and changed ones overwritten. The task
// runs as the target site's UID without privileges, so restic can't
// restore the files' original owners and they belong to the target site,
// also when the snapshot is of another site (restic ignores lchown
// permission errors when not root). The volume's own root is left as it
// is.
func restoreFilesCommand(snap backup.SnapshotID) []string {
	return []string{
		"restic", "restore", "--json", "--quiet", "--retry-lock", LockWait.String(),
		string(snap) + ":" + WPContentPath,
		"--target", WPContentPath,
		"--delete",
	}
}

// shortID is the short form restic shows snapshot IDs in.
func shortID(snap backup.SnapshotID) string {
	if len(snap) > 8 {
		return string(snap[:8])
	}
	return string(snap)
}
