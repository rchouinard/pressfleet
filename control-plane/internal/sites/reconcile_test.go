package sites_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/rchouinard/pressfleet/control-plane/internal/objectcache/objectcachetest"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
	"github.com/rchouinard/pressfleet/control-plane/internal/sites"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
)

// updates counts the provisioner's UpdateSite calls per site.
func (e *env) updates() map[string]int {
	n := map[string]int{}
	for _, c := range e.prov.Calls() {
		if c.Op == "UpdateSite" {
			n[c.SiteID]++
		}
	}
	return n
}

func (e *env) setStatus(t *testing.T, siteID string, status store.SiteStatus) {
	t.Helper()
	if _, err := store.NewSites(e.db).SetStatus(context.Background(), siteID, status, ""); err != nil {
		t.Fatal(err)
	}
}

// reconcileJobs returns the site.reconcile jobs, newest first.
func (e *env) reconcileJobs(t *testing.T) []store.Job {
	t.Helper()
	page, err := store.NewJobs(e.db).List(context.Background(), store.JobFilter{Types: []string{sites.ReconcileJobType}, Limit: 200})
	if err != nil {
		t.Fatal(err)
	}
	return page.Jobs
}

func TestEnqueueReconcile(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		running := e.running(t, "Running")
		stopped := e.running(t, "Stopped")
		if _, err := e.svc.Stop(ctx, stopped.ID, ""); err != nil {
			t.Fatal(err)
		}
		failed := e.running(t, "Failed")
		e.setStatus(t, failed.ID, store.SiteFailed)
		deleting := e.running(t, "Deleting")
		e.setStatus(t, deleting.ID, store.SiteDeleting)
		e.runJobs(t)
		before := e.updates()

		n, err := e.svc.EnqueueReconcile(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if n != 3 {
			t.Errorf("queued %d jobs, want 3 (every site but the one being deleted)", n)
		}
		// Restarting again before the jobs ran doesn't pile them up.
		if n, err := e.svc.EnqueueReconcile(ctx); err != nil || n != 0 {
			t.Errorf("second EnqueueReconcile = %d, %v; want 0", n, err)
		}
		for _, j := range e.reconcileJobs(t) {
			if j.LockKey != sites.LockKey(j.TargetID) || j.TargetType != "site" {
				t.Errorf("job = %+v", j)
			}
		}

		e.runJobs(t)
		for _, j := range e.reconcileJobs(t) {
			if j.State != store.JobSucceeded {
				t.Errorf("job for %s: %s %s", j.TargetID, j.State, j.Error)
			}
		}
		after := e.updates()
		for _, tc := range []struct {
			site    store.Site
			updated bool
			status  store.SiteStatus
		}{
			{running, true, store.SiteRunning},
			{stopped, true, store.SiteStopped},
			{failed, false, store.SiteFailed},
			{deleting, false, store.SiteDeleting},
		} {
			if got := after[tc.site.ID] - before[tc.site.ID]; (got == 1) != tc.updated || got > 1 {
				t.Errorf("%s: UpdateSite called %d times, want updated = %v", tc.site.Name, got, tc.updated)
			}
			if got := e.site(t, tc.site.ID); got.Status != tc.status || got.StatusMessage != "" {
				t.Errorf("%s: status %s %q, want %s", tc.site.Name, got.Status, got.StatusMessage, tc.status)
			}
		}
		if s, _ := e.prov.Site(stopped.ID); s.Phase != provisioner.SiteStopped {
			t.Errorf("stopped site's stack is %s after reconciling", s.Phase)
		}

		// With the earlier jobs done, the next start queues again.
		if n, err := e.svc.EnqueueReconcile(ctx); err != nil || n != 3 {
			t.Errorf("EnqueueReconcile after the jobs ran = %d, %v; want 3", n, err)
		}
	})
}

func TestReconcileSkipsSitesWithoutContainers(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		site := e.running(t, "Gone")
		if err := e.prov.DeleteSite(ctx, site.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := e.svc.EnqueueReconcile(ctx); err != nil {
			t.Fatal(err)
		}
		e.runJobs(t)
		if jobs := e.reconcileJobs(t); len(jobs) != 1 || jobs[0].State != store.JobSucceeded {
			t.Fatalf("jobs = %+v", jobs)
		}
		if n := e.updates()[site.ID]; n != 0 {
			t.Errorf("UpdateSite called %d times for a site without containers", n)
		}
		if got := e.site(t, site.ID); got.Status != store.SiteRunning {
			t.Errorf("status = %s", got.Status)
		}
	})
}

func TestReconcileFailureKeepsStatusAndRecordsError(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		site := e.running(t, "Stubborn")
		e.prov.Err = func(op, _ string) error {
			if op == "UpdateSite" {
				return errors.New("nginx container unhealthy")
			}
			return nil
		}
		if _, err := e.svc.EnqueueReconcile(ctx); err != nil {
			t.Fatal(err)
		}
		e.runJobs(t)
		if jobs := e.reconcileJobs(t); len(jobs) != 1 || jobs[0].State != store.JobFailed {
			t.Fatalf("jobs = %+v", jobs)
		}
		// The containers still run, so only the message changes.
		got := e.site(t, site.ID)
		if got.Status != store.SiteRunning || !strings.Contains(got.StatusMessage, "applying the platform configuration failed") ||
			!strings.Contains(got.StatusMessage, "nginx container unhealthy") {
			t.Errorf("site = %s %q", got.Status, got.StatusMessage)
		}
		// The site can still be managed.
		e.prov.Err = nil
		if _, err := e.svc.Stop(ctx, site.ID, ""); err != nil {
			t.Errorf("stop after a failed reconciliation: %v", err)
		}
	})
}

// Sites provisioned before the object cache existed get their user at
// startup, and it reaches their containers.
func TestReconcileCreatesObjectCacheUsers(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		site := e.running(t, "Old")
		failed := e.running(t, "Failed")
		e.setStatus(t, failed.ID, store.SiteFailed)
		for _, id := range []string{site.ID, failed.ID} {
			if err := e.cache.DeleteUser(ctx, id); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := e.svc.EnqueueReconcile(ctx); err != nil {
			t.Fatal(err)
		}
		e.runJobs(t)
		if !e.cache.HasUser(site.ID) {
			t.Error("running site got no object cache user")
		}
		if s, _ := e.prov.Site(site.ID); s.Config.RedisSecretRef != objectcachetest.SecretRef(site.ID) {
			t.Errorf("RedisSecretRef = %q", s.Config.RedisSecretRef)
		}
		if e.cache.HasUser(failed.ID) {
			t.Error("failed site got an object cache user")
		}

		// If the user can't be created, the containers are left alone and
		// the error is recorded.
		if err := e.cache.DeleteUser(ctx, site.ID); err != nil {
			t.Fatal(err)
		}
		e.cache.Err = func(op, _ string) error {
			if op == "CreateUser" {
				return errors.New("object cache unreachable")
			}
			return nil
		}
		before := e.updates()[site.ID]
		if _, err := e.svc.EnqueueReconcile(ctx); err != nil {
			t.Fatal(err)
		}
		e.runJobs(t)
		if n := e.updates()[site.ID] - before; n != 0 {
			t.Errorf("UpdateSite called %d times without an object cache user", n)
		}
		if got := e.site(t, site.ID); got.Status != store.SiteRunning || !strings.Contains(got.StatusMessage, "object cache unreachable") {
			t.Errorf("site = %s %q", got.Status, got.StatusMessage)
		}
	})
}
