// Package ingress holds what ties the platform's Traefik edge router
// (design doc §3, §6) to the rest of PressFleet: the names of its entry
// points, certificate resolvers, and site routes, and the DNS provider
// credentials of the preview wildcard certificate. The platform package
// renders Traefik with these names; the Compose driver puts them in site
// containers' labels. It imports only package redact, so both can use it.
//
// A container is routed only if it carries PlatformLabel set to the
// platform's Compose project name (config.Config.PlatformProject) and
// traefik.enable=true.
//
// # Site routes
//
// Every Traefik object a site defines is named after the site's Compose
// project, pf-<site-id> ([SiteService], [SiteName]), so names never collide
// between sites and say which site they belong to. The site's nginx
// container carries them as labels (see the Compose driver's routing.go):
//
//	pressfleet.platform=<platform project>
//	traefik.enable=true
//	traefik.docker.network=pf-<site-id>
//	traefik.http.services.pf-<site-id>.loadbalancer.server.port=8080
//	traefik.http.routers.pf-<site-id>-preview.rule=Host(`<slug>.<preview base domain>`)
//	traefik.http.routers.pf-<site-id>-preview.entrypoints=websecure
//	traefik.http.routers.pf-<site-id>-preview.tls.certresolver=acme-preview
//	traefik.http.routers.pf-<site-id>-preview.service=pf-<site-id>
//	traefik.http.routers.pf-<site-id>-preview.middlewares=pf-<site-id>-noindex
//	traefik.http.middlewares.pf-<site-id>-noindex.headers.customresponseheaders.X-Robots-Tag=noindex
//
// The preview domain has a router of its own ([SuffixPreview]) that no
// other domain shares. Middlewares meant for the preview domain alone
// (noindex, [SuffixNoIndex]; the optional basic auth, [SuffixPreviewAuth])
// are attached to that router only, so production domains never get them.
// Go-live (#45) and aliases (#46) add routers of their own and must keep to
// this:
//
//   - The primary domain and aliases get a router named with
//     [SuffixPrimary] (one router whose rule ORs their Host matchers, or
//     one per domain with a further suffix), using CertResolverHTTP and the
//     HSTS middleware ([SuffixHSTS]), and never a preview middleware.
//   - Once a site is live, its preview router redirects to the primary
//     domain ([SuffixPreviewRedirect], a redirectRegex middleware) instead
//     of serving the site. The preview middlewares stay in front of the
//     redirect, so a protected preview domain doesn't reveal the primary
//     domain to someone without its password.
//   - Every router is mirrored by a maintenance twin while the site is in
//     maintenance mode (see MaintenanceService), so a new router must be in
//     the list the driver mirrors.
package ingress

import "strings"

// PlatformLabel is the container label Traefik's Docker provider is
// constrained to. Its value must be the platform's Compose project name, so
// platforms sharing a Docker engine never route each other's containers.
const PlatformLabel = "pressfleet.platform"

// Traefik entry points.
const (
	// EntryPointHTTP listens on port 80. It redirects every request to
	// HTTPS, except ACME HTTP-01 challenges, which Traefik answers first.
	EntryPointHTTP = "web"
	// EntryPointHTTPS listens on port 443 (TCP for HTTP/1.1 and HTTP/2, UDP
	// for HTTP/3). Routers that name no entry point use it.
	EntryPointHTTPS = "websecure"
	// EntryPointInternal serves Traefik's ping endpoint and Prometheus
	// metrics (/metrics) on InternalPort. It isn't published on the host.
	EntryPointInternal = "traefik"
	// InternalPort is EntryPointInternal's port inside the Traefik container.
	InternalPort = 8082
)

// CertResolverHTTP issues one certificate per host name with ACME HTTP-01.
// Routers for the dashboard domain and for sites' primary domains and
// aliases use it.
const CertResolverHTTP = "acme-http"

