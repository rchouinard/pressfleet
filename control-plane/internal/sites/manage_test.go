package sites_test

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
	"github.com/rchouinard/pressfleet/control-plane/internal/sites"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
)

func ptr[T any](v T) *T { return &v }

// running creates a site and runs its provisioning.
func (e *env) running(t *testing.T, name string) store.Site {
	t.Helper()
	site, _ := e.create(t, name)
	e.runJobs(t)
	if got := e.site(t, site.ID); got.Status != store.SiteRunning {
		t.Fatalf("site after provisioning = %+v", got)
	}
	return e.site(t, site.ID)
}

func TestList(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		a, _ := e.create(t, "Alpha")
		b, _ := e.create(t, "Beta")
		now := time.Now()
		if _, err := db.ExecContext(ctx, "INSERT INTO site_aliases (site_id, domain, created_at) VALUES (?, ?, ?), (?, ?, ?)",
			a.ID, "www.alpha.example", now, a.ID, "alpha.example", now); err != nil {
			t.Fatal(err)
		}
		page, aliases, err := e.svc.List(ctx, store.SiteFilter{ClientGroupID: e.group})
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Sites) != 2 || page.NextCursor != "" {
			t.Fatalf("page = %+v", page)
		}
		if got := strings.Join(aliases[a.ID], ","); got != "alpha.example,www.alpha.example" || len(aliases[b.ID]) != 0 {
			t.Errorf("aliases = %v", aliases)
		}
		// Listing never asks the runtime.
		for _, c := range e.prov.Calls() {
			if c.Op == "GetSite" {
				t.Errorf("List called GetSite")
			}
		}
	})
}

func TestRuntime(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		site := e.running(t, "Live")
		if rt := e.svc.Runtime(ctx, site.ID); rt.Phase != sites.RuntimePhase(provisioner.SiteRunning) {
			t.Errorf("running site: %+v", rt)
		}
		if rt := e.svc.Runtime(ctx, "no-such-site"); rt.Phase != sites.RuntimeAbsent {
			t.Errorf("missing stack: %+v", rt)
		}
		e.prov.Err = func(string, string) error { return errors.New("docker: permission denied on /var/run/docker.sock") }
		rt := e.svc.Runtime(ctx, site.ID)
		if rt.Phase != sites.RuntimeUnknown || rt.Message == "" || strings.Contains(rt.Message, "docker.sock") {
			t.Errorf("unreachable runtime: %+v", rt)
		}
		e.prov.Err = func(string, string) error { return context.DeadlineExceeded }
		if rt := e.svc.Runtime(ctx, site.ID); rt.Phase != sites.RuntimeUnknown || !strings.Contains(rt.Message, "in time") {
			t.Errorf("slow runtime: %+v", rt)
		}
	})
}

func TestUpdate(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		site := e.running(t, "Upgrade")

		saved, job, err := e.svc.Update(ctx, site.ID, sites.SiteUpdate{PHPVersion: ptr("8.3"), CPU: ptr("2"), MemoryLimitMB: ptr(1024)})
		if err != nil {
			t.Fatal(err)
		}
		if saved.Status != store.SiteUpdating || saved.PHPVersion != "8.3" || saved.CPULimit != "2" || saved.MemoryLimitMB != 1024 ||
			saved.MemoryReservationMB != site.MemoryReservationMB {
			t.Errorf("saved = %+v", saved)
		}
		if job.Type != sites.UpdateJobType || job.LockKey != sites.LockKey(site.ID) || !strings.Contains(string(job.Payload), `"php_version":"8.4"`) {
			t.Errorf("job = %+v (payload %s)", job, job.Payload)
		}
		// One change at a time.
		if _, _, err := e.svc.Update(ctx, site.ID, sites.SiteUpdate{PHPVersion: ptr("8.4")}); !errors.Is(err, sites.ErrBusy) {
			t.Errorf("second update: err = %v, want ErrBusy", err)
		}
		if _, err := e.svc.Stop(ctx, site.ID, ""); !errors.Is(err, sites.ErrBusy) {
			t.Errorf("stop while updating: err = %v, want ErrBusy", err)
		}

		e.runJobs(t)
		if d := e.job(t, job.ID); d.Job.State != store.JobSucceeded {
			t.Fatalf("job %s: %s", d.Job.State, d.Job.Error)
		}
		if got := e.site(t, site.ID); got.Status != store.SiteRunning || got.StatusMessage != "" || got.PHPVersion != "8.3" {
			t.Errorf("site = %+v", got)
		}
		s, _ := e.prov.Site(site.ID)
		if s.Config.PHPVersion != "8.3" || s.Config.Resources != (provisioner.ResourceLimits{CPU: "2", MemoryReservationMB: site.MemoryReservationMB, MemoryLimitMB: 1024}) {
			t.Errorf("provisioner config = %+v", s.Config)
		}

		// A stopped site is updated and stays stopped.
		if _, err := e.svc.Stop(ctx, site.ID, ""); err != nil {
			t.Fatal(err)
		}
		e.runJobs(t)
		if _, _, err := e.svc.Update(ctx, site.ID, sites.SiteUpdate{PHPVersion: ptr("8.4")}); err != nil {
			t.Fatal(err)
		}
		e.runJobs(t)
		if got := e.site(t, site.ID); got.Status != store.SiteStopped || got.PHPVersion != "8.4" {
			t.Errorf("stopped site after update = %+v", got)
		}
		if s, _ := e.prov.Site(site.ID); s.Phase != provisioner.SiteStopped || s.Config.PHPVersion != "8.4" {
			t.Errorf("stopped stack after update = %+v", s)
		}
	})
}

