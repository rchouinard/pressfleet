package kubernetes

import (
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
	"github.com/rchouinard/pressfleet/control-plane/internal/wpconfig"
)

// Defaults for Config's WP-CLI settings, which are the Compose driver's.
const (
	DefaultWPCLITimeout   = 10 * time.Minute
	DefaultWPCLIMaxOutput = 8 << 20 // bytes of stdout
)

const (
	// wpcliPath is WP-CLI in the php container (images/README.md).
	wpcliPath = "/usr/local/bin/wp"
	// opTimeout bounds the page cache purge and the code reload.
	opTimeout = 2 * time.Minute
	// probeTimeout bounds one ProbeHTTP call.
	probeTimeout = 45 * time.Second
)

// ExecWPCLI runs WP-CLI in the site pod's php container, as the site's UID
// (the pod's security context), and returns its standard output.
//
// args reach WP-CLI verbatim as its argv: the exec API takes an argument
// vector, so nothing passes through a shell. That vector travels as the
// exec request's URL query, which the API server's audit log records, so
// args must hold no secret (ExecWPCLIInput takes one on standard input);
// the driver keeps them out of its own errors and logs all the same.
//
// The run is bounded by ctx and the configured WP-CLI timeout. Closing an
// exec stream doesn't stop the process, so WP-CLI runs in a process group
// of its own under a small script (provisioner.WPCLIRunArgv), as under
// Compose: the group is signalled, TERM then KILL, when the deadline
// passes, when ctx ends (by a second exec, which delays the return by the
// grace period at most), and when WP-CLI exits. Standard output
// beyond the configured maximum is dropped and reported with
// ErrOutputTruncated. A non-zero exit yields a *provisioner.ExecError
// carrying the exit code and the end of standard error.
func (p *Provisioner) ExecWPCLI(ctx context.Context, siteID string, args []string) (string, error) {
	return p.ExecWPCLIInput(ctx, siteID, args, nil)
}

// ExecWPCLIInput is ExecWPCLI with stdin as WP-CLI's standard input. The
// exec API streams it over the upgraded connection, not in the request's
// URL, and closes it when stdin is drained. A nil stdin requests no input
// stream, and WP-CLI's standard input is empty.
func (p *Provisioner) ExecWPCLIInput(ctx context.Context, siteID string, args []string, stdin io.Reader) (string, error) {
	for _, a := range args {
		if strings.IndexByte(a, 0) >= 0 {
			return "", fmt.Errorf("exec wp-cli on site %s: %w: arguments can't contain NUL bytes", siteID, ErrInvalidCommand)
		}
	}
	ctx, cancel := context.WithTimeout(ctx, p.wpcliTimeout)
	defer cancel()
	t, err := p.sitePod(ctx, siteID)
	if err != nil {
		return "", fmt.Errorf("exec wp-cli: %w", err)
	}
	callID := provisioner.NewWPCLICallID()
	deadline, _ := ctx.Deadline()
	cmd := provisioner.WPCLIRunArgv(callID, time.Until(deadline), append([]string{wpcliPath}, args...)...)
	res, err := p.runInput(ctx, t, cmd, stdin, p.wpcliMaxOutput)
	if err != nil {
		if ctx.Err() != nil {
			p.stopWPCLI(ctx, t, callID)
		}
		return "", fmt.Errorf("exec wp-cli on site %s: %w", siteID, err)
	}
	if res.Truncated {
		return string(res.Stdout), fmt.Errorf("exec wp-cli on site %s: %w to the first %d bytes", siteID, ErrOutputTruncated, p.wpcliMaxOutput)
	}
	return string(res.Stdout), nil
}

// stopWPCLI stops the WP-CLI call whose context has ended, with a second
// exec in its container: closing the first call's stream doesn't signal the
// process (provisioner.WPCLIRunArgv). It outlives ctx, and bounds itself. If
// it fails, the run script's own timer ends the call at its deadline.
func (p *Provisioner) stopWPCLI(ctx context.Context, t execTarget, callID string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), provisioner.WPCLIStopTimeout)
	defer cancel()
	if _, err := p.run(ctx, t, provisioner.WPCLIStopArgv(callID), execMaxOutput); err != nil {
		p.log.WarnContext(ctx, "stopping WP-CLI after its call ended failed", "pod", t.Pod, "error", err)
	}
}

