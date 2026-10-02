package metrics

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
)

// MetricPageCacheRequests counts a site's requests by FastCGI cache status
// (labels: cache_status). The metrics API's page_cache_ratio chart reads it.
const MetricPageCacheRequests = "pressfleet_site_page_cache_requests_total"

// cacheStatuses are the values of the cache_status label that are always
// reported, so the series exist (at 0) before the first request of each
// kind. Other statuses nginx can report are added as they are seen.
var cacheStatuses = []string{"hit", "miss", "bypass"}

// knownCacheStatuses are the other values of nginx's $upstream_cache_status.
// Anything else becomes "other", which keeps the label's values bounded.
var knownCacheStatuses = map[string]bool{
	"hit": true, "miss": true, "bypass": true,
	"expired": true, "stale": true, "updating": true, "revalidated": true,
}

// PageCacheCollector counts FastCGI cache hits, misses, and bypasses per
// site (§7.1 Page cache).
//
// The source is the nginx access log, read through [provisioner.LogReader],
// which already carries $upstream_cache_status on every request
// (images/site-nginx). The alternative, an nginx module or stub status
// extension, would need another module in the image and a second scrape
// path, and no stock module counts by cache status. Requests that do not go
// through the cache (static files, or a site with the cache off) log an
// empty status and are not counted.
//
// A counter must only go up, but logs are a window, so the collector reads
// each site's log once, from where the previous run ended to the start of
// this one, and adds what it finds to counters it keeps in memory. The
// counters, and so the series, restart from 0 when the control plane does,
// which rate() and increase() handle; requests logged while it was down are
// not counted. A site's first run only sets the starting point.
type PageCacheCollector struct {
	// Logs reads the access log.
	Logs provisioner.LogReader
	// Every is the collection interval; zero means the service default.
	Every time.Duration
	// Now returns the time; nil means [time.Now]. It is for tests.
	Now func() time.Time

	mu    sync.Mutex
	sites map[string]*cacheCursor
}

// cacheCursor is a site's position in its access log and its counters.
type cacheCursor struct {
	// next is where the next read starts: the end of the previous one.
	next   time.Time
	counts map[string]float64
}

var _ Collector = (*PageCacheCollector)(nil)

// Name implements [Collector].
func (*PageCacheCollector) Name() string { return "page_cache" }

// Interval implements [Collector].
func (c *PageCacheCollector) Interval() time.Duration { return c.Every }

// Collect implements [Collector].
func (c *PageCacheCollector) Collect(ctx context.Context, targets []Target) ([]Sample, error) {
	now := time.Now
	if c.Now != nil {
		now = c.Now
	}
	until := now()

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.sites == nil {
		c.sites = map[string]*cacheCursor{}
	}
	live := make(map[string]bool, len(targets))
	var out []Sample
	var errs []error
	for _, t := range targets {
		live[t.SiteID] = true
		cur := c.sites[t.SiteID]
		if cur == nil {
			cur = &cacheCursor{next: until, counts: map[string]float64{}}
			for _, s := range cacheStatuses {
				cur.counts[s] = 0
			}
			c.sites[t.SiteID] = cur
		} else if err := c.read(ctx, t.SiteID, cur, until); err != nil {
			if !errors.Is(err, provisioner.ErrSiteNotFound) && !errors.Is(err, provisioner.ErrSiteNotRunning) {
				errs = append(errs, fmt.Errorf("site %s: %w", t.SiteID, err))
			}
			// The cursor stays, so the next run reads this window too.
		}
		for status, n := range cur.counts {
			out = append(out, t.Sample(MetricPageCacheRequests, n, "cache_status", status))
		}
	}
	for id := range c.sites {
		if !live[id] {
			delete(c.sites, id)
		}
	}
	return out, errors.Join(errs...)
}

// read adds the cache statuses logged in [cur.next, until) to cur and moves
// cur.next to until. Nothing changes if reading fails part way.
func (c *PageCacheCollector) read(ctx context.Context, siteID string, cur *cacheCursor, until time.Time) error {
	if !until.After(cur.next) {
		return nil
	}
	opts := provisioner.LogOptions{
		Components: []provisioner.Component{provisioner.ComponentNginx},
		Sources:    []provisioner.LogSource{provisioner.LogAccess},
		Since:      cur.next, Until: until,
	}
	found := map[string]float64{}
	for e, err := range c.Logs.Logs(ctx, siteID, opts) {
		if err != nil {
			return err
		}
		if status := cacheStatus(e.Line); status != "" {
			found[status]++
		}
	}
	for status, n := range found {
		cur.counts[status] += n
	}
	cur.next = until
	return nil
}

// cacheStatus returns the cache_status label for an access log line, or ""
// if the request did not use the cache or the line is not an access log
// entry.
func cacheStatus(line string) string {
	var entry struct {
		Status string `json:"upstream_cache_status"`
	}
	if json.Unmarshal([]byte(line), &entry) != nil {
		return ""
	}
	s := strings.ToLower(entry.Status)
	switch {
	case s == "" || s == "-":
		return ""
	case knownCacheStatuses[s]:
		return s
	}
	return "other"
}
