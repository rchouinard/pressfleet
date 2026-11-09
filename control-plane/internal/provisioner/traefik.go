package provisioner

import "context"

// TraefikMetricsReader reads Traefik's Prometheus metrics. It is its own
// interface, outside [Driver], so that the metrics collector is the only
// thing that needs it.
//
// They name every site and its traffic, so no site may read them. On
// Compose, Traefik serves them on a loopback-only entry point, because it
// is on every site's network, and the driver fetches them from inside the
// Traefik container. On Kubernetes the entry point is on the pod network
// behind a NetworkPolicy that admits the control plane alone, and the
// driver reads it over the network, with no exec into Traefik's pod.
type TraefikMetricsReader interface {
	// TraefikMetrics returns the body of Traefik's /metrics endpoint in
	// the Prometheus text exposition format.
	TraefikMetrics(ctx context.Context) ([]byte, error)
}

// TraefikServiceNamer is implemented by a driver whose Traefik names a
// site's services differently from the Docker provider's pf-<site-id>@docker
// (the Kubernetes driver's come from Traefik's HTTP provider, @http).
// The traffic collector maps Traefik's per-service series to sites with it,
// summing a site's services. It is optional: without it the collector
// assumes the Docker names.
type TraefikServiceNamer interface {
	// TraefikServiceSite returns the ID of the site that serves the
	// Traefik service with this name (the metrics' service label,
	// provider suffix included), or false if it is no site's. It does
	// not check that the site exists.
	TraefikServiceSite(service string) (siteID string, ok bool)
}
