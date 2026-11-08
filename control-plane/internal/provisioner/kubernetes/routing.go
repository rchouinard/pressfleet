package kubernetes

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/rchouinard/pressfleet/control-plane/internal/ingress"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
)

// Routing and maintenance mode (#217, design doc §5.1, §5.2, §5.4, §4.8).
//
// A site's routes are IngressRoutes and Middlewares in its namespace, named
// as on Compose (package ingress) and rendered from templates/k8s/routes.yaml.tmpl.
// They are separate objects from the Deployment, so changing domains
// changes no pod: Traefik's CRD provider picks the objects up within a
// moment.
//
// Maintenance routes can't be in the site's namespace. Traefik runs with
// allowCrossNamespace: false, so a route there can't name the maintenance
// page's Service in the platform namespace, and running a maintenance page
// in every site namespace would multiply a Deployment and a NetworkPolicy
// hole by the number of sites. Instead the twins are IngressRoutes in the
// platform namespace, next to the Service, where the control plane's Role
// lets it manage them (the chart). They hold only host names and the
// maintenance page's address: no site data, and no way to reach a site's
// pod. RenderSite writes them to MaintenanceFile in the site's directory;
// SetMaintenance applies or deletes them, and they carry MaintenanceLabel
// so the driver finds them without remembering a list. Deleting the site's
// namespace doesn't delete them, so DeleteSite does.

// MaintenanceFile is the site's maintenance routes, which RenderSite writes
// into the site's directory (not its manifest directory: they belong to
// the platform namespace).
const MaintenanceFile = "kubernetes-maintenance.yaml"

// MaintenanceLabel marks a site's maintenance routes in the platform
// namespace, with the site's ID as the value.
const MaintenanceLabel = "pressfleet.io/maintenance-for"

// ErrMaintenanceUnavailable means the site can't be put into maintenance
// mode: its maintenance routes were never rendered, or the cluster doesn't
// have Traefik's CRDs.
var ErrMaintenanceUnavailable = errors.New("kubernetes: maintenance mode is unavailable")

var ingressRouteKind = schema.GroupVersionKind{Group: "traefik.io", Version: "v1alpha1", Kind: "IngressRoute"}

// redirectRegex matches any URL Traefik sees and captures its path and
// query, which a redirect to the primary domain keeps (as on Compose).
const redirectRegex = `^https?://[^/]+(.*)$`

// hostname matches the lowercase DNS names a router rule may contain; it
// keeps anything that could break out of a Host(`...`) matcher out of the
// routes.
var hostname = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

func validHostname(h string) bool { return len(h) <= 253 && hostname.MatchString(h) }

// routeData is one IngressRoute, as the templates see it.
type routeData struct {
	Name         string
	Rule         string
	CertResolver string
	Priority     int
	Middlewares  []string
	// Domain marks a custom domain's router.
	Domain bool
}

type authData struct {
	// Name is both the middleware's and its Secret's.
	Name string
	// Users is the htpasswd line: only the bcrypt hash of the password.
	Users string
}

type redirectData struct {
	Name, Regex, Replacement string
}

// routingData is what routes.yaml.tmpl and maintenance-routes.tmpl see.
type routingData struct {
	EntryPoint string
	Routes     []routeData
	NoIndex    string
	Auth       *authData
	// HSTS is the HSTS middleware's name, empty if the security set's
	// headers are off (§6.1).
	HSTS       string
	HSTSMaxAge int
	Redirects  []redirectData
}

// maintenanceData is the maintenance twins of the site's routers.
type maintenanceData struct {
	Service string
	Port    int
	Routes  []routeData
}

