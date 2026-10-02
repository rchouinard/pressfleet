package provisioner

import "context"

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
