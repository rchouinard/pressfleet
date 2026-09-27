package sites_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/rchouinard/pressfleet/control-plane/internal/domains"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
	"github.com/rchouinard/pressfleet/control-plane/internal/sites"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
)

// fakeDNS is a DNS check whose answers tests change, as records
// propagating would: a domain in wrong fails, any other passes.
type fakeDNS struct {
	mu      sync.Mutex
	wrong   map[string]string // domain -> problem
	err     error
	checked [][]string
}

func (f *fakeDNS) Check(_ context.Context, names []string) ([]domains.Problem, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.checked = append(f.checked, slices.Clone(names))
	if f.err != nil {
		return nil, f.err
	}
	var out []domains.Problem
	for _, n := range names {
		if p, ok := f.wrong[n]; ok {
			out = append(out, domains.Problem{Domain: n, Detail: p})
		}
	}
	return out, nil
}

func (f *fakeDNS) set(domain, problem string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if problem == "" {
		delete(f.wrong, domain)
	} else {
		f.wrong[domain] = problem
	}
}

func (f *fakeDNS) calls() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.checked
	f.checked = nil
	return out
}

// newGoLiveEnv returns an env whose DNS check is dns, and whose dashboard
// domain is fleet.example.test.
func newGoLiveEnv(t *testing.T, db *store.DB) (*env, *fakeDNS) {
	t.Helper()
	dns := &fakeDNS{wrong: map[string]string{}}
	e := newEnvWith(t, db, testPreviewBase, func(o *sites.Options) {
		o.DNSChecker = dns
		o.DashboardDomain = "fleet.example.test"
	})
	return e, dns
}

// goLive requests a go-live and runs its job, failing the test unless it
// succeeds.
func (e *env) goLive(t *testing.T, siteID string, req sites.GoLive) store.JobDetail {
	t.Helper()
	_, _, job, err := e.svc.GoLive(context.Background(), siteID, req)
	if err != nil {
		t.Fatalf("GoLive(%+v): %v", req, err)
	}
	e.runJobs(t)
	d := e.job(t, job.ID)
	if d.Job.State != store.JobSucceeded {
		t.Fatalf("go-live job %s: %s\n%s", d.Job.State, d.Job.Error, jobLog(d))
	}
	return d
}

// rewrites returns the site's WP-CLI calls that go-live makes to rewrite
// its URLs, and forgets every call.
func (e *env) rewrites(siteID string) []string {
	var out []string
	for _, c := range e.wp.pluginCalls(siteID) {
		if strings.HasPrefix(c, "search-replace ") || strings.HasPrefix(c, "option update home") ||
			strings.HasPrefix(c, "option update siteurl") || c == "option get home" {
			out = append(out, c)
		}
	}
	return out
}

