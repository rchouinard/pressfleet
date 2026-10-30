package restic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
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
func (e *Engine) Restore(ctx context.Context, siteID string, snap backup.SnapshotID, targetSiteID string, opts backup.RestoreOptions) error {
	if err := opts.Validate(); err != nil {
		return fmt.Errorf("restore snapshot %s of site %q into site %q: %w", snap, siteID, targetSiteID, err)
	}
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
	found, err := e.findSnapshot(ctx, groupID, siteID, snap)
	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	full := found.ID
	// Which code directories the snapshot has, found before anything is
	// touched, since a restic restore of a missing one fails.
	var codeDirs map[string]bool
	if opts.Files == backup.RestoreFilesCode {
		if codeDirs, err = e.snapshotCodeDirs(ctx, groupID, found); err != nil {
			return fmt.Errorf("%s: %w", what, err)
		}
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
		SecretFiles:   access.SecretFiles,
		RepositoryDir: access.RepositoryDir,
		BackupNetwork: access.BackupNetwork,
		Timeout:       e.timeout,
	}

	// The dump first: it changes nothing, so a snapshot without one fails
	// before the site is touched.
	var dump string
	if opts.Database {
		dumpTask := task
		dumpTask.Command = restoreDumpCommand(full)
		dumpTask.TransferDir = transfer
		if _, err := restoreResult(e.tasks.RunTask(ctx, targetSiteID, dumpTask)); err != nil {
			return fmt.Errorf("%s: extracting the database dump: %w", what, err)
		}
		dump = filepath.Join(transfer, DatabaseDumpName)
		if fi, err := os.Stat(dump); err != nil || !fi.Mode().IsRegular() {
			return fmt.Errorf("%s: %w: the snapshot has no database dump", what, ErrRestoreFailed)
		}
	}

	var summary message
	// run runs one wp-content task and adds up what restic restored and
	// deleted.
	run := func(command []string) error {
		t := task
		t.Command = command
		t.WPContent = provisioner.MountReadWrite
		s, err := restoreResult(e.tasks.RunTask(ctx, targetSiteID, t))
		if err != nil {
			return err
		}
		summary.FilesRestored += s.FilesRestored
		summary.FilesDeleted += s.FilesDeleted
		return nil
	}
	switch opts.Files {
	case backup.RestoreFilesAll:
		if err := run(restoreFilesCommand(full)); err != nil {
			return fmt.Errorf("%s: restoring wp-content: %w", what, err)
		}
	case backup.RestoreFilesCode:
		for _, dir := range backup.CodeDirs {
			command := restoreCodeDirCommand(full, dir)
			if !codeDirs[dir] {
				command = removeCodeDirCommand(dir)
			}
			if err := run(command); err != nil {
				return fmt.Errorf("%s: restoring wp-content/%s: %w", what, dir, err)
			}
		}
	}
	if opts.Database {
		if err := e.dbs.Import(ctx, targetSiteID, dump); err != nil {
			return fmt.Errorf("%s: importing the database: %w", what, err)
		}
	}
	e.log.InfoContext(ctx, "snapshot restored", "site_id", siteID, "target_site_id", targetSiteID, "client_group_id", groupID,
		"snapshot_id", full, "files_scope", opts.Files, "database", opts.Database,
		"files_restored", summary.FilesRestored, "files_deleted", summary.FilesDeleted, "duration", time.Since(start))
	return nil
}

// snapshotCodeDirs reports which of the code directories the snapshot's
// wp-content holds. It lists the snapshot's wp-content, one level deep, in
// a repository task.
func (e *Engine) snapshotCodeDirs(ctx context.Context, groupID string, s backup.Snapshot) (map[string]bool, error) {
	wp, ok := snapshotPath(s, path.Base(WPContentPath))
	if !ok {
		return nil, fmt.Errorf("%w: the snapshot has no wp-content", ErrRestoreFailed)
	}
	out, err := e.repos.Restic(ctx, groupID, "ls", "--json", "--quiet", "--retry-lock", LockWait.String(), string(s.ID), wp)
	if err != nil {
		return nil, fmt.Errorf("listing the snapshot's wp-content: %w: %w", ErrRestoreFailed, err)
	}
	dirs := map[string]bool{}
	for line := range strings.Lines(string(out)) {
		var n struct {
			Type string `json:"type"`
			Path string `json:"path"`
		}
		if json.Unmarshal([]byte(line), &n) != nil || n.Type != "dir" {
			continue
		}
		if name, ok := strings.CutPrefix(n.Path, wp+"/"); ok && slices.Contains(backup.CodeDirs, name) {
			dirs[name] = true
		}
	}
	return dirs, nil
}

// findSnapshot returns the site's snapshot snap, whose ID may be a unique
// prefix, or ErrSnapshotNotFound.
func (e *Engine) findSnapshot(ctx context.Context, groupID, siteID string, snap backup.SnapshotID) (backup.Snapshot, error) {
	snaps, err := e.snapshots(ctx, groupID, TagSite+siteID)
	if err != nil {
		return backup.Snapshot{}, fmt.Errorf("listing the site's snapshots: %w", err)
	}
	var found []backup.Snapshot
	for _, s := range snaps {
		if s.ID == snap {
			return s, nil
		}
		if strings.HasPrefix(string(s.ID), string(snap)) {
			found = append(found, s)
		}
	}
	if len(found) != 1 {
		return backup.Snapshot{}, fmt.Errorf("snapshot %s of site %s: %w", snap, siteID, backup.ErrSnapshotNotFound)
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

// restoreCodeDirCommand makes one code directory of wp-content match the
// snapshot's, like restoreFilesCommand does for all of wp-content but with
// the directory as both source and target, so nothing else in wp-content is
// read or deleted. The directory must exist in the snapshot.
func restoreCodeDirCommand(snap backup.SnapshotID, dir string) []string {
	p := WPContentPath + "/" + dir
	return []string{
		"restic", "restore", "--json", "--quiet", "--retry-lock", LockWait.String(),
		string(snap) + ":" + p,
		"--target", p,
		"--delete",
	}
}

// removeCodeDirCommand removes a code directory the snapshot doesn't have,
// since a restore of a directory missing from the snapshot would fail. The
// output isn't restic's, so restoreResult finds no summary and counts
// nothing.
func removeCodeDirCommand(dir string) []string {
	return []string{"rm", "-rf", "--", WPContentPath + "/" + dir}
}

// shortID is the short form restic shows snapshot IDs in.
func shortID(snap backup.SnapshotID) string {
	if len(snap) > 8 {
		return string(snap[:8])
	}
	return string(snap)
}
