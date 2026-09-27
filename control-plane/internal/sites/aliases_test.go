package sites_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/rchouinard/pressfleet/control-plane/internal/sites"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
)

// TestSetAliases covers alias management (#46): aliases of a live site
// change with a job that only reroutes them, new aliases get the DNS
// check, and a failed change restores the previous aliases.
func TestSetAliases(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e, dns := newGoLiveEnv(t, db)
		site := e.running(t, "Acme")

		// Not live yet.
		if _, _, _, err := e.svc.SetAliases(ctx, site.ID, sites.SetAliases{Aliases: []string{"acme.example"}}); !errors.Is(err, sites.ErrNotLive) {
			t.Errorf("before go-live: %v", err)
		}
		e.goLive(t, site.ID, sites.GoLive{PrimaryDomain: "www.acme.example", Aliases: []string{"acme.example"}})
		dns.calls()
		e.wp.pluginCalls(site.ID)
		flushes := e.cache.Flushes(site.ID)
		live, _ := e.prov.Site(site.ID)
		purges := live.CachePurges

		// Add one, keep one: only the new one is checked.
		saved, aliases, job, err := e.svc.SetAliases(ctx, site.ID, sites.SetAliases{Aliases: []string{"Shop.Acme.Example", "acme.example"}})
		if err != nil {
			t.Fatal(err)
		}
		if saved.Status != store.SiteUpdating || job.Type != sites.AliasesJobType || !slices.Equal(aliases, []string{"acme.example", "shop.acme.example"}) {
			t.Errorf("saved %+v, aliases %v, job %s", saved, aliases, job.Type)
		}
		if got := dns.calls(); len(got) != 1 || !slices.Equal(got[0], []string{"shop.acme.example"}) {
			t.Errorf("DNS checks = %q", got)
		}
		e.runJobs(t)
		if d := e.job(t, job.ID); d.Job.State != store.JobSucceeded {
			t.Fatalf("job %s: %s", d.Job.State, d.Job.Error)
		}
		live, _ = e.prov.Site(site.ID)
		if !slices.Equal(live.Config.Aliases, []string{"acme.example", "shop.acme.example"}) || live.Config.PrimaryDomain != "www.acme.example" {
			t.Errorf("provisioner config = %+v", live.Config)
		}
		if got := e.site(t, site.ID); got.Status != store.SiteRunning || got.PrimaryDomain != "www.acme.example" {
			t.Errorf("site = %+v", got)
		}
		// WordPress and the caches weren't touched.
		if calls := e.wp.pluginCalls(site.ID); len(calls) != 0 {
			t.Errorf("WP-CLI calls: %q", calls)
		}
		if e.cache.Flushes(site.ID) != flushes || live.CachePurges != purges {
			t.Error("an alias change purged caches")
		}

		// A new alias whose DNS isn't there yet is refused, changing
		// nothing; removing aliases needs no check.
		dns.set("blog.acme.example", "it does not resolve")
		_, _, _, err = e.svc.SetAliases(ctx, site.ID, sites.SetAliases{Aliases: []string{"blog.acme.example"}})
		var dnsErr *sites.DNSCheckError
		if !errors.As(err, &dnsErr) || len(dnsErr.Fields) != 1 || !strings.HasPrefix(dnsErr.Fields[0].Detail, "blog.acme.example") {
			t.Errorf("unresolved alias: %v", err)
		}
		if _, a, _ := e.svc.Get(ctx, site.ID); len(a) != 2 {
			t.Errorf("a refused change left aliases %v", a)
		}
		dns.calls()
		_, _, job, err = e.svc.SetAliases(ctx, site.ID, sites.SetAliases{Aliases: nil})
		if err != nil {
			t.Fatal(err)
		}
		e.runJobs(t)
		if got := dns.calls(); len(got) != 0 {
			t.Errorf("removing aliases checked DNS: %q", got)
		}
		if live, _ := e.prov.Site(site.ID); len(live.Config.Aliases) != 0 || e.job(t, job.ID).Job.State != store.JobSucceeded {
			t.Errorf("after removing: %+v", live.Config)
		}

		// A stopped site can change its aliases, and stays stopped.
		if _, err := e.svc.Stop(ctx, site.ID, ""); err != nil {
			t.Fatal(err)
		}
		e.runJobs(t)
		e.goLiveAliases(t, site.ID, []string{"acme.example"})
		if got := e.site(t, site.ID); got.Status != store.SiteStopped {
			t.Errorf("status after changing a stopped site's aliases = %s", got.Status)
		}
		if _, err := e.svc.Start(ctx, site.ID, ""); err != nil {
			t.Fatal(err)
		}
		e.runJobs(t)

		// A job that fails for good puts the previous aliases back.
		e.prov.Err = func(op, _ string) error {
			if op == "UpdateSite" {
				return errors.New("traefik directory not writable")
			}
			return nil
		}
		_, _, job, err = e.svc.SetAliases(ctx, site.ID, sites.SetAliases{Aliases: []string{"shop.acme.example"}, SkipDNSCheck: true})
		if err != nil {
			t.Fatal(err)
		}
		e.runJobs(t)
		e.prov.Err = nil
		if d := e.job(t, job.ID); d.Job.State != store.JobFailed {
			t.Errorf("job %s", d.Job.State)
		}
		if _, a, _ := e.svc.Get(ctx, site.ID); !slices.Equal(a, []string{"acme.example"}) {
			t.Errorf("aliases after the failure = %v", a)
		}
		if got := e.site(t, site.ID); got.Status != store.SiteFailed || !strings.Contains(got.StatusMessage, "changing aliases failed") {
			// Reverting the routes failed too (UpdateSite always failed),
			// so the site is failed.
			t.Errorf("site after the failure = %s %q", got.Status, got.StatusMessage)
		}

		// Validation: an alias may not be the primary domain or another
		// site's.
		other := e.running(t, "Other")
		e.goLive(t, other.ID, sites.GoLive{PrimaryDomain: "other.example"})
		if _, err := store.NewSites(db).SetStatus(ctx, site.ID, store.SiteRunning, ""); err != nil {
			t.Fatal(err)
		}
		for _, bad := range [][]string{{"www.acme.example"}, {"other.example"}, {"x." + testPreviewBase}} {
			var ve *sites.ValidationError
			if _, _, _, err := e.svc.SetAliases(ctx, site.ID, sites.SetAliases{Aliases: bad, SkipDNSCheck: true}); !errors.As(err, &ve) {
				t.Errorf("SetAliases(%v): %v", bad, err)
			}
		}
	})
}

// goLiveAliases sets a site's aliases, failing the test unless the job
// succeeds.
func (e *env) goLiveAliases(t *testing.T, siteID string, aliases []string) {
	t.Helper()
	_, _, job, err := e.svc.SetAliases(context.Background(), siteID, sites.SetAliases{Aliases: aliases, SkipDNSCheck: true})
	if err != nil {
		t.Fatal(err)
	}
	e.runJobs(t)
	if d := e.job(t, job.ID); d.Job.State != store.JobSucceeded {
		t.Fatalf("aliases job %s: %s", d.Job.State, d.Job.Error)
	}
}
