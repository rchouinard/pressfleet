package metrics

import (
	"context"
	"fmt"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/store"
)

// UpdatesCollector reports each site's pending plugin and theme updates,
// read from the state database where package pluginupdates records them
// (#125, design doc §5.6). It scrapes no container.
//
//	pressfleet_site_pending_updates{kind}    gauge, plugins ("plugin") or themes ("theme") with an update available
//
// Both series are absent for a site that hasn't been checked yet. The
// default alert rule updates_pending warns on their sum.
type UpdatesCollector struct {
	// DB is the state database.
	DB store.Querier
	// Every is the collection interval; zero means the service default.
	Every time.Duration
}

var _ Collector = UpdatesCollector{}

// Name implements [Collector].
func (UpdatesCollector) Name() string { return "pending_updates" }

// Interval implements [Collector].
func (c UpdatesCollector) Interval() time.Duration { return c.Every }

// Collect implements [Collector].
func (c UpdatesCollector) Collect(ctx context.Context, targets []Target) ([]Sample, error) {
	counts, err := store.NewUpdateReports(c.DB).Counts(ctx)
	if err != nil {
		return nil, fmt.Errorf("pending updates: %w", err)
	}
	var out []Sample
	for _, t := range targets {
		n, ok := counts[t.SiteID]
		if !ok {
			continue
		}
		out = append(out,
			t.Sample("pressfleet_site_pending_updates", float64(n.Plugins), "kind", store.UpdateKindPlugin),
			t.Sample("pressfleet_site_pending_updates", float64(n.Themes), "kind", store.UpdateKindTheme),
		)
	}
	return out, nil
}
