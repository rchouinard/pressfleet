package metricsapi_test

import (
	"context"
	"errors"
	"math"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/metrics"
	"github.com/rchouinard/pressfleet/control-plane/internal/metrics/metricstest"
	"github.com/rchouinard/pressfleet/control-plane/internal/metricsapi"
)

var now = time.Date(2026, 10, 1, 12, 7, 31, 0, time.UTC)

func newService(q metrics.Querier) *metricsapi.Service {
	return metricsapi.New(q, func() time.Time { return now })
}

func matrix(labels map[string]string, values ...float64) metrics.Result {
	s := metrics.Series{Labels: labels}
	for i, v := range values {
		s.Points = append(s.Points, metrics.Point{Time: now.Add(time.Duration(i-len(values)) * time.Minute), Value: v})
	}
	return metrics.Result{Type: metrics.Matrix, Series: []metrics.Series{s}}
}

func TestRangeQueriesTheCatalog(t *testing.T) {
	q := &metricstest.Querier{}
	res, err := newService(q).Range(context.Background(), metricsapi.Request{
		Site: true, SiteID: "S1", Metrics: []string{"latency", "uptime"},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Defaults: the last hour, a round step, and edges aligned to it.
	if res.Step != 15*time.Second || !res.End.Equal(time.Date(2026, 10, 1, 12, 7, 30, 0, time.UTC)) || res.End.Sub(res.Start) != time.Hour {
		t.Errorf("window = %v to %v step %v", res.Start, res.End, res.Step)
	}
	// Results are in catalog order, whatever the order of the request, and
	// a metric without data is an empty list, not an error.
	var ids []string
	for _, r := range res.Results {
		ids = append(ids, r.Entry.ID)
		if r.Series == nil || len(r.Series) != 0 {
			t.Errorf("%s: series = %v, want empty and non-nil", r.Entry.ID, r.Series)
		}
	}
	if !slices.Equal(ids, []string{"uptime", "latency"}) {
		t.Errorf("results = %v", ids)
	}
	queries := q.Queries()
	slices.Sort(queries)
	if len(queries) != 4 || !strings.Contains(strings.Join(queries, "\n"), `avg_over_time(pressfleet_site_up{site_id="S1"}[120s])`) {
		t.Errorf("queries = %q", queries)
	}
}

func TestRangeShapesSeries(t *testing.T) {
	q := &metricstest.Querier{Result: metrics.Result{Type: metrics.Matrix, Series: []metrics.Series{
		{Labels: map[string]string{"__name__": "x", "site_id": "S1", "client_group_id": "G1", "status_class": "5xx"},
			Points: []metrics.Point{{Time: now, Value: 2}, {Time: now.Add(time.Minute), Value: math.NaN()}, {Time: now.Add(2 * time.Minute), Value: 3}}},
		{Labels: map[string]string{"status_class": "2xx"}, Points: []metrics.Point{{Time: now, Value: math.Inf(1)}}},
		{Labels: map[string]string{"status_class": "4xx"}, Points: []metrics.Point{{Time: now, Value: 1}}},
	}}}
	res, err := newService(q).Range(context.Background(), metricsapi.Request{Site: true, SiteID: "S1", Metrics: []string{"status_codes"}})
	if err != nil {
		t.Fatal(err)
	}
	got := res.Results[0].Series
	// The all-NaN-or-infinite series is dropped; ordering is by labels.
	if len(got) != 2 || got[0].Labels["status_class"] != "4xx" || got[1].Labels["status_class"] != "5xx" {
		t.Fatalf("series = %+v", got)
	}
	if l := got[1].Labels; len(l) != 1 {
		t.Errorf("labels = %v: the site's own labels and __name__ should be gone", l)
	}
	if p := got[1].Points; len(p) != 2 || p[0].Value != 2 || p[1].Value != 3 {
		t.Errorf("points = %v: NaN should be dropped", p)
	}
}

func TestMultiQueryEntriesLabelTheirSeries(t *testing.T) {
	q := &metricstest.Querier{Result: matrix(nil, 1)}
	res, err := newService(q).Range(context.Background(), metricsapi.Request{Site: true, SiteID: "S1", Metrics: []string{"latency"}})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, s := range res.Results[0].Series {
		got = append(got, s.Labels["percentile"])
	}
	if !slices.Equal(got, []string{"p50", "p95", "p99"}) {
		t.Errorf("percentiles = %v", got)
	}
}

func TestRangeWindow(t *testing.T) {
	at := func(s string) time.Time {
		v, err := time.Parse(time.RFC3339, s)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	tests := []struct {
		name       string
		req        metricsapi.Request
		wantErr    string
		start, end string
		step       time.Duration
	}{
		{name: "defaults", start: "2026-10-01T11:07:30Z", end: "2026-10-01T12:07:30Z", step: 15 * time.Second},
		{name: "a day picks a round step", req: metricsapi.Request{Start: at("2026-09-30T12:07:31Z")},
			start: "2026-09-30T12:05:00Z", end: "2026-10-01T12:05:00Z", step: 5 * time.Minute},
		{name: "explicit step aligns", req: metricsapi.Request{Start: at("2026-10-01T10:00:00Z"), End: at("2026-10-01T11:00:00Z"), Step: time.Minute},
			start: "2026-10-01T10:00:00Z", end: "2026-10-01T11:00:00Z", step: time.Minute},
		{name: "end in the future is capped", req: metricsapi.Request{Start: at("2026-10-01T11:00:00Z"), End: at("2027-01-01T00:00:00Z"), Step: time.Minute},
			start: "2026-10-01T11:00:00Z", end: "2026-10-01T12:07:00Z", step: time.Minute},
		{name: "start after end", req: metricsapi.Request{Start: at("2026-10-01T11:00:00Z"), End: at("2026-10-01T10:00:00Z")}, wantErr: "start must be before end"},
		{name: "start equals end", req: metricsapi.Request{Start: at("2026-10-01T11:00:00Z"), End: at("2026-10-01T11:00:00Z")}, wantErr: "start must be before end"},
		{name: "range too long", req: metricsapi.Request{Start: at("2025-01-01T00:00:00Z")}, wantErr: "at most 366 days"},
		{name: "step too small", req: metricsapi.Request{Step: 14 * time.Second}, wantErr: "at least 15 seconds"},
		{name: "step longer than the range", req: metricsapi.Request{Step: 2 * time.Hour}, wantErr: "step must not be longer than the range"},
		{name: "step not whole seconds", req: metricsapi.Request{Step: 20*time.Second + time.Millisecond}, wantErr: "whole number of seconds"},
		{name: "too many points", req: metricsapi.Request{Start: at("2026-10-01T00:00:00Z"), Step: 15 * time.Second}, wantErr: "at most 1500 are allowed"},
		{name: "exactly the most points", req: metricsapi.Request{Start: at("2026-10-01T10:00:00Z"), End: at("2026-10-01T12:00:00Z"), Step: 5 * time.Second * 3},
			start: "2026-10-01T10:00:00Z", end: "2026-10-01T12:00:00Z", step: 15 * time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := tt.req
			req.Site, req.SiteID, req.Metrics = true, "S1", []string{"uptime"}
			q := &metricstest.Querier{}
			res, err := newService(q).Range(context.Background(), req)
			if tt.wantErr != "" {
				if !errors.Is(err, metricsapi.ErrInvalid) || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want invalid containing %q", err, tt.wantErr)
				}
				if len(q.Queries()) != 0 {
					t.Errorf("queried despite the error: %q", q.Queries())
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !res.Start.Equal(at(tt.start)) || !res.End.Equal(at(tt.end)) || res.Step != tt.step {
				t.Errorf("window = %v to %v step %v; want %s to %s step %v", res.Start, res.End, res.Step, tt.start, tt.end, tt.step)
			}
		})
	}
}

// TestRateWindowFollowsStep checks that a long step widens the rate window
// (so no sample is skipped) and a short one never narrows it below two
// minutes.
func TestRateWindowFollowsStep(t *testing.T) {
	for _, tt := range []struct {
		step time.Duration
		want string
	}{{15 * time.Second, "[120s]"}, {time.Minute, "[120s]"}, {10 * time.Minute, "[600s]"}} {
		q := &metricstest.Querier{}
		_, err := newService(q).Range(context.Background(), metricsapi.Request{
			Site: true, SiteID: "S1", Metrics: []string{"request_rate"}, Start: now.Add(-6 * time.Hour), Step: tt.step,
		})
		if err != nil {
			t.Fatal(err)
		}
		if got := q.Queries(); len(got) != 1 || !strings.Contains(got[0], tt.want) {
			t.Errorf("step %v: queries = %q, want window %s", tt.step, got, tt.want)
		}
	}
}

func TestSelectionValidation(t *testing.T) {
	many := []string{"uptime", "http_check_status", "response_time", "request_rate", "status_codes", "latency", "page_cache_ratio",
		"php_workers", "php_max_children_reached", "php_slow_requests", "cpu", "memory", "restarts", "wp_content_size",
		"database_size", "object_cache_keys", "object_cache_memory", "object_cache_hit_ratio", "tls_expiry", "backup_last_success", "backup_size"}
	tests := []struct {
		name    string
		req     metricsapi.Request
		wantErr string
	}{
		{"nothing selected", metricsapi.Request{Site: true, SiteID: "S1"}, "at least one metric or group"},
		{"unknown metric", metricsapi.Request{Site: true, SiteID: "S1", Metrics: []string{"nope"}}, `unknown metric "nope"`},
		{"platform metric on a site", metricsapi.Request{Site: true, SiteID: "S1", Metrics: []string{"host_cpu"}}, "unknown metric"},
		{"site metric on the platform", metricsapi.Request{Metrics: []string{"uptime"}}, "unknown metric"},
		{"raw promql as an id", metricsapi.Request{Site: true, SiteID: "S1", Metrics: []string{`up{job="x"}`}}, "unknown metric"},
		{"unknown group", metricsapi.Request{Site: true, SiteID: "S1", Groups: []string{"host"}}, "unknown group"},
		{"too many metrics", metricsapi.Request{Site: true, SiteID: "S1", Metrics: many}, "at most 20 metrics"},
		{"injected site id", metricsapi.Request{Site: true, SiteID: `x"} or vector(1) #`, Metrics: []string{"uptime"}}, "site ID is malformed"},
		{"empty site id", metricsapi.Request{Site: true, Metrics: []string{"uptime"}}, "site ID is malformed"},
		{"injected client group", metricsapi.Request{ClientGroupID: `x"} or up{a="`, Metrics: []string{"restic_repository_size"}}, "client_group_id is malformed"},
		{"client group on a site", metricsapi.Request{Site: true, SiteID: "S1", ClientGroupID: "G1", Metrics: []string{"uptime"}}, "client_group_id"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			q := &metricstest.Querier{}
			svc := newService(q)
			calls := map[string]func() error{
				"range": func() error { _, err := svc.Range(context.Background(), tt.req); return err },
			}
			if tt.name != "nothing selected" { // a summary without a selection uses the defaults
				calls["summary"] = func() error { _, err := svc.Summary(context.Background(), tt.req); return err }
			}
			for name, call := range calls {
				err := call()
				if !errors.Is(err, metricsapi.ErrInvalid) || !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("%s: err = %v, want invalid containing %q", name, err, tt.wantErr)
				}
			}
			if got := q.Queries(); len(got) != 0 {
				t.Errorf("queried despite the error: %q", got)
			}
		})
	}
}

func TestGroupsExpand(t *testing.T) {
	q := &metricstest.Querier{}
	res, err := newService(q).Range(context.Background(), metricsapi.Request{
		Site: true, SiteID: "S1", Groups: []string{"traffic", "php"}, Metrics: []string{"uptime", "request_rate"},
	})
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, r := range res.Results {
		ids = append(ids, r.Entry.ID)
	}
	want := []string{"uptime", "request_rate", "status_codes", "latency", "php_workers", "php_max_children_reached", "php_slow_requests"}
	if !slices.Equal(ids, want) {
		t.Errorf("ids = %v, want %v", ids, want)
	}
}

func TestPlatformClientGroupFilter(t *testing.T) {
	q := &metricstest.Querier{}
	_, err := newService(q).Range(context.Background(), metricsapi.Request{ClientGroupID: "G-1", Metrics: []string{"restic_repository_size", "host_cpu"}})
	if err != nil {
		t.Fatal(err)
	}
	queries := q.Queries()
	slices.Sort(queries)
	want := []string{
		`1 - sum by (node) (rate(pressfleet_host_cpu_seconds_total{mode="idle"}[120s])) / sum by (node) (rate(pressfleet_host_cpu_seconds_total[120s]))`,
		`sum by (client_group_id) (pressfleet_restic_repository_size_bytes{client_group_id="G-1"})`,
	}
	if !slices.Equal(queries, want) {
		t.Errorf("queries = %q", queries)
	}
}

func TestSummary(t *testing.T) {
	q := &metricstest.Querier{Result: metrics.Result{Type: metrics.Vector, Series: []metrics.Series{
		{Labels: map[string]string{"domain": "a.example"}, Points: []metrics.Point{{Time: now, Value: 7}}},
	}}}
	svc := newService(q)
	res, err := svc.Summary(context.Background(), metricsapi.Request{Site: true, SiteID: "S1"})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Time.Equal(now) {
		t.Errorf("time = %v", res.Time)
	}
	var ids []string
	for _, r := range res.Results {
		ids = append(ids, r.Entry.ID)
		if len(r.Series) != 1 || r.Series[0].Points[0].Value != 7 {
			t.Errorf("%s: series = %+v", r.Entry.ID, r.Series)
		}
	}
	want := []string{"uptime", "http_check_status", "response_time", "request_rate", "wp_content_size", "database_size", "tls_expiry", "backup_last_success"}
	if !slices.Equal(ids, want) {
		t.Errorf("default summary = %v, want %v", ids, want)
	}
	for _, query := range q.Queries() {
		if !strings.Contains(query, `site_id="S1"`) {
			t.Errorf("query isn't scoped to the site: %s", query)
		}
	}

	// Named metrics replace the default set; rates use the summary window.
	q2 := &metricstest.Querier{}
	if _, err := newService(q2).Summary(context.Background(), metricsapi.Request{Metrics: []string{"redis_evictions"}}); err != nil {
		t.Fatal(err)
	}
	if got := q2.Queries(); !slices.Equal(got, []string{`rate(pressfleet_redis_evicted_keys_total[300s])`}) {
		t.Errorf("queries = %q", got)
	}
}

func TestUnavailable(t *testing.T) {
	down := &metricstest.Querier{Err: errors.Join(metrics.ErrUnavailable, errors.New("connection refused"))}
	req := metricsapi.Request{Site: true, SiteID: "S1", Metrics: []string{"uptime", "latency", "cpu"}}
	for name, svc := range map[string]*metricsapi.Service{"down": newService(down), "no store": newService(nil)} {
		if _, err := svc.Range(context.Background(), req); !errors.Is(err, metrics.ErrUnavailable) {
			t.Errorf("%s range: err = %v, want ErrUnavailable", name, err)
		}
		if _, err := svc.Summary(context.Background(), req); !errors.Is(err, metrics.ErrUnavailable) {
			t.Errorf("%s summary: err = %v, want ErrUnavailable", name, err)
		}
	}
	// A rejected query is not an outage.
	bad := &metricstest.Querier{Err: metrics.ErrQuery}
	if _, err := newService(bad).Range(context.Background(), req); errors.Is(err, metrics.ErrUnavailable) || !errors.Is(err, metrics.ErrQuery) {
		t.Errorf("rejected query: err = %v", err)
	}
}

// blockingQuerier records the most queries that run at once.
type blockingQuerier struct {
	running, peak atomic.Int32
	mu            sync.Mutex
}

func (b *blockingQuerier) Query(context.Context, string, time.Time) (metrics.Result, error) {
	return metrics.Result{}, nil
}

func (b *blockingQuerier) QueryRange(ctx context.Context, _ string, _, _ time.Time, _ time.Duration) (metrics.Result, error) {
	n := b.running.Add(1)
	defer b.running.Add(-1)
	b.mu.Lock()
	if n > b.peak.Load() {
		b.peak.Store(n)
	}
	b.mu.Unlock()
	select {
	case <-time.After(5 * time.Millisecond):
	case <-ctx.Done():
	}
	return metrics.Result{}, nil
}

func TestQueriesRunConcurrentlyWithABound(t *testing.T) {
	b := &blockingQuerier{}
	// Every site entry: 30 queries.
	var ids []string
	for _, e := range metricsapi.SiteEntries()[:20] {
		ids = append(ids, e.ID)
	}
	if _, err := newService(b).Range(context.Background(), metricsapi.Request{Site: true, SiteID: "S1", Metrics: ids}); err != nil {
		t.Fatal(err)
	}
	if peak := b.peak.Load(); peak < 2 || peak > 8 {
		t.Errorf("peak concurrency = %d, want between 2 and 8", peak)
	}
}
