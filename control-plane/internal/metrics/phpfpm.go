package metrics

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
)

// PHP-FPM metric names. The alert rules (php_fpm_saturation) and the metrics
// API's PHP charts read them.
const (
	MetricPHPFPMActive             = "pressfleet_site_php_fpm_active_processes"
	MetricPHPFPMIdle               = "pressfleet_site_php_fpm_idle_processes"
	MetricPHPFPMMaxChildren        = "pressfleet_site_php_fpm_max_children"
	MetricPHPFPMMaxChildrenReached = "pressfleet_site_php_fpm_max_children_reached_total"
	MetricPHPFPMSlowRequests       = "pressfleet_site_php_fpm_slow_requests_total"
)

// PHPFPMCollector reports each site's PHP-FPM pool: active and idle workers,
// the pool's limit, and the counts of times it ran out of workers and of
// slow requests (§7.1 PHP).
//
// The driver reads the status page ([provisioner.FPMStatusReader]); the two
// counters are PHP-FPM's own, reported as they are, and reset when the
// site's php container restarts. A site that is not running reports
// nothing.
type PHPFPMCollector struct {
	// Status reads a site's PHP-FPM status.
	Status provisioner.FPMStatusReader
	// Every is the collection interval; zero means the service default.
	Every time.Duration
}

var _ Collector = PHPFPMCollector{}

// Name implements [Collector].
func (PHPFPMCollector) Name() string { return "php_fpm" }

// Interval implements [Collector].
func (c PHPFPMCollector) Interval() time.Duration { return c.Every }

// Collect implements [Collector].
func (c PHPFPMCollector) Collect(ctx context.Context, targets []Target) ([]Sample, error) {
	var out []Sample
	var errs []error
	for _, t := range targets {
		if err := ctx.Err(); err != nil {
			errs = append(errs, err)
			break
		}
		st, err := c.Status.FPMStatus(ctx, t.SiteID)
		if err != nil {
			// A stopped or just-deleted site has no pool to report on.
			if !errors.Is(err, provisioner.ErrSiteNotFound) && !errors.Is(err, provisioner.ErrSiteNotRunning) {
				errs = append(errs, fmt.Errorf("site %s: %w", t.SiteID, err))
			}
			continue
		}
		out = append(out,
			t.Sample(MetricPHPFPMActive, float64(st.ActiveProcesses)),
			t.Sample(MetricPHPFPMIdle, float64(st.IdleProcesses)),
			t.Sample(MetricPHPFPMMaxChildrenReached, float64(st.MaxChildrenReached)),
			t.Sample(MetricPHPFPMSlowRequests, float64(st.SlowRequests)),
		)
		// Without a limit the saturation ratio would divide by zero.
		if st.MaxChildren > 0 {
			out = append(out, t.Sample(MetricPHPFPMMaxChildren, float64(st.MaxChildren)))
		}
	}
	return out, errors.Join(errs...)
}
