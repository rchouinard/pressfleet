package compose

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
)

// fpmTimeout bounds one FPMStatus call, including the docker CLI.
const fpmTimeout = 15 * time.Second

// ErrFPMStatus means the php container did not return a usable PHP-FPM
// status.
var ErrFPMStatus = errors.New("compose: no PHP-FPM status from the site")

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
	st, err := provisioner.ParseFPMStatus(out, ErrFPMStatus)
	if err != nil {
		return nil, fmt.Errorf("read PHP-FPM status of site %s: %w", siteID, err)
	}
	return st, nil
}

// fpmArgv is the `docker compose` argv, after the project flags, that runs
// provisioner.FPMScript as user, killed after timeout like WP-CLI (wpCLIArgv).
func fpmArgv(user string, timeout time.Duration) []string {
	argv := wpCLIArgv(user, timeout, nil)
	// wpCLIArgv ends with the WP-CLI path; run the shell instead.
	argv = argv[:len(argv)-1]
	return append(argv, "sh", "-c", provisioner.FPMScript)
}