// TestGoLive covers the go-live workflow (#45, design doc §5.2): the
// domains are recorded, routed, WordPress's URLs rewritten, both caches
// purged, and the site is running on its primary domain; going live again
// moves it to another primary domain.
func TestGoLive(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e, dns := newGoLiveEnv(t, db)
		site := e.running(t, "Acme")
		e.wp.pluginCalls(site.ID)
		preview := site.PreviewDomain

		saved, aliases, job, err := e.svc.GoLive(ctx, site.ID, sites.GoLive{
			PrimaryDomain: " WWW.Acme.Example. ",
			Aliases:       []string{"shop.acme.example", "Acme.Example"},
		})
		if err != nil {
			t.Fatal(err)
		}
		if saved.PrimaryDomain != "www.acme.example" || saved.Status != store.SiteUpdating || job.Type != sites.GoLiveJobType ||
			!slices.Equal(aliases, []string{"acme.example", "shop.acme.example"}) || !slices.Equal(saved.URLRewriteFrom, []string{preview}) {
			t.Errorf("saved %+v, aliases %v, job %s", saved, aliases, job.Type)
		}
		if got := dns.calls(); len(got) != 1 || !slices.Equal(got[0], []string{"www.acme.example", "acme.example", "shop.acme.example"}) {
			t.Errorf("DNS checks = %q", got)
		}
		purges := e.cache.Flushes(site.ID)
		e.runJobs(t)
		d := e.job(t, job.ID)
		if d.Job.State != store.JobSucceeded {
			t.Fatalf("job %s: %s", d.Job.State, d.Job.Error)
		}
		var result map[string]string
		if err := json.Unmarshal(d.Job.Result, &result); err != nil || result["url"] != "https://www.acme.example" {
			t.Errorf("result = %s", d.Job.Result)
		}

		got := e.site(t, site.ID)
		if got.Status != store.SiteRunning || got.PrimaryDomain != "www.acme.example" || len(got.URLRewriteFrom) != 0 || sites.URL(got) != "https://www.acme.example" {
			t.Errorf("site after go-live = %+v", got)
		}
		if _, a, _ := e.svc.Get(ctx, site.ID); !slices.Equal(a, []string{"acme.example", "shop.acme.example"}) {
			t.Errorf("aliases = %v", a)
		}
		live, _ := e.prov.Site(site.ID)
		if live.Config.PrimaryDomain != "www.acme.example" || !slices.Equal(live.Config.Aliases, []string{"acme.example", "shop.acme.example"}) {
			t.Errorf("provisioner config = %+v", live.Config)
		}
		want := []string{
			"option get home",
			"search-replace //" + preview + " //www.acme.example --all-tables-with-prefix --skip-columns=guid --format=count",
			`search-replace \/\/` + preview + ` \/\/www.acme.example --all-tables-with-prefix --skip-columns=guid --format=count`,
			"option update home https://www.acme.example",
			"option update siteurl https://www.acme.example",
		}
		if got := e.rewrites(site.ID); !slices.Equal(got, want) {
			t.Errorf("rewrite calls:\n got %q\nwant %q", got, want)
		}
		// One flush between search-replace and the options, one to purge.
		if e.cache.Flushes(site.ID) != purges+2 || live.CachePurges != 1 {
			t.Errorf("object cache flushes %d (was %d), page cache purges %d", e.cache.Flushes(site.ID), purges, live.CachePurges)
		}

		// Updates and reconciliation keep the domains.
		if _, _, _, err := e.svc.Update(ctx, site.ID, sites.SiteUpdate{CPU: ptr("2")}); err != nil {
			t.Fatal(err)
		}
		if _, err := e.svc.EnqueueReconcile(ctx); err != nil {
			t.Fatal(err)
		}
		e.runJobs(t)
		if live, _ := e.prov.Site(site.ID); live.Config.PrimaryDomain != "www.acme.example" || len(live.Config.Aliases) != 2 {
			t.Errorf("an update or reconcile dropped the domains: %+v", live.Config)
		}

		// Moving to another primary domain rewrites the old one's URLs too,
		// and keeping the old one as an alias doesn't check its DNS again.
		e.wp.pluginCalls(site.ID)
		e.goLive(t, site.ID, sites.GoLive{PrimaryDomain: "acme.example", Aliases: []string{"www.acme.example"}})
		if got := dns.calls(); len(got) != 0 {
			t.Errorf("DNS checks of domains the site had = %q", got)
		}
		calls := e.rewrites(site.ID)
		for _, want := range []string{
			"search-replace //" + preview + " //acme.example --all-tables-with-prefix --skip-columns=guid --format=count",
			"search-replace //www.acme.example //acme.example --all-tables-with-prefix --skip-columns=guid --format=count",
			"option update home https://acme.example",
		} {
			if !slices.Contains(calls, want) {
				t.Errorf("calls lack %q:\n%q", want, calls)
			}
		}
		if live, _ := e.prov.Site(site.ID); live.Config.PrimaryDomain != "acme.example" || !slices.Equal(live.Config.Aliases, []string{"www.acme.example"}) {
			t.Errorf("after moving: %+v", live.Config)
		}
	})
}

// TestGoLiveDNSCheck covers the DNS check: a domain that doesn't resolve
// to this host fails the request with a clear error and changes nothing,
// and the same request succeeds once DNS has propagated (#45 acceptance:
// "retry after DNS failure succeeds").
func TestGoLiveDNSCheck(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e, dns := newGoLiveEnv(t, db)
		site := e.running(t, "Acme")
		jobsBefore := e.count(t, "SELECT COUNT(*) FROM jobs")

		dns.set("www.acme.example", "it resolves to 192.0.2.1, which is not this host (203.0.113.7).")
		req := sites.GoLive{PrimaryDomain: "www.acme.example", Aliases: []string{"acme.example"}}
		_, _, _, err := e.svc.GoLive(ctx, site.ID, req)
		var dnsErr *sites.DNSCheckError
		if !errors.As(err, &dnsErr) || len(dnsErr.Fields) != 1 || dnsErr.Fields[0].Field != "/primary_domain" ||
			!strings.Contains(dnsErr.Fields[0].Detail, "www.acme.example: it resolves to 192.0.2.1") {
			t.Fatalf("err = %v", err)
		}
		got := e.site(t, site.ID)
		if got.PrimaryDomain != "" || got.Status != store.SiteRunning || len(got.URLRewriteFrom) != 0 ||
			e.count(t, "SELECT COUNT(*) FROM jobs") != jobsBefore || e.count(t, "SELECT COUNT(*) FROM site_aliases") != 0 {
			t.Errorf("a failed DNS check left state behind: %+v", got)
		}

		// The record propagates; the same request now succeeds.
		dns.set("www.acme.example", "")
		e.goLive(t, site.ID, req)
		if got := e.site(t, site.ID); got.PrimaryDomain != "www.acme.example" || got.Status != store.SiteRunning {
			t.Errorf("after the retry: %+v", got)
		}

		// Without an address to compare with, the check can't run.
		dns.err = domains.ErrHostUnknown
		_, _, _, err = e.svc.GoLive(ctx, site.ID, sites.GoLive{PrimaryDomain: "new.acme.example"})
		if !errors.As(err, &dnsErr) || !strings.Contains(dnsErr.Detail, "public_ip") {
			t.Errorf("unknown host: %v", err)
		}
		// Skipping the check goes ahead (a host behind a CDN).
		dns.calls()
		e.goLive(t, site.ID, sites.GoLive{PrimaryDomain: "new.acme.example", SkipDNSCheck: true})
		if got := dns.calls(); len(got) != 0 {
			t.Errorf("a skipped check ran: %q", got)
		}
	})
}

