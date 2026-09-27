package compose_test

import (
	"bytes"
	"context"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	"github.com/rchouinard/pressfleet/control-plane/internal/ingress"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner/compose"
)

// liveSiteRender is testSiteRender for a site that went live with two
// aliases, and whose preview domain is protected.
func liveSiteRender() compose.SiteRender {
	in := testSiteRender()
	in.Site.PrimaryDomain = "www.acme.example"
	in.Site.Aliases = []string{"acme.example", "shop.acme.example"}
	in.Site.PreviewAuth = &provisioner.BasicAuth{Username: "preview", PasswordHash: "$2a$04$abcdefghijklmnopqrstuuq5MvA9Wh8m6cHfVZ1gXWLU4.o9rSPTa"}
	return in
}

// domainsFile is what the tests read back from a domains file.
type domainsFile struct {
	HTTP struct {
		Routers map[string]struct {
			Rule        string
			EntryPoints []string `yaml:"entryPoints"`
			Priority    int
			Service     string
			Middlewares []string
			TLS         struct {
				CertResolver string `yaml:"certResolver"`
			}
		}
		Middlewares map[string]map[string]map[string]any
	}
}

func readDomainsFile(t *testing.T, path string) domainsFile {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var f domainsFile
	if err := yaml.Unmarshal(raw, &f); err != nil {
		t.Fatalf("%v:\n%s", err, raw)
	}
	return f
}

func TestDomainRoutesGolden(t *testing.T) {
	dir, _ := renderSite(t, liveSiteRender())
	got, err := os.ReadFile(filepath.Join(dir, compose.SiteDomainsFile))
	if err != nil {
		t.Fatal(err)
	}
	golden := filepath.Join("testdata", "site-domains.yaml.golden")
	if *update {
		if err := os.WriteFile(golden, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s differs from %s (run go test -update to accept):\n%s", compose.SiteDomainsFile, golden, got)
	}
}

// TestDomainRoutes checks the routes of a live site (#45, #46) against the
// scheme in package ingress: the primary domain and each alias on routers
// of their own with HTTP-01 certificates and HSTS, aliases and the preview
// domain redirecting to the primary domain, and no preview middleware on a
// production domain.
func TestDomainRoutes(t *testing.T) {
	dir, _ := renderSite(t, liveSiteRender())
	f := readDomainsFile(t, filepath.Join(dir, compose.SiteDomainsFile))
	p := "pf-" + testSiteID
	service := p + "@docker"
	type route struct {
		rule, resolver string
		priority       int
		middlewares    []string
	}
	want := map[string]route{
		p + "-primary": {"Host(`www.acme.example`)", "acme-http", 0, []string{p + "-hsts"}},
		p + "-alias-" + ingress.AliasKey("acme.example"):      {"Host(`acme.example`)", "acme-http", 0, []string{p + "-hsts", p + "-alias-redirect"}},
		p + "-alias-" + ingress.AliasKey("shop.acme.example"): {"Host(`shop.acme.example`)", "acme-http", 0, []string{p + "-hsts", p + "-alias-redirect"}},
		p + "-preview-redirect": {"Host(`acme.preview.example.com`)", "acme-preview", 10000,
			[]string{p + "-noindex", p + "-preview-auth", p + "-preview-redirect"}},
	}
	if len(f.HTTP.Routers) != len(want) {
		t.Errorf("routers: %v", slices.Sorted(maps.Keys(f.HTTP.Routers)))
	}
	for name, w := range want {
		r, ok := f.HTTP.Routers[name]
		if !ok {
			t.Errorf("no router %s", name)
			continue
		}
		if r.Rule != w.rule || r.TLS.CertResolver != w.resolver || r.Priority != w.priority || r.Service != service ||
			!slices.Equal(r.EntryPoints, []string{"websecure"}) || !slices.Equal(r.Middlewares, w.middlewares) {
			t.Errorf("router %s = %+v, want %+v", name, r, w)
		}
	}
	mw := f.HTTP.Middlewares
	if sts := mw[p+"-hsts"]["headers"]; sts["stsSeconds"] != 31536000 || sts["stsIncludeSubdomains"] != nil {
		t.Errorf("hsts middleware = %v", sts)
	}
	for _, name := range []string{p + "-alias-redirect", p + "-preview-redirect"} {
		r := mw[name]["redirectRegex"]
		if r["regex"] != `^https?://[^/]+(.*)$` || r["replacement"] != "https://www.acme.example${1}" || r["permanent"] != true {
			t.Errorf("%s = %v", name, r)
		}
	}
	if auth := mw[p+"-preview-auth"]["basicAuth"]; auth["removeHeader"] != true ||
		!slices.Equal(auth["users"].([]any), []any{"preview:" + liveSiteRender().Site.PreviewAuth.PasswordHash}) {
		t.Errorf("preview auth = %v", auth)
	}
	if h := mw[p+"-noindex"]["headers"]["customResponseHeaders"]; h.(map[string]any)["X-Robots-Tag"] != "noindex" {
		t.Errorf("noindex = %v", h)
	}

	// Without security headers there is no HSTS, and without protection
	// no basic auth.
	in := liveSiteRender()
	in.Security.Headers = false
	in.Site.PreviewAuth = nil
	dir, _ = renderSite(t, in)
	f = readDomainsFile(t, filepath.Join(dir, compose.SiteDomainsFile))
	if _, ok := f.HTTP.Middlewares[p+"-hsts"]; ok {
		t.Error("an HSTS middleware without security headers")
	}
	if _, ok := f.HTTP.Middlewares[p+"-preview-auth"]; ok {
		t.Error("a basic auth middleware without protection")
	}
	if r := f.HTTP.Routers[p+"-primary"]; len(r.Middlewares) != 0 {
		t.Errorf("primary middlewares without security headers: %v", r.Middlewares)
	}
	if r := f.HTTP.Routers[p+"-preview-redirect"]; !slices.Equal(r.Middlewares, []string{p + "-noindex", p + "-preview-redirect"}) {
		t.Errorf("preview redirect middlewares without protection: %v", r.Middlewares)
	}
}

// TestDomainsKeepLabels checks that going live and changing aliases leave
// compose.yaml alone, so no container is recreated: the custom domains
// live in the domains file only.
func TestDomainsKeepLabels(t *testing.T) {
	preview := testSiteRender()
	preview.Site.PreviewAuth = liveSiteRender().Site.PreviewAuth
	dirA, _ := renderSite(t, preview)
	dirB, _ := renderSite(t, liveSiteRender())
	a, err := os.ReadFile(filepath.Join(dirA, compose.SiteComposeFile))
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dirB, compose.SiteComposeFile))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a, b) {
		t.Error("going live changed compose.yaml")
	}
	if _, err := os.Stat(filepath.Join(dirA, compose.SiteDomainsFile)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a preview-only site has a domains file: %v", err)
	}

	// Rendering the site after it lost its domains removes the file.
	if _, err := compose.RenderSite(dirB, preview); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dirB, compose.SiteDomainsFile)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the domains file outlived the domains: %v", err)
	}
}

