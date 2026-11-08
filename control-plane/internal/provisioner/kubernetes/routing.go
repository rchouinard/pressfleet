package kubernetes

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"

	"github.com/rchouinard/pressfleet/control-plane/internal/ingress"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
)

// Routing and maintenance mode (#217, #322, design doc §5.1, §5.2, §5.4,
// §4.8).
//
// A site's routes are Traefik routers, middlewares, and a service, named as
// on Compose (package ingress), which the control plane serves to Traefik's
// HTTP provider (traefikconfig.go). RenderSite writes them, as JSON, to
// RoutesFile in the site's manifest directory, where the templates
// rollout's snapshots keep them with the manifests; Traefik picks a
// change up within its poll interval. They are no object of the cluster,
// so changing domains changes no pod, and the site's service is its nginx
// Service by name (nginx.<site namespace>.svc), which Traefik reaches
// through the site's NetworkPolicy.
//
// Maintenance routes are a twin of each router at a higher priority,
// pointing at the maintenance page's service, which the chart defines in
// Traefik's file provider (pf-maintenance@file). RenderSite writes them to
// MaintenanceFile in the site's directory; SetMaintenance copies that to
// MaintenanceRoutesFile, which is served, or removes the copy. DeleteSite
// removes both of the served files before anything else, so a site being
// deleted stops being routed at once.

// RoutesFile is the site's routes, in its manifest directory (not a
// manifest: LoadDir reads only *.yaml).
const RoutesFile = "traefik-routes.json"

// MaintenanceFile is the site's maintenance routes as RenderSite writes
// them into the site's directory, and MaintenanceRoutesFile the copy that
// SetMaintenance serves while the site is in maintenance mode.
const (
	MaintenanceFile       = "kubernetes-maintenance.json"
	MaintenanceRoutesFile = "traefik-maintenance.json"
)

// maintenanceService is the maintenance page's Traefik service, from the
// chart's file provider (charts/pressfleet/templates/traefik-config.yaml).
const maintenanceService = ingress.MaintenanceService + "@file"

// redirectRegex matches any URL Traefik sees and captures its path and
// query, which a redirect to the primary domain keeps (as on Compose).
const redirectRegex = `^https?://[^/]+(.*)$`

// hostname matches the lowercase DNS names a router rule may contain; it
// keeps anything that could break out of a Host(`...`) matcher out of the
// routes.
var hostname = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

func validHostname(h string) bool { return len(h) <= 253 && hostname.MatchString(h) }

