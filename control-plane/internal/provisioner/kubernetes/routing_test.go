package kubernetes

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

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

func TestRoutingGolden(t *testing.T) {
	p := testProvisioner(t)
	dir, err := p.RenderSite(t.Context(), liveSite())
	if err != nil {
		t.Fatal(err)
	}
	routes, err := os.ReadFile(filepath.Join(dir, "routes.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "routes-live.yaml", routes)
	siteDir, _ := p.layout.SiteDir(testSiteID)
	maint, err := os.ReadFile(filepath.Join(siteDir, MaintenanceFile))
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "maintenance-live.yaml", maint)

	// Both apply, and the maintenance routes are outside the site's
	// namespace, where the control plane may write them.
	if err := p.ApplyDir(t.Context(), dir); err != nil {
		t.Fatal(err)
	}
	objs, err := DecodeManifests(maint)
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range objs {
		if o.GetNamespace() != p.platformNamespace {
			t.Errorf("%s %s is in %q, want the platform namespace", o.GetKind(), o.GetName(), o.GetNamespace())
		}
	}
}

// routeNames lists the names of the objects of kind in namespace.
func routeNames(t *testing.T, p *Provisioner, gvk string, namespace string) []string {
	t.Helper()
	for k := range kinds {
		if k.Kind != gvk {
			continue
		}
		list, err := p.resource(kinds[k], namespace).List(t.Context(), metav1.ListOptions{})
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, o := range list.Items {
			names = append(names, o.GetName())
		}
		slices.Sort(names)
		return names
	}
	t.Fatalf("no kind %s", gvk)
	return nil
}

func TestSiteRouting(t *testing.T) {
	id := testSiteID
	name := func(s ...string) string { return ingress.SiteName(id, s...) }
	alias := func(a string) string { return name(ingress.SuffixAlias, ingress.AliasKey(a)) }

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
			name("preview"), name("primary"), alias("www.example.com"), alias("example.org"), name("preview-redirect"),
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
			name("preview"), name("primary"), alias("www.example.com"), alias("example.org"), name("preview-redirect"),
		},
		mw: []string{
			name("alias-redirect"), name("noindex"), name("preview-auth"), name("preview-redirect"),
		},
		previewM: []string{name("noindex"), name("preview-auth")},
		aliasM:   []string{name("alias-redirect")},
	}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, m, err := siteRouting(tt.site(), tt.hsts)
			if err != nil {
				t.Fatal(err)
			}
			var routers, mw []string
			for _, rt := range r.Routes {
				routers = append(routers, rt.Name)
			}
			mw = append(mw, r.NoIndex)
			if r.Auth != nil {
				mw = append(mw, r.Auth.Name)
			}
			if r.HSTS != "" {
				mw = append(mw, r.HSTS)
			}
			for _, rd := range r.Redirects {
				mw = append(mw, rd.Name)
			}
			slices.Sort(mw)
			if !slices.Equal(routers, tt.routers) {
				t.Errorf("routers = %q, want %q", routers, tt.routers)
			}
			if !slices.Equal(mw, tt.mw) {
				t.Errorf("middlewares = %q, want %q", mw, tt.mw)
			}
			if got := r.Routes[0].Middlewares; !slices.Equal(got, tt.previewM) {
				t.Errorf("preview middlewares = %q, want %q", got, tt.previewM)
			}
			if tt.aliasM != nil {
				if got := r.Routes[2].Middlewares; !slices.Equal(got, tt.aliasM) {
					t.Errorf("alias middlewares = %q, want %q", got, tt.aliasM)
				}
			}
			// One maintenance twin per router, except the preview redirect,
			// which its twin shadows; twins win over everything.
			wantTwins := len(r.Routes)
			if tt.site().PrimaryDomain != "" {
				wantTwins--
			}
			if len(m.Routes) != wantTwins {
				t.Fatalf("%d maintenance routes, want %d", len(m.Routes), wantTwins)
			}
			for i, tw := range m.Routes {
				if tw.Name != r.Routes[i].Name+"-maintenance" || tw.Rule != r.Routes[i].Rule ||
					tw.CertResolver != r.Routes[i].CertResolver || tw.Priority != ingress.MaintenancePriority {
					t.Errorf("twin %d = %+v of %+v", i, tw, r.Routes[i])
				}
			}
		})
	}
}

