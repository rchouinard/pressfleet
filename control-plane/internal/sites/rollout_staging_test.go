package sites_test

import (
	"context"
	"testing"

	"github.com/rchouinard/pressfleet/control-plane/internal/sites"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
)

// Startup reconcile renders a staging site as staging, with outbound mail
// off, whatever its containers were rendered with before (a site cloned by
// an earlier release, or restored on a new host), and leaves a production
// site's files as they are.
func TestReconcileRendersStagingSites(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		prod := e.running(t, "Production")
		stg := e.running(t, "Staging")
		// The containers still hold the production rendering; only the
		// record says staging.
		stg = e.stagingOf(t, stg, prod, true)
		prodBefore, _ := e.prov.Site(prod.ID)

		if _, err := e.svc.EnqueueReconcile(context.Background()); err != nil {
			t.Fatal(err)
		}
		e.runJobs(t)

		rt, _ := e.prov.Site(stg.ID)
		if rt.Config.Environment != "staging" || !rt.Config.DisableMail {
			t.Errorf("staging site rendered with Environment %q, DisableMail %v", rt.Config.Environment, rt.Config.DisableMail)
		}
		prodAfter, _ := e.prov.Site(prod.ID)
		if prodAfter.Config.Environment != "production" || prodAfter.Config.DisableMail ||
			prodAfter.Config.Environment != prodBefore.Config.Environment || prodAfter.Config.DisableMail != prodBefore.Config.DisableMail {
			t.Errorf("production site rendered with Environment %q, DisableMail %v", prodAfter.Config.Environment, prodAfter.Config.DisableMail)
		}
		if got := e.site(t, stg.ID); got.Status != store.SiteRunning {
			t.Errorf("staging site is %s", got.Status)
		}
	})
}

// Core rollouts include staging sites, each under its own policy: approving
// a major release for the production site doesn't move its staging site.
func TestCoreRolloutIncludesStagingSites(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newCoreEnv(t, db)
		prod := e.siteOn(t, "Prod", "7.1.2")
		stg := e.stagingOf(t, e.siteOn(t, "Stg", "7.1.2"), prod, true)

		// Minor releases need no approval, and reach both.
		e.rollout(t)
		e.checkCore(t, prod.ID, "7.1.4")
		e.checkCore(t, stg.ID, "7.1.4")

		e.setPolicy(t, prod.ID, sites.CorePolicyUpdate{ApprovedBranch: "7.2"})
		e.runJobs(t)
		e.checkCore(t, prod.ID, "7.2.1")
		e.checkCore(t, stg.ID, "7.1.4")

		e.setPolicy(t, stg.ID, sites.CorePolicyUpdate{ApprovedBranch: "7.2"})
		e.runJobs(t)
		e.checkCore(t, stg.ID, "7.2.1")
		if jobs := e.coreJobs(t, stg.ID); len(jobs) != 2 || jobs[1].State != store.JobSucceeded {
			t.Errorf("staging site's core update jobs = %+v", jobs)
		}
	})
}

// A staging site deleted before the rollout reaches it is skipped like any
// other site.
func TestCoreRolloutSkipsDeletedStagingSite(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newCoreEnv(t, db)
		prod := e.siteOn(t, "Prod", "7.1.2")
		stg := e.stagingOf(t, e.siteOn(t, "Stg", "7.1.2"), prod, true)
		// Listed as running, gone by the time its turn comes.
		if _, err := store.NewSites(db).SetStatus(context.Background(), stg.ID, store.SiteDeleting, "", store.SiteRunning); err != nil {
			t.Fatal(err)
		}
		e.rollout(t)
		e.checkCore(t, prod.ID, "7.1.4")
		if n := len(e.coreJobs(t, stg.ID)); n != 0 {
			t.Errorf("%d core update jobs for the deleting staging site", n)
		}
		if ro, err := e.svc.Rollouts().Rollout(context.Background(), sites.CoreRolloutName); err != nil || ro.Halted() || len(ro.Failures) != 0 {
			t.Errorf("rollout = %+v, %v", ro, err)
		}
	})
}

// Image rollouts include staging sites, and a staging site whose deletion
// starts during its update doesn't count as a failure.
func TestImageRolloutIncludesStagingSites(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newImageEnv(t, db, func(o *sites.Options) { o.CoreUpdates.MaxFailures = 1 })
		prod := e.siteOn(t, "Prod", "7.2.1")
		stg := e.stagingOf(t, e.siteOn(t, "Stg", "7.2.1"), prod, true)

		newPHP, newNginx := build(e.tags.PHP, 2), build(e.tags.Nginx, 2)
		e.prov.SetLocalImage(e.tags.PHP, newPHP)
		e.prov.SetLocalImage(e.tags.Nginx, newNginx)
		e.rollout(t)
		e.checkRuns(t, prod.ID, newPHP, newNginx)
		e.checkRuns(t, stg.ID, newPHP, newNginx)

		// The next build; the staging site's deletion starts while its
		// update checks its health.
		next := build(e.tags.PHP, 3)
		e.prov.SetLocalImage(e.tags.PHP, next)
		e.status = func(siteID string) int {
			if siteID == stg.ID {
				if _, err := store.NewSites(db).SetStatus(context.Background(), siteID, store.SiteDeleting, "", store.SiteUpdating); err != nil {
					t.Error(err)
				}
				return 500
			}
			return 200
		}
		e.rollout(t)
		if jobs := e.imageJobs(t, stg.ID); len(jobs) != 2 || jobs[1].State != store.JobFailed {
			t.Fatalf("staging site's image update jobs = %+v, want the second failed", jobs)
		}
		if ro := e.rolloutState(t); ro.Halted() || ro.ConsecutiveFailures != 0 || len(ro.Failures) != 0 {
			t.Errorf("rollout = %+v; a deleted site's failure counted", ro)
		}
		e.checkRuns(t, prod.ID, next, newNginx)
	})
}

// Template rollouts include staging sites.
func TestTemplateRolloutIncludesStagingSites(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newTemplateEnv(t, db)
		prod := e.siteOn(t, "Prod", "7.2.1")
		stg := e.stagingOf(t, e.siteOn(t, "Stg", "7.2.1"), prod, true)

		e.tmpl.SetFingerprint(newTemplates)
		e.rollout(t)
		e.checkTemplates(t, prod.ID, newTemplates)
		e.checkTemplates(t, stg.ID, newTemplates)
		if jobs := e.templateJobs(t, stg.ID); len(jobs) != 1 || jobs[0].State != store.JobSucceeded {
			t.Errorf("staging site's template update jobs = %+v", jobs)
		}
		if _, pending, err := e.svc.Rollouts().TemplateProgress(context.Background()); err != nil || pending != 0 {
			t.Errorf("pending = %d, %v", pending, err)
		}
	})
}
