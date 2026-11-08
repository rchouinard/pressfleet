package kubernetes

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/rchouinard/pressfleet/control-plane/internal/ingress"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
)

// liveSite is a site that went live with two aliases and a protected
// preview domain.
func liveSite() *provisioner.SiteConfig {
	cfg := testSite()
	cfg.PrimaryDomain = "example.com"
	cfg.Aliases = []string{"www.example.com", "example.org"}
	cfg.PreviewAuth = &provisioner.BasicAuth{
		Username:     "client",
		PasswordHash: "$2y$10$abcdefghijklmnopqrstuuK0.8mQY1jQ5mVJm0Xx5r8oZ9c3w1y2K",
	}
	return cfg
}

// otherSiteID is a second site's ID.
const otherSiteID = "01hzy3k5q7m9c2v4b6n8r0t1wy"

func TestRoutingGolden(t *testing.T) {
	p := testProvisioner(t)
	dir, err := p.RenderSite(t.Context(), liveSite())
	if err != nil {
		t.Fatal(err)
	}
	routes, err := os.ReadFile(filepath.Join(dir, RoutesFile))
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "routes-live.json", routes)
	siteDir, _ := p.layout.SiteDir(testSiteID)
	maint, err := os.ReadFile(filepath.Join(siteDir, MaintenanceFile))
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "maintenance-live.json", maint)
	// Not a manifest: nothing of the routes goes to the API server.
	objs, err := LoadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range objs {
		if strings.Contains(o.GetAPIVersion(), "traefik") {
			t.Errorf("a Traefik object is among the manifests: %s %s", o.GetKind(), o.GetName())
		}
	}
}

// served returns what the provider serves Traefik, failing the test if it
// can't.
func served(t *testing.T, p *Provisioner) traefikConfig {
	t.Helper()
	c, err := p.servedRoutes()
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// routerNames lists the names of the routers in c whose name has the
// suffix (all of them for "").
func routerNames(c traefikConfig, suffix string) []string {
	var names []string
	for n := range c.HTTP.Routers {
		if strings.HasSuffix(n, suffix) {
			names = append(names, n)
		}
	}
	slices.Sort(names)
	return names
}

func sortedKeys[V any](m map[string]V) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

func TestSiteRouting(t *testing.T) {
	id := testSiteID
	name := func(s ...string) string { return ingress.SiteName(id, s...) }
	alias := func(a string) string { return name(ingress.SuffixAlias, ingress.AliasKey(a)) }
	const service = "http://nginx.pf-x.svc"

	tests := []struct {
		name     string
		site     func() *provisioner.SiteConfig
		hsts     bool
		routers  []string
		mw       []string
		previewM []string // the preview router's middlewares
		aliasM   []string // the first alias router's middlewares
	}{{
		name:     "preview only",
		site:     testSite,
		hsts:     true,
		routers:  []string{name("preview")},
		mw:       []string{name("noindex")},
		previewM: []string{name("noindex")},
	}, {
		name: "live with HSTS",
		site: liveSite,
		hsts: true,
		routers: []string{
			alias("example.org"), alias("www.example.com"), name("preview"), name("preview-redirect"), name("primary"),
		},
		mw: []string{
			name("alias-redirect"), name("hsts"), name("noindex"), name("preview-auth"), name("preview-redirect"),
		},
		previewM: []string{name("noindex"), name("preview-auth")},
		aliasM:   []string{name("hsts"), name("alias-redirect")},
	}, {
		name: "live without HSTS",
		site: liveSite,
		hsts: false,
		routers: []string{
			alias("example.org"), alias("www.example.com"), name("preview"), name("preview-redirect"), name("primary"),
		},
		mw: []string{
			name("alias-redirect"), name("noindex"), name("preview-auth"), name("preview-redirect"),
		},
		previewM: []string{name("noindex"), name("preview-auth")},
		aliasM:   []string{name("alias-redirect")},
	}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, m, err := siteRouting(tt.site(), tt.hsts, service)
			if err != nil {
				t.Fatal(err)
			}
			slices.Sort(tt.routers)
			if got := sortedKeys(r.HTTP.Routers); !slices.Equal(got, tt.routers) {
				t.Errorf("routers = %q, want %q", got, tt.routers)
			}
			if got := sortedKeys(r.HTTP.Middlewares); !slices.Equal(got, tt.mw) {
				t.Errorf("middlewares = %q, want %q", got, tt.mw)
			}
			if got := r.HTTP.Routers[name("preview")].Middlewares; !slices.Equal(got, tt.previewM) {
				t.Errorf("preview middlewares = %q, want %q", got, tt.previewM)
			}
			if tt.aliasM != nil {
				if got := r.HTTP.Routers[alias("www.example.com")].Middlewares; !slices.Equal(got, tt.aliasM) {
					t.Errorf("alias middlewares = %q, want %q", got, tt.aliasM)
				}
			}
			// One service, the site's nginx, under the name Traefik's
			// metrics are mapped to sites by.
			svc, ok := r.HTTP.Services[ingress.SiteService(id)]
			if len(r.HTTP.Services) != 1 || !ok || len(svc.LoadBalancer.Servers) != 1 || svc.LoadBalancer.Servers[0].URL != service {
				t.Errorf("services = %+v", r.HTTP.Services)
			}
			for n, rt := range r.HTTP.Routers {
				if rt.Service != ingress.SiteService(id) || !slices.Equal(rt.EntryPoints, []string{ingress.EntryPointHTTPS}) {
					t.Errorf("router %s = %+v", n, rt)
				}
			}
			if !r.owns(id) || !m.owns(id) {
				t.Error("the routes name something that isn't the site's")
			}
			// One maintenance twin per router, except the preview redirect,
			// which its twin shadows; twins win over everything.
			wantTwins := len(r.HTTP.Routers)
			if tt.site().PrimaryDomain != "" {
				wantTwins--
			}
			if len(m.HTTP.Routers) != wantTwins || len(m.HTTP.Middlewares) != 0 || len(m.HTTP.Services) != 0 {
				t.Fatalf("maintenance routes = %+v, want %d routers", m.HTTP, wantTwins)
			}
			for n, tw := range m.HTTP.Routers {
				orig, ok := r.HTTP.Routers[strings.TrimSuffix(n, "-maintenance")]
				if !ok || tw.Rule != orig.Rule || tw.TLS != orig.TLS || tw.Priority != ingress.MaintenancePriority ||
					tw.Service != "pf-maintenance@file" || len(tw.Middlewares) != 0 {
					t.Errorf("twin %s = %+v of %+v", n, tw, orig)
				}
			}
		})
	}
}