// PurgeCache empties the FastCGI cache that nginx keeps in the pod's cache
// volume, from the php container, which mounts it too. The volume is an
// emptyDir, so a stopped site has no cache and nothing to purge.
func (p *Provisioner) PurgeCache(ctx context.Context, siteID string) error {
	ctx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()
	t, err := p.runningSitePod(ctx, siteID)
	if err != nil {
		return fmt.Errorf("purge cache: %w", err)
	}
	if t.Pod == "" {
		return nil
	}
	cmd := timed(ctx, "find", wpconfig.FastCGICachePath, "-mindepth", "1", "-delete")
	if _, err := p.run(ctx, t, cmd, execMaxOutput); err != nil {
		return fmt.Errorf("purge page cache of site %s: %w", siteID, err)
	}
	p.log.InfoContext(ctx, "site page cache purged", "site_id", siteID)
	return nil
}

// ReloadCode sends SIGUSR2 to PHP-FPM's master, the php container's main
// process (PID 1): it re-executes itself with its listening socket kept
// open, workers finish their requests, and new connections wait in the
// socket's backlog meanwhile, which empties the opcode cache without a
// restart. A site that isn't running has nothing cached.
func (p *Provisioner) ReloadCode(ctx context.Context, siteID string) error {
	ctx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()
	t, err := p.runningSitePod(ctx, siteID)
	if err != nil {
		return fmt.Errorf("reload code: %w", err)
	}
	if t.Pod == "" {
		return nil
	}
	// kill is a shell builtin; the image has no kill(1) binary.
	if _, err := p.run(ctx, t, []string{"sh", "-c", "kill -USR2 1"}, execMaxOutput); err != nil {
		return fmt.Errorf("reload code of site %s: %w", siteID, err)
	}
	p.log.InfoContext(ctx, "site php reloaded", "site_id", siteID)
	return nil
}

// probeScript sends one HTTP request to the pod's nginx over the pod's
// loopback and prints the response status. It runs with the PHP CLI in the
// php container; nginx trusts the pod's loopback as a proxy
// (loopbackCIDR), so X-Forwarded-Proto: https counts, as it does for
// Traefik. A raw socket needs no allow_url_fopen and reads nothing beyond
// the status line. The host and path arrive as argv, never as code.
const probeScript = `error_reporting(0);
[, $host, $path] = $argv;
$f = fsockopen('127.0.0.1', 8080, $errno, $errstr, 10);
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
var ErrProbeFailed = errors.New("kubernetes: no HTTP response from the site")

// ProbeHTTP sends req to the pod's nginx from its php container
// (provisioner.Prober).
func (p *Provisioner) ProbeHTTP(ctx context.Context, siteID string, req provisioner.ProbeRequest) (*provisioner.ProbeResponse, error) {
	if !probeHost.MatchString(req.Host) || !probePath.MatchString(req.Path) {
		return nil, fmt.Errorf("probe site %s: %w: host %q, path %q", siteID, ErrInvalidCommand, req.Host, req.Path)
	}
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	t, err := p.sitePod(ctx, siteID)
	if err != nil {
		return nil, fmt.Errorf("probe site: %w", err)
	}
	res, err := p.run(ctx, t, timed(ctx, "php", "-r", probeScript, "--", req.Host, req.Path), execMaxOutput)
	if err != nil {
		var ee *provisioner.ExecError
		if errors.As(err, &ee) {
			return nil, fmt.Errorf("probe site %s: %w: %s", siteID, ErrProbeFailed, strings.TrimSpace(ee.Stderr))
		}
		return nil, fmt.Errorf("probe site %s: %w", siteID, err)
	}
	code, err := strconv.Atoi(strings.TrimSpace(string(res.Stdout)))
	if err != nil || code < 100 || code > 599 {
		return nil, fmt.Errorf("probe site %s: %w: unexpected output %q", siteID, ErrProbeFailed, res.Stdout)
	}
	return &provisioner.ProbeResponse{StatusCode: code}, nil
}
