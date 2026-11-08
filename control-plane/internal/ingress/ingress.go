// Package ingress holds what ties the platform's Traefik edge router
// (design doc §3, §6) to the rest of PressFleet: the names of its entry
// points, certificate resolvers, and site routes, and the DNS provider
// credentials of the preview wildcard certificate. The platform package
// renders Traefik with these names; the Compose driver puts them in site
// containers' labels and route files, the Kubernetes driver in IngressRoutes.
// It imports only package redact and, for [SiteRouters], package provisioner
// (not a driver), so both can use it.
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
//
// # Custom domains
//
// A live site's primary domain and aliases (design doc §5.2) are routed
// from Traefik's file provider rather than from labels: the Compose driver
// writes a file per live site, pf-<site-id>-domains.yaml ([SuffixDomains]),
// into the directory Traefik watches. Traefik applies a changed file within
// a couple of seconds without touching the site's containers, whereas a
// changed label means recreating nginx and a gap in service while the new
// container becomes healthy, so going live, changing the primary domain,
// and adding or removing aliases cause no downtime. The file's routers use
// the service the labels define, pf-<site-id>@docker, so they follow the
// nginx container like the preview router does:
//
//   - pf-<site-id>-primary ([SuffixPrimary]) routes the primary domain with
//     CertResolverHTTP and, when the security set's headers are on (§6.1),
//     the HSTS middleware ([SuffixHSTS]; [HSTSMaxAge], no includeSubDomains).
//     It never has a preview middleware.
//   - pf-<site-id>-alias-<key> ([SuffixAlias], [AliasKey]) routes one alias
//     each, so every alias gets a certificate of its own and one alias whose
//     DNS is wrong can't hold up the others' certificates. They answer with
//     a permanent redirect to the primary domain that keeps the path and
//     query ([SuffixAliasRedirect]), after HSTS when it is on.
//   - pf-<site-id>-preview-redirect ([SuffixPreviewRedirect]) takes over
//     the preview domain from the preview router, at
//     [PreviewRedirectPriority], and redirects it to the primary domain
//     the same way. The preview middlewares stay in front of the redirect
//     (the file defines its own copies), so a protected preview domain
//     doesn't reveal the primary domain to someone without its password.
//
// The middlewares the file's routers use are defined in the same file, so
// it depends on nothing of the site's but its service.
//
// Every router, label or file, is mirrored by a maintenance twin while the
// site is in maintenance mode (see MaintenanceService); the driver builds
// both from one list of the site's routers, so a new router must be in it.
package ingress

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

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
	// metrics (/metrics) on InternalPort, on the loopback address of the
	// Traefik container only. Traefik is on every site's network, so a
	// listener on any other address would be open to every site.
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
// [SiteName]), so the scheme is decided in one place.
const (
	// SuffixPreview names the router of the preview domain.
	SuffixPreview = "preview"
	// SuffixNoIndex names the middleware that sends X-Robots-Tag: noindex
	// on the preview domain (§5.1).
	SuffixNoIndex = "noindex"
	// SuffixPreviewAuth names the preview domain's optional basicAuth
	// middleware (§5.1).
	SuffixPreviewAuth = "preview-auth"
	// SuffixPrimary names the router of the primary domain (§5.2).
	SuffixPrimary = "primary"
	// SuffixAlias, followed by [AliasKey], names the router of an alias.
	SuffixAlias = "alias"
	// SuffixHSTS names the HSTS middleware of the primary domain and
	// aliases (§6.1).
	SuffixHSTS = "hsts"
	// SuffixPreviewRedirect names the router and the middleware that
	// redirect the preview domain to the primary domain after go-live
	// (§5.2).
	SuffixPreviewRedirect = "preview-redirect"
	// SuffixAliasRedirect names the middleware that redirects aliases to
	// the primary domain.
	SuffixAliasRedirect = "alias-redirect"
	// SuffixDomains names a live site's file of custom domain routes in
	// Traefik's file provider directory (plus ".yaml").
	SuffixDomains = "domains"
)

// PreviewRedirectPriority is the priority of a live site's preview
// redirect router. Traefik gives routers without a priority the length of
// their rule, which for the preview router's rule is far below this, so
// the redirect takes the preview domain over; maintenance routers
// (MaintenancePriority) are above it.
const PreviewRedirectPriority = 10000

// HSTSMaxAge is the max-age, in seconds, of the Strict-Transport-Security
// header on production domains: one year, without includeSubDomains, since
// a client's other subdomains may not serve HTTPS (§6.1).
const HSTSMaxAge = 31536000

// AliasKey is the part of an alias's router name that identifies the
// alias: the first 10 hex digits of the SHA-256 of the domain. Router
// names can't contain dots, and replacing them could make two aliases
// collide (a-b.example and a.b-example); a hash keeps each alias's name
// stable however the list changes.
func AliasKey(domain string) string {
	sum := sha256.Sum256([]byte(domain))
	return hex.EncodeToString(sum[:5])
}

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
