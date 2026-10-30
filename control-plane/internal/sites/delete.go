package sites

import (
	"context"
	"errors"
	"fmt"

	"github.com/rchouinard/pressfleet/control-plane/internal/backup"
	"github.com/rchouinard/pressfleet/control-plane/internal/jobs"
	"github.com/rchouinard/pressfleet/control-plane/internal/sitecron"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
)

// Step names of the site.delete job. Keep them stable: a queued job resumes
// by step name.
const (
	stepMarkDeleting  = "mark_deleting"
	stepDeleteStack   = "delete_stack"
	stepDropDatabase  = "drop_database"
	stepDeleteCache   = "delete_object_cache"
	stepDeleteSecrets = "delete_secrets"
	stepDeleteBackups = "delete_backups"
	stepDeleteRecord  = "delete_record"
)

// ErrNoBackupEngine means the site's backups were to be deleted with it,
// but the service has no backup engine; a wiring bug.
var ErrNoBackupEngine = errors.New("sites: deleting a site's backups needs a backup engine")

// DeleteOptions are the choices of a site deletion.
type DeleteOptions struct {
	// DeleteBackups also removes the site's snapshots from its client
	// group's repository, and the platform's records of them. It can't be
	// undone. Without it the backups outlive the site, and can still be
	// listed, exported, and restored into another site of the group.
	DeleteBackups bool
}

// deletePayload is the payload of a site.delete job. Jobs queued before
// it existed have none, which means the backups stay.
type deletePayload struct {
	DeleteBackups bool `json:"delete_backups,omitempty"`
}

// unfinished are the job states of a job that will still run.
var unfinished = []store.JobState{store.JobQueued, store.JobRunning, store.JobRollingBack}

// Delete sets the site's status to deleting and queues its deletion job,
// returning the job. If a deletion job is already queued or running, it is
// returned instead of queuing another, with the options it was queued
// with. A site whose provisioning job is
// still running can be deleted: the deletion shares the site's lock key, so
// it runs once provisioning has finished or rolled back, and provisioning
// stops at its next step when it sees the site is being deleted. It returns
// ErrNotFound if the site doesn't exist, including once it has been
// deleted, ErrNoBackupEngine if opts.DeleteBackups is set but the
// service can't reach backups, and a *StagingSitesError (ErrHasStagingSites)
// for a production site with staging sites that aren't being deleted.
// Nothing is changed then.
func (s *Service) Delete(ctx context.Context, siteID, createdByUserID string, opts DeleteOptions) (store.Job, error) {
	if opts.DeleteBackups && s.engine == nil {
		return store.Job{}, ErrNoBackupEngine
	}
	var job store.Job
	err := s.db.InTx(ctx, func(q store.Querier) error {
		sites := store.NewSites(q)
		site, err := sites.Get(ctx, siteID)
		if errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("site %s: %w", siteID, ErrNotFound)
		}
		if err != nil {
			return err
		}
		if site.Status == store.SiteDeleting {
			page, err := store.NewJobs(q).List(ctx, store.JobFilter{
				Types: []string{DeleteJobType}, States: unfinished, TargetType: store.JobTargetSite, TargetID: siteID, Limit: 1,
			})
			if err != nil {
				return err
			}
			if len(page.Jobs) > 0 {
				job = page.Jobs[0]
				return nil
			}
			// The last deletion job gave up; try again.
		}
		// Refuse here rather than leave it to the foreign key at the job's
		// last step, which would fail after everything else was removed.
		children, err := sites.Children(ctx, siteID)
		if err != nil {
			return err
		}
		var blocking []store.Site
		for _, c := range children {
			if c.Status != store.SiteDeleting {
				blocking = append(blocking, c)
			}
		}
		if len(blocking) > 0 {
			return &StagingSitesError{Sites: blocking}
		}
		if _, err := sites.SetStatus(ctx, siteID, store.SiteDeleting, ""); err != nil {
			return err
		}
		job, err = jobs.Enqueue(ctx, q, jobs.Spec{
			Type:            DeleteJobType,
			TargetType:      store.JobTargetSite,
			TargetID:        siteID,
			LockKey:         store.SiteLockKey(siteID),
			CreatedByUserID: createdByUserID,
			Payload:         deletePayload(opts),
		})
		return err
	})
	if err != nil {
		return store.Job{}, err
	}
	s.log.InfoContext(ctx, "site deletion queued", "site_id", siteID, "job_id", job.ID, "delete_backups", opts.DeleteBackups)
	return job, nil
}

