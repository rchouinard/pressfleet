package api_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/api"
	"github.com/rchouinard/pressfleet/control-plane/internal/api/gen"
	"github.com/rchouinard/pressfleet/control-plane/internal/auth"
	"github.com/rchouinard/pressfleet/control-plane/internal/id"
	"github.com/rchouinard/pressfleet/control-plane/internal/metrics"
	"github.com/rchouinard/pressfleet/control-plane/internal/metrics/metricstest"
	"github.com/rchouinard/pressfleet/control-plane/internal/metricsapi"
	"github.com/rchouinard/pressfleet/control-plane/internal/sites"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
)

// metricsEnv is a handler with two sites and a fake metrics store.
type metricsEnv struct {
	h       http.Handler
	q       *metricstest.Querier
	a, b    store.Site
	fakeAut *fakeAuth
}

func newMetricsEnv(t *testing.T, db *store.DB) *metricsEnv {
	t.Helper()
	ctx := context.Background()
	e := newSiteEnv(t, db, testPreviewBase)
	q := &metricstest.Querier{}
	fa := &fakeAuth{}
	h := newHandler(t, api.Options{DB: db, Authenticator: fa, Sites: e.svc, Metrics: metricsapi.New(q, nil)})
	group := &store.ClientGroup{Name: "Acme"}
	if err := store.NewClientGroups(db).Create(ctx, group); err != nil {
		t.Fatal(err)
	}
	create := func(name string) store.Site {
		s, _, err := e.svc.Create(ctx, sites.NewSite{ClientGroupID: group.ID, Name: name, AdminEmail: "o@example.com", PHPVersion: "8.4"})
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	return &metricsEnv{h: h, q: q, a: create("Alpha"), b: create("Beta"), fakeAut: fa}
}

func (e *metricsEnv) get(t *testing.T, target string) *httptest.ResponseRecorder {
	t.Helper()
	return do(t, e.h, request{authed: true, method: http.MethodGet, target: target})
}

func TestMetricCatalogEndpoint(t *testing.T) {
	h := newHandler(t, api.Options{Authenticator: &fakeAuth{}})
	w := do(t, h, request{authed: true, method: http.MethodGet, target: "/api/v1/metrics/catalog"})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body)
	}
	cat := decodeJSON[gen.MetricCatalog](t, w.Body.Bytes())
	if len(cat.Items) != len(metricsapi.Entries()) {
		t.Fatalf("catalog has %d items, want %d", len(cat.Items), len(metricsapi.Entries()))
	}
	byID := map[string]gen.MetricDescriptor{}
	for _, d := range cat.Items {
		byID[d.ID] = d
	}
	if d := byID["status_codes"]; d.Unit != gen.MetricUnit("per_second") || d.Scope != "site" || d.Group != "traffic" ||
		!slices.Equal(d.Labels, []string{"status_class"}) || d.Kind != gen.MetricKind("rate") {
		t.Errorf("status_codes = %+v", d)
	}
	if d := byID["restic_repository_size"]; d.Scope != "client_group" {
		t.Errorf("restic_repository_size = %+v", d)
	}
	if d := byID["uptime"]; d.Labels == nil {
		t.Error("labels of an unlabeled metric must be [], not null")
	}
	if cat.Limits.MaxPoints != metricsapi.MaxPoints || cat.Limits.MinStepSeconds != 15 || cat.Limits.DefaultRangeSeconds != 3600 {
		t.Errorf("limits = %+v", cat.Limits)
	}
}

