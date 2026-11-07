package kubernetes

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/rchouinard/pressfleet/control-plane/internal/ingress"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
)

var (
	_ provisioner.FPMStatusReader      = (*Provisioner)(nil)
	_ provisioner.TraefikMetricsReader = (*Provisioner)(nil)
)

const (
	// fpmTimeout bounds one FPMStatus call.
	fpmTimeout = 15 * time.Second
	// traefikMetricsTimeout bounds one TraefikMetrics call.
	traefikMetricsTimeout = 15 * time.Second
	// traefikMaxOutput bounds Traefik's metrics: a series per site and
	// status class and a histogram per site.
	traefikMaxOutput = 16 << 20
	// TraefikContainer is the name of Traefik's container in the chart.
	TraefikContainer = "traefik"
	// traefikPodSelector selects Traefik's pods (charts/pressfleet).
	traefikPodSelector = "app.kubernetes.io/name=pressfleet,app.kubernetes.io/component=traefik"
)

// ErrFPMStatus means the php container did not return a usable PHP-FPM
// status.
var ErrFPMStatus = errors.New("kubernetes: no PHP-FPM status from the site")

// ErrTraefikMetrics means Traefik's pod did not return its metrics.
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

// TraefikMetrics reads Traefik's Prometheus metrics from inside its pod
// (provisioner.TraefikMetricsReader). The endpoint listens on the pod's
// loopback only (security review F5), so no other pod can reach it. With
// several Traefik pods it reads the first running one.
func (p *Provisioner) TraefikMetrics(ctx context.Context) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, traefikMetricsTimeout)
	defer cancel()
	pods, err := p.typed.CoreV1().Pods(p.platformNamespace).List(ctx, metav1.ListOptions{LabelSelector: traefikPodSelector})
	if err != nil {
		return nil, fmt.Errorf("read Traefik metrics: %w", err)
	}
	var target *corev1.Pod
	for i := range pods.Items {
		if containerRunning(&pods.Items[i], TraefikContainer) {
			target = &pods.Items[i]
			break
		}
	}
	if target == nil {
		return nil, fmt.Errorf("read Traefik metrics: %w: no running Traefik pod in namespace %s", ErrTraefikMetrics, p.platformNamespace)
	}
	t := execTarget{Namespace: target.Namespace, Pod: target.Name, Container: TraefikContainer}
	// The wget the Traefik image ships.
	cmd := []string{"wget", "-q", "-O-", "http://127.0.0.1:" + strconv.Itoa(ingress.InternalPort) + "/metrics"}
	res, err := p.run(ctx, t, cmd, traefikMaxOutput)
	if err != nil {
		var ee *provisioner.ExecError
		if errors.As(err, &ee) {
			return nil, fmt.Errorf("read Traefik metrics: %w: %s", ErrTraefikMetrics, ee.Stderr)
		}
		return nil, fmt.Errorf("read Traefik metrics: %w", err)
	}
	if res.Truncated {
		return nil, fmt.Errorf("read Traefik metrics: %w: more than %d bytes", ErrOutputTruncated, traefikMaxOutput)
	}
	return res.Stdout, nil
}
