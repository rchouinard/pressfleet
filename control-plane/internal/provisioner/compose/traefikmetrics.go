package compose

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/ingress"
)

// traefikMetricsTimeout bounds one TraefikMetrics call, including the
// docker CLI.
const traefikMetricsTimeout = 15 * time.Second

// ErrTraefikMetrics means Traefik's container did not return its metrics.
var ErrTraefikMetrics = errors.New("compose: no metrics from Traefik")

// TraefikMetrics reads Traefik's Prometheus metrics from inside its
// container (provisioner.TraefikMetricsReader). The endpoint listens on
// loopback only (templates/platform/traefik.yaml.tmpl), so no container on a
// site network can reach it, and neither can the control plane over the
// platform network.
func (p *Provisioner) TraefikMetrics(ctx context.Context) ([]byte, error) {
	if p.traefik == "" {
		return nil, fmt.Errorf("read Traefik metrics: %w: no Traefik container configured", ErrTraefikMetrics)
	}
	ctx, cancel := context.WithTimeout(ctx, traefikMetricsTimeout)
	defer cancel()
	out, err := p.docker(ctx, nil, traefikMetricsArgv(p.traefik)...)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, fmt.Errorf("read Traefik metrics: %w", ctxErr)
		}
		var ce *CommandError
		if errors.As(err, &ce) {
			return nil, fmt.Errorf("read Traefik metrics: %w: %s", ErrTraefikMetrics, ce.Stderr)
		}
		return nil, fmt.Errorf("read Traefik metrics: %w", err)
	}
	return out, nil
}

// traefikMetricsArgv is the docker argv that prints the metrics, with the
// wget the Traefik image ships.
func traefikMetricsArgv(container string) []string {
	return []string{
		"container", "exec", container,
		"wget", "-q", "-O-", "http://127.0.0.1:" + strconv.Itoa(ingress.InternalPort) + "/metrics",
	}
}
