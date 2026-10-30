package sites_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
	"github.com/rchouinard/pressfleet/control-plane/internal/sites"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
)

// stagingOf makes site a staging site of parent. Nothing creates staging
// sites yet (#202), so tests link two provisioned sites directly.
func (e *env) stagingOf(t *testing.T, site, parent store.Site, mailDisabled bool) store.Site {
	t.Helper()
	if _, err := e.db.ExecContext(context.Background(),
		"UPDATE sites SET parent_site_id = ?, mail_disabled = ? WHERE id = ?", parent.ID, mailDisabled, site.ID); err != nil {
		t.Fatal(err)
	}
	return e.site(t, site.ID)
}

func TestSiteConfigEnvironment(t *testing.T) {
	for _, tc := range []struct {
		name string
		site store.Site
		env  provisioner.Environment
		mail bool
	}{
		{"production", store.Site{}, provisioner.EnvironmentProduction, false},
		{"staging", store.Site{ParentSiteID: "p", MailDisabled: true}, provisioner.EnvironmentStaging, true},
		{"staging with mail", store.Site{ParentSiteID: "p"}, provisioner.EnvironmentStaging, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := sites.SiteConfig(tc.site, nil, "", "")
			if cfg.Environment != tc.env || cfg.DisableMail != tc.mail {
				t.Errorf("Environment = %q, DisableMail = %v; want %q, %v", cfg.Environment, cfg.DisableMail, tc.env, tc.mail)
			}
		})
	}
}

func TestStagingOutboundMail(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		prod := e.running(t, "Production")
		stg := e.stagingOf(t, e.running(t, "Staging"), prod, true)

		// A production site has no mail switch.
		_, _, _, err := e.svc.Update(ctx, prod.ID, sites.SiteUpdate{OutboundMail: ptr(true)})
		var ve *sites.ValidationError
		if !errors.As(err, &ve) || len(ve.Fields) != 1 || ve.Fields[0].Field != "/outbound_mail" {
			t.Fatalf("production: err = %v, want a validation error on /outbound_mail", err)
		}
		if got := e.site(t, prod.ID); got.Status != store.SiteRunning || got.MailDisabled {
			t.Errorf("production after refusal = %+v", got)
		}

		// Turning it on re-renders the site through the update job.
		saved, job, _, err := e.svc.Update(ctx, stg.ID, sites.SiteUpdate{OutboundMail: ptr(true)})
		if err != nil {
			t.Fatal(err)
		}
		if saved.MailDisabled || saved.Status != store.SiteUpdating || job.Type != sites.UpdateJobType {
			t.Errorf("saved = %+v, job = %+v", saved, job)
		}
		e.runJobs(t)
		if d := e.job(t, job.ID); d.Job.State != store.JobSucceeded {
			t.Fatalf("job %s: %s", d.Job.State, d.Job.Error)
		}
		rendered, _ := e.prov.Site(stg.ID)
		if rendered.Config.Environment != provisioner.EnvironmentStaging || rendered.Config.DisableMail {
			t.Errorf("rendered with Environment %q, DisableMail %v", rendered.Config.Environment, rendered.Config.DisableMail)
		}

		// And off again.
		if _, _, _, err := e.svc.Update(ctx, stg.ID, sites.SiteUpdate{OutboundMail: ptr(false)}); err != nil {
			t.Fatal(err)
		}
		e.runJobs(t)
		rendered, _ = e.prov.Site(stg.ID)
		if got := e.site(t, stg.ID); !got.MailDisabled || got.Status != store.SiteRunning || !rendered.Config.DisableMail {
			t.Errorf("after turning mail off: site = %+v, DisableMail = %v", got, rendered.Config.DisableMail)
		}
	})
}

func TestStagingSitesRefuseDomains(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		stg := e.stagingOf(t, e.running(t, "Staging"), e.running(t, "Production"), true)

		_, _, _, err := e.svc.GoLive(ctx, stg.ID, sites.GoLive{PrimaryDomain: "www.acme.example", SkipDNSCheck: true})
		var ve *sites.ValidationError
		if !errors.As(err, &ve) || ve.Fields[0].Field != "/primary_domain" {
			t.Errorf("GoLive: err = %v, want a validation error", err)
		}
		_, _, _, err = e.svc.SetAliases(ctx, stg.ID, sites.SetAliases{Aliases: []string{"acme.example"}, SkipDNSCheck: true})
		if !errors.As(err, &ve) || ve.Fields[0].Field != "/aliases" {
			t.Errorf("SetAliases: err = %v, want a validation error", err)
		}
		if got := e.site(t, stg.ID); got.Status != store.SiteRunning || got.PrimaryDomain != "" {
			t.Errorf("site after refusals = %+v", got)
		}
		if n := e.count(t, "SELECT COUNT(*) FROM jobs WHERE type IN (?, ?)", sites.GoLiveJobType, sites.AliasesJobType); n != 0 {
			t.Errorf("%d domain jobs queued", n)
		}
	})
}

func TestDeleteProductionSiteWithStagingSites(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		prod := e.running(t, "Production")
		stg := e.stagingOf(t, e.running(t, "Staging"), prod, true)
		jobsBefore := e.count(t, "SELECT COUNT(*) FROM jobs")

		// Refused before anything changes, naming the staging site.
		_, err := e.svc.Delete(ctx, prod.ID, "", sites.DeleteOptions{})
		var se *sites.StagingSitesError
		if !errors.Is(err, sites.ErrHasStagingSites) || !errors.As(err, &se) || len(se.Sites) != 1 || se.Sites[0].ID != stg.ID {
			t.Fatalf("err = %v, want ErrHasStagingSites naming the staging site", err)
		}
		if !strings.Contains(err.Error(), stg.Slug) {
			t.Errorf("error %q doesn't name %s", err, stg.Slug)
		}
		if got := e.site(t, prod.ID); got.Status != store.SiteRunning {
			t.Errorf("production status = %s, want running", got.Status)
		}
		if n := e.count(t, "SELECT COUNT(*) FROM jobs"); n != jobsBefore {
			t.Errorf("%d jobs queued by a refused deletion", n-jobsBefore)
		}
		if _, ok := e.prov.Site(prod.ID); !ok {
			t.Error("the production stack was removed")
		}

		// The staging site deletes on its own.
		_, err = e.svc.Delete(ctx, stg.ID, "", sites.DeleteOptions{})
		if err != nil {
			t.Fatal(err)
		}

		// While it is being deleted, the production site can be queued
		// behind it; the foreign key still guards the last step.
		if _, err := e.svc.Delete(ctx, prod.ID, "", sites.DeleteOptions{}); err != nil {
			t.Fatalf("delete with the staging site deleting: %v", err)
		}
	})
}

func TestDeleteProductionSiteAfterStagingSitesAreGone(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		prod := e.running(t, "Production")
		stg := e.stagingOf(t, e.running(t, "Staging"), prod, true)
		if _, err := e.svc.Delete(ctx, stg.ID, "", sites.DeleteOptions{}); err != nil {
			t.Fatal(err)
		}
		e.runJobs(t)
		if _, err := store.NewSites(db).Get(ctx, stg.ID); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("staging site after deletion: %v", err)
		}
		if _, err := e.svc.Delete(ctx, prod.ID, "", sites.DeleteOptions{}); err != nil {
			t.Fatal(err)
		}
		e.runJobs(t)
		if got, err := store.NewSites(db).Get(ctx, prod.ID); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("production site after deletion: %+v, %v", got, err)
		}
	})
}
