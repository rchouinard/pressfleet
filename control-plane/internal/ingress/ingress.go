// Package ingress holds what ties the platform's Traefik edge router
// (design doc §3, §6) to the rest of PressFleet: the names of its entry
// points and certificate resolvers, and the DNS provider credentials of the
// preview wildcard certificate. The platform package renders Traefik with
// these names; the Compose driver puts them in site containers' labels.
// It imports only package redact, so both can use it.
//
// A container is routed only if it carries PlatformLabel set to the
// platform's Compose project name (config.Config.PlatformProject) and
// traefik.enable=true. A site's preview router typically looks like:
//
//	pressfleet.platform=pressfleet
//	traefik.enable=true
//	traefik.docker.network=pf-<site-id>
//	traefik.http.routers.<name>.rule=Host(`<slug>.<preview base domain>`)
//	traefik.http.routers.<name>.entrypoints=websecure
//	traefik.http.routers.<name>.tls.certresolver=acme-preview
//	traefik.http.services.<name>.loadbalancer.server.port=8080
//
// Routers for primary domains and aliases use CertResolverHTTP instead.
package ingress

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
