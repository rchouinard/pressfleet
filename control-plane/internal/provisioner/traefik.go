package provisioner

import "context"

// TraefikMetricsReader reads Traefik's Prometheus metrics. It is its own
// interface, outside [Driver], so that the metrics collector is the only
// thing that needs it.
//
// Traefik serves them on a loopback-only entry point, because anything on a
// network it can be reached over, a site network included, would read every
// site's traffic. The driver therefore fetches them from inside the Traefik
// container rather than over the network.
type TraefikMetricsReader interface {
	// TraefikMetrics returns the body of Traefik's /metrics endpoint in
	// the Prometheus text exposition format.
	TraefikMetrics(ctx context.Context) ([]byte, error)
}

// TraefikServiceNamer is implemented by a driver whose Traefik names a
// site's service differently from the Docker provider's pf-<site-id>@docker
// (the Kubernetes CRD provider names it after the namespace). The traffic
// collector maps Traefik's per-service series to sites with it. It is
// optional: without it the collector assumes the Docker names.
type TraefikServiceNamer interface {
	// TraefikServiceName returns the name Traefik's metrics give the
	// site's service, provider suffix included.
	TraefikServiceName(siteID string) string
}
