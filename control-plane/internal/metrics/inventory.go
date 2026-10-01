package metrics

import (
	"context"
	"time"
)

// InventoryCollector reports one pressfleet_site_info series per site, with
// the site's status and versions as labels. It scrapes nothing, so it
// exercises the pipeline end to end and gives dashboards and the alert
// engine a list of sites to join other metrics against.
type InventoryCollector struct {
	// Every is the collection interval; zero means the service default.
	Every time.Duration
}

var _ Collector = InventoryCollector{}

// Name implements [Collector].
func (InventoryCollector) Name() string { return "inventory" }

// Interval implements [Collector].
func (c InventoryCollector) Interval() time.Duration { return c.Every }

// Collect implements [Collector].
func (InventoryCollector) Collect(_ context.Context, targets []Target) ([]Sample, error) {
	out := make([]Sample, 0, len(targets))
	for _, t := range targets {
		out = append(out, t.Sample("pressfleet_site_info", 1,
			"status", string(t.Site.Status),
			"php_version", t.Site.PHPVersion,
			"wp_version", t.Site.WPCoreVersion,
		))
	}
	return out, nil
}
