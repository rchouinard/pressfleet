package sites_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner/provisionertest"
	"github.com/rchouinard/pressfleet/control-plane/internal/sites"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
)

// Template fingerprints of the simulated releases.
const (
	oldTemplates   = "r1-old"
	newTemplates   = "r2-new"
	fixedTemplates = "r3-fixed"
)

// templateEnv is a coreEnv whose sites run the latest WordPress, so the
// core rollout leaves them alone, with a driver that fingerprints its
// renders. Renders with a broken fingerprint fail the health check.
type templateEnv struct {
	*coreEnv
	tmpl *provisionertest.Templates

	mu     sync.Mutex
	broken map[string]bool
}

func newTemplateEnv(t *testing.T, db *store.DB, opts ...func(*sites.Options)) *templateEnv {
	t.Helper()
	e := &templateEnv{broken: map[string]bool{}}
	opts = append([]func(*sites.Options){func(o *sites.Options) {
		e.tmpl = provisionertest.NewTemplates(o.Provisioner.(*provisionertest.Fake), oldTemplates)
		o.Provisioner = e.tmpl
	}}, opts...)
	e.coreEnv = newCoreEnv(t, db, opts...)
	e.status = func(siteID string) int {
		e.mu.Lock()
		defer e.mu.Unlock()
		if e.broken[e.tmpl.Rendered(siteID)] {
			return 500
		}
		return 200
	}
	return e
}

func (e *templateEnv) breakTemplates(fingerprint string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.broken[fingerprint] = true
}

// checkTemplates checks that the site's containers and record are on the
// templates, and that it is running.
func (e *templateEnv) checkTemplates(t *testing.T, siteID, fingerprint string) {
	t.Helper()
	if got := e.tmpl.Rendered(siteID); got != fingerprint {
		t.Errorf("site %s is rendered with %q, want %q", siteID, got, fingerprint)
	}
	site := e.site(t, siteID)
	if site.TemplateFingerprint != fingerprint {
		t.Errorf("site %s records templates %q, want %q", siteID, site.TemplateFingerprint, fingerprint)
	}
	if site.Status != store.SiteRunning {
		t.Errorf("site %s is %s (%s), want running", siteID, site.Status, site.StatusMessage)
	}
}

// templateJobs returns the site's template update jobs, oldest first.
func (e *templateEnv) templateJobs(t *testing.T, siteID string) []store.Job {
	t.Helper()
	page, err := store.NewJobs(e.db).List(context.Background(), store.JobFilter{
		Types: []string{sites.TemplateUpdateJobType}, TargetType: store.JobTargetSite, TargetID: siteID,
	})
	if err != nil {
		t.Fatal(err)
	}
	out := page.Jobs
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

func (e *templateEnv) templateRollout(t *testing.T) sites.RolloutStatus {
	t.Helper()
	ro, err := e.svc.Rollout(context.Background(), sites.TemplateRolloutName)
	if err != nil {
		t.Fatal(err)
	}
	return ro
}

func (e *templateEnv) reconcile(t *testing.T) {
	t.Helper()
	if _, err := e.svc.EnqueueReconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	e.runJobs(t)
}

func TestTemplateRolloutSiteBySite(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newTemplateEnv(t, db)
		var ids []string
		for _, name := range []string{"A", "B", "C"} {
			ids = append(ids, e.siteOn(t, name, "7.2.1").ID)
		}
		// Provisioning records the templates the site was rendered with.
		for _, id := range ids {
			e.checkTemplates(t, id, oldTemplates)
		}
		e.rollout(t)
		if n := len(e.templateJobs(t, ids[0])); n != 0 {
			t.Fatalf("%d template update jobs without new templates", n)
		}

		// An upgrade to a release with other templates.
		e.tmpl.SetFingerprint(newTemplates)
		if err := e.svc.EnqueueRollout(context.Background()); err != nil {
			t.Fatal(err)
		}
		for range 100 {
			ran, err := e.pool.RunNext(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if !ran {
				break
			}
			if err := e.svc.EnqueueRollout(context.Background()); err != nil {
				t.Fatal(err)
			}
			updating := 0
			for _, id := range ids {
				if e.site(t, id).Status == store.SiteUpdating {
					updating++
				}
			}
			if updating > 1 {
				t.Fatalf("%d sites updating at once", updating)
			}
		}
		var last time.Time
		for i, id := range ids {
			e.checkTemplates(t, id, newTemplates)
			jobs := e.templateJobs(t, id)
			if len(jobs) != 1 || jobs[0].State != store.JobSucceeded {
				t.Fatalf("template update jobs of site %d = %+v", i, jobs)
			}
			if jobs[0].StartedAt.Before(last) {
				t.Errorf("site %d updated before site %d finished", i, i-1)
			}
			last = jobs[0].FinishedAt
			if res := jobResult(t, jobs[0]); res["templates"] != newTemplates {
				t.Errorf("job result = %v", res)
			}
		}
		if ro := e.templateRollout(t); ro.Halted() || ro.ConsecutiveFailures != 0 || len(ro.Failures) != 0 || ro.Fingerprint != newTemplates {
			t.Errorf("rollout = %+v", ro)
		}
	})
}