func TestGoLiveValidation(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e, _ := newGoLiveEnv(t, db)
		site := e.running(t, "Acme")
		other := e.running(t, "Other")
		e.goLive(t, other.ID, sites.GoLive{PrimaryDomain: "other.example", Aliases: []string{"www.other.example"}})

		many := make([]string, sites.MaxAliases+1)
		for i := range many {
			many[i] = "a" + strings.Repeat("x", i) + ".acme.example"
		}
		for _, tc := range []struct {
			name  string
			req   sites.GoLive
			field string
		}{
			{"empty", sites.GoLive{}, "/primary_domain"},
			{"url", sites.GoLive{PrimaryDomain: "https://acme.example/"}, "/primary_domain"},
			{"ip", sites.GoLive{PrimaryDomain: "203.0.113.7"}, "/primary_domain"},
			{"wildcard alias", sites.GoLive{PrimaryDomain: "acme.example", Aliases: []string{"*.acme.example"}}, "/aliases/0"},
			{"preview base", sites.GoLive{PrimaryDomain: "acme." + testPreviewBase}, "/primary_domain"},
			{"own preview domain", sites.GoLive{PrimaryDomain: "acme.example", Aliases: []string{site.PreviewDomain}}, "/aliases/0"},
			{"dashboard", sites.GoLive{PrimaryDomain: "fleet.example.test"}, "/primary_domain"},
			{"alias is primary", sites.GoLive{PrimaryDomain: "acme.example", Aliases: []string{"ACME.example"}}, "/aliases/0"},
			{"alias twice", sites.GoLive{PrimaryDomain: "acme.example", Aliases: []string{"a.acme.example", "a.acme.example"}}, "/aliases/1"},
			{"too many aliases", sites.GoLive{PrimaryDomain: "acme.example", Aliases: many}, "/aliases"},
			{"another site's primary", sites.GoLive{PrimaryDomain: "other.example"}, "/primary_domain"},
			{"another site's alias", sites.GoLive{PrimaryDomain: "acme.example", Aliases: []string{"www.other.example"}}, "/aliases"},
		} {
			_, _, _, err := e.svc.GoLive(ctx, site.ID, tc.req)
			var ve *sites.ValidationError
			if !errors.As(err, &ve) || !slices.ContainsFunc(ve.Fields, func(f sites.FieldError) bool { return f.Field == tc.field }) {
				t.Errorf("%s: err = %v, want a problem with %s", tc.name, err, tc.field)
			}
		}
		if got := e.site(t, site.ID); got.PrimaryDomain != "" || got.Status != store.SiteRunning {
			t.Errorf("refused requests changed the site: %+v", got)
		}

		if _, _, _, err := e.svc.GoLive(ctx, "01j9zq3v6t8k4m2n5p7r9s1x3y", sites.GoLive{PrimaryDomain: "acme.example"}); !errors.Is(err, sites.ErrNotFound) {
			t.Errorf("missing site: %v", err)
		}
		if _, err := e.svc.Stop(ctx, site.ID, ""); err != nil {
			t.Fatal(err)
		}
		e.runJobs(t)
		if _, _, _, err := e.svc.GoLive(ctx, site.ID, sites.GoLive{PrimaryDomain: "acme.example"}); !errors.Is(err, sites.ErrNotRunning) {
			t.Errorf("stopped site: %v", err)
		}
		if _, err := store.NewSites(db).SetStatus(ctx, site.ID, store.SiteUpdating, ""); err != nil {
			t.Fatal(err)
		}
		if _, _, _, err := e.svc.GoLive(ctx, site.ID, sites.GoLive{PrimaryDomain: "acme.example"}); !errors.Is(err, sites.ErrBusy) {
			t.Errorf("busy site: %v", err)
		}
	})
}

