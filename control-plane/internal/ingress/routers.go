package ingress

import (
	"slices"
	"strings"

	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
)

// Router is one of a site's Traefik routers, as data the drivers render
// into labels, route files, or the routes they serve Traefik. Both drivers
// build them from [SiteRouters], so a router added there reaches every
// driver, its maintenance twins included.
type Router struct {
	Name         string
	Rule         string
	CertResolver string
	Middlewares  []string
	// Domain marks a router of a custom domain, which Compose defines in
	// the domains file rather than in nginx's labels.
	Domain bool
}

// HostRule is a Traefik rule matching any of the hosts. The hosts must have
// been validated: a backquote would end the matcher.
func HostRule(hosts ...string) string {
	parts := make([]string, len(hosts))
	for i, h := range hosts {
		parts[i] = "Host(`" + h + "`)"
	}
	return strings.Join(parts, " || ")
}

// SiteRouters returns the site's routers: the preview domain's, then, once
// the site is live, the primary domain's and one per alias, in the order of
// SiteConfig.Aliases. Everything built from this list follows. hsts adds
// the HSTS middleware to the custom domains' routers (§6.1). It does not
// include the preview redirect, which the drivers add where they define the
// primary domain's redirects.
func SiteRouters(site *provisioner.SiteConfig, hsts bool) []Router {
	routers := []Router{{
		Name:         SiteName(site.ID, SuffixPreview),
		Rule:         HostRule(site.PreviewDomain),
		CertResolver: CertResolverPreview,
		Middlewares:  PreviewMiddlewares(site),
	}}
	if site.PrimaryDomain == "" {
		return routers
	}
	// HSTS is outermost, so the aliases' redirects carry it too.
	var prod []string
	if hsts {
		prod = append(prod, SiteName(site.ID, SuffixHSTS))
	}
	routers = append(routers, Router{
		Name:         SiteName(site.ID, SuffixPrimary),
		Rule:         HostRule(site.PrimaryDomain),
		CertResolver: CertResolverHTTP,
		Middlewares:  prod,
		Domain:       true,
	})
	for _, alias := range site.Aliases {
		routers = append(routers, Router{
			Name:         SiteName(site.ID, SuffixAlias, AliasKey(alias)),
			Rule:         HostRule(alias),
			CertResolver: CertResolverHTTP,
			Middlewares:  append(slices.Clone(prod), SiteName(site.ID, SuffixAliasRedirect)),
			Domain:       true,
		})
	}
	return routers
}

// PreviewMiddlewares are the preview router's middlewares. The first is
// the outermost, so noindex also marks the 401 that basic auth answers
// with.
func PreviewMiddlewares(site *provisioner.SiteConfig) []string {
	mw := []string{SiteName(site.ID, SuffixNoIndex)}
	if site.PreviewAuth != nil {
		mw = append(mw, SiteName(site.ID, SuffixPreviewAuth))
	}
	return mw
}
