package kubernetes

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
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

// fpmScript asks PHP-FPM's status page over FastCGI, as the container's
// health check asks for its ping (images/site-php/healthcheck.sh), then
// prints pm.max_children, which the status page lacks, from the
// container's environment.
const fpmScript = `SCRIPT_NAME=/fpm-status SCRIPT_FILENAME=/fpm-status REQUEST_METHOD=GET QUERY_STRING=json ` +
	`cgi-fcgi -bind -connect 127.0.0.1:9000
printf '\nmax_children=%s\n' "${PHP_FPM_MAX_CHILDREN:-}"`

var fpmMaxChildren = regexp.MustCompile(`(?m)^max_children=(\d*)$`)

// FPMStatus reads the site's PHP-FPM status from its php container
// (provisioner.FPMStatusReader).
func (p *Provisioner) FPMStatus(ctx context.Context, siteID string) (*provisioner.FPMStatus, error) {
	ctx, cancel := context.WithTimeout(ctx, fpmTimeout)
	defer cancel()
	t, err := p.sitePod(ctx, siteID)
	if err != nil {
		return nil, fmt.Errorf("read PHP-FPM status: %w", err)
	}
	res, err := p.run(ctx, t, timed(ctx, "sh", "-c", fpmScript), execMaxOutput)
	if err != nil {
		var ee *provisioner.ExecError
		if errors.As(err, &ee) {
			return nil, fmt.Errorf("read PHP-FPM status of site %s: %w: %s", siteID, ErrFPMStatus, ee.Stderr)
		}
		return nil, fmt.Errorf("read PHP-FPM status of site %s: %w", siteID, err)
	}
	st, err := parseFPMStatus(res.Stdout)
	if err != nil {
		return nil, fmt.Errorf("read PHP-FPM status of site %s: %w", siteID, err)
	}
	return st, nil
}

// parseFPMStatus reads fpmScript's output: FastCGI response headers, the
// status page's JSON, and the max_children line.
func parseFPMStatus(out []byte) (*provisioner.FPMStatus, error) {
	start := bytes.IndexByte(out, '{')
	if start < 0 {
		return nil, fmt.Errorf("%w: no JSON in %q", ErrFPMStatus, out)
	}
	var raw struct {
		Idle        int `json:"idle processes"`
		Active      int `json:"active processes"`
		MaxReached  int `json:"max children reached"`
		SlowRequest int `json:"slow requests"`
	}
	// A decoder stops after the first value, leaving the trailer unread.
	if err := json.NewDecoder(bytes.NewReader(out[start:])).Decode(&raw); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrFPMStatus, err)
	}
	st := &provisioner.FPMStatus{
		ActiveProcesses: raw.Active, IdleProcesses: raw.Idle,
		MaxChildrenReached: raw.MaxReached, SlowRequests: raw.SlowRequest,
	}
	if m := fpmMaxChildren.FindSubmatch(out); m != nil && len(m[1]) > 0 {
		n, err := strconv.Atoi(string(m[1]))
		if err != nil {
			return nil, fmt.Errorf("%w: max_children %q", ErrFPMStatus, m[1])
		}
		st.MaxChildren = n
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