// siteRouting derives the site's routes from its configuration. Its list of
// routers is ingress.SiteRouters, the Compose driver's too: the preview domain's, then, once
// the site is live, the primary domain's and one per alias, in the order of
// SiteConfig.Aliases, and the preview redirect that takes the preview
// domain over. hsts adds the HSTS middleware to the custom domains (§6.1).
func siteRouting(site *provisioner.SiteConfig, hsts bool) (routingData, maintenanceData, error) {
	if err := validateRouting(site); err != nil {
		return routingData{}, maintenanceData{}, fmt.Errorf("%w: %w", ErrInvalidSite, err)
	}
	name := func(suffixes ...string) string { return ingress.SiteName(site.ID, suffixes...) }

	r := routingData{EntryPoint: ingress.EntryPointHTTPS, NoIndex: name(ingress.SuffixNoIndex)}
	if a := site.PreviewAuth; a != nil {
		r.Auth = &authData{Name: name(ingress.SuffixPreviewAuth), Users: a.Username + ":" + a.PasswordHash}
	}
	var routers []routeData
	for _, rt := range ingress.SiteRouters(site, hsts) {
		routers = append(routers, routeData{
			Name:         rt.Name,
			Rule:         rt.Rule,
			CertResolver: rt.CertResolver,
			Middlewares:  rt.Middlewares,
			Domain:       rt.Domain,
		})
	}
	if hsts && site.PrimaryDomain != "" {
		r.HSTS, r.HSTSMaxAge = name(ingress.SuffixHSTS), ingress.HSTSMaxAge
	}
	preview := ingress.PreviewMiddlewares(site)

	// Maintenance twins mirror the routers above only: the preview redirect
	// is shadowed by the twin of the preview router, which has a higher
	// priority.
	m := maintenanceData{Service: ingress.MaintenanceHost, Port: ingress.MaintenancePort}
	for _, rt := range routers {
		m.Routes = append(m.Routes, routeData{
			Name:         rt.Name + "-" + ingress.SuffixMaintenance,
			Rule:         rt.Rule,
			CertResolver: rt.CertResolver,
			Priority:     ingress.MaintenancePriority,
		})
	}

	if site.PrimaryDomain != "" {
		// The preview domain now redirects to the primary domain, behind the
		// preview router's own middlewares, so a protected preview domain
		// asks for the password before it reveals where the site went.
		routers = append(routers, routeData{
			Name:         name(ingress.SuffixPreviewRedirect),
			Rule:         ingress.HostRule(site.PreviewDomain),
			CertResolver: ingress.CertResolverPreview,
			Priority:     ingress.PreviewRedirectPriority,
			Middlewares:  append(slices.Clone(preview), name(ingress.SuffixPreviewRedirect)),
		})
		replacement := "https://" + site.PrimaryDomain + "${1}"
		r.Redirects = []redirectData{
			{Name: name(ingress.SuffixPreviewRedirect), Regex: redirectRegex, Replacement: replacement},
			{Name: name(ingress.SuffixAliasRedirect), Regex: redirectRegex, Replacement: replacement},
		}
	}
	r.Routes = routers
	return r, m, nil
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

// renderMaintenance writes the site's maintenance routes into its
// directory, unless they are unchanged.
func renderMaintenance(siteDir string, data manifestData) error {
	tmpl, err := siteTemplates()
	if err != nil {
		return fmt.Errorf("parsing site templates: %w", err)
	}
	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, "maintenance-routes.tmpl", data); err != nil {
		return fmt.Errorf("rendering maintenance-routes.tmpl: %w", err)
	}
	if _, err := DecodeManifests(buf.Bytes()); err != nil {
		return fmt.Errorf("rendering maintenance-routes.tmpl: %w", err)
	}
	return writeIfChanged(filepath.Join(siteDir, MaintenanceFile), buf.Bytes())
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
		err = p.applyMaintenance(ctx, siteID)
	} else {
		err = p.removeMaintenance(ctx, siteID, nil)
	}
	if err != nil {
		return fmt.Errorf("set maintenance of site %s: %w", siteID, err)
	}
	p.log.InfoContext(ctx, "site maintenance mode changed", "site_id", siteID, "enabled", enabled)
	return nil
}

// applyMaintenance applies the site's rendered maintenance routes to the
// platform namespace and deletes twins of routers the site no longer has.
func (p *Provisioner) applyMaintenance(ctx context.Context, siteID string) error {
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
	objs, err := DecodeManifests(content)
	if err != nil {
		return err
	}
	if err := p.Apply(ctx, objs); err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("%w: Traefik's CRDs aren't installed: %w", ErrMaintenanceUnavailable, err)
		}
		return err
	}
	keep := map[string]bool{}
	for _, o := range objs {
		keep[o.GetName()] = true
	}
	return p.removeMaintenance(ctx, siteID, keep)
}

// removeMaintenance deletes the site's maintenance routes from the platform
// namespace, except those named in keep. Any site can have them removed.
func (p *Provisioner) removeMaintenance(ctx context.Context, siteID string, keep map[string]bool) error {
	res := p.resource(kinds[ingressRouteKind], p.platformNamespace)
	list, err := res.List(ctx, metav1.ListOptions{LabelSelector: MaintenanceLabel + "=" + siteID})
	if apierrors.IsNotFound(err) {
		return nil // no CRDs, so no routes
	} else if err != nil {
		return fmt.Errorf("listing maintenance routes: %w", err)
	}
	for _, item := range list.Items {
		if keep[item.GetName()] {
			continue
		}
		if err := res.Delete(ctx, item.GetName(), metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("deleting maintenance route %s/%s: %w", p.platformNamespace, item.GetName(), err)
		}
	}
	return nil
}

// maintenanceBeforeApply puts a site that is in maintenance mode on the
// maintenance page before its pod is replaced, so visitors see the page
// rather than errors.
func (p *Provisioner) maintenanceBeforeApply(ctx context.Context, cfg *provisioner.SiteConfig) error {
	if !cfg.Maintenance {
		return nil
	}
	if err := p.applyMaintenance(ctx, cfg.ID); err != nil {
		return fmt.Errorf("maintenance mode: %w", err)
	}
	return nil
}

// maintenanceAfterApply takes a site that isn't in maintenance mode off the
// page once its pod is ready, which also repairs routes left by a failed
// job.
func (p *Provisioner) maintenanceAfterApply(ctx context.Context, cfg *provisioner.SiteConfig) error {
	if cfg.Maintenance {
		return nil
	}
	if err := p.removeMaintenance(ctx, cfg.ID, nil); err != nil {
		return fmt.Errorf("maintenance mode: %w", err)
	}
	return nil
}
