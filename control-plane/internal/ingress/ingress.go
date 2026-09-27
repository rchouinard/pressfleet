// Package ingress holds the names that tie the platform's Traefik edge
// router (design doc §3, §6) to the routes other parts of PressFleet give
// it. The platform package renders Traefik with these names; the Compose
// driver puts them in site containers' labels. Keeping them in one small
// package with no dependencies lets both import it.
//
// A container is routed only if it carries PlatformLabel set to the
// platform's Compose project name (config.Config.PlatformProject) and
// traefik.enable=true. A site router typically looks like:
//
//	pressfleet.platform=pressfleet
//	traefik.enable=true
//	traefik.docker.network=pf-<site-id>
//	traefik.http.routers.<name>.rule=Host(`<host>`)
//	traefik.http.routers.<name>.entrypoints=websecure
//	traefik.http.routers.<name>.tls.certresolver=acme-http
//	traefik.http.services.<name>.loadbalancer.server.port=8080
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

// TraefikService is the Traefik service of the platform Compose project.
// Its container is platform.TraefikContainer; the Compose driver connects
// it to site networks under compose.TraefikAlias.
const TraefikService = "traefik"