func TestUpdateValidation(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		site := e.running(t, "Strict")
		for _, tt := range []struct {
			name  string
			req   sites.SiteUpdate
			field string
		}{
			{"unknown PHP", sites.SiteUpdate{PHPVersion: ptr("7.4")}, "/php_version"},
			{"EOL PHP", sites.SiteUpdate{PHPVersion: ptr("8.1")}, "/php_version"},
			{"too little CPU", sites.SiteUpdate{CPU: ptr("0.1")}, "/resources/cpu"},
			{"reservation over limit", sites.SiteUpdate{MemoryReservationMB: ptr(600)}, "/resources/memory_reservation_mb"},
		} {
			_, _, err := e.svc.Update(ctx, site.ID, tt.req)
			var ve *sites.ValidationError
			if !errors.As(err, &ve) || len(ve.Fields) != 1 || ve.Fields[0].Field != tt.field {
				t.Errorf("%s: err = %v", tt.name, err)
			}
		}
		if got := e.site(t, site.ID); got.Status != store.SiteRunning || got.Settings() != site.Settings() {
			t.Errorf("a rejected update changed the site: %+v", got)
		}

		// A site on a branch past end of life can still change resources.
		eol := site.Settings()
		eol.PHPVersion = "8.1"
		if _, err := store.NewSites(db).SetSettings(ctx, site.ID, eol); err != nil {
			t.Fatal(err)
		}
		if _, _, err := e.svc.Update(ctx, site.ID, sites.SiteUpdate{PHPVersion: ptr("8.1"), MemoryLimitMB: ptr(640)}); err != nil {
			t.Errorf("resources of an EOL site: %v", err)
		}

		if _, _, err := e.svc.Update(ctx, "01j9zq3v6t8k4m2n5p7r9s1x3y", sites.SiteUpdate{CPU: ptr("1")}); !errors.Is(err, sites.ErrNotFound) {
			t.Errorf("missing site: err = %v", err)
		}
	})
}

func TestManageRefusesSitesWithoutAStack(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		site, _ := e.create(t, "Pending")
		if _, _, err := e.svc.Update(ctx, site.ID, sites.SiteUpdate{CPU: ptr("2")}); !errors.Is(err, sites.ErrBusy) {
			t.Errorf("update while provisioning: err = %v, want ErrBusy", err)
		}
		if _, err := e.svc.Start(ctx, site.ID, ""); !errors.Is(err, sites.ErrBusy) {
			t.Errorf("start while provisioning: err = %v, want ErrBusy", err)
		}
		// Provisioning fails for good: the site is failed, without secrets.
		e.prov.Err = func(op, _ string) error {
			if op == "CreateSite" {
				return errors.New("no space left on device")
			}
			return nil
		}
		e.runJobs(t)
		if got := e.site(t, site.ID); got.Status != store.SiteFailed {
			t.Fatalf("site = %+v", got)
		}
		e.prov.Err = nil
		if _, _, err := e.svc.Update(ctx, site.ID, sites.SiteUpdate{CPU: ptr("2")}); !errors.Is(err, sites.ErrNotProvisioned) {
			t.Errorf("update after failed provisioning: err = %v", err)
		}
		if _, err := e.svc.Stop(ctx, site.ID, ""); !errors.Is(err, sites.ErrNotProvisioned) {
			t.Errorf("stop after failed provisioning: err = %v", err)
		}
	})
}

func TestUpdateFailureReverts(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		site := e.running(t, "Reverted")
		// Every attempt to apply the new settings fails; the revert works.
		var calls atomic.Int32
		e.prov.Err = func(op, _ string) error {
			if op == "UpdateSite" && calls.Add(1) <= store.DefaultMaxAttempts {
				return errors.New("php container unhealthy")
			}
			return nil
		}
		_, job, err := e.svc.Update(ctx, site.ID, sites.SiteUpdate{PHPVersion: ptr("8.3"), MemoryLimitMB: ptr(1024)})
		if err != nil {
			t.Fatal(err)
		}
		e.runJobs(t)
		if d := e.job(t, job.ID); d.Job.State != store.JobFailed {
			t.Fatalf("job %s: %s", d.Job.State, d.Job.Error)
		}
		got := e.site(t, site.ID)
		if got.Status != store.SiteRunning || got.Settings() != site.Settings() ||
			!strings.Contains(got.StatusMessage, "update failed and was reverted") || !strings.Contains(got.StatusMessage, "php container unhealthy") {
			t.Errorf("site = %+v", got)
		}
		if s, _ := e.prov.Site(site.ID); s.Config.PHPVersion != site.PHPVersion || s.Config.Resources.MemoryLimitMB != site.MemoryLimitMB {
			t.Errorf("provisioner config = %+v", s.Config)
		}
		// The site can be changed again.
		if _, _, err := e.svc.Update(ctx, site.ID, sites.SiteUpdate{PHPVersion: ptr("8.3")}); err != nil {
			t.Errorf("update after a reverted one: %v", err)
		}
	})
}

