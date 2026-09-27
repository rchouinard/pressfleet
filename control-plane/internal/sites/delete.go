package sites

import (
	"context"
	"errors"
	"fmt"

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
	stepDeleteRecord  = "delete_record"
)

// unfinished are the job states of a job that will still run.
var unfinished = []store.JobState{store.JobQueued, store.JobRunning, store.JobRollingBack}

// Delete sets the site's status to deleting and queues its deletion job,
// returning the job. If a deletion job is already queued or running, it is
// returned instead of queuing another. A site whose provisioning job is
// still running can be deleted: the deletion shares the site's lock key, so
// it runs once provisioning has finished or rolled back, and provisioning
// stops at its next step when it sees the site is being deleted. It returns
// ErrNotFound if the site doesn't exist, including once it has been
// deleted.
func (s *Service) Delete(ctx context.Context, siteID, createdByUserID string) (store.Job, error) {
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
				Types: []string{DeleteJobType}, States: unfinished, TargetType: "site", TargetID: siteID, Limit: 1,
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
		if _, err := sites.SetStatus(ctx, siteID, store.SiteDeleting, ""); err != nil {
			return err
		}
		job, err = jobs.Enqueue(ctx, q, jobs.Spec{
			Type:            DeleteJobType,
			TargetType:      "site",
			TargetID:        siteID,
			LockKey:         LockKey(siteID),
			CreatedByUserID: createdByUserID,
		})
		return err
	})
	if err != nil {
		return store.Job{}, err
	}
	s.log.InfoContext(ctx, "site deletion queued", "site_id", siteID, "job_id", job.ID)
	return job, nil
}

// deleteHandler removes everything that belongs to a site, in the reverse
// order of provisioning, and the site row last. Every step succeeds when
// its work is already done, so a retried or repeated deletion converges,
// and a site that no longer exists is simply finished off.
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
