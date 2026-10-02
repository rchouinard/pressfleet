package metrics_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/metrics"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner/provisionertest"
)

func accessLine(status string) string {
	return `{"time":"2026-10-01T12:00:00+00:00","status":200,"upstream_cache_status":"` + status + `"}`
}

func values(samples []metrics.Sample, name string) map[string]map[string]float64 {
	out := map[string]map[string]float64{}
	for _, s := range samples {
		if s.Name != name {
			continue
		}
		site := s.Labels[metrics.LabelSiteID]
		if out[site] == nil {
			out[site] = map[string]float64{}
		}
		out[site][s.Labels["cache_status"]] = s.Value
	}
	return out
}

func TestPageCacheCollector(t *testing.T) {
	ctx := context.Background()
	fake := provisionertest.New()
	if _, err := fake.CreateSite(ctx, &provisioner.SiteConfig{
		ID: "s1", ClientGroupID: "g1", PreviewDomain: "s1.preview.example.com", PHPVersion: "8.4",
		WPCoreVersion: "6.9.1", DBSecretRef: "secret/db/s1", Resources: provisioner.DefaultResourceLimits(),
		UID: provisioner.MinSiteUID, WPSaltsSecretRef: "secret/salts/s1",
	}); err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	now := t0
	c := &metrics.PageCacheCollector{Logs: fake, Now: func() time.Time { return now }}
	targets := []metrics.Target{{SiteID: "s1", ClientGroupID: "g1"}}
	add := func(at time.Duration, line string) {
		fake.AddLogs("s1", provisioner.LogEntry{Time: t0.Add(at), Component: provisioner.ComponentNginx, Line: line})
	}

	// The first run only sets the start: older entries are not counted.
	add(-time.Second, accessLine("HIT"))
	got, err := c.Collect(ctx, targets)
	if err != nil {
		t.Fatal(err)
	}
	for status, v := range values(got, metrics.MetricPageCacheRequests)["s1"] {
		if v != 0 {
			t.Errorf("first run: %s = %v, want 0", status, v)
		}
	}
	if len(values(got, metrics.MetricPageCacheRequests)["s1"]) != 3 {
		t.Errorf("first run reported %d statuses, want hit, miss, bypass", len(values(got, metrics.MetricPageCacheRequests)["s1"]))
	}

	// Entries in the window count; empty status, static files, and other
	// log sources do not.
	add(time.Second, accessLine("HIT"))
	add(2*time.Second, accessLine("HIT"))
	add(3*time.Second, accessLine("MISS"))
	add(4*time.Second, accessLine("EXPIRED"))
	add(5*time.Second, accessLine("WEIRD"))
	add(6*time.Second, accessLine(""))
	add(7*time.Second, "2026/10/01 12:00:07 [error] 7#7: boom")
	now = t0.Add(10 * time.Second)
	got, err = c.Collect(ctx, targets)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]float64{"hit": 2, "miss": 1, "bypass": 0, "expired": 1, "other": 1}
	assertCounts(t, got, want)
	for _, s := range got {
		if s.Labels[metrics.LabelClientGroupID] != "g1" {
			t.Errorf("sample %v lacks the client group", s)
		}
	}

	// Counters are cumulative, and an entry is never counted twice.
	add(11*time.Second, accessLine("BYPASS"))
	now = t0.Add(20 * time.Second)
	got, _ = c.Collect(ctx, targets)
	want["bypass"] = 1
	assertCounts(t, got, want)
	now = t0.Add(30 * time.Second)
	got, _ = c.Collect(ctx, targets)
	assertCounts(t, got, want)
}

func assertCounts(t *testing.T, got []metrics.Sample, want map[string]float64) {
	t.Helper()
	have := values(got, metrics.MetricPageCacheRequests)["s1"]
	if len(have) != len(want) {
		t.Errorf("counts = %v, want %v", have, want)
		return
	}
	for status, n := range want {
		if have[status] != n {
			t.Errorf("counts = %v, want %v", have, want)
			return
		}
	}
}

func TestPageCacheCollectorErrors(t *testing.T) {
	ctx := context.Background()
	fake := provisionertest.New()
	boom := errors.New("boom")
	fake.Err = func(op, siteID string) error {
		if op == "Logs" && siteID == "s1" {
			return boom
		}
		return nil
	}
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	c := &metrics.PageCacheCollector{Logs: fake, Now: func() time.Time { return now }}
	targets := []metrics.Target{{SiteID: "s1", ClientGroupID: "g"}, {SiteID: "s2", ClientGroupID: "g"}}
	if _, err := c.Collect(ctx, targets); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	got, err := c.Collect(ctx, targets)
	if !errors.Is(err, boom) {
		t.Errorf("err = %v, want boom", err)
	}
	// The site that failed still reports its last counts; the other
	// (not found: no such site in the fake) is skipped silently.
	if len(values(got, metrics.MetricPageCacheRequests)["s1"]) == 0 {
		t.Error("the failing site reported nothing")
	}
}
