// Package export exports sites for download: a site's database (design
// doc §5.8, decision #61), or a whole site from the live site or from a
// backup snapshot (§5.7). Agencies get the files through the platform,
// tied to their platform account, without ever seeing the site's database
// password.
//
// # Kinds
//
// Every export is a record (store.Export) and one or more files in the
// exports directory, named <export ID>.<file name>:
//
//   - database (store.ExportDatabase): FileDatabase, the site's database
//     dumped with sitedb.Manager.Dump and compressed with gzip.
//   - site (store.ExportSite): FileWPContent and FileDatabase, the two
//     files site import takes (§5.7), so an export can be imported again
//     here or moved to another host. The archive holds the site's
//     wp-content (its paths start with wp-content/) without the
//     platform's host drop-ins (siteimport.HostDropIns), which point at
//     its object cache and page cache and would break the site elsewhere,
//     and without anything but regular files and directories, which
//     import skips too. It is read by a task that runs as the site's UID
//     with the volume mounted read-only (provisioner.TaskRunner).
//   - snapshot (store.ExportSnapshot): the same two files, from a backup
//     snapshot (Snapshots), so an earlier version of a site can be had
//     without restoring over the live one, also once the site is deleted.
//
// The record keeps what the files hold for whoever imports them (the
// site's URL, table prefix, WordPress core and PHP versions, and when its
// content was read; store.ExportSource).
//
// # Jobs
//
// The API calls [Service.ExportDatabase], [Service.ExportSite], or
// [Service.ExportBackup], which check what is exported and queue a job.
// The steps, each skipped where it doesn't apply:
//
//  1. record_export: create the export's record with status running. Its
//     ID goes into the job's data first ([DataExportID]), so a retried
//     step finds it. Undo marks a running export failed, with the job's
//     error, and deletes its files.
//  2. check_space (site and snapshot): estimate the export's size, as
//     the size of the site's database plus its wp-content (or the size of
//     what the snapshot holds), put it in the job's result
//     (estimated_size_bytes), and refuse to go on if the exports
//     directory's file system has less room ([ErrInsufficientSpace]).
//     Exports are compressed, so the estimate is an upper bound.
//  3. dump_database: write FileDatabase.
//  4. archive_wp_content (site and snapshot): write FileWPContent.
//  5. mark_ready: mark the export ready with its files and their expiry,
//     the TTL from now.
//  6. finish: put the export in the job's result: export_id, files (each
//     with name, size_bytes, and download_url), size_bytes, and
//     expires_at, and for a database export download_url, as before.
//
// Files are written through a temporary name and renamed, so a name only
// ever holds a complete file, and a retried step starts over.
//
// Database and site exports use the site's lock key, site:<id>, so they
// never run alongside a restore or anything else that changes the site.
// mariadb-dump takes a consistent snapshot without locking tables, and
// the archive is read while the site keeps serving, so a file that
// changes while it is read is archived as it was then. Snapshot exports
// take no lock: they read the client group's repository (restic waits for
// the exclusive lock of a prune or check), and touch nothing of the site.
//
// # Downloading and expiry
//
// [Service.Open] opens a ready export's file for the download endpoints,
// which the API authorizes and audits like the export itself. An export
// whose expiry has passed is expired on the spot: its files are deleted,
// its record is marked expired, and it is reported as not found, like any
// other export that isn't ready. The [CleanupJobType] job, on a schedule
// every few minutes ([EnsureCleanupSchedule]), does the same for every
// expired export nobody asked for ([Service.Sweep]), and deletes files
// that belong to no export that is running or ready, such as those a
// crash left behind.
package export

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/backup"
	"github.com/rchouinard/pressfleet/control-plane/internal/id"
	"github.com/rchouinard/pressfleet/control-plane/internal/jobs"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
)

// Job types, and the cleanup job's schedule.
const (
	// JobType exports a site's database.
	JobType = "site.database_export"
	// SiteJobType exports a live site.
	SiteJobType = "site.export"
	// BackupJobType exports a site from a backup snapshot.
	BackupJobType = "backup.export"
	// CleanupJobType deletes expired exports of every kind. It is named
	// for database exports, which came first, and keeps the name so the
	// schedule created for them carries on.
	CleanupJobType = "database_export.cleanup"
	// CleanupScheduleName names the cleanup job's schedule.
	CleanupScheduleName = "database-export-cleanup"
)

