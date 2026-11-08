package provisioner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
)

// FPMStatus is a snapshot of a site's PHP-FPM pool (§7.1 PHP), from
// PHP-FPM's own status page.
type FPMStatus struct {
	// ActiveProcesses and IdleProcesses are the workers serving a request
	// and the ones waiting for one. The request that reads the status is
	// itself served by a worker, so ActiveProcesses is at least 1.
	ActiveProcesses int
	IdleProcesses   int
	// MaxChildren is the pool's limit (pm.max_children). Unlike the other
	// fields it is configuration, which the status page does not report.
	MaxChildren int
	// MaxChildrenReached counts the times the pool hit MaxChildren since
	// the pool started, and SlowRequests the requests that exceeded the
	// slow log timeout. Both are cumulative and reset when PHP-FPM
	// restarts.
	MaxChildrenReached int
	SlowRequests       int
}

// FPMStatusReader reads the PHP-FPM status of a site. It is its own
// interface, outside [Driver], so that the metrics collector is the only
// thing that needs it.
type FPMStatusReader interface {
	// FPMStatus returns the pool's status. A site that does not exist
	// yields ErrSiteNotFound, and one that is not running
	// ErrSiteNotRunning.
	FPMStatus(ctx context.Context, siteID string) (*FPMStatus, error)
}

// FPMScript asks PHP-FPM's status page over FastCGI, as the container's
// health check asks for its ping (images/site-php/healthcheck.sh), then
// prints pm.max_children, which the status page lacks, from the
// container's environment. Going through FastCGI in the php container needs
// neither nginx's status port nor an address the control plane can reach.
// Both drivers run it with sh -c in the site's php container and give the
// output to [ParseFPMStatus].
const FPMScript = `SCRIPT_NAME=/fpm-status SCRIPT_FILENAME=/fpm-status REQUEST_METHOD=GET QUERY_STRING=json ` +
	`cgi-fcgi -bind -connect 127.0.0.1:9000
printf '\nmax_children=%s\n' "${PHP_FPM_MAX_CHILDREN:-}"`

var fpmMaxChildren = regexp.MustCompile(`(?m)^max_children=(\d*)$`)

// ParseFPMStatus reads FPMScript's output: FastCGI response headers, the
// status page's JSON, and the max_children line. Every error wraps
// errStatus, the calling driver's own sentinel, which its callers branch on.
func ParseFPMStatus(out []byte, errStatus error) (*FPMStatus, error) {
	start := bytes.IndexByte(out, '{')
	if start < 0 {
		return nil, fmt.Errorf("%w: no JSON in %q", errStatus, out)
	}
	var raw struct {
		Idle        int `json:"idle processes"`
		Active      int `json:"active processes"`
		MaxReached  int `json:"max children reached"`
		SlowRequest int `json:"slow requests"`
	}
	// A decoder stops after the first value, leaving the trailer unread.
	if err := json.NewDecoder(bytes.NewReader(out[start:])).Decode(&raw); err != nil {
		return nil, fmt.Errorf("%w: %w", errStatus, err)
	}
	st := &FPMStatus{
		ActiveProcesses: raw.Active, IdleProcesses: raw.Idle,
		MaxChildrenReached: raw.MaxReached, SlowRequests: raw.SlowRequest,
	}
	if m := fpmMaxChildren.FindSubmatch(out); m != nil && len(m[1]) > 0 {
		n, err := strconv.Atoi(string(m[1]))
		if err != nil {
			return nil, fmt.Errorf("%w: max_children %q", errStatus, m[1])
		}
		st.MaxChildren = n
	}
	return st, nil
}