func TestSiteMetricsEndpoint(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newMetricsEnv(t, db)
		base := "/api/v1/sites/" + e.a.ID + "/metrics"

		// No collector feeds the metric: an empty series, not an error.
		w := e.get(t, base+"?metric=uptime&metric=status_codes")
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", w.Code, w.Body)
		}
		got := decodeJSON[gen.MetricsRange](t, w.Body.Bytes())
		if len(got.Metrics) != 2 || got.Metrics[0].ID != "uptime" || got.Metrics[1].ID != "status_codes" ||
			got.Metrics[0].Series == nil || len(got.Metrics[0].Series) != 0 {
			t.Fatalf("empty result = %s", w.Body)
		}
		if got.StepSeconds != 15 || got.End.Sub(got.Start) != time.Hour {
			t.Errorf("window = %v..%v step %d", got.Start, got.End, got.StepSeconds)
		}

		// Series carry their labels, points, and the latest value.
		t0 := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
		e.q.Result = metrics.Result{Type: metrics.Matrix, Series: []metrics.Series{{
			Labels: map[string]string{"status_class": "5xx", "site_id": e.a.ID},
			Points: []metrics.Point{{Time: t0, Value: 0.5}, {Time: t0.Add(time.Minute), Value: 1.5}},
		}}}
		w = e.get(t, base+"?metric=status_codes&step=60")
		got = decodeJSON[gen.MetricsRange](t, w.Body.Bytes())
		if w.Code != http.StatusOK || got.StepSeconds != 60 || len(got.Metrics[0].Series) != 1 {
			t.Fatalf("%d: %s", w.Code, w.Body)
		}
		s := got.Metrics[0].Series[0]
		if len(s.Labels) != 1 || s.Labels["status_class"] != "5xx" || len(s.Points) != 2 || s.Latest.Value != 1.5 || !s.Latest.Time.Equal(t0.Add(time.Minute)) {
			t.Errorf("series = %+v", s)
		}
		if got.Metrics[0].Unit != gen.MetricUnit("per_second") || got.Metrics[0].Kind != gen.MetricKind("rate") {
			t.Errorf("unit/kind = %q/%q", got.Metrics[0].Unit, got.Metrics[0].Kind)
		}

		// Groups expand, and every query is scoped to the site in the path.
		e.q.Result = metrics.Result{}
		before := len(e.q.Queries())
		if w := e.get(t, base+"?group=availability&group=traffic"); w.Code != http.StatusOK {
			t.Fatalf("groups: %d %s", w.Code, w.Body)
		}
		queries := e.q.Queries()[before:]
		if len(queries) != 8 {
			t.Errorf("availability and traffic need 8 queries, got %d", len(queries))
		}
		for _, q := range queries {
			if !strings.Contains(q, `site_id="`+e.a.ID+`"`) || strings.Contains(q, e.b.ID) {
				t.Errorf("query isn't scoped to the site in the path: %s", q)
			}
		}
	})
}

func TestSiteMetricsSummaryEndpoint(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newMetricsEnv(t, db)
		base := "/api/v1/sites/" + e.a.ID + "/metrics/summary"
		t0 := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
		e.q.Result = metrics.Result{Type: metrics.Vector, Series: []metrics.Series{{
			Labels: map[string]string{"domain": "a.example.com"}, Points: []metrics.Point{{Time: t0, Value: 1.8e9}},
		}}}
		w := e.get(t, base+"?metric=tls_expiry")
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", w.Code, w.Body)
		}
		got := decodeJSON[gen.MetricsSummary](t, w.Body.Bytes())
		// A Unix time needs the precision of a double.
		if len(got.Metrics) != 1 || len(got.Metrics[0].Values) != 1 || got.Metrics[0].Values[0].Value != 1.8e9 ||
			got.Metrics[0].Values[0].Labels["domain"] != "a.example.com" || !got.Metrics[0].Values[0].Time.Equal(t0) {
			t.Fatalf("summary = %s", w.Body)
		}

		// Without a selection: the default set, each metric present.
		e.q.Result = metrics.Result{}
		w = e.get(t, base)
		got = decodeJSON[gen.MetricsSummary](t, w.Body.Bytes())
		if w.Code != http.StatusOK || len(got.Metrics) != 8 || got.Metrics[0].Values == nil {
			t.Fatalf("default summary = %d %s", w.Code, w.Body)
		}
	})
}