func TestTemplateRolloutRevertsAndHalts(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newTemplateEnv(t, db, func(o *sites.Options) { o.CoreUpdates.MaxFailures = 2 })
		var ids []string
		for _, name := range []string{"A", "B", "C"} {
			ids = append(ids, e.siteOn(t, name, "7.2.1").ID)
		}
		e.tmpl.SetFingerprint(newTemplates)
		e.breakTemplates(newTemplates)
		e.rollout(t)
		for i, id := range ids[:2] {
			jobs := e.templateJobs(t, id)
			if len(jobs) != 1 || jobs[0].State != store.JobFailed || !strings.Contains(jobs[0].Error, "500") {
				t.Fatalf("site %d: template update jobs = %+v", i, jobs)
			}
			// Back on the previous definition and fingerprint.
			e.checkTemplates(t, id, oldTemplates)
			if msg := e.site(t, id).StatusMessage; !strings.Contains(msg, "reverted to the previous definition") {
				t.Errorf("site %d status message = %q", i, msg)
			}
		}
		if n := len(e.templateJobs(t, ids[2])); n != 0 {
			t.Fatalf("the rollout updated site 2 after halting")
		}
		ro := e.templateRollout(t)
		if !ro.Halted() || ro.ConsecutiveFailures != 2 || len(ro.Failures) != 2 || ro.Failures[0].Target != newTemplates {
			t.Fatalf("rollout = %+v; want halted after 2 failures", ro)
		}

		// A restart reconciles the sites, but leaves the ones on other
		// templates to the rollout: no re-render puts the broken templates
		// on them.
		before := e.updates()
		e.reconcile(t)
		for i, id := range ids {
			e.checkTemplates(t, id, oldTemplates)
			if n := e.updates()[id] - before[id]; n != 0 {
				t.Errorf("site %d: reconciliation re-rendered it %d times", i, n)
			}
		}
		for _, j := range e.reconcileJobs(t) {
			if d := e.job(t, j.ID); j.State != store.JobSucceeded || !strings.Contains(jobLog(d), "waiting for the templates rollout") {
				t.Errorf("reconcile job of %s: %s\n%s", j.TargetID, j.State, jobLog(d))
			}
		}

		// Resuming tries the site that wasn't, but not the failed ones.
		if _, err := e.svc.ResumeRollout(context.Background(), sites.TemplateRolloutName); err != nil {
			t.Fatal(err)
		}
		e.runJobs(t)
		for i, id := range ids {
			if n := len(e.templateJobs(t, id)); n != 1 {
				t.Errorf("site %d: %d template update jobs after resuming, want 1", i, n)
			}
		}

		// A release with fixed templates rolls out to every site.
		e.tmpl.SetFingerprint(fixedTemplates)
		e.rollout(t)
		for _, id := range ids {
			e.checkTemplates(t, id, fixedTemplates)
		}
		if ro := e.templateRollout(t); ro.Halted() || ro.ConsecutiveFailures != 0 || len(ro.Failures) != 0 {
			t.Errorf("rollout after the fix = %+v", ro)
		}
	})
}

