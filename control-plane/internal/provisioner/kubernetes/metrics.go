package kubernetes

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/id"
	"github.com/rchouinard/pressfleet/control-plane/internal/ingress"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
)

var (
	_ provisioner.FPMStatusReader      = (*Provisioner)(nil)
	_ provisioner.TraefikMetricsReader = (*Provisioner)(nil)
	_ provisioner.TraefikServiceNamer  = (*Provisioner)(nil)
)

const (
	// fpmTimeout bounds one FPMStatus call.
	fpmTimeout = 15 * time.Second
	// traefikMetricsTimeout bounds one TraefikMetrics call.
	traefikMetricsTimeout = 15 * time.Second
	// traefikMaxOutput bounds Traefik's metrics: a series per site and
	// status class and a histogram per site.
	traefikMaxOutput = 16 << 20
	// TraefikMetricsService and TraefikMetricsPort are the ClusterIP
	// Service in the platform namespace, as the chart names it, of
	// Traefik's metrics entry point. Only the control plane may connect
	// to it (the chart's NetworkPolicy).
	TraefikMetricsService = "traefik-metrics"
	TraefikMetricsPort    = 9100
)

// ErrFPMStatus means the php container did not return a usable PHP-FPM
// status.
var ErrFPMStatus = errors.New("kubernetes: no PHP-FPM status from the site")

// ErrTraefikMetrics means Traefik did not return its metrics.
var ErrTraefikMetrics = errors.New("kubernetes: no metrics from Traefik")

// FPMStatus reads the site's PHP-FPM status from its php container
// (provisioner.FPMStatusReader).
func (p *Provisioner) FPMStatus(ctx context.Context, siteID string) (*provisioner.FPMStatus, error) {
	ctx, cancel := context.WithTimeout(ctx, fpmTimeout)
	defer cancel()
	t, err := p.sitePod(ctx, siteID)
	if err != nil {
		return nil, fmt.Errorf("read PHP-FPM status: %w", err)
	}
	res, err := p.run(ctx, t, timed(ctx, "sh", "-c", provisioner.FPMScript), execMaxOutput)
	if err != nil {
		var ee *provisioner.ExecError
		if errors.As(err, &ee) {
			return nil, fmt.Errorf("read PHP-FPM status of site %s: %w: %s", siteID, ErrFPMStatus, ee.Stderr)
		}
		return nil, fmt.Errorf("read PHP-FPM status of site %s: %w", siteID, err)
	}
	st, err := provisioner.ParseFPMStatus(res.Stdout, ErrFPMStatus)
	if err != nil {
		return nil, fmt.Errorf("read PHP-FPM status of site %s: %w", siteID, err)
	}
	return st, nil
}

// traefikMetricsURL is where the control plane reads Traefik's metrics.
func (p *Provisioner) traefikMetricsURL() string {
	if p.metricsURL != "" {
		return p.metricsURL
	}
	return "http://" + TraefikMetricsService + "." + p.platformNamespace + ".svc:" + strconv.Itoa(TraefikMetricsPort) + "/metrics"
}

// TraefikMetrics reads Traefik's Prometheus metrics over the network
// (provisioner.TraefikMetricsReader), from the entry point the chart gives
// them, behind a NetworkPolicy that admits the control plane alone. It
// needs no exec into Traefik's pod (#322).
func (p *Provisioner) TraefikMetrics(ctx context.Context) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, traefikMetricsTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.traefikMetricsURL(), nil)
	if err != nil {
		return nil, fmt.Errorf("read Traefik metrics: %w", err)
	}
	resp, err := p.metricsClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("read Traefik metrics: %w: %w", ErrTraefikMetrics, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("read Traefik metrics: %w: status %s", ErrTraefikMetrics, resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, traefikMaxOutput+1))
	if err != nil {
		return nil, fmt.Errorf("read Traefik metrics: %w: %w", ErrTraefikMetrics, err)
	}
	if len(body) > traefikMaxOutput {
		return nil, fmt.Errorf("read Traefik metrics: %w: more than %d bytes", ErrOutputTruncated, traefikMaxOutput)
	}
	return body, nil
}

// traefikHTTPProvider is the suffix Traefik's metrics give the services of
// its HTTP provider, which the control plane feeds.
const traefikHTTPProvider = "@http"

// TraefikServiceSite maps a Traefik service to its site
// (provisioner.TraefikServiceNamer). A site's routes all use one service,
// named as on Compose (ingress.SiteService), from Traefik's HTTP provider:
// pf-<site-id>@http. The maintenance page's service is the chart's, from
// the file provider, so requests it answers count for no site, as on
// Compose.
func (p *Provisioner) TraefikServiceSite(service string) (string, bool) {
	rest, ok := strings.CutSuffix(service, traefikHTTPProvider)
	if !ok {
		return "", false
	}
	siteID, ok := strings.CutPrefix(rest, ingress.SiteService(""))
	if !ok || len(siteID) != id.Len || p.validSiteNamespace(siteID) != nil {
		return "", false
	}
	return siteID, true
}