func TestSiteMetricsErrors(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newMetricsEnv(t, db)
		base := "/api/v1/sites/" + e.a.ID + "/metrics"
		for _, tt := range []struct {
			name, target string
			status       int
			detail       string
		}{
			{"unknown site", "/api/v1/sites/" + id.New() + "/metrics?metric=uptime", http.StatusNotFound, ""},
			{"unknown site summary", "/api/v1/sites/" + id.New() + "/metrics/summary", http.StatusNotFound, ""},
			{"nothing selected", base, http.StatusBadRequest, "at least one metric or group"},
			{"unknown metric", base + "?metric=bogus", http.StatusBadRequest, `unknown metric "bogus"`},
			{"promql in metric", base + "?" + url.Values{"metric": {`up{x="y"}`}}.Encode(), http.StatusBadRequest, "unknown metric"},
			{"platform metric", base + "?metric=host_cpu", http.StatusBadRequest, "unknown metric"},
			{"unknown group", base + "?group=host", http.StatusBadRequest, "unknown group"},
			{"step too small", base + "?metric=uptime&step=5", http.StatusBadRequest, "at least 15 seconds"},
			{"too many points", base + "?metric=uptime&step=15&start=" + url.QueryEscape(time.Now().Add(-24*time.Hour).Format(time.RFC3339)), http.StatusBadRequest, "points per series"},
			{"huge step", base + "?metric=uptime&step=99999999999999", http.StatusBadRequest, "step must not be longer than the range"},
			{"range too long", base + "?metric=uptime&start=2020-01-01T00:00:00Z", http.StatusBadRequest, "at most 366 days"},
			{"start after end", base + "?metric=uptime&start=2026-10-01T00:00:00Z&end=2026-09-01T00:00:00Z", http.StatusBadRequest, "start must be before end"},
			{"bad time", base + "?metric=uptime&start=yesterday", http.StatusBadRequest, ""},
			{"bad step", base + "?metric=uptime&step=fast", http.StatusBadRequest, ""},
			{"too many metrics", base + "?group=traffic&group=php&group=resources&group=storage&group=object_cache&group=backups&metric=uptime&metric=http_check_status&metric=response_time&metric=page_cache_ratio&metric=tls_expiry", http.StatusBadRequest, "at most 20 metrics"},
		} {
			t.Run(tt.name, func(t *testing.T) {
				w := e.get(t, tt.target)
				if w.Code != tt.status {
					t.Fatalf("status = %d, want %d: %s", w.Code, tt.status, w.Body)
				}
				if p := decodeProblem(t, w); !strings.Contains(p.Detail, tt.detail) {
					t.Errorf("detail = %q, want it to contain %q", p.Detail, tt.detail)
				}
			})
		}
		if n := len(e.q.Queries()); n != 0 {
			t.Errorf("%d queries ran for invalid requests: %q", n, e.q.Queries())
		}
	})
}

func TestMetricsUnavailable(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newMetricsEnv(t, db)
		e.q.Err = errors.Join(metrics.ErrUnavailable, errors.New("dial tcp 10.9.8.7:8428: connection refused"))
		for _, target := range []string{
			"/api/v1/sites/" + e.a.ID + "/metrics?metric=uptime",
			"/api/v1/sites/" + e.a.ID + "/metrics/summary",
			"/api/v1/metrics/platform?metric=host_cpu",
			"/api/v1/metrics/platform/summary",
		} {
			w := e.get(t, target)
			if w.Code != http.StatusServiceUnavailable {
				t.Errorf("%s: status = %d: %s", target, w.Code, w.Body)
				continue
			}
			if p := decodeProblem(t, w); strings.Contains(p.Detail, "10.9.8.7") {
				t.Errorf("%s: the problem leaks the store's address: %q", target, p.Detail)
			}
		}

		// A query the store rejects is a server bug: 500, not 503.
		e.q.Err = metrics.ErrQuery
		if w := e.get(t, "/api/v1/metrics/platform?metric=host_cpu"); w.Code != http.StatusInternalServerError {
			t.Errorf("rejected query: status = %d", w.Code)
		}

		// Metrics turned off: no store at all.
		h := newHandler(t, api.Options{DB: db, Authenticator: &fakeAuth{}})
		if w := do(t, h, request{authed: true, method: http.MethodGet, target: "/api/v1/metrics/platform?metric=host_cpu"}); w.Code != http.StatusServiceUnavailable {
			t.Errorf("no store: status = %d: %s", w.Code, w.Body)
		}
	})
}

