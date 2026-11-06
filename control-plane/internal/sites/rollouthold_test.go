package sites_test

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
	"github.com/rchouinard/pressfleet/control-plane/internal/sites"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
)

// holdSwitch is a RolloutHold the test turns on and off, as the CLI does by
// writing the upgrade record.
type holdSwitch struct{ on atomic.Bool }

func (h *holdSwitch) hold() (string, bool) {
	if h.on.Load() {
		return "the upgrade to 1.2.0 failed and hasn't been retried or rolled back", true
	}
	return "", false
}

func countJobs(t *testing.T, db *store.DB, types ...string) int {
	t.Helper()
	page, err := store.NewJobs(db).List(context.Background(), store.JobFilter{Types: types})
	if err != nil {
		t.Fatal(err)
	}
	return len(page.Jobs)
}

// While a hold is on, the core and templates rollouts queue no site job; a
// user-requested update and the startup reconcile run as usual; and when the
// hold lifts, the rollouts resume without recreating the service.
func TestRolloutHoldCoreAndTemplates(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		var h holdSwitch
		h.on.Store(true)
		e := newTemplateEnv(t, db, func(o *sites.Options) { o.RolloutHold = h.hold })
		a := e.siteOn(t, "A", "7.1.2")
		b := e.siteOn(t, "B", "7.1.2")
		// Both rollouts have work: a security release for core, and new
		// templates.
		e.tmpl.SetFingerprint(newTemplates)

		for range 2 {
			e.rollout(t)
		}
		if n := countJobs(t, db, sites.CoreUpdateJobType, sites.TemplateUpdateJobType); n != 0 {
			t.Fatalf("%d site update jobs queued while held", n)
		}
		e.checkCore(t, a.ID, "7.1.2")
		e.checkTemplates(t, a.ID, oldTemplates)
		for _, name := range []string{sites.CoreRolloutName, sites.TemplateRolloutName, sites.ImageRolloutName} {
			ro, err := e.svc.Rollouts().Rollout(context.Background(), name)
			if err != nil {
				t.Fatal(err)
			}
			if !ro.Held || ro.HoldReason == "" {
				t.Errorf("%s rollout = Held %v, %q; want held with a reason", name, ro.Held, ro.HoldReason)
			}
		}

		// The startup reconcile runs as usual: it re-renders the site with
		// the current templates only when its rollout reaches it, so what
		// it does here is queue and finish its own jobs.
		e.reconcile(t)
		if n := countJobs(t, db, sites.TemplateUpdateJobType); n != 0 {
			t.Errorf("the reconcile queued %d template updates", n)
		}

		// A user-requested core update runs.
		if _, _, err := e.svc.Rollouts().UpdateCore(context.Background(), b.ID, ""); err != nil {
			t.Fatalf("UpdateCore while held: %v", err)
		}
		e.runJobs(t)
		e.checkCore(t, b.ID, "7.1.4")

		// The upgrade is confirmed: the next passes roll out.
		// A site is in one update at a time, so a site the templates
		// rollout took first waits for the next scheduled pass of the
		// core rollout; passes stand in for the schedule.
		h.on.Store(false)
		for range 3 {
			e.rollout(t)
		}
		e.checkCore(t, a.ID, "7.1.4")
		e.checkTemplates(t, a.ID, newTemplates)
		e.checkTemplates(t, b.ID, newTemplates)
		for _, name := range []string{sites.CoreRolloutName, sites.TemplateRolloutName} {
			if ro, err := e.svc.Rollouts().Rollout(context.Background(), name); err != nil || ro.Held {
				t.Errorf("%s rollout after the hold lifted = Held %v, %v", name, ro.Held, err)
			}
		}
	})
}

// The image rollout is held too.
func TestRolloutHoldImages(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		var h holdSwitch
		h.on.Store(true)
		e := newImageEnv(t, db, func(o *sites.Options) { o.RolloutHold = h.hold })
		id := e.siteOn(t, "A", "7.2.1").ID
		oldPHP, oldNginx := e.runs(id)[provisioner.ComponentPHP], e.runs(id)[provisioner.ComponentNginx]
		newPHP, newNginx := build(e.tags.PHP, 2), build(e.tags.Nginx, 2)
		e.prov.SetLocalImage(e.tags.PHP, newPHP)
		e.prov.SetLocalImage(e.tags.Nginx, newNginx)

		e.rollout(t)
		if n := len(e.imageJobs(t, id)); n != 0 {
			t.Fatalf("%d image update jobs while held", n)
		}
		e.checkRuns(t, id, oldPHP, oldNginx)
		if ro := e.rolloutState(t); !ro.Held {
			t.Errorf("rollout = %+v, want held", ro)
		}

		h.on.Store(false)
		e.rollout(t)
		e.checkRuns(t, id, newPHP, newNginx)
	})
}

// Without a hold function, as in every install that never upgraded,
// rollouts run.
func TestRolloutNoHold(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newCoreEnv(t, db)
		e.siteOn(t, "A", "7.1.2")
		if _, held := e.svc.Rollouts().Hold(); held {
			t.Fatal("held without a hold function")
		}
		e.rollout(t)
		if n := countJobs(t, db, sites.CoreUpdateJobType); n != 1 {
			t.Errorf("%d core update jobs, want 1", n)
		}
	})
}
