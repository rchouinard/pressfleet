package restic

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/backup"
	"github.com/rchouinard/pressfleet/control-plane/internal/backup/repository"
	"github.com/rchouinard/pressfleet/control-plane/internal/config"
	"github.com/rchouinard/pressfleet/control-plane/internal/id"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
)

// Snapshot layout (see the package documentation).
const (
	// WPContentPath is where a snapshot holds the site's wp-content.
	WPContentPath = provisioner.TaskWPContentPath
	// DatabaseDumpName is the dump's file name in the transfer directory.
	DatabaseDumpName = "database.sql"
	// DatabasePath is where a snapshot holds the database dump.
	DatabasePath = provisioner.TaskTransferPath + "/" + DatabaseDumpName

	// Tag prefixes; each tag is the prefix followed by the value.
	TagSite   = "site:"
	TagWPCore = "wp-core:"
	TagKind   = "kind:"
	TagBackup = "backup:"
)

// Host returns the host name of the site's snapshots.
func Host(siteID string) string { return "site-" + siteID }

// Defaults.
const (
	// DefaultTaskTimeout bounds one backup task.
	DefaultTaskTimeout = 6 * time.Hour
	// LockWait is how long a backup waits for a conflicting (exclusive)
	// repository lock, held by a prune or check, to be released.
	LockWait = 30 * time.Minute
)

// ErrBackupFailed is wrapped by errors from a restic backup that ran but
// failed. Errors for a locked repository or a wrong password wrap
// repository.ErrLocked and repository.ErrWrongPassword too.
var ErrBackupFailed = errors.New("restic backup failed")

// Repositories opens client group repositories; *repository.Manager
// implements it.
type Repositories interface {
	// TaskAccess ensures the group's repository exists and returns what a
	// site task needs to use it.
	TaskAccess(ctx context.Context, clientGroupID string) (repository.TaskAccess, error)
	// Restic runs restic against the group's repository in the control
	// plane process.
	Restic(ctx context.Context, clientGroupID string, args ...string) ([]byte, error)
}

// Databases dumps and imports site databases; sitedb.Manager implements
// it.
type Databases interface {
	Dump(ctx context.Context, siteID, destPath string) error
	Import(ctx context.Context, siteID, srcPath string) error
}

// Options configure an Engine.
type Options struct {
	// DB is the state database, for site and backup records. Required.
	DB *store.DB
	// Repositories opens client group repositories. Required.
	Repositories Repositories
	// Databases dumps and imports site databases. Required.
	Databases Databases
	// Tasks runs restic next to the site's data. Required. Tasks use the
	// driver's default image (config task_image), the control plane image,
	// which ships restic.
	Tasks provisioner.TaskRunner
	// Layout locates the transfer directories (Layout.TmpDir). Required.
	Layout config.Layout
	// TaskTimeout bounds one backup task; zero means DefaultTaskTimeout.
	TaskTimeout time.Duration
	// Retention is the policy Prune applies; the zero policy keeps every
	// snapshot.
	Retention backup.Retention
	// CheckReadDataPercent is the share of a repository's data, 0 to 100,
	// that Check reads back.
	CheckReadDataPercent int
	// Logger is required.
	Logger *slog.Logger
}

// Engine is the Restic backup engine.
type Engine struct {
	db      *store.DB
	repos   Repositories
	dbs     Databases
	tasks   provisioner.TaskRunner
	layout  config.Layout
	timeout time.Duration
	keep    backup.Retention
	readPct int
	log     *slog.Logger
}

var _ backup.Engine = (*Engine)(nil)

