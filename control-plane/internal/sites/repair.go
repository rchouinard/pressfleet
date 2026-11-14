package sites

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/jobs"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
)

// Sites stuck in updating (#282).
//
// A site job that changes a site's status puts it in updating when the job
// is queued, and the job's last Undo step puts a status back when it fails.
// A rollback stops at the first Undo that fails for good, so that last step
// can be skipped, and then nothing is working on the site while manageable
// refuses every change because it is updating. Rather than harden every
// Undo, the job system tells us when a job finishes as rollback_failed
// ([jobs.Handler].OnRollbackFailed, set in [Service.Handlers]), and a
// periodic repair ([Service.RunStatusRepair]) covers what the hook can't:
// the state database being unavailable when the job finished, a site that
// was stuck before this existed, and jobs that end failed without having
// undone their first step.
//
// Both do the same thing, [Service.repairStuckSite]: a site in updating with
// no unfinished job of a type that owns that status becomes failed, which
// can be restored. Only those types count, in both directions. A job of
// another type that ends rollback_failed (a backup, say) must not touch a
// site whose update is queued, and a site shouldn't have to wait for jobs
// that don't own the status: site.wp_cron runs every few minutes for every
// site, and startup queues a site.reconcile for each.

// statusOwningJobTypes are the job types that set a site's status to
// updating and are responsible for taking it out again. When a job type
// starts doing that, add it here.
var statusOwningJobTypes = []string{
	UpdateJobType, AliasesJobType, GoLiveJobType, RestoreJobType, PushJobType, RefreshJobType,
	CoreUpdateJobType, ImageUpdateJobType, TemplateUpdateJobType, RecoverJobType,
}

// statusRepairInterval is how often [Service.RunStatusRepair] looks for
// stuck sites. It only needs to catch what the finish hook missed.
const statusRepairInterval = 5 * time.Minute

// withStatusRepair sets the rollback_failed hook on the handlers of the
// job types that own the updating status.
func (s *Service) withStatusRepair(handlers []jobs.Handler) []jobs.Handler {
	for i, h := range handlers {
		for _, typ := range statusOwningJobTypes {
			if h.Type == typ {
				handlers[i].OnRollbackFailed = s.onRollbackFailed
			}
		}
	}
	return handlers
}

func (s *Service) onRollbackFailed(ctx context.Context, job store.Job) error {
	if job.TargetType != store.JobTargetSite {
		return nil
	}
	_, err := s.repairStuckSite(ctx, job.TargetID)
	return err
}

// RepairStuckSites moves every site that is updating with no unfinished job
// of a status-owning type to failed, and returns how many it moved.
func (s *Service) RepairStuckSites(ctx context.Context) (int, error) {
	sites := store.NewSites(s.db)
	repaired, cursor := 0, ""
	for {
		page, err := sites.List(ctx, store.SiteFilter{Status: store.SiteUpdating, Before: cursor, Limit: 200})
		if err != nil {
			return repaired, fmt.Errorf("list updating sites: %w", err)
		}
		for _, site := range page.Sites {
			ok, err := s.repairStuckSite(ctx, site.ID)
			if err != nil {
				return repaired, err
			}
			if ok {
				repaired++
			}
		}
		if page.NextCursor == "" {
			return repaired, nil
		}
		cursor = page.NextCursor
	}
}

// RunStatusRepair repairs stuck sites now and then every few minutes until
// ctx is done.
func (s *Service) RunStatusRepair(ctx context.Context) error {
	t := time.NewTicker(statusRepairInterval)
	defer t.Stop()
	for {
		if _, err := s.RepairStuckSites(ctx); err != nil && ctx.Err() == nil {
			s.log.WarnContext(ctx, "repairing sites stuck in updating failed; trying again later", "error", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

// repairStuckSite marks the site failed if it is updating and no job that
// could still change that is queued, running, or rolling back. The check and
// the change share a transaction. Nothing can queue such a job meanwhile:
// every path that does so sets updating in the same transaction as it
// enqueues, and none starts from a site that is already updating.
func (s *Service) repairStuckSite(ctx context.Context, siteID string) (bool, error) {
	repaired := false
	err := s.db.InTx(ctx, func(q store.Querier) error {
		site, err := store.NewSites(q).Get(ctx, siteID)
		if errors.Is(err, store.ErrNotFound) || (err == nil && site.Status != store.SiteUpdating) {
			return nil
		}
		if err != nil {
			return err
		}
		sj := store.NewJobs(q)
		filter := store.JobFilter{Types: statusOwningJobTypes, TargetType: store.JobTargetSite, TargetID: siteID, Limit: 1}
		filter.States = []store.JobState{store.JobQueued, store.JobRunning, store.JobRollingBack}
		page, err := sj.List(ctx, filter)
		if err != nil {
			return err
		}
		if len(page.Jobs) > 0 {
			return nil
		}
		filter.States = nil
		page, err = sj.List(ctx, filter)
		if err != nil {
			return err
		}
		var jobErr string
		if len(page.Jobs) > 0 {
			jobErr = page.Jobs[0].Error
		}
		msg := statusMessage("a job failed and couldn't be rolled back, so the site may not match its settings; restore it from a backup", jobErr)
		repaired, err = store.NewSites(q).SetStatus(ctx, siteID, store.SiteFailed, msg, store.SiteUpdating)
		return err
	})
	if err != nil {
		return false, fmt.Errorf("repair site %s: %w", siteID, err)
	}
	if repaired {
		s.log.WarnContext(ctx, "site was stuck updating with no job working on it; marked it failed", "site_id", siteID)
	}
	return repaired, nil
}