func TestUpdateRevertFailureMarksFailed(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		site := e.running(t, "Broken")
		e.prov.Err = func(op, _ string) error {
			if op == "UpdateSite" {
				return errors.New("docker daemon unavailable")
			}
			return nil
		}
		_, job, err := e.svc.Update(ctx, site.ID, sites.SiteUpdate{PHPVersion: ptr("8.3")})
		if err != nil {
			t.Fatal(err)
		}
		e.runJobs(t)
		// The rollback finishes, so the site isn't stuck updating.
		if d := e.job(t, job.ID); d.Job.State != store.JobFailed {
			t.Fatalf("job %s: %s", d.Job.State, d.Job.Error)
		}
		got := e.site(t, site.ID)
		if got.Status != store.SiteFailed || got.PHPVersion != site.PHPVersion || !strings.HasPrefix(got.StatusMessage, "update failed: ") {
			t.Errorf("site = %+v", got)
		}
		e.prov.Err = nil
		if _, _, err := e.svc.Update(ctx, site.ID, sites.SiteUpdate{PHPVersion: ptr("8.3")}); err != nil {
			t.Fatalf("retrying on a failed site: %v", err)
		}
		e.runJobs(t)
		if got := e.site(t, site.ID); got.Status != store.SiteRunning || got.PHPVersion != "8.3" || got.StatusMessage != "" {
			t.Errorf("site after retrying = %+v", got)
		}
	})
}

func TestDeleteWhileUpdateQueued(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		site := e.running(t, "Doomed")
		_, update, err := e.svc.Update(ctx, site.ID, sites.SiteUpdate{PHPVersion: ptr("8.3")})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := e.svc.Delete(ctx, site.ID, ""); err != nil {
			t.Fatal(err)
		}
		e.runJobs(t)
		if d := e.job(t, update.ID); d.Job.State != store.JobFailed {
			t.Errorf("update job %s: %s", d.Job.State, d.Job.Error)
		}
		for _, c := range e.prov.Calls() {
			if c.Op == "UpdateSite" {
				t.Error("the update ran on a site being deleted")
			}
		}
		e.assertGone(t, site.ID)
	})
}

func TestStartStop(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		site := e.running(t, "Power")

		job, err := e.svc.Stop(ctx, site.ID, "")
		if err != nil {
			t.Fatal(err)
		}
		if job.Type != sites.StopJobType || job.LockKey != sites.LockKey(site.ID) {
			t.Errorf("job = %+v", job)
		}
		// The status changes when the job has done its work.
		if got := e.site(t, site.ID); got.Status != store.SiteRunning {
			t.Errorf("status before the job ran = %s", got.Status)
		}
		e.runJobs(t)
		if got := e.site(t, site.ID); got.Status != store.SiteStopped {
			t.Errorf("status after stop = %s", got.Status)
		}
		if s, _ := e.prov.Site(site.ID); s.Phase != provisioner.SiteStopped {
			t.Errorf("phase after stop = %s", s.Phase)
		}
		// Stopping again converges.
		if _, err := e.svc.Stop(ctx, site.ID, ""); err != nil {
			t.Fatal(err)
		}
		e.runJobs(t)

		if _, err := e.svc.Start(ctx, site.ID, ""); err != nil {
			t.Fatal(err)
		}
		e.runJobs(t)
		if got := e.site(t, site.ID); got.Status != store.SiteRunning {
			t.Errorf("status after start = %s", got.Status)
		}

		// A start that keeps failing marks the site failed; starting it
		// again once the problem is gone recovers it.
		e.prov.Err = func(op, _ string) error {
			if op == "StartSite" {
				return errors.New("php is unhealthy")
			}
			return nil
		}
		job, err = e.svc.Start(ctx, site.ID, "")
		if err != nil {
			t.Fatal(err)
		}
		e.runJobs(t)
		if d := e.job(t, job.ID); d.Job.State != store.JobFailed {
			t.Errorf("job %s", d.Job.State)
		}
		if got := e.site(t, site.ID); got.Status != store.SiteFailed || !strings.Contains(got.StatusMessage, "starting failed") {
			t.Errorf("site = %+v", got)
		}
		e.prov.Err = nil
		if _, err := e.svc.Start(ctx, site.ID, ""); err != nil {
			t.Fatal(err)
		}
		e.runJobs(t)
		if got := e.site(t, site.ID); got.Status != store.SiteRunning || got.StatusMessage != "" {
			t.Errorf("site after recovering = %+v", got)
		}

		if _, err := e.svc.Start(ctx, "01j9zq3v6t8k4m2n5p7r9s1x3y", ""); !errors.Is(err, sites.ErrNotFound) {
			t.Errorf("missing site: err = %v", err)
		}
	})
}