// New returns an Engine.
func New(opts Options) (*Engine, error) {
	if opts.DB == nil || opts.Repositories == nil || opts.Databases == nil || opts.Tasks == nil || opts.Logger == nil {
		return nil, errors.New("restic: DB, Repositories, Databases, Tasks, and Logger are required")
	}
	if !filepath.IsAbs(opts.Layout.Root) {
		return nil, fmt.Errorf("restic: data directory must be an absolute path, got %q", opts.Layout.Root)
	}
	if opts.CheckReadDataPercent < 0 || opts.CheckReadDataPercent > 100 {
		return nil, fmt.Errorf("restic: CheckReadDataPercent must be from 0 to 100, got %d", opts.CheckReadDataPercent)
	}
	for _, n := range []int{opts.Retention.KeepLast, opts.Retention.KeepHourly, opts.Retention.KeepDaily,
		opts.Retention.KeepWeekly, opts.Retention.KeepMonthly, opts.Retention.KeepYearly} {
		if n < 0 {
			return nil, fmt.Errorf("restic: invalid retention policy %+v", opts.Retention)
		}
	}
	e := &Engine{
		db: opts.DB, repos: opts.Repositories, dbs: opts.Databases, tasks: opts.Tasks,
		layout: opts.Layout, timeout: opts.TaskTimeout, keep: opts.Retention, readPct: opts.CheckReadDataPercent, log: opts.Logger,
	}
	if e.timeout <= 0 {
		e.timeout = DefaultTaskTimeout
	}
	return e, nil
}

var (
	// tagValue is what a tag value may hold: restic splits --tag values on
	// commas, and the values are also shown to people.
	tagValue = regexp.MustCompile(`^[A-Za-z0-9._+-]+$`)
	kindName = regexp.MustCompile(`^[a-z_]+$`)
)

// Backup implements backup.Engine; see the package documentation.
func (e *Engine) Backup(ctx context.Context, siteID string, opts backup.Options) (backup.Snapshot, error) {
	if !id.Valid(siteID) {
		return backup.Snapshot{}, fmt.Errorf("back up site %q: invalid site ID", siteID)
	}
	if opts.Kind != "" && !kindName.MatchString(opts.Kind) {
		return backup.Snapshot{}, fmt.Errorf("back up site %s: invalid kind %q", siteID, opts.Kind)
	}
	if opts.BackupID != "" && !id.Valid(opts.BackupID) {
		return backup.Snapshot{}, fmt.Errorf("back up site %s: invalid backup ID %q", siteID, opts.BackupID)
	}
	site, err := store.NewSites(e.db).Get(ctx, siteID)
	if errors.Is(err, store.ErrNotFound) {
		return backup.Snapshot{}, fmt.Errorf("back up site %s: %w", siteID, backup.ErrSiteNotFound)
	}
	if err != nil {
		return backup.Snapshot{}, fmt.Errorf("back up site %s: %w", siteID, err)
	}
	if !tagValue.MatchString(site.WPCoreVersion) {
		return backup.Snapshot{}, fmt.Errorf("back up site %s: invalid WordPress core version %q", siteID, site.WPCoreVersion)
	}
	access, err := e.repos.TaskAccess(ctx, site.ClientGroupID)
	if err != nil {
		return backup.Snapshot{}, fmt.Errorf("back up site %s: %w", siteID, err)
	}
	if opts.BackupID != "" {
		snap, found, err := e.find(ctx, site.ClientGroupID, siteID, opts.BackupID)
		if err != nil {
			return backup.Snapshot{}, fmt.Errorf("back up site %s: looking for an earlier attempt's snapshot: %w", siteID, err)
		}
		if found {
			e.log.InfoContext(ctx, "backup already taken", "site_id", siteID, "backup_id", opts.BackupID, "snapshot_id", snap.ID)
			return snap, nil
		}
	}

	start := time.Now()
	transfer, err := e.transferDir(siteID, opts.BackupID)
	if err != nil {
		return backup.Snapshot{}, fmt.Errorf("back up site %s: %w", siteID, err)
	}
	defer func() {
		if err := os.RemoveAll(transfer); err != nil {
			e.log.ErrorContext(ctx, "removing a backup transfer directory failed", "site_id", siteID, "dir", transfer, "error", err)
		}
	}()
	if err := e.dbs.Dump(ctx, siteID, filepath.Join(transfer, DatabaseDumpName)); err != nil {
		return backup.Snapshot{}, fmt.Errorf("back up site %s: dumping the database: %w", siteID, err)
	}
	res, runErr := e.tasks.RunTask(ctx, siteID, provisioner.Task{
		Command:       backupCommand(siteID, site.WPCoreVersion, opts),
		Env:           access.Env,
		SecretEnv:     access.SecretEnv,
		WPContent:     provisioner.MountReadOnly,
		TransferDir:   transfer,
		RepositoryDir: access.RepositoryDir,
		Timeout:       e.timeout,
	})
	snap, err := backupResult(res, runErr)
	if err != nil {
		return backup.Snapshot{}, fmt.Errorf("back up site %s: %w", siteID, err)
	}
	snap.SiteID, snap.WPCoreVersion, snap.Kind, snap.BackupID = siteID, site.WPCoreVersion, opts.Kind, opts.BackupID
	snap.Tags = backupTags(siteID, site.WPCoreVersion, opts)
	e.log.InfoContext(ctx, "site backed up", "site_id", siteID, "client_group_id", site.ClientGroupID,
		"snapshot_id", snap.ID, "size_bytes", snap.SizeBytes, "added_bytes", snap.AddedBytes,
		"warnings", len(snap.Warnings), "duration", time.Since(start))
	return snap, nil
}

