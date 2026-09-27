package sites

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/rchouinard/pressfleet/control-plane/internal/jobs"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
)

// Converging existing sites on startup.
//
// Some settings are install-wide rather than per site: the WordPress
// security set (§6.1), and later the SMTP relay and the like. The driver
// applies them whenever it renders a site, so new sites and every update get
// them, but a site nobody touches would keep what it was rendered with. The
// control plane therefore queues a [ReconcileJobType] job for every site
// when it starts ([Service.EnqueueReconcile]), and a change made to the
// configuration reaches existing sites after a restart.
//
// Hooking in:
//
//   - A setting the driver renders into a site's files or containers (a
//     wp-config.php constant, a container environment variable) needs no
//     code here: give it to the driver as install-wide configuration, and
//     the apply step's provisioner.UpdateSite re-renders the site, recreating
//     only the containers whose configuration changed.
//   - State outside the site's containers that must be put back at startup
//     (such as the site's object cache user) is a step of its own: add it to
//     reconcileSteps, before apply if the containers depend on it. Steps must be idempotent and cheap when nothing changed, since
//     they run for every site on every start, and they should skip sites
//     reconcileSite says to leave alone.

// ReconcileJobType converges one site with the install-wide configuration.
const ReconcileJobType = "site.reconcile"

// Step names of the site.reconcile job. Keep them stable: a queued job
// resumes by step name.
const (
	stepReconcileCache = "object_cache"
	stepReconcileApply = "apply"
)

func (s *Service) reconcileHandler() jobs.Handler {
	return jobs.Handler{Type: ReconcileJobType, Steps: s.reconcileSteps()}
}

// reconcileSteps are the site.reconcile job's steps, in order. Features
// that need to restore per-site state at startup add theirs here. The first
// step carries failReconcile, so it runs last in a rollback whichever step
// failed.
func (s *Service) reconcileSteps() []jobs.Step {
	return []jobs.Step{
		{Name: stepReconcileCache, Do: s.reconcileObjectCache, Undo: s.failReconcile},
		{Name: stepReconcileApply, Do: s.reconcileApply},
	}
}

// reconcilable are the statuses whose sites are reconciled. Sites being
// provisioned, updated, or deleted are left to the job that is working on
// them (provisioning and updates render with the current configuration
// anyway), and failed sites are left for the operator: they pick up the
// configuration with their next update.
var reconcilable = []store.SiteStatus{store.SiteRunning, store.SiteStopped}

// EnqueueReconcile queues a [ReconcileJobType] job for every site, except
// sites being deleted and sites that already have one waiting, so restarts
// in quick succession don't pile jobs up. The jobs share each site's lock
// key, so one queued behind a running provisioning or update job runs after
// it and then decides whether there is anything to do. It returns how many
// jobs it queued.
func (s *Service) EnqueueReconcile(ctx context.Context) (int, error) {
	ids, err := store.NewSites(s.db).ListIDs(ctx, store.SiteDeleting)
	if err != nil {
		return 0, fmt.Errorf("reconcile sites: %w", err)
	}
	queued := 0
	for _, id := range ids {
		ok, err := s.enqueueReconcile(ctx, id)
		if err != nil {
			return queued, fmt.Errorf("reconcile site %s: %w", id, err)
		}
		if ok {
			queued++
		}
	}
	if queued > 0 {
		s.log.InfoContext(ctx, "site reconciliation queued", "sites", queued)
	}
	return queued, nil
}

func (s *Service) enqueueReconcile(ctx context.Context, siteID string) (bool, error) {
	queued := false
	err := s.db.InTx(ctx, func(q store.Querier) error {
		page, err := store.NewJobs(q).List(ctx, store.JobFilter{
			States: []store.JobState{store.JobQueued}, Types: []string{ReconcileJobType},
			TargetType: "site", TargetID: siteID, Limit: 1,
		})
		if err != nil {
			return err
		}
		if len(page.Jobs) > 0 {
			return nil
		}
		_, err = jobs.Enqueue(ctx, q, jobs.Spec{
			Type:       ReconcileJobType,
			TargetType: "site",
			TargetID:   siteID,
			LockKey:    LockKey(siteID),
		})
		queued = err == nil
		return err
	})
	return queued, err
}

// reconcileSite loads the job's site and reports whether it should be
// reconciled; if not, it logs why.
func (s *Service) reconcileSite(ctx context.Context, r *jobs.Run) (store.Site, bool, error) {
	site, err := s.jobSite(ctx, r)
	if errors.Is(err, ErrNotFound) {
		r.Logf("The site no longer exists.")
		return store.Site{}, false, nil
	}
	if err != nil {
		return store.Site{}, false, err
	}
	switch {
	case site.WPSaltsSecretRef == "":
		r.Logf("The site was never provisioned; nothing to reconcile.")
		return site, false, nil
	case !slices.Contains(reconcilable, site.Status):
		r.Logf("The site is %s; leaving it alone.", site.Status)
		return site, false, nil
	}
	return site, true, nil
}

// reconcileObjectCache makes sure the site has its object cache user
// (objectcache.Manager.CreateUser is idempotent, and cheap for a site that
// has one), which gives sites provisioned before the object cache existed
// theirs. The reference is kept for apply, which renders it into the site.
// Users lost when the cache server restarts while the control plane runs
// are restored by the object cache manager itself (objectcache/redis
// Manager.Watch), not by this job, which only runs at startup.
func (s *Service) reconcileObjectCache(ctx context.Context, r *jobs.Run) error {
	site, ok, err := s.reconcileSite(ctx, r)
	if err != nil || !ok {
		return err
	}
	ref, err := s.caches.CreateUser(ctx, site.ID)
	if err != nil {
		return err
	}
	if err := r.Set(dataCacheSecretRef, ref); err != nil {
		return err
	}
	r.Logf("The site's object cache user is in place.")
	return nil
}

// reconcileApply re-renders the site with provisioner.UpdateSite, which
// recreates only the containers whose configuration changed and keeps a
// stopped site stopped. A site without containers is left alone: there is
// nothing to converge, and its status tells the operator already.
func (s *Service) reconcileApply(ctx context.Context, r *jobs.Run) error {
	site, ok, err := s.reconcileSite(ctx, r)
	if err != nil || !ok {
		return err
	}
	st, err := s.converge(ctx, r, site)
	if errors.Is(err, provisioner.ErrSiteNotFound) {
		r.Warnf("The site has no containers; nothing to reconcile.")
		return nil
	}
	if err != nil {
		return err
	}
	r.Logf("The site's containers match the platform configuration (%s).", st.Phase)
	return nil
}

// failReconcile records a reconciliation that gave up. Only the status
// message changes, unless the containers are no longer running or stopped,
// in which case the site is marked failed. A site that another job has
// taken over since is left alone.
func (s *Service) failReconcile(ctx context.Context, r *jobs.Run) error {
	siteID := r.Job().TargetID
	status := s.observedStatus(ctx, siteID)
	msg := statusMessage("applying the platform configuration failed", r.Job().Error)
	changed, err := store.NewSites(s.db).SetStatus(ctx, siteID, status, msg, reconcilable...)
	if err != nil {
		return err
	}
	if changed {
		r.Warnf("The site is %s.", status)
	}
	return nil
}
