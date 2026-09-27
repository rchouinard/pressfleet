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

// hostRule is a Traefik rule matching any of the hosts.
func hostRule(hosts ...string) string {
	parts := make([]string, len(hosts))
	for i, h := range hosts {
		parts[i] = "Host(`" + h + "`)"
	}
	return strings.Join(parts, " || ")
}

// router is one of a site's Traefik routers, as data both the nginx
// labels and the maintenance routes (maintenance.go) are made from.
type router struct {
	Name         string
	Rule         string
	CertResolver string
	Middlewares  []string
}

// siteRouters returns the site's routers: for now the preview domain's
// alone. Go-live (#45) adds the primary domain's here; everything built
// from this list (labels, maintenance routes) follows.
func siteRouters(site *provisioner.SiteConfig) []router {
	return []router{{
		Name:         ingress.SiteName(site.ID, ingress.SuffixPreview),
		Rule:         hostRule(site.PreviewDomain),
		CertResolver: ingress.CertResolverPreview,
		Middlewares:  []string{ingress.SiteName(site.ID, ingress.SuffixNoIndex)},
	}}
}

// validateRouting checks what the routing labels are made from.
func validateRouting(site *provisioner.SiteConfig) error {
	if !validHostname(site.PreviewDomain) {
		return fmt.Errorf("preview domain %q is not a valid lowercase host name", site.PreviewDomain)
	}
	return nil
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
	for _, r := range siteRouters(site) {
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