// backupTags are the tags of a new snapshot.
func backupTags(siteID, coreVersion string, opts backup.Options) []string {
	tags := []string{TagSite + siteID, TagWPCore + coreVersion}
	if opts.Kind != "" {
		tags = append(tags, TagKind+opts.Kind)
	}
	if opts.BackupID != "" {
		tags = append(tags, TagBackup+opts.BackupID)
	}
	return tags
}

// backupCommand is the task's argv.
func backupCommand(siteID, coreVersion string, opts backup.Options) []string {
	cmd := []string{
		"restic", "backup",
		// Only the summary and errors, as JSON lines.
		"--json", "--quiet",
		// The scan only estimates progress, which nobody watches.
		"--no-scan",
		"--retry-lock", LockWait.String(),
		"--host", Host(siteID),
	}
	for _, t := range backupTags(siteID, coreVersion, opts) {
		cmd = append(cmd, "--tag", t)
	}
	return append(cmd, WPContentPath, DatabasePath)
}

// transferDir creates an empty transfer directory for the backup, named
// after it so a retry replaces what an earlier attempt left.
func (e *Engine) transferDir(siteID, backupID string) (string, error) {
	return e.newTransferDir("backup-", siteID, backupID)
}

// newTransferDir creates an empty transfer directory named
// <prefix><site ID>-<name>, with a random name if name is empty, replacing
// one an earlier attempt left.
func (e *Engine) newTransferDir(prefix, siteID, name string) (string, error) {
	tmp := e.layout.TmpDir()
	if err := os.MkdirAll(tmp, 0o700); err != nil {
		return "", fmt.Errorf("creating the transfer directory: %w", err)
	}
	if name == "" {
		name = strings.ToLower(rand.Text()[:10])
	}
	dir := filepath.Join(tmp, prefix+siteID+"-"+name)
	if err := os.RemoveAll(dir); err != nil {
		return "", fmt.Errorf("removing an earlier attempt's transfer directory: %w", err)
	}
	if err := os.Mkdir(dir, 0o700); err != nil {
		return "", fmt.Errorf("creating the transfer directory: %w", err)
	}
	return dir, nil
}

// find returns the site's snapshot tagged with the backup ID, if any.
func (e *Engine) find(ctx context.Context, groupID, siteID, backupID string) (backup.Snapshot, bool, error) {
	snaps, err := e.snapshots(ctx, groupID, TagSite+siteID+","+TagBackup+backupID)
	if err != nil || len(snaps) == 0 {
		return backup.Snapshot{}, false, err
	}
	return snaps[len(snaps)-1], true, nil
}

