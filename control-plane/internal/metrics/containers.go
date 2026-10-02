package metrics

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
)

// Container metric names. The label "container" holds the site component
// ("php", "nginx"), which is what the dashboard queries group by.
const (
	MetricContainerCPUSeconds = "pressfleet_container_cpu_usage_seconds_total"
	MetricContainerMemory     = "pressfleet_container_memory_usage_bytes"
	MetricContainerRestarts   = "pressfleet_container_restarts_total"
)

// LabelContainer is the label naming a site's component.
const LabelContainer = "container"

// ContainerCollector reports CPU time, memory, and restarts of each site's
// containers, from the driver's stats (§7.1 Resources). Platform containers
// (Traefik, MariaDB, ...) are not covered: the driver samples sites only.
type ContainerCollector struct {
	// Stats samples a site's components.
	Stats provisioner.StatsReader
	// Every is the collection interval; zero means the service default.
	Every time.Duration
}

var _ Collector = ContainerCollector{}

// Name implements [Collector].
func (ContainerCollector) Name() string { return "container_stats" }

// Interval implements [Collector].
func (c ContainerCollector) Interval() time.Duration { return c.Every }

// Collect implements [Collector]. A site that fails doesn't stop the
// others: its error is joined into the result, with the samples of the
// sites that worked. A site the driver doesn't know (still being
// provisioned, say) has no containers yet and is skipped silently.
func (c ContainerCollector) Collect(ctx context.Context, targets []Target) ([]Sample, error) {
	var out []Sample
	var errs []error
	for _, t := range targets {
		if ctx.Err() != nil {
			return out, ctx.Err()
		}
		stats, err := c.Stats.Stats(ctx, t.SiteID)
		if errors.Is(err, provisioner.ErrSiteNotFound) {
			continue
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("site %s: %w", t.SiteID, err))
			continue
		}
		for _, s := range stats {
			comp := string(s.Component)
			out = append(out,
				t.Sample(MetricContainerCPUSeconds, s.CPUUsage.Seconds(), LabelContainer, comp),
				t.Sample(MetricContainerMemory, float64(s.MemoryBytes), LabelContainer, comp),
				t.Sample(MetricContainerRestarts, float64(s.Restarts), LabelContainer, comp),
			)
		}
	}
	return out, errors.Join(errs...)
}
