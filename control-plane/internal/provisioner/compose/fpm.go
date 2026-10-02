package compose

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
)

// fpmTimeout bounds one FPMStatus call, including the docker CLI.
const fpmTimeout = 15 * time.Second

// ErrFPMStatus means the php container did not return a usable PHP-FPM
// status.
var ErrFPMStatus = errors.New("compose: no PHP-FPM status from the site")

// fpmScript asks PHP-FPM's status page over FastCGI, as the container's
// health check asks for its ping (images/site-php/healthcheck.sh), then
// prints pm.max_children, which the status page lacks, from the
// container's environment. Going through FastCGI in the php container needs
// neither nginx's status port nor an address the control plane can reach.
const fpmScript = `SCRIPT_NAME=/fpm-status SCRIPT_FILENAME=/fpm-status REQUEST_METHOD=GET QUERY_STRING=json ` +
	`cgi-fcgi -bind -connect 127.0.0.1:9000
printf '\nmax_children=%s\n' "${PHP_FPM_MAX_CHILDREN:-}"`

var fpmMaxChildren = regexp.MustCompile(`(?m)^max_children=(\d*)$`)

// FPMStatus reads the site's PHP-FPM status from its php container
// (provisioner.FPMStatusReader). It runs as the site's UID, like WP-CLI.
func (p *Provisioner) FPMStatus(ctx context.Context, siteID string) (*provisioner.FPMStatus, error) {
	if err := validSiteID(siteID); err != nil {
		return nil, fmt.Errorf("read PHP-FPM status: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, fpmTimeout)
	defer cancel()
	user, err := p.phpUser(ctx, siteID)
	if err != nil {
		return nil, fmt.Errorf("read PHP-FPM status: %w", err)
	}
	deadline, _ := ctx.Deadline()
	proj := p.project(siteID)
	proj.Docker = p.wpcliDocker
	out, err := proj.run(ctx, nil, fpmArgv(user, time.Until(deadline))...)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, fmt.Errorf("read PHP-FPM status of site %s: %w", siteID, ctxErr)
		}
		var ce *CommandError
		if errors.As(err, &ce) {
			return nil, fmt.Errorf("read PHP-FPM status of site %s: %w: %s", siteID, ErrFPMStatus, ce.Stderr)
		}
		return nil, fmt.Errorf("read PHP-FPM status of site %s: %w", siteID, err)
	}
	st, err := parseFPMStatus(out)
	if err != nil {
		return nil, fmt.Errorf("read PHP-FPM status of site %s: %w", siteID, err)
	}
	return st, nil
}

// fpmArgv is the `docker compose` argv, after the project flags, that runs
// fpmScript as user, killed after timeout like WP-CLI (wpCLIArgv).
func fpmArgv(user string, timeout time.Duration) []string {
	argv := wpCLIArgv(user, timeout, nil)
	// wpCLIArgv ends with the WP-CLI path; run the shell instead.
	argv = argv[:len(argv)-1]
	return append(argv, "sh", "-c", fpmScript)
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
