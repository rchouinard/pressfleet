// Package sitebackup runs site backups as jobs (design doc §5.3) and keeps
// the platform's records of them (store.BackupRecord). The snapshots
// themselves are taken by a backup.Engine (package backup/restic).
//
// # Taking a backup
//
// Everything that backs up a site goes through a [JobType] job or through
// [Service.Steps]:
//
//   - The API (POST /sites/{id}/backups) calls [Service.Backup], which
//     checks the site and queues a manual backup.
//   - Each site's backup schedule (#53, [EnsureSchedule]) queues [JobType]
//     jobs with Payload JobPayload{Kind: store.BackupScheduled}. A
//     scheduled job for a site that can't be backed up any more (deleted,
//     being deleted, or never provisioned) skips the backup and succeeds.
//   - Code that must back up a site as part of its own job, such as a
//     restore's pre-restore snapshot (#55), appends [Service.Steps] to its
//     job's steps and reads the result with [Service.JobBackup]. It can't
//     queue a [JobType] job and wait for it: both jobs would hold the site's
//     lock key.
//
// The job's target is the site. Its steps are:
//
//  1. record_backup: check that the site can be backed up (it exists and
//     is neither being provisioned nor deleted), then create its backup
//     record with status running. The record's ID is stored in the job's
//     data first ([DataBackupID]), so a retried step finds it. Undo marks
//     a record that is still running as failed, with the job's error.
//  2. take_snapshot: backup.Engine.Backup with the record's ID, then mark
//     the record succeeded with the snapshot's ID and sizes. The engine
//     returns the snapshot an earlier attempt took if its result was lost,
//     so a retry never takes a second one; a record that already succeeded
//     is left alone.
//  3. finish (JobType jobs only): put the backup's ID, snapshot ID, and
//     site ID in the job's result.
//
// A job that fails for good leaves a failed record, and the engine has
// already removed its temporary files. Snapshots are never deleted by a
// rollback: a restore that rolls back after its pre-restore backup
// succeeded keeps it (only running records are marked failed).
//
// # Concurrency
//
// Backup jobs use the site's lock key, site:<id> (LockKey, the same as
// sites.LockKey), so a site is never backed up twice at once and a backup
// never runs alongside another job that changes the site (an update,
// restore, or deletion). Backups of different sites run concurrently, also
// within one client group's repository. See package backup/restic for how
// prune and check (#54) are kept apart from backups.
package sitebackup

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/rchouinard/pressfleet/control-plane/internal/backup"
	"github.com/rchouinard/pressfleet/control-plane/internal/jobs"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
)

// JobType backs up a site.
const JobType = "site.backup"

// Step names. Keep them stable: a queued job resumes by step name, and
// other job types include the first two.
const (
	StepRecord   = "record_backup"
	StepSnapshot = "take_snapshot"
	stepFinish   = "finish"
)

// DataBackupID is the job data key under which the steps keep the ID of the
// backup record they created (jobs.Run.Get).
const DataBackupID = "backup_id"

// dataSkipped is the job data key that says a scheduled backup was
// skipped, so the later steps do nothing.
const dataSkipped = "skipped"

// Sentinel errors.
var (
	// ErrSiteNotFound means the site doesn't exist.
	ErrSiteNotFound = errors.New("site not found")
	// ErrClientGroupNotFound means the client group doesn't exist.
	ErrClientGroupNotFound = errors.New("client group not found")
	// ErrNotBackupable means the site has nothing to back up right now: it
	// is being provisioned or deleted, or its provisioning failed.
	ErrNotBackupable = errors.New("the site can't be backed up")
)

// JobPayload is the payload of a JobType job.
type JobPayload struct {
	// Kind is what asked for the backup; empty means store.BackupManual.
	Kind store.BackupKind `json:"kind,omitempty"`
}

// Enqueue queues a JobType job that backs up the site. q may be a
// transaction, to queue the job atomically with other changes. It doesn't
// look at the site: the job checks it when it runs.
func Enqueue(ctx context.Context, q store.Querier, siteID string, kind store.BackupKind, createdByUserID string) (store.Job, error) {
	if !kind.Valid() {
		return store.Job{}, fmt.Errorf("queue backup of site %s: invalid kind %q", siteID, kind)
	}
	job, err := jobs.Enqueue(ctx, q, jobs.Spec{
		Type:            JobType,
		TargetType:      store.JobTargetSite,
		TargetID:        siteID,
		Payload:         JobPayload{Kind: kind},
		LockKey:         store.SiteLockKey(siteID),
		CreatedByUserID: createdByUserID,
	})
	if err != nil {
		return store.Job{}, fmt.Errorf("queue backup of site %s: %w", siteID, err)
	}
	return job, nil
}

