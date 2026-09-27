package sites_test

import (
	"context"
	"errors"
	"testing"

	"github.com/rchouinard/pressfleet/control-plane/internal/sites"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
)

// TestMaintenance covers maintenance mode (#44): the flag is recorded and
// applied by a job, kept by updates, and put back when the job fails.
func TestMaintenance(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		site := e.running(t, "Closed")
		inMaintenance := func() bool {
			t.Helper()
			s, _ := e.prov.Site(site.ID)
			return s.Maintenance
		}

		saved, job, err := e.svc.SetMaintenance(ctx, site.ID, true, "")
		if err != nil {
			t.Fatal(err)
		}
		if !saved.Maintenance || saved.Status != store.SiteRunning || job.Type != sites.MaintenanceJobType {
			t.Errorf("saved %+v, job %s", saved, job.Type)
		}
		e.runJobs(t)
		if d := e.job(t, job.ID); d.Job.State != store.JobSucceeded || !inMaintenance() {
			t.Fatalf("job %s (%s); in maintenance = %v", d.Job.State, d.Job.Error, inMaintenance())
		}

		// Updates keep it: SiteConfig carries the flag.
		if _, _, _, err := e.svc.Update(ctx, site.ID, sites.SiteUpdate{CPU: ptr("2")}); err != nil {
			t.Fatal(err)
		}
		e.runJobs(t)
		if s, _ := e.prov.Site(site.ID); !s.Config.Maintenance || !inMaintenance() {
			t.Error("an update took the site out of maintenance mode")
		}

		// Toggles converge on the last one.
		if _, _, err := e.svc.SetMaintenance(ctx, site.ID, false, ""); err != nil {
			t.Fatal(err)
		}
		if _, _, err := e.svc.SetMaintenance(ctx, site.ID, true, ""); err != nil {
			t.Fatal(err)
		}
		if _, _, err := e.svc.SetMaintenance(ctx, site.ID, false, ""); err != nil {
			t.Fatal(err)
		}
		e.runJobs(t)
		if inMaintenance() || e.site(t, site.ID).Maintenance {
			t.Error("the last toggle (off) didn't win")
		}

		// A job that fails for good puts the flag back.
		e.prov.Err = func(op, _ string) error {
			if op == "SetMaintenance" {
				return errors.New("traefik directory not writable")
			}
			return nil
		}
		_, job, err = e.svc.SetMaintenance(ctx, site.ID, true, "")
		if err != nil {
			t.Fatal(err)
		}
		e.runJobs(t)
		if d := e.job(t, job.ID); d.Job.State != store.JobFailed || e.site(t, site.ID).Maintenance {
			t.Errorf("failed job %s; maintenance flag %v", d.Job.State, e.site(t, site.ID).Maintenance)
		}
		e.prov.Err = nil

		// Busy and unknown sites.
		if _, _, err := e.svc.SetMaintenance(ctx, "01j9zq3v6t8k4m2n5p7r9s1x3y", true, ""); !errors.Is(err, sites.ErrNotFound) {
			t.Errorf("missing site: %v", err)
		}
		if _, err := store.NewSites(db).SetStatus(ctx, site.ID, store.SiteUpdating, ""); err != nil {
			t.Fatal(err)
		}
		if _, _, err := e.svc.SetMaintenance(ctx, site.ID, true, ""); !errors.Is(err, sites.ErrBusy) {
			t.Errorf("busy site: %v", err)
		}
	})
}
