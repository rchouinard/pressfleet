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
