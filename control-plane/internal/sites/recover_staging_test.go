package sites_test

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
	"github.com/rchouinard/pressfleet/control-plane/internal/sites"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
)

// A staging site with no snapshot of its own comes back from its production
// site's newest one, at its own preview URL, with a warning; one with a
// snapshot of its own uses that; one whose production site has none fails
// like any site without a backup. Production sites recover as before.
func TestRecoverStagingSites(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newRestoreEnv(t, db)
		prod := e.runningSite(t, "Prod", "post=old")
		prodURL := "https://" + prod.PreviewDomain
		e.snapshot(t, prod.ID)
		e.dbs.SetData(prod.ID, []byte(strings.Replace(e.data(prod.ID), "post=old", "post=new\nlink="+prodURL+"/about", 1)))
		newest := e.snapshot(t, prod.ID)

		bare := e.stagingOf(t, e.runningSite(t, "Bare", "post=stale"), prod, true)
		own := e.stagingOf(t, e.runningSite(t, "Own", "post=own"), prod, true)
		ownSnap := e.snapshot(t, own.ID)

		prod2 := e.runningSite(t, "Prod2", "post=nothing")
		orphan := e.stagingOf(t, e.runningSite(t, "Orphan", "post=nothing"), prod2, true)

		all := []store.Site{prod, bare, own, prod2, orphan}
		var ids []string
		for _, s := range all {
			ids = append(ids, s.ID)
		}
		e.loseHost(t, ids...)
		if rec := e.prepareRecovery(t); len(rec.Recovering) != len(all) {
			t.Fatalf("recovering %v, want %d sites", rec.Recovering, len(all))
		}
		e.runJobs(t)

		// The staging site with no snapshot of its own.
		d := e.recoverJob(t, bare.ID)
		if d.Job.State != store.JobSucceeded {
			t.Fatalf("recovering the bare staging site: %s %s\n%s", d.Job.State, d.Job.Error, jobLog(d))
		}
		bareURL := "https://" + bare.PreviewDomain
		want := strings.ReplaceAll(e.data(prod.ID), "//"+prod.PreviewDomain, "//"+bare.PreviewDomain)
		if got := e.data(bare.ID); got != want || !strings.Contains(got, "link="+bareURL+"/about") || !strings.Contains(got, "home="+bareURL+"\n") {
			t.Errorf("bare staging database = %q, want %q", got, want)
		}
		if got := e.site(t, bare.ID); got.Status != store.SiteRunning || got.StatusMessage != "" {
			t.Errorf("bare staging site: %s %q", got.Status, got.StatusMessage)
		}
		if rt, ok := e.prov.Site(bare.ID); !ok || rt.Config.Environment != provisioner.EnvironmentStaging || !rt.Config.DisableMail {
			t.Errorf("rendered bare staging site: %v %+v", ok, rt.Config)
		}
		var warned bool
		for _, l := range d.Logs {
			warned = warned || l.Level == "warn" && strings.Contains(l.Message, prod.ID)
		}
		if !warned {
			t.Errorf("no warning naming the production site in\n%s", jobLog(d))
		}
		var restored []string
		for _, c := range e.engine.Calls() {
			if c.Op == "Restore" && c.SiteID == prod.ID && c.Snapshot == newest.ID {
				restored = append(restored, string(c.Snapshot))
			}
		}
		if !slices.Equal(restored, []string{string(newest.ID), string(newest.ID)}) {
			t.Errorf("restores of production's newest snapshot = %q, want one into production and one into the staging site", restored)
		}

		// The one with its own snapshot: that snapshot, as is, no warning.
		d = e.recoverJob(t, own.ID)
		if d.Job.State != store.JobSucceeded {
			t.Fatalf("recovering the staging site with a snapshot: %s %s\n%s", d.Job.State, d.Job.Error, jobLog(d))
		}
		if got, wantOwn := e.data(own.ID), string(e.engine.data[ownSnap.ID]); got != wantOwn {
			t.Errorf("own staging database = %q, want the snapshot's %q", got, wantOwn)
		}
		for _, l := range d.Logs {
			if l.Level == "warn" {
				t.Errorf("unexpected warning %q", l.Message)
			}
		}
		if calls := e.wpCalls(own.ID, "search-replace"); len(calls) != 0 {
			t.Errorf("own snapshot was rewritten: %q", calls)
		}

		// Production recovers from its own newest snapshot, URLs untouched.
		if d := e.recoverJob(t, prod.ID); d.Job.State != store.JobSucceeded || !strings.Contains(e.data(prod.ID), "link="+prodURL+"/about") {
			t.Errorf("production: %s %s, database %q", d.Job.State, d.Job.Error, e.data(prod.ID))
		}

		// Neither site has a snapshot.
		d = e.recoverJob(t, orphan.ID)
		if got := e.site(t, orphan.ID); d.Job.State != store.JobFailed || got.Status != store.SiteFailed ||
			!strings.Contains(got.StatusMessage, "production site has a backup") {
			t.Errorf("orphan: job %s %q, site %s %q", d.Job.State, d.Job.Error, got.Status, got.StatusMessage)
		}
	})
}

// The staging site recovers while its production site hasn't: the snapshot
// comes from the repository, not from the production site's containers,
// status, or job.
func TestRecoverStagingSiteBeforeProduction(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newRestoreEnv(t, db)
		prod := e.runningSite(t, "Prod", "post=hello")
		e.snapshot(t, prod.ID)
		stg := e.stagingOf(t, e.runningSite(t, "Stg", ""), prod, true)
		e.loseHost(t, prod.ID, stg.ID)
		e.prepareRecovery(t)
		// Production's job waits; the site stays updating, with no
		// containers.
		if _, err := db.ExecContext(context.Background(),
			"UPDATE jobs SET run_after = ? WHERE target_id = ? AND type = ?", time.Now().Add(time.Hour), prod.ID, sites.RecoverJobType); err != nil {
			t.Fatal(err)
		}
		e.runJobs(t)
		if d := e.recoverJob(t, stg.ID); d.Job.State != store.JobSucceeded {
			t.Fatalf("recovery: %s %s\n%s", d.Job.State, d.Job.Error, jobLog(d))
		}
		if got := e.site(t, prod.ID); got.Status != store.SiteUpdating {
			t.Errorf("production is %s, want updating", got.Status)
		}
		if _, ok := e.prov.Site(prod.ID); ok {
			t.Error("production has containers")
		}
		if !strings.Contains(e.data(stg.ID), "post=hello") {
			t.Errorf("staging database = %q", e.data(stg.ID))
		}
	})
}