// TestGoLiveRollback checks that a go-live failing before WordPress's
// URLs are touched is rolled back: the previous routes and domains are
// back, and the site runs on its preview domain.
func TestGoLiveRollback(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e, _ := newGoLiveEnv(t, db)
		site := e.running(t, "Acme")
		e.wp.failCommand = func(args []string) error {
			if slices.Equal(args, []string{"option", "get", "home"}) {
				return &provisioner.ExecError{ExitCode: 255, Stderr: "Error establishing a database connection"}
			}
			return nil
		}
		_, _, job, err := e.svc.GoLive(ctx, site.ID, sites.GoLive{PrimaryDomain: "www.acme.example", Aliases: []string{"acme.example"}})
		if err != nil {
			t.Fatal(err)
		}
		e.runJobs(t)
		d := e.job(t, job.ID)
		if d.Job.State != store.JobFailed {
			t.Fatalf("job %s: %s", d.Job.State, d.Job.Error)
		}
		got := e.site(t, site.ID)
		if got.PrimaryDomain != "" || got.Status != store.SiteRunning || len(got.URLRewriteFrom) != 0 ||
			!strings.Contains(got.StatusMessage, "going live failed and was reverted") {
			t.Errorf("site after the rollback = %+v", got)
		}
		if n := e.count(t, "SELECT COUNT(*) FROM site_aliases WHERE site_id = ?", site.ID); n != 0 {
			t.Errorf("%d aliases left", n)
		}
		if live, _ := e.prov.Site(site.ID); live.Config.PrimaryDomain != "" || len(live.Config.Aliases) != 0 {
			t.Errorf("routes after the rollback: %+v", live.Config)
		}
		for _, c := range e.wp.pluginCalls(site.ID) {
			if strings.HasPrefix(c, "search-replace") {
				t.Errorf("rolled back go-live rewrote URLs: %s", c)
			}
		}

		// Nothing stops a new attempt.
		e.wp.failCommand = nil
		e.goLive(t, site.ID, sites.GoLive{PrimaryDomain: "www.acme.example"})
	})
}

// TestGoLiveUnfinished checks a go-live that fails once the URL rewrite
// has started: it keeps the new domains and routes rather than reverting
// the database, says so, and going live again with the same domains
// finishes the rewrite.
func TestGoLiveUnfinished(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e, dns := newGoLiveEnv(t, db)
		site := e.running(t, "Acme")
		e.wp.failCommand = func(args []string) error {
			if len(args) > 0 && args[0] == "search-replace" && strings.HasPrefix(args[1], `\/\/`) {
				return &provisioner.ExecError{ExitCode: 1, Stderr: "Error: table wp_posts is corrupt"}
			}
			return nil
		}
		req := sites.GoLive{PrimaryDomain: "www.acme.example"}
		_, _, job, err := e.svc.GoLive(ctx, site.ID, req)
		if err != nil {
			t.Fatal(err)
		}
		e.runJobs(t)
		if d := e.job(t, job.ID); d.Job.State != store.JobFailed {
			t.Fatalf("job %s", d.Job.State)
		}
		got := e.site(t, site.ID)
		if got.PrimaryDomain != "www.acme.example" || got.Status != store.SiteRunning ||
			!slices.Equal(got.URLRewriteFrom, []string{site.PreviewDomain}) ||
			!strings.Contains(got.StatusMessage, "didn't finish") || !strings.Contains(got.StatusMessage, "Go live again with the same domains") {
			t.Errorf("site after the failed rewrite = %+v", got)
		}
		if live, _ := e.prov.Site(site.ID); live.Config.PrimaryDomain != "www.acme.example" {
			t.Errorf("the routes were reverted: %+v", live.Config)
		}

		// Going live again with the same domains finishes it, without a
		// DNS check of domains the site has.
		e.wp.failCommand = nil
		e.wp.pluginCalls(site.ID)
		dns.calls()
		e.goLive(t, site.ID, req)
		if got := dns.calls(); len(got) != 0 {
			t.Errorf("DNS checks = %q", got)
		}
		if !slices.Contains(e.rewrites(site.ID), `search-replace \/\/`+site.PreviewDomain+` \/\/www.acme.example --all-tables-with-prefix --skip-columns=guid --format=count`) {
			t.Error("the retry didn't rewrite the preview URLs")
		}
		if got := e.site(t, site.ID); got.Status != store.SiteRunning || got.StatusMessage != "" || len(got.URLRewriteFrom) != 0 {
			t.Errorf("after finishing: %+v", got)
		}
	})
}