func TestSiteRoutingResolvers(t *testing.T) {
	r, _, err := siteRouting(liveSite(), true)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, rt := range r.Routes {
		got[rt.Name] = rt.CertResolver
	}
	if got[ingress.SiteName(testSiteID, "preview")] != ingress.CertResolverPreview ||
		got[ingress.SiteName(testSiteID, "primary")] != ingress.CertResolverHTTP {
		t.Errorf("resolvers = %v", got)
	}
	if last := r.Routes[len(r.Routes)-1]; last.Priority != ingress.PreviewRedirectPriority {
		t.Errorf("preview redirect priority = %d", last.Priority)
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
// IngressRoutes and Middlewares and nothing of the pod.
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

// Domains change, and routes the site no longer has are deleted.
func TestUpdateSiteReroutes(t *testing.T) {
	p := cluster(t, nil)
	ctx := t.Context()
	ns := p.SiteNamespace(testSiteID)
	if _, err := p.CreateSite(ctx, liveSite()); err != nil {
		t.Fatal(err)
	}
	routes := routeNames(t, p, "IngressRoute", ns)
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
	if got := routeNames(t, p, "IngressRoute", ns); len(got) != 4 {
		t.Errorf("routes after dropping an alias = %q", got)
	}
	if got := routeNames(t, p, "Middleware", ns); slices.Contains(got, ingress.SiteName(testSiteID, "preview-auth")) {
		t.Errorf("the auth middleware is left: %q", got)
	}
	if _, err := p.typed.CoreV1().Secrets(ns).Get(ctx, ingress.SiteName(testSiteID, "preview-auth"), metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("the auth Secret is left: %v", err)
	}
	if after := p.testDeployment(t).ResourceVersion; after != before {
		t.Errorf("the Deployment was touched (resource version %s -> %s)", before, after)
	}

	// Back to the preview domain alone.
	if _, err := p.UpdateSite(ctx, testSite()); err != nil {
		t.Fatal(err)
	}
	if got := routeNames(t, p, "IngressRoute", ns); len(got) != 1 {
		t.Errorf("routes of a site that is not live = %q", got)
	}
}

func TestPreviewAuthSecret(t *testing.T) {
	p := testProvisioner(t)
	dir, err := p.RenderSite(t.Context(), liveSite())
	if err != nil {
		t.Fatal(err)
	}
	name := ingress.SiteName(testSiteID, ingress.SuffixPreviewAuth)
	secret := renderedObject(t, dir, "Secret", name)
	if users, _, _ := unstructured.NestedString(secret.Object, "stringData", "users"); users != "client:"+liveSite().PreviewAuth.PasswordHash {
		t.Errorf("users = %q", users)
	}
	mw := renderedObject(t, dir, "Middleware", name)
	if ref, _, _ := unstructured.NestedString(mw.Object, "spec", "basicAuth", "secret"); ref != name {
		t.Errorf("basicAuth secret = %q", ref)
	}
	if rm, _, _ := unstructured.NestedBool(mw.Object, "spec", "basicAuth", "removeHeader"); !rm {
		t.Error("the Authorization header isn't removed")
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
	platform := p.platformNamespace
	if got := routeNames(t, p, "IngressRoute", platform); len(got) != 0 {
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
	got := routeNames(t, p, "IngressRoute", platform)
	if !slices.Equal(got, want) {
		t.Fatalf("maintenance routes = %q, want %q", got, want)
	}
	// Twice is the same.
	if err := p.SetMaintenance(ctx, testSiteID, true); err != nil {
		t.Fatal(err)
	}

	obj, err := p.resource(kinds[ingressRouteKind], platform).Get(ctx, ingress.SiteName(testSiteID, "primary", "maintenance"), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	route, _, _ := unstructured.NestedSlice(obj.Object, "spec", "routes")
	r := route[0].(map[string]any)
	svc := r["services"].([]any)[0].(map[string]any)
	if r["match"] != "Host(`example.com`)" || r["priority"] != int64(ingress.MaintenancePriority) ||
		svc["name"] != ingress.MaintenanceHost || svc["port"] != int64(ingress.MaintenancePort) {
		t.Errorf("primary twin = %v", r)
	}
	if obj.GetLabels()[MaintenanceLabel] != testSiteID {
		t.Errorf("labels = %v", obj.GetLabels())
	}
	// A twin never takes a namespace-crossing reference: the service is
	// the maintenance page's, in the namespace of the route itself.
	if _, ok := svc["namespace"]; ok {
		t.Error("the twin names a namespace for its service")
	}
	// The site's own routes are untouched.
	if got := routeNames(t, p, "IngressRoute", p.SiteNamespace(testSiteID)); len(got) != 5 {
		t.Errorf("site routes = %q", got)
	}

	if err := p.SetMaintenance(ctx, testSiteID, false); err != nil {
		t.Fatal(err)
	}
	if got := routeNames(t, p, "IngressRoute", platform); len(got) != 0 {
		t.Errorf("routes left after turning maintenance off: %q", got)
	}
}

// Maintenance mode survives UpdateSite, follows domain changes, and comes
// off only when the site's record says so.
func TestMaintenanceAcrossUpdate(t *testing.T) {
	p := cluster(t, nil)
	ctx := t.Context()
	platform := p.platformNamespace
	cfg := testSite()
	cfg.Maintenance = true
	if _, err := p.CreateSite(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	if got := routeNames(t, p, "IngressRoute", platform); len(got) != 1 {
		t.Fatalf("a site created in maintenance mode has routes %q", got)
	}

	live := liveSite()
	live.Maintenance = true
	if _, err := p.UpdateSite(ctx, live); err != nil {
		t.Fatal(err)
	}
	if got := routeNames(t, p, "IngressRoute", platform); len(got) != 4 {
		t.Errorf("after going live in maintenance mode: %q", got)
	}
	live.Aliases = nil
	if _, err := p.UpdateSite(ctx, live); err != nil {
		t.Fatal(err)
	}
	if got := routeNames(t, p, "IngressRoute", platform); len(got) != 2 {
		t.Errorf("after dropping the aliases: %q", got)
	}

	live.Maintenance = false
	if _, err := p.UpdateSite(ctx, live); err != nil {
		t.Fatal(err)
	}
	if got := routeNames(t, p, "IngressRoute", platform); len(got) != 0 {
		t.Errorf("after leaving maintenance mode: %q", got)
	}
}

func TestDeleteSiteRemovesMaintenanceRoutes(t *testing.T) {
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
	if got := routeNames(t, p, "IngressRoute", p.platformNamespace); len(got) != 0 {
		t.Errorf("routes left after DeleteSite: %q", got)
	}
}

// Other sites' and the platform's own routes aren't touched.
func TestMaintenanceLeavesOtherRoutes(t *testing.T) {
	p := cluster(t, nil)
	ctx := t.Context()
	other := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "traefik.io/v1alpha1", "kind": "IngressRoute",
		"metadata": map[string]any{"name": "pf-dashboard", "namespace": p.platformNamespace},
	}}
	other2 := other.DeepCopy()
	other2.SetName("pf-other-preview-maintenance")
	other2.SetLabels(map[string]string{MaintenanceLabel: "other"})
	if err := p.Apply(ctx, []*unstructured.Unstructured{other, other2}); err != nil {
		t.Fatal(err)
	}
	if _, err := p.CreateSite(ctx, testSite()); err != nil {
		t.Fatal(err)
	}
	if err := p.SetMaintenance(ctx, testSiteID, true); err != nil {
		t.Fatal(err)
	}
	if err := p.SetMaintenance(ctx, testSiteID, false); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(routeNames(t, p, "IngressRoute", p.platformNamespace), ","); got != "pf-dashboard,pf-other-preview-maintenance" {
		t.Errorf("routes = %s", got)
	}
}