func TestSiteRoutingResolvers(t *testing.T) {
	r, _, err := siteRouting(liveSite(), true, "http://nginx")
	if err != nil {
		t.Fatal(err)
	}
	routers := r.HTTP.Routers
	if routers[ingress.SiteName(testSiteID, "preview")].TLS.CertResolver != ingress.CertResolverPreview ||
		routers[ingress.SiteName(testSiteID, "primary")].TLS.CertResolver != ingress.CertResolverHTTP {
		t.Errorf("routers = %+v", routers)
	}
	if rd := routers[ingress.SiteName(testSiteID, "preview-redirect")]; rd.Priority != ingress.PreviewRedirectPriority ||
		rd.TLS.CertResolver != ingress.CertResolverPreview || rd.Rule != "Host(`blog.preview.example.com`)" {
		t.Errorf("preview redirect = %+v", rd)
	}
	redirect := r.HTTP.Middlewares[ingress.SiteName(testSiteID, "alias-redirect")].RedirectRegex
	if redirect == nil || redirect.Replacement != "https://example.com${1}" || !redirect.Permanent {
		t.Errorf("alias redirect = %+v", redirect)
	}
	if hsts := r.HTTP.Middlewares[ingress.SiteName(testSiteID, "hsts")].Headers; hsts == nil || hsts.STSSeconds != ingress.HSTSMaxAge {
		t.Errorf("HSTS = %+v", hsts)
	}
}

func TestSiteRoutingRejects(t *testing.T) {
	tests := map[string]func(*provisioner.SiteConfig){
		"injected rule":     func(c *provisioner.SiteConfig) { c.PrimaryDomain = "a.example.com`) || Host(`b.example.com" },
		"upper case":        func(c *provisioner.SiteConfig) { c.PrimaryDomain = "Example.com" },
		"alias twice":       func(c *provisioner.SiteConfig) { c.PrimaryDomain = "a.com"; c.Aliases = []string{"b.com", "b.com"} },
		"primary is alias":  func(c *provisioner.SiteConfig) { c.PrimaryDomain = "a.com"; c.Aliases = []string{"a.com"} },
		"preview reused":    func(c *provisioner.SiteConfig) { c.PrimaryDomain = c.PreviewDomain },
		"alias, no primary": func(c *provisioner.SiteConfig) { c.Aliases = []string{"b.com"} },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			cfg := testSite()
			mutate(cfg)
			if _, err := testProvisioner(t).RenderSite(t.Context(), cfg); !errors.Is(err, ErrInvalidSite) {
				t.Errorf("RenderSite = %v, want ErrInvalidSite", err)
			}
		})
	}
}

