package compose

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
)

// probeTimeout bounds one ProbeHTTP call, including the docker CLI.
const probeTimeout = 45 * time.Second

// probeScript sends one HTTP request to the site's nginx, on the site
// network by its service name, and prints the response status. It runs
// with the PHP CLI in the php container: the php container is on the site
// network, which nginx trusts as a proxy (NGINX_TRUSTED_PROXIES=local,
// images/README.md), so X-Forwarded-Proto: https counts, as it does for
// Traefik. A raw socket needs no allow_url_fopen and reads nothing beyond
// the status line. The host and path arrive as argv, never as code.
const probeScript = `error_reporting(0);
[, $host, $path] = $argv;
$f = fsockopen('` + string(provisioner.ComponentNginx) + `', 8080, $errno, $errstr, 10);
if (!$f) { fwrite(STDERR, "connecting to nginx: $errstr\n"); exit(1); }
stream_set_timeout($f, 30);
fwrite($f, "GET $path HTTP/1.1\r\nHost: $host\r\nX-Forwarded-Proto: https\r\nUser-Agent: PressFleet-HealthCheck\r\nConnection: close\r\n\r\n");
$line = fgets($f);
if ($line === false || !preg_match('#^HTTP/[0-9.]+ ([0-9]{3})#', $line, $m)) { fwrite(STDERR, "nginx sent no HTTP response\n"); exit(1); }
echo $m[1];
`

var (
	// probeHost is a host name without a port; ingress validates domains
	// fully before they reach a site.
	probeHost = regexp.MustCompile(`^[A-Za-z0-9.-]{1,253}$`)
	// probePath is an origin-form request target of printable ASCII.
	probePath = regexp.MustCompile(`^/[!-~]{0,1000}$`)
)

// ErrProbeFailed means the probe got no HTTP response from the site's
// nginx.
var ErrProbeFailed = errors.New("compose: no HTTP response from the site")

// ProbeHTTP sends req to the site's nginx from its php container
// (provisioner.Prober). It runs as the site's UID, like WP-CLI.
func (p *Provisioner) ProbeHTTP(ctx context.Context, siteID string, req provisioner.ProbeRequest) (*provisioner.ProbeResponse, error) {
	if err := validSiteID(siteID); err != nil {
		return nil, fmt.Errorf("probe site: %w", err)
	}
	if !probeHost.MatchString(req.Host) || !probePath.MatchString(req.Path) {
		return nil, fmt.Errorf("probe site %s: %w: host %q, path %q", siteID, ErrInvalidCommand, req.Host, req.Path)
	}
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	user, err := p.phpUser(ctx, siteID)
	if err != nil {
		return nil, fmt.Errorf("probe site: %w", err)
	}
	deadline, _ := ctx.Deadline()
	proj := p.project(siteID)
	proj.Docker = p.wpcliDocker
	out, err := proj.run(ctx, nil, probeArgv(user, time.Until(deadline), req)...)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, fmt.Errorf("probe site %s: %w", siteID, ctxErr)
		}
		var ce *CommandError
		if errors.As(err, &ce) {
			return nil, fmt.Errorf("probe site %s: %w: %s", siteID, ErrProbeFailed, ce.Stderr)
		}
		return nil, fmt.Errorf("probe site %s: %w", siteID, err)
	}
	code, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil || code < 100 || code > 599 {
		return nil, fmt.Errorf("probe site %s: %w: unexpected output %q", siteID, ErrProbeFailed, out)
	}
	return &provisioner.ProbeResponse{StatusCode: code}, nil
}

// probeArgv is the `docker compose` argv, after the project flags, that
// runs the probe as user, killed after timeout (timedArgv).
func probeArgv(user string, timeout time.Duration, req provisioner.ProbeRequest) []string {
	return timedArgv(user, timeout, "php", "-r", probeScript, "--", req.Host, req.Path)
}