// Step names. Keep them stable: a queued job resumes by step name.
const (
	StepRecord    = "record_export"
	StepSpace     = "check_space"
	StepDump      = "dump_database"
	StepArchive   = "archive_wp_content"
	StepMarkReady = "mark_ready"
	stepFinish    = "finish"
)

// DataExportID is the job data key under which the steps keep the ID of the
// export they created.
const DataExportID = "export_id"

// Defaults.
const (
	// DefaultTTL is how long a database export can be downloaded when
	// Options.TTL is zero (§5.8).
	DefaultTTL = time.Hour
	// DefaultSiteTTL is how long a site or snapshot export can be
	// downloaded when Options.SiteTTL is zero: a site can take long to
	// download, and a download can be resumed.
	DefaultSiteTTL = 24 * time.Hour
	// DefaultTaskTimeout bounds reading a site's wp-content.
	DefaultTaskTimeout = 6 * time.Hour
)

// cleanupInterval is how often the cleanup job runs. Downloads check the
// expiry themselves, so this only bounds how long an expired file stays on
// disk.
const cleanupInterval = 5 * time.Minute

// The cleanup job targets the platform; job targets need a type and an ID.
const (
	cleanupTargetType = "platform"
	cleanupTargetID   = "database_exports"
)

// Sentinel errors.
var (
	// ErrSiteNotFound means the site doesn't exist.
	ErrSiteNotFound = errors.New("site not found")
	// ErrNotExportable means the site can't be exported right now: it is
	// being provisioned or deleted, or its provisioning failed.
	ErrNotExportable = errors.New("the site can't be exported")
	// ErrBackupNotFound means the backup doesn't exist.
	ErrBackupNotFound = errors.New("backup not found")
	// ErrBackupNotExportable means the backup has no snapshot to export:
	// it is running or failed, or its snapshot was removed.
	ErrBackupNotExportable = errors.New("the backup can't be exported")
	// ErrExportNotFound means there is no export to download: it doesn't
	// exist, belongs to another site, isn't ready, has expired, or has no
	// file by that name.
	ErrExportNotFound = errors.New("export not found")
	// ErrInsufficientSpace means the data directory lacks room for the
	// export.
	ErrInsufficientSpace = errors.New("not enough free disk space for the export")
)

// Dumper dumps site databases; sitedb.Manager implements it.
type Dumper interface {
	// Dump writes a logical dump of the site's database to destPath.
	// Returns sitedb.ErrDatabaseNotFound if the site has no database.
	Dump(ctx context.Context, siteID string, destPath string) error
}

// DatabaseSizer measures site databases; mariadb.Manager implements it.
type DatabaseSizer interface {
	// DatabaseSizes returns the size in bytes of every site database, by
	// site ID.
	DatabaseSizes(ctx context.Context) (map[string]int64, error)
}

// Snapshots reads backup snapshots; restic.Engine implements it.
type Snapshots interface {
	// DumpFiles writes the snapshot's wp-content to w as a tar archive
	// with paths relative to wp-content, and returns the snapshot.
	DumpFiles(ctx context.Context, siteID string, snap backup.SnapshotID, w io.Writer) (backup.Snapshot, error)
	// DumpDatabase writes the snapshot's database dump (plain SQL) to w,
	// and returns the snapshot.
	DumpDatabase(ctx context.Context, siteID string, snap backup.SnapshotID, w io.Writer) (backup.Snapshot, error)
}

// LockKey is the lock key of database and site export jobs: the site's,
// shared with every job that changes the site (sites.LockKey).
func LockKey(siteID string) string { return "site:" + siteID }

// DownloadPath is the API path that downloads a database export, from
// before exports had kinds.
func DownloadPath(siteID, exportID string) string {
	return "/api/v1/sites/" + url.PathEscape(siteID) + "/database/exports/" + url.PathEscape(exportID) + "/download"
}

// Path is the API path of an export.
func Path(exportID string) string { return "/api/v1/exports/" + url.PathEscape(exportID) }

// FilePath is the API path that downloads one of an export's files.
func FilePath(exportID, name string) string { return Path(exportID) + "/files/" + url.PathEscape(name) }