// deleteHandler removes everything that belongs to a site, in the reverse
// order of provisioning, and the site row last. Every step succeeds when
// its work is already done, so a retried or repeated deletion converges,
// and a site that no longer exists is simply finished off.
//
// The site's backups, when the deletion includes them (DeleteOptions), go
// after everything else but the site row. Removing them destroys the last
// copy of the site's data, so it runs only once the site's stack,
// database, cache user, and secrets are gone: a deletion that fails
// earlier leaves every snapshot in place to restore from, and a running
// site never loses its backups to a deletion that then stops. After the
// backups only the site row (with its schedules) remains, removed in one
// transaction. A deletion that fails for good marks the site failed with
// its record intact, and deleting it again (with the same choice) finishes
// the work; no step is undone, because nothing a deletion removed can be
// put back.
func (s *Service) deleteHandler() jobs.Handler {
	return jobs.Handler{
		Type: DeleteJobType,
		Steps: []jobs.Step{
			// Deletion can't be undone; the only "rollback" is to say it
			// failed, so the site can be deleted again.
			{Name: stepMarkDeleting, Do: s.markDeleting, Undo: s.failDeletion},
			{Name: stepDeleteStack, Do: s.deleteStack},
			{Name: stepDropDatabase, Do: s.dropDatabase},
			{Name: stepDeleteCache, Do: s.deleteObjectCache},
			{Name: stepDeleteSecrets, Do: s.deleteSecrets},
			{Name: stepDeleteBackups, Do: s.deleteBackups},
			{Name: stepDeleteRecord, Do: s.deleteRecord},
		},
	}
}

func (s *Service) markDeleting(ctx context.Context, r *jobs.Run) error {
	siteID := r.Job().TargetID
	changed, err := store.NewSites(s.db).SetStatus(ctx, siteID, store.SiteDeleting, "")
	if err != nil {
		return err
	}
	if !changed {
		r.Logf("The site record is already gone; removing anything left behind.")
		return nil
	}
	r.Logf("Deleting the site.")
	return nil
}

// failDeletion records why a deletion gave up. The site is marked failed,
// so DELETE queues a new deletion job.
func (s *Service) failDeletion(ctx context.Context, r *jobs.Run) error {
	msg := statusMessage("deletion failed", r.Job().Error)
	if _, err := store.NewSites(s.db).SetStatus(ctx, r.Job().TargetID, store.SiteFailed, msg, store.SiteDeleting); err != nil {
		return err
	}
	r.Warnf("Deletion did not finish; delete the site again to retry.")
	return nil
}

// deleteSecrets deletes the site's salts, admin password, and preview password; the database
// credentials went with the database (sitedb.Manager.DropDatabase), the
// object cache credentials with the cache user
// (objectcache.Manager.DeleteUser), and the rendered files with the stack
// (provisioner.DeleteSite).
func (s *Service) deleteSecrets(ctx context.Context, r *jobs.Run) error {
	siteID := r.Job().TargetID
	err := s.db.InTx(ctx, func(q store.Querier) error {
		site, err := store.NewSites(q).Get(ctx, siteID)
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		return s.deleteSiteSecrets(ctx, q, site)
	})
	if err != nil {
		return err
	}
	r.Logf("Deleted the site's secrets.")
	return nil
}