func TestTemplateRolloutFailedRevert(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newTemplateEnv(t, db)
		site := e.siteOn(t, "A", "7.2.1")
		e.tmpl.SetFingerprint(newTemplates)
		e.breakTemplates(newTemplates)
		e.tmpl.Err = func(op, _ string) error {
			if op == "RestoreRender" {
				return errors.New("disk full")
			}
			return nil
		}
		e.rollout(t)
		jobs := e.templateJobs(t, site.ID)
		if len(jobs) != 1 || jobs[0].State != store.JobFailed {
			t.Fatalf("template update jobs = %+v", jobs)
		}
		got := e.site(t, site.ID)
		if got.Status != store.SiteFailed || !strings.Contains(got.StatusMessage, "couldn't be put back") {
			t.Errorf("site is %s (%s), want failed", got.Status, got.StatusMessage)
		}
		// It runs the new render, and its record says so.
		if got.TemplateFingerprint != newTemplates || e.tmpl.Rendered(site.ID) != newTemplates {
			t.Errorf("site records %q and runs %q, want %q", got.TemplateFingerprint, e.tmpl.Rendered(site.ID), newTemplates)
		}
	})
}

// Sites rendered before fingerprints were recorded have none: the rollout
// re-renders them, and stopped ones are reconciled instead.
func TestTemplateRolloutSitesWithoutFingerprint(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newTemplateEnv(t, db)
		running := e.siteOn(t, "Running", "7.2.1")
		stopped := e.siteOn(t, "Stopped", "7.2.1")
		if _, err := e.svc.Stop(ctx, stopped.ID, ""); err != nil {
			t.Fatal(err)
		}
		e.runJobs(t)
		for _, id := range []string{running.ID, stopped.ID} {
			if _, err := store.NewSites(db).SetTemplateFingerprint(ctx, id, ""); err != nil {
				t.Fatal(err)
			}
		}

		// The control plane starts: reconciliation, then the rollout.
		e.reconcile(t)
		if got := e.site(t, stopped.ID); got.TemplateFingerprint != oldTemplates || got.Status != store.SiteStopped {
			t.Errorf("stopped site after reconciling: %q, %s", got.TemplateFingerprint, got.Status)
		}
		if got := e.site(t, running.ID).TemplateFingerprint; got != "" {
			t.Errorf("the reconciliation recorded %q for a running site", got)
		}
		e.rollout(t)
		e.checkTemplates(t, running.ID, oldTemplates)
		if n := len(e.templateJobs(t, stopped.ID)); n != 0 {
			t.Errorf("%d template update jobs for a stopped site", n)
		}
	})
}

// A template update an earlier release queued fails without counting
// against the current release's rollout, which then updates the site.
func TestTemplateUpdateFromEarlierRelease(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newTemplateEnv(t, db, func(o *sites.Options) { o.CoreUpdates.MaxFailures = 1 })
		site := e.siteOn(t, "A", "7.2.1")
		e.tmpl.SetFingerprint(newTemplates)
		if err := e.svc.EnqueueRollout(ctx); err != nil {
			t.Fatal(err)
		}
		for len(e.templateJobs(t, site.ID)) == 0 {
			if ran, err := e.pool.RunNext(ctx); err != nil || !ran {
				t.Fatalf("no template update queued: %v", err)
			}
		}
		// Upgraded again before the job ran.
		e.tmpl.SetFingerprint(fixedTemplates)
		e.rollout(t)
		jobs := e.templateJobs(t, site.ID)
		if len(jobs) != 2 || jobs[0].State != store.JobFailed || !strings.Contains(jobs[0].Error, "this release has") ||
			jobs[1].State != store.JobSucceeded {
			t.Fatalf("template update jobs = %+v", jobs)
		}
		e.checkTemplates(t, site.ID, fixedTemplates)
		if ro := e.templateRollout(t); ro.Halted() || ro.ConsecutiveFailures != 0 || len(ro.Failures) != 0 {
			t.Errorf("rollout = %+v", ro)
		}
	})
}
