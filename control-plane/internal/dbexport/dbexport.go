// Package dbexport exports site databases for download (design doc §5.8,
// decision #61): agencies get a site's database as a compressed dump
// through the platform, tied to their platform account, without ever
// seeing the site's database password.
//
// # Exporting
//
// The API (POST /sites/{id}/database/export) calls [Service.Export], which
// checks the site and queues a [JobType] job with the site as its target.
// The job's steps are:
//
//  1. record_export: check that the site has a database (it exists, is
//     neither being provisioned nor deleted, and was provisioned), then
//     create the export's record (store.DatabaseExport) with status
//     running. Its ID goes into the job's data first ([DataExportID]), so
//     a retried step finds it. Undo marks a running export failed, with the
//     job's error, and deletes its files.
//  2. dump_database: sitedb.Manager.Dump into <dir>/<id>.sql, compress it
//     with gzip into <dir>/<id>.sql.gz (through a temporary file and a
//     rename, so the name only ever holds a complete file), delete the
//     plain dump, and mark the export ready with the file's size and its
//     expiry, TTL from now. An export that is already ready is left alone.
//  3. finish: put the export's ID, download URL ([DownloadPath]), size,
//     and expiry in the job's result.
//
// Export jobs use the site's lock key, site:<id>, so a dump never runs
// alongside a restore or anything else that changes the site. mariadb-dump
// takes a consistent snapshot without locking tables, so the site keeps
// serving.
//
// # Downloading and expiry
//
// [Service.Open] opens a ready export's file for the download endpoint,
// which the API authorizes and audits like the export itself. An export
// whose expiry has passed is expired on the spot: its file is deleted, its
// record is marked expired, and it is reported as not found, like any
// other export that isn't ready. The [CleanupJobType] job, on a schedule
// every few minutes ([EnsureCleanupSchedule]), does the same for every
// expired export nobody asked for ([Service.Sweep]), and deletes files
// that belong to no export that is running or ready, such as those a
// crash left behind.
package dbexport

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/jobs"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
)

// Job types, and the cleanup job's schedule.
const (
	// JobType exports a site's database.
	JobType = "site.database_export"
	// CleanupJobType deletes expired exports.
	CleanupJobType = "database_export.cleanup"
	// CleanupScheduleName names the cleanup job's schedule.
	CleanupScheduleName = "database-export-cleanup"
)

// Step names. Keep them stable: a queued job resumes by step name.
const (
	StepRecord = "record_export"
	StepDump   = "dump_database"
	stepFinish = "finish"
)

// DataExportID is the job data key under which the steps keep the ID of the
// export they created.
const DataExportID = "export_id"

// DefaultTTL is how long an export can be downloaded when Options.TTL is
// zero (§5.8).
const DefaultTTL = time.Hour

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
	// ErrNotExportable means the site has no database to export right now:
	// it is being provisioned or deleted, or its provisioning failed.
	ErrNotExportable = errors.New("the site's database can't be exported")
	// ErrExportNotFound means there is no export to download: it doesn't
	// exist, belongs to another site, isn't ready, or has expired.
	ErrExportNotFound = errors.New("database export not found")
)

// Dumper dumps site databases; sitedb.Manager implements it.
type Dumper interface {
	// Dump writes a logical dump of the site's database to destPath.
	// Returns sitedb.ErrDatabaseNotFound if the site has no database.
	Dump(ctx context.Context, siteID string, destPath string) error
}

// LockKey is the lock key of export jobs: the site's, shared with every job
// that changes the site (sites.LockKey).
func LockKey(siteID string) string { return "site:" + siteID }

// DownloadPath is the API path that downloads an export.
func DownloadPath(siteID, exportID string) string {
	return "/api/v1/sites/" + url.PathEscape(siteID) + "/database/exports/" + url.PathEscape(exportID) + "/download"
}