// TestMaintenanceCoversDomains checks that maintenance mode (#44) mirrors
// the custom domains' routers too.
func TestMaintenanceCoversDomains(t *testing.T) {
	dir, _ := renderSite(t, liveSiteRender())
	raw, err := os.ReadFile(filepath.Join(dir, compose.SiteMaintenanceFile))
	if err != nil {
		t.Fatal(err)
	}
	var f maintenanceFile
	if err := yaml.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	p := "pf-" + testSiteID
	for name, rule := range map[string]string{
		p + "-preview-maintenance": "Host(`acme.preview.example.com`)",
		p + "-primary-maintenance": "Host(`www.acme.example`)",
		p + "-alias-" + ingress.AliasKey("acme.example") + "-maintenance":      "Host(`acme.example`)",
		p + "-alias-" + ingress.AliasKey("shop.acme.example") + "-maintenance": "Host(`shop.acme.example`)",
	} {
		r, ok := f.HTTP.Routers[name]
		if !ok || r.Rule != rule || r.Priority != ingress.MaintenancePriority || len(r.Middlewares) != 0 {
			t.Errorf("maintenance router %s = %+v (found %v)", name, r, ok)
		}
	}
	if len(f.HTTP.Routers) != 4 {
		t.Errorf("%d maintenance routers, want 4", len(f.HTTP.Routers))
	}
}

func TestDomainValidation(t *testing.T) {
	for name, edit := range map[string]func(*provisioner.SiteConfig){
		"invalid primary":      func(s *provisioner.SiteConfig) { s.PrimaryDomain = "WWW.Acme.example" },
		"backquote in primary": func(s *provisioner.SiteConfig) { s.PrimaryDomain = "a`) || Host(`b.example" },
		"invalid alias":        func(s *provisioner.SiteConfig) { s.Aliases = []string{"acme.example/x"} },
		"aliases without primary": func(s *provisioner.SiteConfig) {
			s.PrimaryDomain = ""
		},
		"alias is the primary":    func(s *provisioner.SiteConfig) { s.Aliases = []string{"www.acme.example"} },
		"alias is the preview":    func(s *provisioner.SiteConfig) { s.Aliases = []string{"acme.preview.example.com"} },
		"primary is the preview":  func(s *provisioner.SiteConfig) { s.PrimaryDomain = "acme.preview.example.com" },
		"alias listed twice":      func(s *provisioner.SiteConfig) { s.Aliases = []string{"acme.example", "acme.example"} },
		"redirect breakout alias": func(s *provisioner.SiteConfig) { s.Aliases = []string{"x.example${1}"} },
	} {
		t.Run(name, func(t *testing.T) {
			in := liveSiteRender()
			edit(in.Site)
			if _, err := compose.RenderSite(filepath.Join(t.TempDir(), "site"), in); !errors.Is(err, compose.ErrInvalidSite) {
				t.Errorf("err = %v, want ErrInvalidSite", err)
			}
		})
	}
}

