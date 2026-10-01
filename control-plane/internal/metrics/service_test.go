package metrics_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/metrics"
	"github.com/rchouinard/pressfleet/control-plane/internal/metrics/metricstest"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
)

type collectorFunc struct {
	name     string
	interval time.Duration
	fn       func(ctx context.Context, targets []metrics.Target) ([]metrics.Sample, error)
}

func (c collectorFunc) Name() string            { return c.name }
func (c collectorFunc) Interval() time.Duration { return c.interval }
func (c collectorFunc) Collect(ctx context.Context, t []metrics.Target) ([]metrics.Sample, error) {
	return c.fn(ctx, t)
}

func newService(t *testing.T, w metrics.Writer, targets metrics.TargetSource, cs ...metrics.Collector) *metrics.Service {
	t.Helper()
	svc, err := metrics.NewService(metrics.ServiceOptions{
		Writer: w, Targets: targets, DefaultInterval: 10 * time.Millisecond, Logger: storetest.Logger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cs {
		if err := svc.Register(c); err != nil {
			t.Fatal(err)
		}
	}
	return svc
}

// start runs svc until the test ends.
func start(t *testing.T, svc *metrics.Service) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Go(func() { _ = svc.Run(ctx) })
	t.Cleanup(func() { cancel(); wg.Wait() })
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func target(site, group string) metrics.Target {
	return metrics.Target{SiteID: site, ClientGroupID: group}
}

// siteInfoCount counts the pressfleet_site_info samples written for site.
func siteInfoCount(w *metricstest.Writer, site string) int {
	n := 0
	for _, s := range w.Named("pressfleet_site_info") {
		if s.Labels["site_id"] == site {
			n++
		}
	}
	return n
}

// runsAfter waits for the collector to complete several more runs.
func runsAfter(t *testing.T, w *metricstest.Writer, extra int) {
	t.Helper()
	n := len(w.Named("pressfleet_collector_targets"))
	eventually(t, "more runs", func() bool { return len(w.Named("pressfleet_collector_targets")) >= n+extra })
}

func TestServiceWritesSamplesAndSelfMetrics(t *testing.T) {
	w, targets := &metricstest.Writer{}, &metricstest.Targets{}
	targets.Set(target("s1", "g1"), target("s2", "g2"))
	c := collectorFunc{name: "demo", fn: func(_ context.Context, ts []metrics.Target) ([]metrics.Sample, error) {
		var out []metrics.Sample
		for _, tg := range ts {
			out = append(out, tg.Sample("pressfleet_demo_value", 7, "kind", "x"))
		}
		return out, nil
	}}
	start(t, newService(t, w, targets, c))
	eventually(t, "two runs", func() bool {
		s := w.Named("pressfleet_collector_samples_written_total")
		return len(s) >= 2 && s[len(s)-1].Value >= 2
	})

	vals := w.Named("pressfleet_demo_value")
	for _, v := range vals[:2] {
		if v.Labels["kind"] != "x" || v.Labels[metrics.LabelSiteID] == "" || v.Labels[metrics.LabelClientGroupID] == "" || v.Time.IsZero() {
			t.Errorf("sample = %+v", v)
		}
	}
	for _, name := range []string{
		"pressfleet_collector_scrape_duration_seconds", "pressfleet_collector_scrapes_total",
		"pressfleet_collector_samples_dropped_total", "pressfleet_collector_write_errors_total",
		"pressfleet_collector_targets", "pressfleet_collector_last_success_timestamp_seconds",
	} {
		s := w.Named(name)
		if len(s) == 0 || s[0].Labels["collector"] != "demo" {
			t.Errorf("self-metric %s missing or unlabelled: %+v", name, s)
		}
	}
	if got := w.Named("pressfleet_collector_targets")[0].Value; got != 2 {
		t.Errorf("targets = %v, want 2", got)
	}
}

func TestServiceIsolatesFailingCollectors(t *testing.T) {
	w, targets := &metricstest.Writer{}, &metricstest.Targets{}
	var good atomic.Int32
	bad := collectorFunc{name: "bad", fn: func(context.Context, []metrics.Target) ([]metrics.Sample, error) {
		return nil, errors.New("boom")
	}}
	panics := collectorFunc{name: "panics", fn: func(context.Context, []metrics.Target) ([]metrics.Sample, error) {
		panic("kaboom")
	}}
	hangs := collectorFunc{name: "hangs", interval: 10 * time.Millisecond, fn: func(ctx context.Context, _ []metrics.Target) ([]metrics.Sample, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	ok := collectorFunc{name: "good", fn: func(context.Context, []metrics.Target) ([]metrics.Sample, error) {
		good.Add(1)
		return []metrics.Sample{{Name: "pressfleet_good", Value: 1}}, nil
	}}
	start(t, newService(t, w, targets, bad, panics, hangs, ok))
	eventually(t, "good collector to keep running", func() bool { return good.Load() >= 3 })

	eventually(t, "errors recorded for each failing collector", func() bool {
		seen := map[string]bool{}
		for _, s := range w.Named("pressfleet_collector_scrapes_total") {
			if s.Labels["result"] == "error" && s.Value >= 2 {
				seen[s.Labels["collector"]] = true
			}
		}
		return seen["bad"] && seen["panics"] && seen["hangs"] && !seen["good"]
	})
	for _, s := range w.Named("pressfleet_collector_last_success_timestamp_seconds") {
		if s.Labels["collector"] != "good" {
			t.Errorf("last success reported for failing collector %s", s.Labels["collector"])
		}
	}
}

func TestServicePartialSamplesWithError(t *testing.T) {
	w, targets := &metricstest.Writer{}, &metricstest.Targets{}
	c := collectorFunc{name: "partial", fn: func(context.Context, []metrics.Target) ([]metrics.Sample, error) {
		return []metrics.Sample{{Name: "pressfleet_partial", Value: 1}}, errors.New("one source failed")
	}}
	start(t, newService(t, w, targets, c))
	eventually(t, "partial sample", func() bool { return len(w.Named("pressfleet_partial")) > 0 })
}

func TestServiceEnforcesLabelsAndNames(t *testing.T) {
	w, targets := &metricstest.Writer{}, &metricstest.Targets{}
	targets.Set(target("s1", "g1"))
	c := collectorFunc{name: "sloppy", fn: func(context.Context, []metrics.Target) ([]metrics.Sample, error) {
		return []metrics.Sample{
			{Name: "pressfleet_fixed", Labels: map[string]string{"site_id": "s1", "client_group_id": "wrong"}, Value: 1},
			{Name: "pressfleet_fixed2", Labels: map[string]string{"site_id": "s1"}, Value: 1},
			{Name: "pressfleet_gone", Labels: map[string]string{"site_id": "deleted"}, Value: 1},
			{Name: "pressfleet_orphan_group", Labels: map[string]string{"client_group_id": "g1"}, Value: 1},
			{Name: "other_prefix", Value: 1},
			{Name: "pressfleet_collector_spoofed", Value: 1},
			{Name: "pressfleet_platform", Value: 1},
		}, nil
	}}
	start(t, newService(t, w, targets, c))
	eventually(t, "a run", func() bool { return len(w.Named("pressfleet_platform")) > 0 })

	for _, name := range []string{"pressfleet_fixed", "pressfleet_fixed2"} {
		s := w.Named(name)
		if len(s) == 0 || s[0].Labels["client_group_id"] != "g1" {
			t.Errorf("%s = %+v, want client_group_id g1", name, s)
		}
	}
	for _, name := range []string{"pressfleet_gone", "pressfleet_orphan_group", "other_prefix", "pressfleet_collector_spoofed"} {
		if s := w.Named(name); len(s) != 0 {
			t.Errorf("%s was written: %+v", name, s)
		}
	}
	eventually(t, "dropped counter", func() bool {
		s := w.Named("pressfleet_collector_samples_dropped_total")
		return len(s) > 0 && s[len(s)-1].Value >= 4
	})
}

func TestServiceCountsWriteErrors(t *testing.T) {
	w, targets := &metricstest.Writer{}, &metricstest.Targets{}
	w.FailWith(errors.New("vm down"))
	c := collectorFunc{name: "demo", fn: func(context.Context, []metrics.Target) ([]metrics.Sample, error) {
		return []metrics.Sample{{Name: "pressfleet_demo", Value: 1}}, nil
	}}
	start(t, newService(t, w, targets, c))
	eventually(t, "failed writes", func() bool { return w.Calls() >= 3 })
	w.FailWith(nil)
	eventually(t, "write errors reported after recovery", func() bool {
		s := w.Named("pressfleet_collector_write_errors_total")
		return len(s) > 0 && s[len(s)-1].Value >= 3
	})
}

func TestServiceTargetErrorFailsTheRun(t *testing.T) {
	w, targets := &metricstest.Writer{}, &metricstest.Targets{}
	targets.FailWith(errors.New("db down"))
	var called atomic.Bool
	c := collectorFunc{name: "demo", fn: func(context.Context, []metrics.Target) ([]metrics.Sample, error) {
		called.Store(true)
		return nil, nil
	}}
	start(t, newService(t, w, targets, c))
	eventually(t, "error scrape", func() bool {
		for _, s := range w.Named("pressfleet_collector_scrapes_total") {
			if s.Labels["result"] == "error" && s.Value > 0 {
				return true
			}
		}
		return false
	})
	if called.Load() {
		t.Error("collector ran without targets")
	}
}

func TestServiceTargetsAreRefreshedEveryRun(t *testing.T) {
	w, targets := &metricstest.Writer{}, &metricstest.Targets{}
	targets.Set(target("s1", "g1"))
	start(t, newService(t, w, targets, metrics.InventoryCollector{}))
	eventually(t, "first site", func() bool { return siteInfoCount(w, "s1") > 0 })

	targets.Set(target("s1", "g1"), target("s2", "g1"))
	eventually(t, "new site without restart", func() bool { return siteInfoCount(w, "s2") > 0 })

	targets.Set(target("s2", "g1"))
	runsAfter(t, w, 2) // a run that started before the change may still include s1
	before := siteInfoCount(w, "s1")
	runsAfter(t, w, 5)
	if after := siteInfoCount(w, "s1"); after != before {
		t.Errorf("deleted site still collected: %d samples became %d", before, after)
	}
}

func TestRegisterValidation(t *testing.T) {
	svc := newService(t, &metricstest.Writer{}, &metricstest.Targets{})
	if err := svc.Register(collectorFunc{name: "a_b1"}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"", "Bad", "has space", "a_b1"} {
		if err := svc.Register(collectorFunc{name: name}); err == nil {
			t.Errorf("Register(%q) succeeded", name)
		}
	}
}

func TestRunStopsWithContext(t *testing.T) {
	svc := newService(t, &metricstest.Writer{}, &metricstest.Targets{}, metrics.InventoryCollector{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error)
	go func() { done <- svc.Run(ctx) }()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop")
	}
	if err := svc.Register(collectorFunc{name: "late"}); err == nil {
		t.Error("Register after Run succeeded")
	}
}

func TestNewServiceRequiredOptions(t *testing.T) {
	good := metrics.ServiceOptions{Writer: &metricstest.Writer{}, Targets: &metricstest.Targets{}, DefaultInterval: time.Second, Logger: storetest.Logger()}
	for name, mod := range map[string]func(*metrics.ServiceOptions){
		"writer":   func(o *metrics.ServiceOptions) { o.Writer = nil },
		"targets":  func(o *metrics.ServiceOptions) { o.Targets = nil },
		"logger":   func(o *metrics.ServiceOptions) { o.Logger = nil },
		"interval": func(o *metrics.ServiceOptions) { o.DefaultInterval = 0 },
	} {
		o := good
		mod(&o)
		if _, err := metrics.NewService(o); err == nil {
			t.Errorf("without %s: NewService succeeded", name)
		}
	}
}