func TestPlatformMetricsEndpoint(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newMetricsEnv(t, db)
		w := e.get(t, "/api/v1/metrics/platform?group=host&metric=restic_repository_size")
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", w.Code, w.Body)
		}
		got := decodeJSON[gen.MetricsRange](t, w.Body.Bytes())
		var ids []string
		for _, m := range got.Metrics {
			ids = append(ids, m.ID)
		}
		if !slices.Equal(ids, []string{"host_cpu", "host_memory", "host_disk_usage", "host_disk_io", "restic_repository_size"}) {
			t.Errorf("ids = %v", ids)
		}

		// Per-client-group metrics are filtered by client_group_id.
		e.q.Result = metrics.Result{}
		before := len(e.q.Queries())
		w = e.get(t, "/api/v1/metrics/platform?metric=restic_repository_size&client_group_id="+e.a.ClientGroupID)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", w.Code, w.Body)
		}
		if qs := e.q.Queries()[before:]; len(qs) != 1 || !strings.Contains(qs[0], `{client_group_id="`+e.a.ClientGroupID+`"}`) {
			t.Errorf("queries = %q", qs)
		}

		// Site metrics belong to the site endpoints; ids are validated.
		if w := e.get(t, "/api/v1/metrics/platform?metric=uptime"); w.Code != http.StatusBadRequest {
			t.Errorf("site metric on the platform: %d", w.Code)
		}
		if w := e.get(t, "/api/v1/metrics/platform?metric=restic_repository_size&client_group_id="+url.QueryEscape(`x"} or vector(1) #`)); w.Code != http.StatusBadRequest {
			t.Errorf("injected client group: %d: %s", w.Code, w.Body)
		}

		w = e.get(t, "/api/v1/metrics/platform/summary")
		if sum := decodeJSON[gen.MetricsSummary](t, w.Body.Bytes()); w.Code != http.StatusOK || len(sum.Metrics) != 8 {
			t.Errorf("default platform summary: %d %s", w.Code, w.Body)
		}
	})
}

func TestMetricsAuthorization(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newMetricsEnv(t, db)
		targets := []string{
			"/api/v1/metrics/catalog",
			"/api/v1/metrics/platform?metric=host_cpu",
			"/api/v1/metrics/platform/summary",
			"/api/v1/sites/" + e.a.ID + "/metrics?metric=uptime",
			"/api/v1/sites/" + e.a.ID + "/metrics/summary",
		}
		for _, target := range targets {
			if w := do(t, e.h, request{method: http.MethodGet, target: target}); w.Code != http.StatusUnauthorized {
				t.Errorf("%s without credentials: %d", target, w.Code)
			}
			// Viewers, the least role, may read.
			e.fakeAut.principal = &auth.Principal{UserID: "01j9zq3v6t8k4m2n5p7r9s1x3y", Role: store.RoleViewer, Method: auth.MethodSession, SessionID: "v"}
			if w := e.get(t, target); w.Code != http.StatusOK {
				t.Errorf("%s as viewer: %d: %s", target, w.Code, w.Body)
			}
			e.fakeAut.principal = nil
		}
	})
}