// TestUpdateSiteDomains checks the provisioner's side of go-live and alias
// changes: the domains file goes into Traefik's directory without any
// container being recreated, follows the site's domains, and goes away
// with them and with the site.
func TestUpdateSiteDomains(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if _, err := f.prov.CreateSite(ctx, f.cfg); err != nil {
		t.Fatal(err)
	}
	routes := filepath.Join(f.routesDir, compose.DomainRoutesFile(testSiteID))
	if _, err := os.Stat(routes); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a preview-only site has domain routes: %v", err)
	}
	f.docker.calls = nil

	f.cfg.PrimaryDomain, f.cfg.Aliases = "www.acme.example", []string{"acme.example"}
	if _, err := f.prov.UpdateSite(ctx, f.cfg); err != nil {
		t.Fatal(err)
	}
	wantCommands(t, f.docker, upWait) // nothing to recreate
	file := readDomainsFile(t, routes)
	if _, ok := file.HTTP.Routers["pf-"+testSiteID+"-primary"]; !ok || len(file.HTTP.Routers) != 3 {
		t.Errorf("routers = %v", file.HTTP.Routers)
	}
	if fi, _ := os.Stat(routes); fi.Mode().Perm() != 0o644 {
		t.Errorf("mode = %v, want 0644", fi.Mode().Perm())
	}
	if site, err := os.ReadFile(filepath.Join(f.siteDir(t), compose.SiteDomainsFile)); err != nil || !bytes.Equal(site, mustRead(t, routes)) {
		t.Errorf("the installed routes differ from the site's copy (%v)", err)
	}

	// Removing an alias rewrites the file.
	f.cfg.Aliases = nil
	if _, err := f.prov.UpdateSite(ctx, f.cfg); err != nil {
		t.Fatal(err)
	}
	if file := readDomainsFile(t, routes); len(file.HTTP.Routers) != 2 {
		t.Errorf("after removing the alias, routers = %v", file.HTTP.Routers)
	}

	// Maintenance mode covers the primary domain.
	f.cfg.Maintenance = true
	if _, err := f.prov.UpdateSite(ctx, f.cfg); err != nil {
		t.Fatal(err)
	}
	if raw := mustRead(t, filepath.Join(f.routesDir, compose.MaintenanceRoutesFile(testSiteID))); !strings.Contains(string(raw), "Host(`www.acme.example`)") {
		t.Errorf("maintenance routes lack the primary domain:\n%s", raw)
	}
	f.cfg.Maintenance = false

	// Losing the domains (a rolled back go-live) removes the file.
	f.cfg.PrimaryDomain = ""
	if _, err := f.prov.UpdateSite(ctx, f.cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(routes); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("domain routes outlived the domains: %v", err)
	}

	// So does deleting the site.
	f.cfg.PrimaryDomain = "www.acme.example"
	if _, err := f.prov.UpdateSite(ctx, f.cfg); err != nil {
		t.Fatal(err)
	}
	if err := f.prov.DeleteSite(ctx, testSiteID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(routes); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("domain routes outlived the site: %v", err)
	}
}

func TestDomainsUnavailable(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	for name, dir := range map[string]string{
		"not configured": "",
		"not rendered":   filepath.Join(f.layout.Root, "no", "such", "dir"),
	} {
		t.Run(name, func(t *testing.T) {
			prov := newProvisionerWith(t, f, f.sec, func(c *compose.ProvisionerConfig) {
				c.TraefikDynamicDir = dir
				c.Images = imageMap{"8.4": {PHP: "site-php:php8.4-wp7.1.2", Nginx: "site-nginx:wp7.1.2"}}
			})
			cfg := *f.cfg
			if _, err := prov.CreateSite(ctx, &cfg); err != nil {
				t.Fatal(err)
			}
			cfg.PrimaryDomain = "www.acme.example"
			if _, err := prov.UpdateSite(ctx, &cfg); !errors.Is(err, compose.ErrDomainsUnavailable) {
				t.Errorf("going live: %v", err)
			}
			cfg.PrimaryDomain = ""
			if _, err := prov.UpdateSite(ctx, &cfg); err != nil {
				t.Errorf("a preview-only site: %v", err)
			}
		})
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
