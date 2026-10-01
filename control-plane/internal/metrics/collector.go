package metrics

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/store"
)

// Collector gathers metrics for the sites it is given. Implementations
// live next to what they scrape (#68 to #74) and are registered with a
// [Service].
type Collector interface {
	// Name identifies the collector in logs and self-metrics, as a
	// snake_case word such as "docker_stats". It must be unique.
	Name() string
	// Interval is how often Collect runs. Zero or less means the
	// service's default interval.
	Interval() time.Duration
	// Collect scrapes the sources and returns the samples. targets is the
	// current list of sites, fresh for this run. Samples for the sites
	// should be built with [Target.Sample]. Collect must stop when ctx
	// ends; its deadline is the collector's timeout.
	//
	// Returning samples together with an error is allowed, for a
	// collector whose sources fail one site at a time: the samples are
	// written and the run counts as failed.
	Collect(ctx context.Context, targets []Target) ([]Sample, error)
}

// Target is a site to collect metrics for.
type Target struct {
	// SiteID and ClientGroupID are the labels every site sample carries.
	SiteID        string
	ClientGroupID string
	// Site is the whole record, as of the start of the run.
	Site store.Site
}

// Sample builds a site-scoped sample for t, now, with [LabelSiteID] and
// [LabelClientGroupID] set. The sample has no time: the service stamps it
// with the start of the run. labels are extra key-value pairs
// ("component", "php", ...); an odd count panics, as a programming error.
func (t Target) Sample(name string, value float64, labels ...string) Sample {
	if len(labels)%2 != 0 {
		panic(fmt.Sprintf("metrics: sample %s: label list %v has an odd length", name, labels))
	}
	m := make(map[string]string, len(labels)/2+2)
	for pair := range slices.Chunk(labels, 2) {
		m[pair[0]] = pair[1]
	}
	m[LabelSiteID] = t.SiteID
	m[LabelClientGroupID] = t.ClientGroupID
	return Sample{Name: name, Labels: m, Value: value}
}

// TargetSource lists the sites to collect for. Implementations must be
// cheap and safe for concurrent use: the service calls it on every run of
// every collector.
type TargetSource interface {
	Targets(ctx context.Context) ([]Target, error)
}

// SiteTargets is a [TargetSource] over the site records.
type SiteTargets struct {
	sites store.Sites
}

var _ TargetSource = SiteTargets{}

// NewSiteTargets returns the sites in the state database, except those
// being deleted, whose containers are about to disappear.
func NewSiteTargets(q store.Querier) SiteTargets {
	return SiteTargets{sites: store.NewSites(q)}
}

// Targets implements [TargetSource].
func (s SiteTargets) Targets(ctx context.Context) ([]Target, error) {
	var out []Target
	f := store.SiteFilter{Limit: 200}
	for {
		page, err := s.sites.List(ctx, f)
		if err != nil {
			return nil, fmt.Errorf("list sites for metrics: %w", err)
		}
		for _, site := range page.Sites {
			if site.Status == store.SiteDeleting {
				continue
			}
			out = append(out, Target{SiteID: site.ID, ClientGroupID: site.ClientGroupID, Site: site})
		}
		if page.NextCursor == "" {
			return out, nil
		}
		f.Before = page.NextCursor
	}
}
