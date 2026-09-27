package sites_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/rchouinard/pressfleet/control-plane/internal/id"
	"github.com/rchouinard/pressfleet/control-plane/internal/sites"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
)

func TestPurgeCache(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		site := e.running(t, "Cached")

		for _, status := range []store.SiteStatus{store.SiteRunning, store.SiteStopped} {
			if status == store.SiteStopped {
				if _, err := e.svc.Stop(ctx, site.ID, ""); err != nil {
					t.Fatal(err)
				}
				e.runJobs(t)
			}
			flushes, purges := e.cache.Flushes(site.ID), e.pagePurges(t, site.ID)
			job, err := e.svc.PurgeCache(ctx, site.ID, "")
			if err != nil {
				t.Fatal(err)
			}
			if job.Type != sites.PurgeCacheJobType || job.TargetID != site.ID || job.LockKey != sites.LockKey(site.ID) {
				t.Errorf("job = %+v", job)
			}
			e.runJobs(t)
			d := e.job(t, job.ID)
			if d.Job.State != store.JobSucceeded {
				t.Fatalf("%s site: job %s: %s", status, d.Job.State, d.Job.Error)
			}
			var steps []string
			for _, s := range d.Steps {
				steps = append(steps, s.Name)
			}
			// The object cache first, so pages aren't rebuilt from stale
			// objects between the two.
			if want := []string{"purge_object_cache", "purge_page_cache", "finish"}; !slices.Equal(steps, want) {
				t.Errorf("steps = %v", steps)
			}
			if e.cache.Flushes(site.ID) != flushes+1 || e.pagePurges(t, site.ID) != purges+1 {
				t.Errorf("%s site: object cache flushes %d -> %d, page cache purges %d -> %d",
					status, flushes, e.cache.Flushes(site.ID), purges, e.pagePurges(t, site.ID))
			}
			if got := e.site(t, site.ID); got.Status != status {
				t.Errorf("status after the purge = %s, want %s", got.Status, status)
			}
		}
	})
}

func TestPurgeCacheRefused(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		if _, err := e.svc.PurgeCache(ctx, id.New(), ""); !errors.Is(err, sites.ErrNotFound) {
			t.Errorf("missing site: %v", err)
		}
		busy, _ := e.create(t, "Busy")
		if _, err := e.svc.PurgeCache(ctx, busy.ID, ""); !errors.Is(err, sites.ErrBusy) {
			t.Errorf("site being provisioned: %v", err)
		}
		e.wp.failInstall = errors.New("install failed")
		e.runJobs(t)
		if _, err := e.svc.PurgeCache(ctx, busy.ID, ""); !errors.Is(err, sites.ErrNotProvisioned) {
			t.Errorf("site whose provisioning failed: %v", err)
		}
	})
}

// TestPurgeCacheFailure checks that a purge that keeps failing fails its
// job and leaves the site alone.
func TestPurgeCacheFailure(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		site := e.running(t, "Flaky")
		e.cache.Err = func(op, _ string) error {
			if op == "FlushCache" {
				return errors.New("object cache unreachable")
			}
			return nil
		}
		job, err := e.svc.PurgeCache(ctx, site.ID, "")
		if err != nil {
			t.Fatal(err)
		}
		e.runJobs(t)
		d := e.job(t, job.ID)
		if d.Job.State != store.JobFailed || !strings.Contains(d.Job.Error, "object cache unreachable") {
			t.Errorf("job %s: %s", d.Job.State, d.Job.Error)
		}
		if e.pagePurges(t, site.ID) != 0 {
			t.Error("the page cache was purged although the object cache wasn't")
		}
		if got := e.site(t, site.ID); got.Status != store.SiteRunning || got.StatusMessage != "" {
			t.Errorf("site = %s %q", got.Status, got.StatusMessage)
		}
	})
}

// pagePurges returns how often the provisioner purged the site's page
// cache.
func (e *env) pagePurges(t *testing.T, siteID string) int {
	t.Helper()
	s, ok := e.prov.Site(siteID)
	if !ok {
		t.Fatalf("site %s has no runtime", siteID)
	}
	return s.CachePurges
}