// siteRouting derives the site's routes and their maintenance twins from
// its configuration. Its list of routers is ingress.SiteRouters, the
// Compose driver's too: the preview domain's, then, once the site is live,
// the primary domain's and one per alias, in the order of
// SiteConfig.Aliases, and the preview redirect that takes the preview
// domain over. hsts adds the HSTS middleware to the custom domains (§6.1).
// service is the site's nginx Service as Traefik reaches it.
func siteRouting(site *provisioner.SiteConfig, hsts bool, service string) (routes, maintenance traefikConfig, err error) {
	if err := validateRouting(site); err != nil {
		return routes, maintenance, fmt.Errorf("%w: %w", ErrInvalidSite, err)
	}
	name := func(suffixes ...string) string { return ingress.SiteName(site.ID, suffixes...) }
	svc := ingress.SiteService(site.ID)
	routes.add(traefikConfig{})
	maintenance.add(traefikConfig{})
	router := func(rt ingress.Router, priority int, service string) traefikRouter {
		return traefikRouter{
			EntryPoints: []string{ingress.EntryPointHTTPS},
			Rule:        rt.Rule,
			Priority:    priority,
			Middlewares: rt.Middlewares,
			Service:     service,
			TLS:         traefikRouterTLS{CertResolver: rt.CertResolver},
		}
	}

	routes.HTTP.Services[svc] = traefikService{LoadBalancer: traefikLoadBalancer{Servers: []traefikServer{{URL: service}}}}
	for _, rt := range ingress.SiteRouters(site, hsts) {
		routes.HTTP.Routers[rt.Name] = router(rt, 0, svc)
		// Twins mirror these routers only: the preview redirect is
		// shadowed by the twin of the preview router, which has a higher
		// priority.
		twin := rt
		twin.Middlewares = nil
		maintenance.HTTP.Routers[rt.Name+"-"+ingress.SuffixMaintenance] = router(twin, ingress.MaintenancePriority, maintenanceService)
	}

	// Preview domains stay out of search engines (§5.1).
	routes.HTTP.Middlewares[name(ingress.SuffixNoIndex)] = traefikMiddleware{
		Headers: &traefikHeaders{CustomResponseHeaders: map[string]string{"X-Robots-Tag": "noindex"}},
	}
	if a := site.PreviewAuth; a != nil {
		// Preview password protection (§5.1): the bcrypt hash, never the
		// password, which makes the provider endpoint sensitive
		// (TraefikConfigHandler). The Authorization header is removed
		// before the request reaches WordPress, which would otherwise try
		// it as an application password and refuse REST API requests.
		routes.HTTP.Middlewares[name(ingress.SuffixPreviewAuth)] = traefikMiddleware{
			BasicAuth: &traefikBasicAuth{Users: []string{a.Username + ":" + a.PasswordHash}, Realm: "Preview", RemoveHeader: true},
		}
	}
	if site.PrimaryDomain == "" {
		return routes, maintenance, nil
	}
	if hsts {
		// Without includeSubDomains: a client's other subdomains may not
		// serve HTTPS (§6.1).
		routes.HTTP.Middlewares[name(ingress.SuffixHSTS)] = traefikMiddleware{Headers: &traefikHeaders{STSSeconds: ingress.HSTSMaxAge}}
	}
	// The preview domain now redirects to the primary domain, behind the
	// preview router's own middlewares, so a protected preview domain asks
	// for the password before it reveals where the site went.
	routes.HTTP.Routers[name(ingress.SuffixPreviewRedirect)] = router(ingress.Router{
		Rule:         ingress.HostRule(site.PreviewDomain),
		CertResolver: ingress.CertResolverPreview,
		Middlewares:  append(slices.Clone(ingress.PreviewMiddlewares(site)), name(ingress.SuffixPreviewRedirect)),
	}, ingress.PreviewRedirectPriority, svc)
	redirect := traefikMiddleware{RedirectRegex: &traefikRedirectRegex{
		Regex: redirectRegex, Replacement: "https://" + site.PrimaryDomain + "${1}", Permanent: true,
	}}
	routes.HTTP.Middlewares[name(ingress.SuffixPreviewRedirect)] = redirect
	routes.HTTP.Middlewares[name(ingress.SuffixAliasRedirect)] = redirect
	return routes, maintenance, nil
}

// validateRouting checks what the routes are made from: every domain must
// be a lowercase host name, which keeps anything that could break out of a
// Host(`...`) matcher or a redirect's replacement out of Traefik's
// configuration, and no domain may be routed twice.
func validateRouting(site *provisioner.SiteConfig) error {
	var errs []error
	if !validHostname(site.PreviewDomain) {
		errs = append(errs, fmt.Errorf("preview domain %q is not a valid lowercase host name", site.PreviewDomain))
	}
	if site.PrimaryDomain == "" {
		if len(site.Aliases) > 0 {
			errs = append(errs, errors.New("aliases need a primary domain"))
		}
		return errors.Join(errs...)
	}
	seen := map[string]bool{site.PreviewDomain: true}
	for i, d := range append([]string{site.PrimaryDomain}, site.Aliases...) {
		what := "primary domain"
		if i > 0 {
			what = "alias"
		}
		switch {
		case !validHostname(d):
			errs = append(errs, fmt.Errorf("%s %q is not a valid lowercase host name", what, d))
		case seen[d]:
			errs = append(errs, fmt.Errorf("%s %q is routed twice", what, d))
		}
		seen[d] = true
	}
	return errors.Join(errs...)
}