// deleteBackups removes the site's snapshots from its client group's
// repository and prunes the data only they used (backup.Engine.ForgetSite),
// then the platform's records of them, if the deletion includes its
// backups. Other sites' snapshots in the shared repository stay.
//
// The forget and prune take restic's exclusive repository lock and wait
// (--retry-lock) for a prune or check job of the repository that holds
// it, the way backups and restores meet those jobs (package
// backup/repomaint); this job keeps the site's lock key, so no backup or
// restore of the site runs meanwhile. Both halves are idempotent: a retry
// finds fewer (or no) snapshots, prunes again, and deletes the records
// that are left. The counts in the job's result add up over attempts.
func (s *Service) deleteBackups(ctx context.Context, r *jobs.Run) error {
	var p deletePayload
	if err := r.Payload(&p); err != nil {
		return err
	}
	if !p.DeleteBackups {
		return nil
	}
	if s.engine == nil {
		return jobs.Permanent(ErrNoBackupEngine)
	}
	siteID := r.Job().TargetID
	res, err := s.engine.ForgetSite(ctx, siteID)
	if errors.Is(err, backup.ErrSiteNotFound) {
		// No site record and no backup records: nothing to find them by.
		r.Logf("The site has no backups to delete.")
		return nil
	}
	if err != nil {
		return fmt.Errorf("delete the site's snapshots: %w", err)
	}
	if err := addResult(r, resultForgottenSnapshots, len(res.Forgotten)); err != nil {
		return err
	}
	n, err := store.NewBackupRecords(s.db).DeleteForSite(ctx, siteID)
	if err != nil {
		return err
	}
	if err := addResult(r, resultDeletedBackupRecords, n); err != nil {
		return err
	}
	r.Logf("Deleted the site's backups: %d snapshots and %d backup records.", len(res.Forgotten), n)
	s.log.InfoContext(ctx, "site backups deleted", "site_id", siteID, "snapshots", len(res.Forgotten), "backup_records", n)
	return nil
}

// Result keys of a site.delete job that deleted the site's backups.
const (
	resultForgottenSnapshots   = "forgotten_snapshots"
	resultDeletedBackupRecords = "deleted_backup_records"
)

// addResult adds n to the job's integer result under key, so a count
// covers every attempt.
func addResult(r *jobs.Run, key string, n int) error {
	var prev int
	if _, err := r.GetResult(key, &prev); err != nil {
		return err
	}
	return r.SetResult(key, prev+n)
}

// deleteRecord deletes the site row together with its schedules.
func (s *Service) deleteRecord(ctx context.Context, r *jobs.Run) error {
	siteID := r.Job().TargetID
	err := s.db.InTx(ctx, func(q store.Querier) error {
		if err := sitecron.RemoveSchedule(ctx, q, siteID); err != nil {
			return err
		}
		// Any other schedule for the site would only enqueue jobs for
		// nothing from now on.
		if _, err := jobs.DeleteTargetSchedules(ctx, q, "site", siteID); err != nil {
			return err
		}
		_, err := store.NewSites(q).Delete(ctx, siteID)
		return err
	})
	if err != nil {
		return err
	}
	r.Logf("Deleted the site record.")
	return nil
}

// statusMessage prefixes a job error and bounds its length.
func statusMessage(prefix, jobErr string) string {
	msg := prefix
	if jobErr != "" {
		msg += ": " + jobErr
	}
	if len(msg) > maxStatusMessage {
		msg = msg[:maxStatusMessage-3] + "..."
	}
	return msg
}

// abortIfDeleting stops a provisioning step for a site that a deletion is
// waiting on, so the deletion doesn't wait for provisioning to finish work
// it would only remove.
func abortIfDeleting(site store.Site) error {
	if site.Status == store.SiteDeleting {
		return jobs.Permanent(errors.New("the site is being deleted"))
	}
	return nil
}
