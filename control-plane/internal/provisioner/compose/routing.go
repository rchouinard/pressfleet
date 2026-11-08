package compose

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/rchouinard/pressfleet/control-plane/internal/ingress"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
)

// Routing (#36): the Traefik labels on a site's nginx container that route
// its domains to it. Names follow the scheme in package ingress.

// label is one container label.
type label struct {
	Key, Value string
}

// hostname matches the lowercase DNS names a router rule may contain. It
// keeps anything that could break out of a Host(`...`) matcher, such as a
// backquote, out of the labels.
var hostname = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

func validHostname(h string) bool {
	return len(h) <= 253 && hostname.MatchString(h)
}

// validateRouting checks what the routes are made from. Every domain must
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

// routingLabels returns the Traefik labels of the site's nginx container,
// sorted by key, or none if platformProject is empty.
func routingLabels(platformProject string, site *provisioner.SiteConfig) ([]label, error) {
	if platformProject == "" {
		return nil, nil
	}
	if strings.ContainsAny(platformProject, "`\"'\n") {
		return nil, errors.New("invalid platform project name")
	}
	service := ingress.SiteService(site.ID)
	m := map[string]string{
		ingress.PlatformLabel:    platformProject,
		"traefik.enable":         "true",
		"traefik.docker.network": SiteProjectName(site.ID),
		"traefik.http.services." + service + ".loadbalancer.server.port": strconv.Itoa(ingress.NginxPort),
		// Preview domains stay out of search engines (§5.1).
		"traefik.http.middlewares." + ingress.SiteName(site.ID, ingress.SuffixNoIndex) +
			".headers.customresponseheaders.X-Robots-Tag": "noindex",
	}
	if a := site.PreviewAuth; a != nil {
		// Preview password protection (§5.1): only the bcrypt hash is in
		// the label. The template doubles its "$"s for Compose. The
		// Authorization header is removed before the request reaches
		// WordPress, which would otherwise try it as an application
		// password and refuse REST API requests.
		auth := "traefik.http.middlewares." + ingress.SiteName(site.ID, ingress.SuffixPreviewAuth) + ".basicauth."
		m[auth+"users"] = a.Username + ":" + a.PasswordHash
		m[auth+"removeheader"] = "true"
		m[auth+"realm"] = "Preview"
	}
	for _, r := range ingress.SiteRouters(site, false) {
		if r.Domain {
			// In the domains file, so domain changes don't recreate nginx.
			continue
		}
		prefix := "traefik.http.routers." + r.Name + "."
		m[prefix+"rule"] = r.Rule
		m[prefix+"entrypoints"] = ingress.EntryPointHTTPS
		m[prefix+"tls.certresolver"] = r.CertResolver
		m[prefix+"service"] = service
		if len(r.Middlewares) > 0 {
			m[prefix+"middlewares"] = strings.Join(r.Middlewares, ",")
		}
	}
	labels := make([]label, 0, len(m))
	for k, v := range m {
		labels = append(labels, label{Key: k, Value: v})
	}
	slices.SortFunc(labels, func(a, b label) int { return strings.Compare(a.Key, b.Key) })
	return labels, nil
}