// Going live, changing aliases, or changing the preview password changes
// the routes and nothing of the pod.
func TestDomainChangesLeavePodAlone(t *testing.T) {
	p := testProvisioner(t)
	deployment := func(cfg *provisioner.SiteConfig) string {
		t.Helper()
		dir, err := p.RenderSite(t.Context(), cfg)
		if err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(filepath.Join(dir, "deployment.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	want := deployment(testSite())
	for name, cfg := range map[string]*provisioner.SiteConfig{"live": liveSite(), "preview again": testSite()} {
		if got := deployment(cfg); got != want {
			t.Errorf("%s: the Deployment changed", name)
		}
	}
}

// Domains change, and routes the site no longer has stop being served.
func TestUpdateSiteReroutes(t *testing.T) {
	p := cluster(t, nil)
	ctx := t.Context()
	if _, err := p.CreateSite(ctx, liveSite()); err != nil {
		t.Fatal(err)
	}
	routes := routerNames(served(t, p), "")
	if len(routes) != 5 {
		t.Fatalf("live site routes = %q, want 5", routes)
	}
	before := p.testDeployment(t).ResourceVersion

	cfg := liveSite()
	cfg.Aliases = []string{"example.org"}
	cfg.PreviewAuth = nil
	if _, err := p.UpdateSite(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	c := served(t, p)
	if got := routerNames(c, ""); len(got) != 4 {
		t.Errorf("routes after dropping an alias = %q", got)
	}
	if _, ok := c.HTTP.Middlewares[ingress.SiteName(testSiteID, "preview-auth")]; ok {
		t.Errorf("the auth middleware is left: %q", sortedKeys(c.HTTP.Middlewares))
	}
	if after := p.testDeployment(t).ResourceVersion; after != before {
		t.Errorf("the Deployment was touched (resource version %s -> %s)", before, after)
	}

	// Back to the preview domain alone.
	if _, err := p.UpdateSite(ctx, testSite()); err != nil {
		t.Fatal(err)
	}
	if got := routerNames(served(t, p), ""); len(got) != 1 {
		t.Errorf("routes of a site that is not live = %q", got)
	}
}

// Preview protection is served as Traefik's basicAuth with the bcrypt hash
// alone: no Secret, which Traefik could no longer read, and never the
// password.
func TestPreviewAuth(t *testing.T) {
	p := testProvisioner(t)
	dir, err := p.RenderSite(t.Context(), liveSite())
	if err != nil {
		t.Fatal(err)
	}
	name := ingress.SiteName(testSiteID, ingress.SuffixPreviewAuth)
	auth := served(t, p).HTTP.Middlewares[name].BasicAuth
	if auth == nil || !slices.Equal(auth.Users, []string{"client:" + liveSite().PreviewAuth.PasswordHash}) || !auth.RemoveHeader {
		t.Errorf("basicAuth = %+v", auth)
	}
	objs, err := LoadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range objs {
		if o.GetKind() == "Secret" && o.GetName() != "wp-config" {
			t.Errorf("a Secret %s is rendered", o.GetName())
		}
	}
}

func TestSetMaintenance(t *testing.T) {
	p := cluster(t, nil)
	ctx := t.Context()
	site := liveSite()
	if err := p.SetMaintenance(ctx, testSiteID, true); !errors.Is(err, provisioner.ErrSiteNotFound) {
		t.Errorf("SetMaintenance of an unknown site = %v, want ErrSiteNotFound", err)
	}
	if err := p.SetMaintenance(ctx, testSiteID, false); err != nil {
		t.Errorf("turning it off for an unknown site: %v", err)
	}
	if err := p.SetMaintenance(ctx, "Bad_ID", true); !errors.Is(err, ErrInvalidSite) {
		t.Errorf("SetMaintenance of a bad ID = %v, want ErrInvalidSite", err)
	}
	if _, err := p.CreateSite(ctx, site); err != nil {
		t.Fatal(err)
	}
	if got := routerNames(served(t, p), "-maintenance"); len(got) != 0 {
		t.Fatalf("maintenance routes of a site that isn't in maintenance mode: %q", got)
	}

	if err := p.SetMaintenance(ctx, testSiteID, true); err != nil {
		t.Fatal(err)
	}
	want := []string{
		ingress.SiteName(testSiteID, "preview", "maintenance"),
		ingress.SiteName(testSiteID, "primary", "maintenance"),
		ingress.SiteName(testSiteID, "alias", ingress.AliasKey("example.org"), "maintenance"),
		ingress.SiteName(testSiteID, "alias", ingress.AliasKey("www.example.com"), "maintenance"),
	}
	slices.Sort(want)
	c := served(t, p)
	if got := routerNames(c, "-maintenance"); !slices.Equal(got, want) {
		t.Fatalf("maintenance routes = %q, want %q", got, want)
	}
	if on, err := p.inMaintenance(testSiteID); err != nil || !on {
		t.Errorf("inMaintenance = %v, %v", on, err)
	}
	// Twice is the same.
	if err := p.SetMaintenance(ctx, testSiteID, true); err != nil {
		t.Fatal(err)
	}
	r := c.HTTP.Routers[ingress.SiteName(testSiteID, "primary", "maintenance")]
	if r.Rule != "Host(`example.com`)" || r.Priority != ingress.MaintenancePriority || r.Service != "pf-maintenance@file" {
		t.Errorf("primary twin = %+v", r)
	}
	// The site's own routes are still served, below the twins.
	if got := routerNames(c, ""); len(got) != 5+4 {
		t.Errorf("routes = %q", got)
	}

	if err := p.SetMaintenance(ctx, testSiteID, false); err != nil {
		t.Fatal(err)
	}
	if got := routerNames(served(t, p), "-maintenance"); len(got) != 0 {
		t.Errorf("routes left after turning maintenance off: %q", got)
	}
}

// Maintenance mode survives UpdateSite, follows domain changes, and comes
// off only when the site's record says so.
func TestMaintenanceAcrossUpdate(t *testing.T) {
	p := cluster(t, nil)
	ctx := t.Context()
	cfg := testSite()
	cfg.Maintenance = true
	if _, err := p.CreateSite(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	twins := func() []string { return routerNames(served(t, p), "-maintenance") }
	if got := twins(); len(got) != 1 {
		t.Fatalf("a site created in maintenance mode has routes %q", got)
	}

	live := liveSite()
	live.Maintenance = true
	if _, err := p.UpdateSite(ctx, live); err != nil {
		t.Fatal(err)
	}
	if got := twins(); len(got) != 4 {
		t.Errorf("after going live in maintenance mode: %q", got)
	}
	live.Aliases = nil
	if _, err := p.UpdateSite(ctx, live); err != nil {
		t.Fatal(err)
	}
	if got := twins(); len(got) != 2 {
		t.Errorf("after dropping the aliases: %q", got)
	}

	live.Maintenance = false
	if _, err := p.UpdateSite(ctx, live); err != nil {
		t.Fatal(err)
	}
	if got := twins(); len(got) != 0 {
		t.Errorf("after leaving maintenance mode: %q", got)
	}
}

func TestDeleteSiteRemovesRoutes(t *testing.T) {
	p := cluster(t, nil)
	ctx := t.Context()
	if _, err := p.CreateSite(ctx, testSite()); err != nil {
		t.Fatal(err)
	}
	if err := p.SetMaintenance(ctx, testSiteID, true); err != nil {
		t.Fatal(err)
	}
	if err := p.DeleteSite(ctx, testSiteID); err != nil {
		t.Fatal(err)
	}
	if got := routerNames(served(t, p), ""); len(got) != 0 {
		t.Errorf("routes left after DeleteSite: %q", got)
	}
}

// One site's maintenance mode leaves the other's routes alone, and the
// provider serves both sites.
func TestMaintenanceLeavesOtherRoutes(t *testing.T) {
	p := cluster(t, nil)
	ctx := t.Context()
	other := testSite()
	other.ID, other.PreviewDomain = otherSiteID, "other.preview.example.com"
	for _, cfg := range []*provisioner.SiteConfig{testSite(), other} {
		if _, err := p.CreateSite(ctx, cfg); err != nil {
			t.Fatal(err)
		}
	}
	if err := p.SetMaintenance(ctx, otherSiteID, true); err != nil {
		t.Fatal(err)
	}
	if err := p.SetMaintenance(ctx, testSiteID, true); err != nil {
		t.Fatal(err)
	}
	if err := p.SetMaintenance(ctx, testSiteID, false); err != nil {
		t.Fatal(err)
	}
	want := []string{
		ingress.SiteName(otherSiteID, "preview"),
		ingress.SiteName(otherSiteID, "preview", "maintenance"),
		ingress.SiteName(testSiteID, "preview"),
	}
	slices.Sort(want)
	c := served(t, p)
	if got := routerNames(c, ""); !slices.Equal(got, want) {
		t.Errorf("routes = %q, want %q", got, want)
	}
	if got := c.HTTP.Services[ingress.SiteService(otherSiteID)].LoadBalancer.Servers; len(got) != 1 || got[0].URL != "http://nginx."+p.SiteNamespace(otherSiteID)+".svc" {
		t.Errorf("the other site's service = %+v", got)
	}
}