// siteService is the address Traefik reaches the site's nginx Service at
// (service.yaml.tmpl: Service nginx, port 80).
func (p *Provisioner) siteService(siteID string) string {
	return "http://nginx." + p.SiteNamespace(siteID) + ".svc"
}

// SetMaintenance implements provisioner.Provisioner: it puts the site's
// domains on the platform's maintenance page or takes them off it. See the
// comment at the top of this file.
func (p *Provisioner) SetMaintenance(ctx context.Context, siteID string, enabled bool) error {
	if err := p.validSiteNamespace(siteID); err != nil {
		return fmt.Errorf("set maintenance: %w", err)
	}
	var err error
	if enabled {
		err = p.applyMaintenance(siteID)
	} else {
		err = p.removeMaintenance(siteID)
	}
	if err != nil {
		return fmt.Errorf("set maintenance of site %s: %w", siteID, err)
	}
	p.log.InfoContext(ctx, "site maintenance mode changed", "site_id", siteID, "enabled", enabled)
	return nil
}

// applyMaintenance serves the site's rendered maintenance routes, in place
// of any it served before.
func (p *Provisioner) applyMaintenance(siteID string) error {
	dir, err := p.layout.SiteDir(siteID)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidSite, err)
	}
	content, err := os.ReadFile(filepath.Join(dir, MaintenanceFile)) //nolint:gosec // G304: inside the site's directory
	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("the site has no rendered routes: %w", provisioner.ErrSiteNotFound)
	} else if err != nil {
		return err
	}
	return writeIfChanged(filepath.Join(dir, MaintenanceRoutesFile), content)
}

// removeMaintenance stops serving the site's maintenance routes. Any site
// can have them removed.
func (p *Provisioner) removeMaintenance(siteID string) error {
	dir, err := p.layout.SiteDir(siteID)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidSite, err)
	}
	if err := os.Remove(filepath.Join(dir, MaintenanceRoutesFile)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// inMaintenance reports whether the site's maintenance routes are served.
func (p *Provisioner) inMaintenance(siteID string) (bool, error) {
	dir, err := p.layout.SiteDir(siteID)
	if err != nil {
		return false, fmt.Errorf("%w: %w", ErrInvalidSite, err)
	}
	_, err = os.Stat(filepath.Join(dir, MaintenanceRoutesFile))
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

// unroute stops serving all of the site's routes, its maintenance routes
// included.
func (p *Provisioner) unroute(siteID string) error {
	dir, err := p.layout.SiteDir(siteID)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidSite, err)
	}
	for _, f := range []string{filepath.Join(dir, SiteManifestDir, RoutesFile), filepath.Join(dir, MaintenanceRoutesFile)} {
		if err := os.Remove(f); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return nil
}

// maintenanceBeforeApply puts a site that is in maintenance mode on the
// maintenance page before its pod is replaced, so visitors see the page
// rather than errors.
func (p *Provisioner) maintenanceBeforeApply(cfg *provisioner.SiteConfig) error {
	if !cfg.Maintenance {
		return nil
	}
	if err := p.applyMaintenance(cfg.ID); err != nil {
		return fmt.Errorf("maintenance mode: %w", err)
	}
	return nil
}

// maintenanceAfterApply takes a site that isn't in maintenance mode off the
// page once its pod is ready, which also repairs routes left by a failed
// job.
func (p *Provisioner) maintenanceAfterApply(cfg *provisioner.SiteConfig) error {
	if cfg.Maintenance {
		return nil
	}
	if err := p.removeMaintenance(cfg.ID); err != nil {
		return fmt.Errorf("maintenance mode: %w", err)
	}
	return nil
}