// Options configure a Service.
type Options struct {
	// DB is the state database. Required.
	DB *store.DB
	// Databases dumps site databases. Required.
	Databases Dumper
	// DatabaseSizes measures them for the space check; nil, or one that
	// fails, leaves the database out of the estimate.
	DatabaseSizes DatabaseSizer
	// Tasks reads site wp-content volumes. Required for site exports.
	Tasks provisioner.TaskRunner
	// Snapshots reads backup snapshots. Required for snapshot exports.
	Snapshots Snapshots
	// Dir is where exports are written, normally config.Layout.ExportsDir.
	// Required; it is created when needed.
	Dir string
	// TTL is how long a database export can be downloaded once it is
	// ready; zero means DefaultTTL.
	TTL time.Duration
	// SiteTTL is the same for site and snapshot exports; zero means
	// DefaultSiteTTL.
	SiteTTL time.Duration
	// TaskTimeout bounds reading a site's wp-content; zero means
	// DefaultTaskTimeout.
	TaskTimeout time.Duration
	// FreeSpace returns the bytes available to the control plane on the
	// file system holding dir; nil means statfs(2).
	FreeSpace func(dir string) (uint64, error)
	// Now is the clock; nil means time.Now.
	Now func() time.Time
	// Logger defaults to discarding output.
	Logger *slog.Logger
}

// Service queues exports, runs their jobs, and serves their files.
type Service struct {
	db      *store.DB
	dbs     Dumper
	sizes   DatabaseSizer
	tasks   provisioner.TaskRunner
	snaps   Snapshots
	dir     string
	ttl     time.Duration
	siteTTL time.Duration
	timeout time.Duration
	free    func(string) (uint64, error)
	now     func() time.Time
	log     *slog.Logger
}