// Options configure a Service.
type Options struct {
	// DB is the state database. Required.
	DB *store.DB
	// Databases dumps site databases. Required.
	Databases Dumper
	// Dir is where exports are written, normally
	// config.Layout.DatabaseExportsDir. Required; it is created when
	// needed.
	Dir string
	// TTL is how long an export can be downloaded once it is ready; zero
	// means DefaultTTL.
	TTL time.Duration
	// Now is the clock; nil means time.Now.
	Now func() time.Time
	// Logger defaults to discarding output.
	Logger *slog.Logger
}

// Service queues database exports, runs their jobs, and serves their
// files.
type Service struct {
	db  *store.DB
	dbs Dumper
	dir string
	ttl time.Duration
	now func() time.Time
	log *slog.Logger
}

// New returns a Service.
func New(opts Options) (*Service, error) {
	if opts.DB == nil || opts.Databases == nil || opts.Dir == "" {
		return nil, errors.New("dbexport: DB, Databases, and Dir are required")
	}
	s := &Service{db: opts.DB, dbs: opts.Databases, dir: opts.Dir, ttl: opts.TTL, now: opts.Now, log: opts.Logger}
	if s.ttl <= 0 {
		s.ttl = DefaultTTL
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.log == nil {
		s.log = slog.New(slog.DiscardHandler)
	}
	return s, nil
}

// TTL is how long an export can be downloaded once it is ready.
func (s *Service) TTL() time.Duration { return s.ttl }

// Export queues a job that exports the site's database. It returns
// ErrSiteNotFound if the site doesn't exist and ErrNotExportable if it has
// no database to export now.
func (s *Service) Export(ctx context.Context, siteID, createdByUserID string) (store.Job, error) {
	var job store.Job
	err := s.db.InTx(ctx, func(q store.Querier) error {
		if err := exportableSite(ctx, q, siteID); err != nil {
			return err
		}
		var err error
		job, err = jobs.Enqueue(ctx, q, jobs.Spec{
			Type:            JobType,
			TargetType:      "site",
			TargetID:        siteID,
			LockKey:         LockKey(siteID),
			CreatedByUserID: createdByUserID,
		})
		if err != nil {
			return fmt.Errorf("queue database export of site %s: %w", siteID, err)
		}
		return nil
	})
	if err != nil {
		return store.Job{}, err
	}
	s.log.InfoContext(ctx, "database export queued", "site_id", siteID, "job_id", job.ID)
	return job, nil
}

// exportableSite checks that the site has a database to export: it exists,
// is neither being provisioned nor deleted, and was provisioned. Stopped
// sites can be exported: their database keeps running on the shared
// server.
func exportableSite(ctx context.Context, q store.Querier, siteID string) error {
	site, err := store.NewSites(q).Get(ctx, siteID)
	if errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("site %s: %w", siteID, ErrSiteNotFound)
	}
	if err != nil {
		return err
	}
	switch site.Status {
	case store.SiteProvisioning, store.SiteDeleting:
		return fmt.Errorf("%w: the site is %s", ErrNotExportable, site.Status)
	}
	if site.WPSaltsSecretRef == "" {
		return fmt.Errorf("%w: it was never provisioned", ErrNotExportable)
	}
	return nil
}

// Handlers returns the job handlers of the export and cleanup jobs, to
// register on the job pool.
func (s *Service) Handlers() []jobs.Handler {
	return []jobs.Handler{
		{Type: JobType, Steps: []jobs.Step{
			{Name: StepRecord, Do: s.record, Undo: s.failExport},
			{Name: StepDump, Do: s.dump},
			{Name: stepFinish, Do: s.finish},
		}},
		{Type: CleanupJobType, Steps: []jobs.Step{
			{Name: "delete_expired_exports", Do: func(ctx context.Context, r *jobs.Run) error {
				n, err := s.Sweep(ctx)
				if err != nil {
					return err
				}
				if n > 0 {
					r.Logf("Deleted %d expired database exports.", n)
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
		return fmt.Errorf("database export cleanup schedule: %w", err)
	}
	return nil
}