// Options configure a Service.
type Options struct {
	// DB is the state database. Required.
	DB *store.DB
	// Engine takes the snapshots. Required.
	Engine backup.Engine
	// Window is the platform's backup window for scheduled backups; zero
	// means DefaultWindow.
	Window Window
	// Logger defaults to discarding output.
	Logger *slog.Logger
}

// Service queues site backups, runs their jobs, and lists their records.
type Service struct {
	db     *store.DB
	engine backup.Engine
	window Window
	log    *slog.Logger
}

// New returns a Service.
func New(opts Options) (*Service, error) {
	if opts.DB == nil || opts.Engine == nil {
		return nil, errors.New("sitebackup: DB and Engine are required")
	}
	s := &Service{db: opts.DB, engine: opts.Engine, window: opts.Window.orDefault(), log: opts.Logger}
	if s.log == nil {
		s.log = slog.New(slog.DiscardHandler)
	}
	return s, nil
}

// Backup queues a manual backup of the site. It returns ErrSiteNotFound if
// the site doesn't exist and ErrNotBackupable if it has nothing to back up
// now.
func (s *Service) Backup(ctx context.Context, siteID, createdByUserID string) (store.Job, error) {
	var job store.Job
	err := s.db.InTx(ctx, func(q store.Querier) error {
		if _, err := backupableSite(ctx, q, siteID); err != nil {
			return err
		}
		var err error
		job, err = Enqueue(ctx, q, siteID, store.BackupManual, createdByUserID)
		return err
	})
	if err != nil {
		return store.Job{}, err
	}
	s.log.InfoContext(ctx, "site backup queued", "site_id", siteID, "job_id", job.ID)
	return job, nil
}

// backupableSite returns the site if it can be backed up: it exists and is
// neither being provisioned nor deleted, and its provisioning didn't fail.
// A stopped site can be backed up; so can one that is being updated (the
// backup waits for the update's job).
func backupableSite(ctx context.Context, q store.Querier, siteID string) (store.Site, error) {
	site, err := store.NewSites(q).Get(ctx, siteID)
	if errors.Is(err, store.ErrNotFound) {
		return store.Site{}, fmt.Errorf("site %s: %w", siteID, ErrSiteNotFound)
	}
	if err != nil {
		return store.Site{}, err
	}
	switch site.Status {
	case store.SiteProvisioning, store.SiteDeleting:
		return store.Site{}, fmt.Errorf("%w: the site is %s", ErrNotBackupable, site.Status)
	}
	if site.WPSaltsSecretRef == "" {
		return store.Site{}, fmt.Errorf("%w: it was never provisioned", ErrNotBackupable)
	}
	return site, nil
}

// List returns a page of the site's backup records, newest first. It
// returns ErrSiteNotFound if the site doesn't exist; the records of a
// deleted site's backups are kept, but not listed here.
func (s *Service) List(ctx context.Context, f store.BackupRecordFilter) (store.BackupRecordPage, error) {
	if _, err := store.NewSites(s.db).Get(ctx, f.SiteID); errors.Is(err, store.ErrNotFound) {
		return store.BackupRecordPage{}, fmt.Errorf("site %s: %w", f.SiteID, ErrSiteNotFound)
	} else if err != nil {
		return store.BackupRecordPage{}, err
	}
	return store.NewBackupRecords(s.db).List(ctx, f)
}

// ListGroup returns a page of the client group's backup records, newest
// first; with f.DeletedSites, those of its sites that have been deleted,
// whose snapshots stay in the group's repository until it is deleted. It
// returns ErrClientGroupNotFound if the group doesn't exist.
func (s *Service) ListGroup(ctx context.Context, f store.BackupRecordFilter) (store.BackupRecordPage, error) {
	if _, err := store.NewClientGroups(s.db).Get(ctx, f.ClientGroupID); errors.Is(err, store.ErrNotFound) {
		return store.BackupRecordPage{}, fmt.Errorf("client group %s: %w", f.ClientGroupID, ErrClientGroupNotFound)
	} else if err != nil {
		return store.BackupRecordPage{}, err
	}
	f.SiteID = ""
	return store.NewBackupRecords(s.db).List(ctx, f)
}