// ListSnapshots implements backup.Engine; see the package documentation.
func (e *Engine) ListSnapshots(ctx context.Context, siteID string) ([]backup.Snapshot, error) {
	if !id.Valid(siteID) {
		return nil, fmt.Errorf("list snapshots of site %q: invalid site ID", siteID)
	}
	groupID, err := e.siteGroup(ctx, siteID)
	if err != nil {
		return nil, fmt.Errorf("list snapshots of site %s: %w", siteID, err)
	}
	snaps, err := e.snapshots(ctx, groupID, TagSite+siteID)
	if errors.Is(err, repository.ErrNoRepository) {
		return []backup.Snapshot{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list snapshots of site %s: %w", siteID, err)
	}
	return snaps, nil
}

// siteGroup returns the client group whose repository holds the site's
// snapshots: the site's, or for a deleted site that of its backups.
func (e *Engine) siteGroup(ctx context.Context, siteID string) (string, error) {
	site, err := store.NewSites(e.db).Get(ctx, siteID)
	if err == nil {
		return site.ClientGroupID, nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return "", err
	}
	group, err := store.NewBackupRecords(e.db).SiteClientGroup(ctx, siteID)
	if errors.Is(err, store.ErrNotFound) {
		return "", backup.ErrSiteNotFound
	}
	return group, err
}

// snapshots lists the snapshots with all of the comma-separated tags,
// oldest first. Listing takes no lock: it only reads snapshot files, and
// it shouldn't fail or wait while a prune holds the exclusive lock.
func (e *Engine) snapshots(ctx context.Context, groupID, tags string) ([]backup.Snapshot, error) {
	out, err := e.repos.Restic(ctx, groupID, "snapshots", "--json", "--no-lock", "--tag", tags)
	if err != nil {
		return nil, err
	}
	return parseSnapshots(out)
}

// resticSnapshot is a snapshot as `restic snapshots --json` prints it.
type resticSnapshot struct {
	ID      string    `json:"id"`
	Time    time.Time `json:"time"`
	Tags    []string  `json:"tags"`
	Summary *struct {
		TotalFilesProcessed int64 `json:"total_files_processed"`
		TotalBytesProcessed int64 `json:"total_bytes_processed"`
		DataAddedPacked     int64 `json:"data_added_packed"`
	} `json:"summary"`
}

func parseSnapshots(out []byte) ([]backup.Snapshot, error) {
	var raw []resticSnapshot
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, fmt.Errorf("reading restic's snapshot list: %w", err)
	}
	snaps := make([]backup.Snapshot, 0, len(raw))
	for _, r := range raw {
		if r.ID == "" {
			return nil, errors.New("reading restic's snapshot list: a snapshot without an ID")
		}
		s := backup.Snapshot{ID: backup.SnapshotID(r.ID), Time: r.Time.UTC(), Tags: r.Tags}
		for _, t := range r.Tags {
			switch {
			case strings.HasPrefix(t, TagSite):
				s.SiteID = strings.TrimPrefix(t, TagSite)
			case strings.HasPrefix(t, TagWPCore):
				s.WPCoreVersion = strings.TrimPrefix(t, TagWPCore)
			case strings.HasPrefix(t, TagKind):
				s.Kind = strings.TrimPrefix(t, TagKind)
			case strings.HasPrefix(t, TagBackup):
				s.BackupID = strings.TrimPrefix(t, TagBackup)
			}
		}
		if r.Summary != nil {
			s.FileCount, s.SizeBytes, s.AddedBytes = r.Summary.TotalFilesProcessed, r.Summary.TotalBytesProcessed, r.Summary.DataAddedPacked
		}
		snaps = append(snaps, s)
	}
	slices.SortStableFunc(snaps, func(a, b backup.Snapshot) int {
		return cmp.Or(a.Time.Compare(b.Time), strings.Compare(string(a.ID), string(b.ID)))
	})
	return snaps, nil
}
