package restic

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"

	"github.com/rchouinard/pressfleet/control-plane/internal/backup"
	"github.com/rchouinard/pressfleet/control-plane/internal/backup/repository"
	"github.com/rchouinard/pressfleet/control-plane/internal/id"
)

// ErrDumpFailed is wrapped by errors from a restic dump that ran but
// failed, and for snapshots that lack what was asked for.
var ErrDumpFailed = errors.New("restic dump failed")

// DumpFiles writes the wp-content of snapshot snap, taken of siteID, to w
// as a tar archive whose paths are relative to wp-content, for a site
// export (design doc §5.7). It returns the snapshot. The site may have
// been deleted since: its snapshots stay in its client group's repository
// until that is deleted. Nothing about the site is touched.
//
// It runs `restic dump --archive tar` in a repository task, like
// Prune and Check, rather than in a site task: a deleted site has no
// containers to run a task with. restic takes a non-exclusive lock and
// waits for an exclusive one (LockWait). The archive holds what restic
// stored, symbolic links and all; the caller decides what to keep. It
// returns backup.ErrSnapshotNotFound if snap isn't one of the site's
// snapshots, and backup.ErrSiteNotFound if the platform knows nothing of
// the site.
func (e *Engine) DumpFiles(ctx context.Context, siteID string, snap backup.SnapshotID, w io.Writer) (backup.Snapshot, error) {
	return e.dump(ctx, siteID, snap, "wp-content", w, func(s backup.Snapshot) ([]string, error) {
		dir, ok := snapshotPath(s, path.Base(WPContentPath))
		if !ok {
			return nil, fmt.Errorf("%w: the snapshot has no wp-content", ErrDumpFailed)
		}
		// <snapshot>:<dir> roots the archive at wp-content itself.
		return []string{"--archive", "tar", string(s.ID) + ":" + dir, "/"}, nil
	})
}

// DumpDatabase writes the database dump of snapshot snap, taken of siteID,
// to w: plain SQL, as Backup stored it (see the package documentation).
// It runs like DumpFiles and returns the same errors.
func (e *Engine) DumpDatabase(ctx context.Context, siteID string, snap backup.SnapshotID, w io.Writer) (backup.Snapshot, error) {
	return e.dump(ctx, siteID, snap, "database dump", w, func(s backup.Snapshot) ([]string, error) {
		file, ok := snapshotPath(s, DatabaseDumpName)
		if !ok {
			return nil, fmt.Errorf("%w: the snapshot has no database dump", ErrDumpFailed)
		}
		dir, name := path.Split(file)
		return []string{string(s.ID) + ":" + strings.TrimSuffix(dir, "/"), "/" + name}, nil
	})
}

// dump runs `restic dump` with the arguments args returns for the site's
// snapshot, writing its output to w.
func (e *Engine) dump(ctx context.Context, siteID string, snap backup.SnapshotID, what string, w io.Writer,
	args func(backup.Snapshot) ([]string, error)) (backup.Snapshot, error) {
	if !id.Valid(siteID) {
		return backup.Snapshot{}, fmt.Errorf("dump snapshot %s of site %q: invalid site ID", snap, siteID)
	}
	if !snapshotIDPattern.MatchString(string(snap)) {
		return backup.Snapshot{}, fmt.Errorf("dump snapshot %q of site %s: %w", snap, siteID, backup.ErrSnapshotNotFound)
	}
	desc := fmt.Sprintf("dump the %s of snapshot %s of site %s", what, shortID(snap), siteID)
	groupID, err := e.siteGroup(ctx, siteID)
	if err != nil {
		return backup.Snapshot{}, fmt.Errorf("%s: %w", desc, err)
	}
	s, err := e.findSnapshot(ctx, groupID, siteID, snap)
	if errors.Is(err, repository.ErrNoRepository) {
		err = fmt.Errorf("%w: %w", backup.ErrSnapshotNotFound, err)
	}
	if err != nil {
		return backup.Snapshot{}, fmt.Errorf("%s: %w", desc, err)
	}
	rest, err := args(s)
	if err != nil {
		return backup.Snapshot{}, fmt.Errorf("%s: %w", desc, err)
	}
	cmd := append([]string{"dump", "--quiet", "--retry-lock", LockWait.String()}, rest...)
	if err := e.repos.ResticTo(ctx, groupID, w, cmd...); err != nil {
		return backup.Snapshot{}, fmt.Errorf("%s: %w: %w", desc, ErrDumpFailed, err)
	}
	return s, nil
}

// snapshotPath returns the path the snapshot holds whose last element is
// name. Snapshots taken by Backup hold WPContentPath and DatabasePath; one
// taken by hand, or by tests on the host, may hold them elsewhere.
func snapshotPath(s backup.Snapshot, name string) (string, bool) {
	for _, p := range s.Paths {
		if path.Base(p) == name && path.IsAbs(p) {
			return path.Clean(p), true
		}
	}
	return "", false
}