// New returns a Service.
func New(opts Options) (*Service, error) {
	if opts.DB == nil || opts.Databases == nil || opts.Dir == "" {
		return nil, errors.New("export: DB, Databases, and Dir are required")
	}
	s := &Service{
		db: opts.DB, dbs: opts.Databases, sizes: opts.DatabaseSizes, tasks: opts.Tasks, snaps: opts.Snapshots,
		dir: opts.Dir, ttl: opts.TTL, siteTTL: opts.SiteTTL, timeout: opts.TaskTimeout, free: opts.FreeSpace,
		now: opts.Now, log: opts.Logger,
	}
	if s.ttl <= 0 {
		s.ttl = DefaultTTL
	}
	if s.siteTTL <= 0 {
		s.siteTTL = DefaultSiteTTL
	}
	if s.timeout <= 0 {
		s.timeout = DefaultTaskTimeout
	}
	if s.free == nil {
		s.free = freeSpace
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.log == nil {
		s.log = slog.New(slog.DiscardHandler)
	}
	return s, nil
}

// TTL is how long an export of the kind can be downloaded once it is
// ready.
func (s *Service) TTL(kind store.ExportKind) time.Duration {
	if kind == store.ExportDatabase {
		return s.ttl
	}
	return s.siteTTL
}

// backupPayload is the payload of a snapshot export job.
type backupPayload struct {
	BackupID string `json:"backup_id"`
}

// ExportDatabase queues a job that exports the site's database. It returns
// ErrSiteNotFound if the site doesn't exist and ErrNotExportable if it has
// no database to export now.
func (s *Service) ExportDatabase(ctx context.Context, siteID, createdByUserID string) (store.Job, error) {
	return s.exportLive(ctx, JobType, siteID, createdByUserID)
}

// ExportSite queues a job that exports the live site: its wp-content and
// its database. It returns ErrSiteNotFound if the site doesn't exist and
// ErrNotExportable if it can't be exported now.
func (s *Service) ExportSite(ctx context.Context, siteID, createdByUserID string) (store.Job, error) {
	if s.tasks == nil {
		return store.Job{}, errors.New("export: no task runner configured for site exports")
	}
	return s.exportLive(ctx, SiteJobType, siteID, createdByUserID)
}

func (s *Service) exportLive(ctx context.Context, jobType, siteID, createdByUserID string) (store.Job, error) {
	var job store.Job
	err := s.db.InTx(ctx, func(q store.Querier) error {
		if _, err := exportableSite(ctx, q, siteID); err != nil {
			return err
		}
		var err error
		job, err = jobs.Enqueue(ctx, q, jobs.Spec{
			Type:            jobType,
			TargetType:      "site",
			TargetID:        siteID,
			LockKey:         LockKey(siteID),
			CreatedByUserID: createdByUserID,
		})
		if err != nil {
			return fmt.Errorf("queue export of site %s: %w", siteID, err)
		}
		return nil
	})
	if err != nil {
		return store.Job{}, err
	}
	s.log.InfoContext(ctx, "export queued", "job_type", jobType, "site_id", siteID, "job_id", job.ID)
	return job, nil
}

// ExportBackup queues a job that exports a site from the snapshot of a
// backup. The site may have been deleted. It returns ErrBackupNotFound if
// the backup doesn't exist and ErrBackupNotExportable if it has no
// snapshot to export.
func (s *Service) ExportBackup(ctx context.Context, backupID, createdByUserID string) (store.Job, error) {
	if s.snaps == nil {
		return store.Job{}, errors.New("export: no snapshot reader configured for snapshot exports")
	}
	var job store.Job
	err := s.db.InTx(ctx, func(q store.Querier) error {
		rec, err := exportableBackup(ctx, q, backupID)
		if err != nil {
			return err
		}
		// The job is the site's while it exists, so it is listed with the
		// site's jobs; a deleted site's is its client group's.
		targetType, targetID := "site", rec.SiteID
		if _, err := store.NewSites(q).Get(ctx, rec.SiteID); errors.Is(err, store.ErrNotFound) {
			targetType, targetID = "client_group", rec.ClientGroupID
		} else if err != nil {
			return err
		}
		job, err = jobs.Enqueue(ctx, q, jobs.Spec{
			Type:            BackupJobType,
			TargetType:      targetType,
			TargetID:        targetID,
			Payload:         backupPayload{BackupID: rec.ID},
			CreatedByUserID: createdByUserID,
		})
		if err != nil {
			return fmt.Errorf("queue export of backup %s: %w", backupID, err)
		}
		return nil
	})
	if err != nil {
		return store.Job{}, err
	}
	s.log.InfoContext(ctx, "export queued", "job_type", BackupJobType, "backup_id", backupID, "job_id", job.ID)
	return job, nil
}

// exportableSite checks that the site can be exported: it exists, is
// neither being provisioned nor deleted, and was provisioned. Stopped
// sites can be exported: their database keeps running on the shared
// server, and their wp-content volume stays.
func exportableSite(ctx context.Context, q store.Querier, siteID string) (store.Site, error) {
	site, err := store.NewSites(q).Get(ctx, siteID)
	if errors.Is(err, store.ErrNotFound) {
		return store.Site{}, fmt.Errorf("site %s: %w", siteID, ErrSiteNotFound)
	}
	if err != nil {
		return store.Site{}, err
	}
	switch site.Status {
	case store.SiteProvisioning, store.SiteDeleting:
		return store.Site{}, fmt.Errorf("%w: the site is %s", ErrNotExportable, site.Status)
	}
	if site.WPSaltsSecretRef == "" {
		return store.Site{}, fmt.Errorf("%w: it was never provisioned", ErrNotExportable)
	}
	return site, nil
}

// exportableBackup checks that the backup has a snapshot to export: it
// succeeded and its snapshot hasn't been found gone.
func exportableBackup(ctx context.Context, q store.Querier, backupID string) (store.BackupRecord, error) {
	if !id.Valid(backupID) {
		return store.BackupRecord{}, fmt.Errorf("backup %q: %w", backupID, ErrBackupNotFound)
	}
	rec, err := store.NewBackupRecords(q).Get(ctx, backupID)
	if errors.Is(err, store.ErrNotFound) {
		return store.BackupRecord{}, fmt.Errorf("backup %s: %w", backupID, ErrBackupNotFound)
	}
	if err != nil {
		return store.BackupRecord{}, err
	}
	switch {
	case rec.Status != store.BackupSucceeded || rec.SnapshotID == "":
		return store.BackupRecord{}, fmt.Errorf("%w: the backup is %s", ErrBackupNotExportable, rec.Status)
	case !rec.ExpiredAt.IsZero():
		return store.BackupRecord{}, fmt.Errorf("%w: its snapshot was removed from the repository", ErrBackupNotExportable)
	}
	return rec, nil
}

// Get returns an export. A ready export whose expiry has passed is expired
// first, so it is reported as it is.
func (s *Service) Get(ctx context.Context, exportID string) (store.Export, error) {
	if !id.Valid(exportID) {
		return store.Export{}, fmt.Errorf("%w: %s", ErrExportNotFound, exportID)
	}
	e, err := store.NewExports(s.db).Get(ctx, exportID)
	if errors.Is(err, store.ErrNotFound) {
		return store.Export{}, fmt.Errorf("%w: %s", ErrExportNotFound, exportID)
	}
	if err != nil {
		return store.Export{}, err
	}
	return s.current(ctx, e)
}

// List returns a page of exports, newest first, expiring those whose
// expiry has passed first.
func (s *Service) List(ctx context.Context, f store.ExportFilter) (store.ExportPage, error) {
	page, err := store.NewExports(s.db).List(ctx, f)
	if err != nil {
		return store.ExportPage{}, err
	}
	for i, e := range page.Exports {
		if page.Exports[i], err = s.current(ctx, e); err != nil {
			return store.ExportPage{}, err
		}
	}
	return page, nil
}

// current expires e if it is ready and its expiry has passed, and returns
// it as it is then.
func (s *Service) current(ctx context.Context, e store.Export) (store.Export, error) {
	if e.Status != store.ExportReady || s.now().Before(e.ExpiresAt) {
		return e, nil
	}
	if err := s.expire(ctx, e); err != nil {
		return store.Export{}, err
	}
	return store.NewExports(s.db).Get(ctx, e.ID)
}

// Handlers returns the job handlers of the export and cleanup jobs, to
// register on the job pool.
func (s *Service) Handlers() []jobs.Handler {
	return []jobs.Handler{
		{Type: JobType, Steps: []jobs.Step{
			{Name: StepRecord, Do: s.recordLive(store.ExportDatabase), Undo: s.failExport},
			{Name: StepDump, Do: s.dump},
			{Name: StepMarkReady, Do: s.markReady},
			{Name: stepFinish, Do: s.finish},
		}},
		{Type: SiteJobType, Steps: []jobs.Step{
			{Name: StepRecord, Do: s.recordLive(store.ExportSite), Undo: s.failExport},
			{Name: StepSpace, Do: s.checkSpace},
			{Name: StepDump, Do: s.dump},
			{Name: StepArchive, Do: s.archive},
			{Name: StepMarkReady, Do: s.markReady},
			{Name: stepFinish, Do: s.finish},
		}},
		{Type: BackupJobType, Steps: []jobs.Step{
			{Name: StepRecord, Do: s.recordBackup, Undo: s.failExport},
			{Name: StepSpace, Do: s.checkSpace},
			{Name: StepDump, Do: s.dump},
			{Name: StepArchive, Do: s.archive},
			{Name: StepMarkReady, Do: s.markReady},
			{Name: stepFinish, Do: s.finish},
		}},
		{Type: CleanupJobType, Steps: []jobs.Step{
			{Name: "delete_expired_exports", Do: func(ctx context.Context, r *jobs.Run) error {
				n, err := s.Sweep(ctx)
				if err != nil {
					return err
				}
				if n > 0 {
					r.Logf("Deleted %d expired exports.", n)
				}
				return nil
			}},
		}},
	}
}

// EnsureCleanupSchedule creates or updates the cleanup job's schedule. It is
// safe to call on every start: an unchanged schedule keeps its next run.
func EnsureCleanupSchedule(ctx context.Context, q store.Querier) error {
	_, err := jobs.UpsertSchedule(ctx, q, jobs.ScheduleSpec{
		Name:            CleanupScheduleName,
		JobType:         CleanupJobType,
		TargetType:      cleanupTargetType,
		TargetID:        cleanupTargetID,
		LockKey:         CleanupJobType,
		Interval:        cleanupInterval,
		MissedRunPolicy: store.MissedCoalesce,
	})
	if err != nil {
		return fmt.Errorf("export cleanup schedule: %w", err)
	}
	return nil
}