// CertResolverPreview issues certificates for preview domains. With a DNS
// provider configured (config.Config.ACME.DNSProvider) it holds one
// wildcard certificate for *.<preview base domain>, obtained with DNS-01
// when Traefik starts, and Traefik serves it for every preview domain
// without issuing anything per site: before issuing, Traefik checks whether
// a certificate it already has covers the host. Without a DNS provider it
// issues one certificate per preview domain with HTTP-01, like
// CertResolverHTTP. Preview routers always name this resolver, so they
// don't depend on the mode.
const CertResolverPreview = "acme-preview"

// PreviewWildcardRouter is the router, defined in Traefik's file provider,
// that makes Traefik obtain the preview wildcard certificate at startup
// when a DNS provider is configured. It routes the bare preview base domain,
// which no site uses, to nothing.
const PreviewWildcardRouter = "pf-preview-wildcard"

// TraefikService is the Traefik service of the platform Compose project.
// Its container is platform.TraefikContainer; the Compose driver connects
// it to site networks under compose.TraefikAlias.
const TraefikService = "traefik"

// Suffixes of the names of a site's Traefik routers and middlewares (see
// [SiteName]). Names that #45 and #46 will use are reserved here so the
// scheme is decided in one place.
const (
	// SuffixPreview names the router of the preview domain.
	SuffixPreview = "preview"
	// SuffixNoIndex names the middleware that sends X-Robots-Tag: noindex
	// on the preview domain (§5.1).
	SuffixNoIndex = "noindex"
	// SuffixPreviewAuth names the preview domain's optional basicAuth
	// middleware (§5.1).
	SuffixPreviewAuth = "preview-auth"
	// SuffixPrimary names the router of the primary domain and aliases
	// (#45, #46).
	SuffixPrimary = "primary"
	// SuffixHSTS names the primary router's HSTS middleware (§6.1).
	SuffixHSTS = "hsts"
	// SuffixPreviewRedirect names the middleware that redirects the
	// preview domain to the primary domain after go-live (§5.2).
	SuffixPreviewRedirect = "preview-redirect"
)

// SiteService is the Traefik service of a site's nginx container. It has
// the name of the site's Compose project, pf-<site-id>.
func SiteService(siteID string) string { return "pf-" + siteID }

// SiteName names one of a site's Traefik routers or middlewares,
// pf-<site-id>-<suffix>..., e.g. pf-<site-id>-preview.
func SiteName(siteID string, suffixes ...string) string {
	return strings.Join(append([]string{SiteService(siteID)}, suffixes...), "-")
}

// NginxPort is the port of a site's nginx container that Traefik forwards
// requests to (images/README.md).
const NginxPort = 8080

// Maintenance mode (design doc §5.4). The platform runs a maintenance page
// service (package maintenance) that Traefik knows as MaintenanceService
// from its file provider. While a site is in maintenance mode, the Compose
// driver writes a file there with a twin of each of the site's routers:
// the same rule, TLS, and entry point, named router name plus
// SuffixMaintenance, with MaintenancePriority so it wins over the router
// it mirrors, and MaintenanceService as its service. The twins don't
// depend on the site's containers, so the page stays up while they are
// stopped or recreated, and removing the file restores the site's own
// routers at once.
const (
	// MaintenanceService is the Traefik service of the maintenance page,
	// defined by the platform in Traefik's file provider.
	MaintenanceService = "pf-maintenance"
	// MaintenanceHost and MaintenancePort are where Traefik reaches the
	// maintenance page service on the platform network (its Compose
	// service name).
	MaintenanceHost = "maintenance"
	MaintenancePort = 8080
	// SuffixMaintenance is appended to a router's name to name its
	// maintenance twin.
	SuffixMaintenance = "maintenance"
	// MaintenancePriority is the priority of maintenance routers. Traefik
	// gives routers without one the length of their rule, so this is above
	// every site router.
	MaintenancePriority = 100000
)
