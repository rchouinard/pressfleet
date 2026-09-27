package sites

import (
	"context"
	"errors"
	"fmt"

	"github.com/rchouinard/pressfleet/control-plane/internal/jobs"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
)

// Purging a site's caches (#41, design doc §5.6).
//
// A site has two caches: the page cache in its own runtime, which
// provisioner.PurgeCache empties, and its database on the shared object
// cache, which objectcache.Manager.FlushCache empties as the admin user.
// Purging goes through the control plane rather than WP-CLI (`wp cache
// flush`, Nginx Helper's purge) so that it works whatever state WordPress
// is in: stopped, with a plugin deactivated, or failing to boot after a
// bad deploy or a restore, which is exactly when a purge is wanted.
//
// The object cache goes first: a page rendered between the two purges
// would otherwise be built from stale objects and cached again.
//
// cacheSteps are also for the workflows that end with a purge, go-live
// (§5.2) and restore (§5.4): they append them to their own steps.

// PurgeCacheJobType purges a site's caches.
const PurgeCacheJobType = "site.purge_cache"

// Step names of the cache purge steps. Keep them stable: a queued job
// resumes by step name.
const (
	stepPurgeObjectCache = "purge_object_cache"
	stepPurgePageCache   = "purge_page_cache"
)

// PurgeCache queues a [PurgeCacheJobType] job that empties the site's
// object cache and page cache. Like start and stop it is refused with
// ErrBusy while another job works on the site, and with ErrNotProvisioned
// for a site whose provisioning failed.
func (s *Service) PurgeCache(ctx context.Context, siteID, createdByUserID string) (store.Job, error) {
	return s.enqueueSiteJob(ctx, siteID, PurgeCacheJobType, createdByUserID)
}

func (s *Service) purgeCacheHandler() jobs.Handler {
	steps := s.cacheSteps()
	steps = append(steps, jobs.Step{Name: stepFinishManage, Do: s.finishPurge})
	return jobs.Handler{Type: PurgeCacheJobType, Steps: steps}
}

// cacheSteps purge the job target's caches: the object cache, then the
// page cache. Neither has anything to undo.
func (s *Service) cacheSteps() []jobs.Step {
	return []jobs.Step{
		{Name: stepPurgeObjectCache, Do: s.purgeObjectCache},
		{Name: stepPurgePageCache, Do: s.purgePageCache},
	}
}

func (s *Service) purgeObjectCache(ctx context.Context, r *jobs.Run) error {
	site, err := s.managedSite(ctx, r)
	if err != nil {
		return err
	}
	if err := s.caches.FlushCache(ctx, site.ID); err != nil {
		return fmt.Errorf("flushing the object cache: %w", err)
	}
	r.Logf("Flushed the object cache.")
	return nil
}

func (s *Service) purgePageCache(ctx context.Context, r *jobs.Run) error {
	site, err := s.managedSite(ctx, r)
	if err != nil {
		return err
	}
	err = s.prov.PurgeCache(ctx, site.ID)
	if errors.Is(err, provisioner.ErrSiteNotFound) {
		// No containers means no cache volume either.
		r.Warnf("The site has no containers, so it has no page cache to purge.")
		return nil
	}
	if err != nil {
		return fmt.Errorf("purging the page cache: %w", err)
	}
	r.Logf("Purged the page cache.")
	return nil
}

func (s *Service) finishPurge(_ context.Context, r *jobs.Run) error {
	return r.SetResult("site_id", r.Job().TargetID)
}
