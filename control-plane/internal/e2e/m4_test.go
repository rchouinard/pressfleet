package e2e_test

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/alerts"
	"github.com/rchouinard/pressfleet/control-plane/internal/api/client"
	"github.com/rchouinard/pressfleet/control-plane/internal/catalog"
	"github.com/rchouinard/pressfleet/control-plane/internal/e2e"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner/compose"
)

// Timing of the M4 test. The platform collects and evaluates every few
// seconds (instead of every 30), but VictoriaMetrics still hides samples
// younger than its -search.latencyOffset (30 seconds by default) from
// queries, so every wait below is counted in minutes.
const (
	m4Interval = 5 * time.Second
	// m4MetricsWait bounds waiting for every catalog metric to have data.
	m4MetricsWait = 5 * time.Minute
	// m4AlertWait bounds waiting for an alert to fire or to resolve, and
	// for a delivery to reach its channel.
	m4AlertWait = 4 * time.Minute
	// m4RuleFor replaces the default rules' durations, which are minutes
	// to a quarter of an hour. Editing them is supported; the expressions,
	// operators, and thresholds stay as shipped.
	m4RuleFor = 10
)

// m4RequiredMetrics are the catalog entries that cover every row of design
// doc §7.1 and §7.2. The test checks that the catalog has them all, so a
// metric the design promises can't be dropped from the catalog unnoticed,
// then that every entry of the catalog, these and any later ones, has data.
var m4RequiredMetrics = []string{
	// §7.1: availability, traffic, page cache, PHP, resources, storage,
	// object cache, TLS, and backups, per site.
	"uptime", "http_check_status", "response_time",
	"request_rate", "status_codes", "latency",
	"page_cache_ratio",
	"php_workers", "php_max_children_reached", "php_slow_requests",
	"cpu", "memory", "restarts",
	"wp_content_size", "database_size",
	"object_cache_keys", "object_cache_memory", "object_cache_hit_ratio",
	"tls_expiry",
	"backup_last_success", "backup_size", "backup_duration",
	// §7.2: host, MariaDB, Redis, jobs, and the Restic repositories.
	"host_cpu", "host_memory", "host_disk_usage", "host_disk_io",
	"mariadb_connections", "mariadb_slow_queries", "mariadb_buffer_pool",
	"redis_memory", "redis_hit_ratio", "redis_evictions",
	"job_queue_depth", "job_failure_rate",
	"restic_repository_size",
}

// TestM4Acceptance automates the M4 "done when" (design doc §8 Phase 4,
// #79): every metric in §7.1 and §7.2 is collected and queryable through
// the API, and each default alert rule fires and resolves. On one platform
// whose certificates come from Pebble, so the control plane's own checks of
// the site succeed, it:
//
//  1. provisions a site, sends it traffic, backs it up, and checks that
//     every entry of the metric catalog (and so every metric the dashboard
//     can chart) has data from the real collectors, from the site's
//     endpoint, the platform's, or both, and that the summaries answer;
//  2. fires each of the default alert rules, one per condition the rule
//     describes, by writing the series it reads into VictoriaMetrics, next
//     to a healthy twin of each series that must not alert, and checks that
//     the rule's alert fires and that a delivery of it reaches a webhook
//     channel;
//  3. clears each condition and checks that the alert resolves and that
//     the resolution is delivered too.
//
// Part 2 and 3 write the series themselves, because most conditions can't
// be caused for real in a test (a full disk, a certificate about to
// expire, a Redis under memory pressure, ten minutes of 5xx responses).
// What they test is everything between the metrics store and the channel:
// the rules as shipped, their evaluation by the alert engine, the alert
// lifecycle, and delivery. Part 1 tests the collectors, which feed the
// store; the unit tests of each collector check the series it writes
// against the names the rules and the catalog read.
func TestM4Acceptance(t *testing.T) {
	projectDir := t.TempDir()
	var siteProject compose.Project
	// Registered before Start, so it runs after the platform teardown: see
	// TestSiteCreateDelete.
	t.Cleanup(func() {
		if siteProject.Name == "" {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if res, err := siteProject.Resources(ctx); err == nil && !res.Empty() {
			t.Logf("removing leftover site project %s", siteProject.Name)
			if err := siteProject.Down(ctx, compose.DownOptions{Volumes: true}); err != nil {
				t.Errorf("removing leftover site project %s: %v", siteProject.Name, err)
			}
		}
	})
	p := e2e.StartWithPebble(t)
	ctx := t.Context()

	// The control plane checks the site over HTTPS, as a visitor does, so
	// it must trust the certificate Pebble issued for the preview domain.
	// Go reads the file SSL_CERT_FILE names instead of the system roots,
	// which the control plane doesn't need here.
	roots := filepath.Join(p.DataDir, "pebble-roots.pem")
	if err := os.WriteFile(roots, p.Pebble.RootsPEM, 0o644); err != nil {
		t.Fatal(err)
	}

	cat, err := catalog.Load()
	if err != nil {
		t.Fatal(err)
	}
	php, ok := cat.DefaultPHPVersion(time.Now())
	if !ok {
		t.Fatal("no default PHP version")
	}
	images, err := cat.Images(php, cat.LatestWordPress().Version)
	if err != nil {
		t.Fatal(err)
	}
	e2e.EnsureImages(t, images.PHP, images.Nginx)

	groupID := createGroup(t, p, "M4 Agency")
	created := createSite(t, p, groupID, "M4 Site", php)
	site := created.Site
	siteProject = compose.Project{Name: compose.SiteProjectName(site.ID), Dir: projectDir}
	provisioned := finishedJob(t, p, created.Job.ID)
	t.Logf("site %s on %s", site.ID, site.PreviewDomain)

	hc := p.VerifyingIngressClient(t)
	home := "https://" + site.PreviewDomain + "/"
	waitCertified(t, hc, home, nil, http.StatusOK, provisioned)

	keepTraffic(ctx, hc, home)

	// A backup gives the backup metrics, and the repository, its size.
	resp, err := p.Client.CreateSiteBackupWithResponse(ctx, site.ID)
	if err != nil {
		t.Fatal(err)
	}
	if resp.JSON202 == nil {
		t.Fatalf("back up: status %d: %s", resp.StatusCode(), resp.Body)
	}
	waitJob(t, p, resp.JSON202.Job.ID)

	// Restarting makes every collector run at once. The storage collectors
	// run every 10 and 30 minutes, so a site created after the control
	// plane started would wait that long for its first sizes.
	p.Restart(t, map[string]string{
		"SSL_CERT_FILE":                                 roots,
		"PRESSFLEET_METRICS_INTERVAL_SECONDS":           fmt.Sprint(int(m4Interval.Seconds())),
		"PRESSFLEET_ALERTS_EVALUATION_INTERVAL_SECONDS": fmt.Sprint(int(m4Interval.Seconds())),
	})

	t.Run("every metric of the design is collected and queryable", func(t *testing.T) {
		checkMetrics(t, p, site.ID, groupID, nil)
	})

	t.Run("each default alert rule fires and resolves", func(t *testing.T) {
		checkAlerts(t, p, site.ID, groupID)
	})
}

// keepTraffic sends requests to a site's home page (an https:// URL ending
// in "/") until ctx ends: rates need requests in their window, and the page
// cache needs hits and misses (a query string always misses the cache).
func keepTraffic(ctx context.Context, hc *http.Client, home string) {
	go func() {
		for ctx.Err() == nil {
			for _, path := range []string{"/", "/?s=m4", "/missing-m4/"} {
				req, err := http.NewRequestWithContext(ctx, http.MethodGet, home[:len(home)-1]+path, nil)
				if err != nil {
					return
				}
				if resp, err := hc.Do(req); err == nil {
					_ = resp.Body.Close()
				}
			}
			select {
			case <-ctx.Done():
			case <-time.After(time.Second):
			}
		}
	}()
}

// checkMetrics checks the catalog and then, until m4MetricsWait is up, that
// every entry has at least one point from the endpoint its scope belongs
// to. targetNoSource adds the entries the target has no source for, with
// the reason.
func checkMetrics(t *testing.T, p *e2e.Platform, siteID, groupID string, targetNoSource map[string]string) {
	ctx := t.Context()
	resp, err := p.Client.GetMetricCatalogWithResponse(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if resp.JSON200 == nil {
		t.Fatalf("metric catalog: status %d: %s", resp.StatusCode(), resp.Body)
	}
	entries := resp.JSON200.Items
	var ids []string
	for _, e := range entries {
		ids = append(ids, e.ID)
	}
	for _, id := range m4RequiredMetrics {
		if !slices.Contains(ids, id) {
			t.Errorf("the catalog has no %q, which design doc §7.1 or §7.2 asks for", id)
		}
	}

	// Entries with no source, which can't have data: the catalog has them
	// ahead of their collectors.
	noSource := map[string]string{
		"object_cache_hit_ratio": "Redis counts lookups for the whole instance, not per site (package metrics, RedisCollector), so there are no per-site hit and miss counters; redis_hit_ratio covers the instance",
	}
	maps.Copy(noSource, targetNoSource)

	// hasData asks for one entry over the last 15 minutes.
	hasData := func(e client.MetricDescriptor) (bool, error) {
		start := time.Now().Add(-15 * time.Minute)
		var res *client.MetricsRange
		switch e.Scope {
		case client.MetricDescriptorScopeSite:
			r, err := p.Client.QuerySiteMetricsWithResponse(ctx, siteID, &client.QuerySiteMetricsParams{Metric: &[]string{e.ID}, Start: &start})
			if err != nil {
				return false, err
			}
			if r.JSON200 == nil {
				return false, fmt.Errorf("status %d: %s", r.StatusCode(), r.Body)
			}
			res = r.JSON200
		default:
			params := &client.QueryPlatformMetricsParams{Metric: &[]string{e.ID}, Start: &start}
			if e.Scope == client.MetricDescriptorScopeClientGroup {
				params.ClientGroupID = &groupID
			}
			r, err := p.Client.QueryPlatformMetricsWithResponse(ctx, params)
			if err != nil {
				return false, err
			}
			if r.JSON200 == nil {
				return false, fmt.Errorf("status %d: %s", r.StatusCode(), r.Body)
			}
			res = r.JSON200
		}
		for _, m := range res.Metrics {
			for _, s := range m.Series {
				if len(s.Points) > 0 {
					return true, nil
				}
			}
		}
		return false, nil
	}

	deadline := time.Now().Add(m4MetricsWait)
	have := map[string]bool{}
	for {
		var missing []string
		for _, e := range entries {
			if have[e.ID] {
				continue
			}
			if why, skip := noSource[e.ID]; skip {
				t.Logf("not checking %s: %s", e.ID, why)
				have[e.ID] = true
				continue
			}
			ok, err := hasData(e)
			if err != nil {
				t.Fatalf("querying %s (%s): %v", e.ID, e.Scope, err)
			}
			if ok {
				have[e.ID] = true
			} else {
				missing = append(missing, e.ID)
			}
		}
		if len(missing) == 0 {
			t.Logf("all %d catalog entries have data", len(entries))
			break
		}
		if time.Now().After(deadline) {
			t.Errorf("no data after %s for: %s", m4MetricsWait, strings.Join(missing, ", "))
			break
		}
		t.Logf("waiting for data: %s", strings.Join(missing, ", "))
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(10 * time.Second):
		}
	}

	t.Run("summaries", func(t *testing.T) {
		site, err := p.Client.GetSiteMetricsSummaryWithResponse(ctx, siteID, &client.GetSiteMetricsSummaryParams{})
		if err != nil {
			t.Fatal(err)
		}
		if site.JSON200 == nil || len(site.JSON200.Metrics) == 0 {
			t.Errorf("site summary: status %d: %s", site.StatusCode(), site.Body)
		}
		plat, err := p.Client.GetPlatformMetricsSummaryWithResponse(ctx, &client.GetPlatformMetricsSummaryParams{})
		if err != nil {
			t.Fatal(err)
		}
		if plat.JSON200 == nil || len(plat.JSON200.Metrics) == 0 {
			t.Errorf("platform summary: status %d: %s", plat.StatusCode(), plat.Body)
		}
	})
}

// m4Sample is one series of a scenario.
type m4Sample struct {
	metric string
	labels map[string]string
	// bad and good are the series' values while the condition holds and
	// while it doesn't. For a counter they are the increase per tick. For
	// a time (timeOffset) they are seconds from the moment of the write.
	bad, good float64
	counter   bool
	timeValue bool
}

// m4Scenario causes the condition of one default alert rule.
type m4Scenario struct {
	// rule is the default rule's name.
	rule string
	// key identifies the scenario in the series' scenario label.
	key string
	// site gives the series the site's labels, which a site rule needs.
	site bool
	// clearBySeries ends the condition by deleting the series instead of
	// writing healthy values: a rate or an increase looks back over minutes,
	// so only the series disappearing ends it in time.
	clearBySeries bool
	// aggregated is for a rule whose expression sums the series' labels
	// away: the alert carries only the site's labels, and a healthy twin
	// would be summed into the bad series.
	aggregated bool
	samples    []m4Sample
}

// m4Scenarios has one scenario per default rule, with values on the bad
// side of its threshold. Each is checked against the shipped expression, in
// the comment.
func m4Scenarios() []m4Scenario {
	return []m4Scenario{
		{rule: "Site down", key: "site_down", site: true, // site_up == 0
			samples: []m4Sample{{metric: alerts.MetricSiteUp, bad: 0, good: 1}}},
		{rule: "Elevated 5xx error rate", key: "elevated_5xx_rate", site: true, clearBySeries: true, aggregated: true, // 5xx share of requests > 5%: 50% of 2 requests per second
			samples: []m4Sample{
				{metric: alerts.MetricSiteHTTPRequests, labels: map[string]string{"status_class": "5xx"}, bad: 10, counter: true},
				{metric: alerts.MetricSiteHTTPRequests, labels: map[string]string{"status_class": "2xx"}, bad: 10, counter: true},
			}},
		{rule: "TLS certificate expiring", key: "tls_certificate_expiring", site: true, // under 14 days
			samples: []m4Sample{{metric: alerts.MetricSiteCertExpiry, labels: map[string]string{"domain": "expiring.m4.e2e.test"}, bad: 10 * 24 * 3600, good: 60 * 24 * 3600, timeValue: true}}},
		{rule: "TLS certificate renewal failing", key: "tls_certificate_renewal_failing", site: true, // under 3 days
			samples: []m4Sample{{metric: alerts.MetricSiteCertExpiry, labels: map[string]string{"domain": "failing.m4.e2e.test"}, bad: 24 * 3600, good: 60 * 24 * 3600, timeValue: true}}},
		{rule: "Backup failed", key: "backup_failed", site: true, // last_run_success == 0
			samples: []m4Sample{{metric: alerts.MetricSiteBackupLastRunOK, bad: 0, good: 1}}},
		{rule: "Backup stale", key: "backup_stale", site: true, // last success over 36 hours ago
			samples: []m4Sample{{metric: alerts.MetricSiteBackupLastSuccess, bad: -48 * 3600, good: -3600, timeValue: true}}},
		{rule: "Disk nearly full", key: "disk_nearly_full", // 1 - avail/size > 90%: 95% against 40%
			samples: []m4Sample{
				{metric: alerts.MetricHostFilesystemSizeBytes, labels: map[string]string{"mountpoint": "/m4"}, bad: 100e9, good: 100e9},
				{metric: alerts.MetricHostFilesystemAvail, labels: map[string]string{"mountpoint": "/m4"}, bad: 5e9, good: 60e9},
			}},
		{rule: "Container restart loop", key: "container_restart_loop", clearBySeries: true, // more than 3 restarts in 15 minutes: one every tick
			samples: []m4Sample{{metric: alerts.MetricContainerRestarts, labels: map[string]string{"container": "php"}, bad: 1, counter: true}}},
		{rule: "PHP-FPM worker saturation", key: "php_fpm_saturation", site: true, // active/max > 90%: 19 of 20 against 2 of 20
			samples: []m4Sample{
				{metric: alerts.MetricSitePHPFPMActive, bad: 19, good: 2},
				{metric: alerts.MetricSitePHPFPMMaxChildren, bad: 20, good: 20},
			}},
		{rule: "Redis eviction spike", key: "redis_eviction_spike", clearBySeries: true, // more than 1 eviction per second: 2
			samples: []m4Sample{{metric: alerts.MetricRedisEvictedKeys, bad: 10, counter: true}}},
		{rule: "Plugin or theme updates pending", key: "updates_pending", site: true, // more than 0 updates
			samples: []m4Sample{{metric: alerts.MetricSitePendingUpdates, labels: map[string]string{"kind": "plugin"}, bad: 2, good: 0}}},
	}
}

// m4Writer writes the scenarios' series into VictoriaMetrics, every tick,
// until it is told to heal them.
type m4Writer struct {
	p               *e2e.Platform
	siteID, groupID string
	scenarios       []m4Scenario
	errorf          func(format string, args ...any)

	mu     sync.Mutex
	tick   int
	healed bool
	wg     sync.WaitGroup
}

// VictoriaMetrics' address inside its container, and the endpoints the
// writer uses.
const (
	m4VictoriaMetrics = "http://127.0.0.1:8428"
	m4ImportPath      = "/api/v1/import/prometheus"
	m4DeleteSeries    = "/api/v1/admin/tsdb/delete_series"
)

// newM4Writer returns a writer for the scenarios.
func newM4Writer(t *testing.T, p *e2e.Platform, siteID, groupID string, scenarios []m4Scenario) *m4Writer {
	return &m4Writer{p: p, siteID: siteID, groupID: groupID, scenarios: scenarios, errorf: t.Errorf}
}

// post sends a form or text body to VictoriaMetrics from inside its own
// container (e2e.Platform.ExecVictoriaMetrics).
func (w *m4Writer) post(ctx context.Context, path, body string) error {
	out, err := w.p.ExecVictoriaMetrics(ctx, "wget", "-q", "-O", "-", "--post-data", body, m4VictoriaMetrics+path)
	if err != nil {
		return fmt.Errorf("posting to %s: %w: %s", path, err, out)
	}
	return nil
}

// lines returns the exposition text of one tick. Every series has a
// "source" label, so the real series of the platform stay apart from them,
// and a "scenario" label, so each alert can be told from the others.
func (w *m4Writer) lines(now time.Time) string {
	var b strings.Builder
	for _, sc := range w.scenarios {
		for _, twin := range []bool{false, true} {
			if twin && sc.aggregated {
				continue
			}
			if w.healed && sc.clearBySeries {
				continue
			}
			bad := !twin && !w.healed
			for _, s := range sc.samples {
				labels := map[string]string{"source": "e2e", "scenario": sc.key}
				if twin {
					labels["scenario"] += "-ok"
				}
				if sc.site {
					labels["site_id"], labels["client_group_id"] = w.siteID, w.groupID
				}
				for k, v := range s.labels {
					labels[k] = v
				}
				value := s.good
				if bad {
					value = s.bad
				}
				switch {
				case s.counter:
					value *= float64(w.tick + 1)
				case s.timeValue:
					value += float64(now.Unix())
				}
				var parts []string
				for _, k := range slices.Sorted(maps.Keys(labels)) {
					parts = append(parts, fmt.Sprintf("%s=%q", k, labels[k]))
				}
				fmt.Fprintf(&b, "%s{%s} %g %d\n", s.metric, strings.Join(parts, ","), value, now.UnixMilli())
			}
		}
	}
	return b.String()
}

// write sends one tick.
func (w *m4Writer) write(ctx context.Context) {
	w.mu.Lock()
	defer w.mu.Unlock()
	body := w.lines(time.Now())
	w.tick++
	if body == "" {
		return
	}
	if err := w.post(ctx, m4ImportPath, body); err != nil && ctx.Err() == nil {
		w.errorf("writing the scenarios' series: %v", err)
	}
}

// run writes a tick every m4Interval until ctx ends.
func (w *m4Writer) run(ctx context.Context) {
	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		for {
			w.write(ctx)
			select {
			case <-ctx.Done():
				return
			case <-time.After(m4Interval):
			}
		}
	}()
}

// heal ends every condition: gauges get their healthy values, and the
// series of the rate scenarios are deleted.
func (w *m4Writer) heal(ctx context.Context) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.healed = true
	var keys []string
	for _, sc := range w.scenarios {
		if sc.clearBySeries {
			keys = append(keys, sc.key)
		}
	}
	if len(keys) > 0 {
		if err := w.deleteSeries(ctx, `{source="e2e",scenario=~"`+strings.Join(keys, "|")+`"}`); err != nil {
			w.errorf("%v", err)
		}
	}
	if err := w.post(ctx, m4ImportPath, w.lines(time.Now())); err != nil {
		w.errorf("writing healthy values: %v", err)
	}
}

// deleteSeries removes the series a selector matches.
func (w *m4Writer) deleteSeries(ctx context.Context, selector string) error {
	return w.post(ctx, m4DeleteSeries, url.Values{"match[]": {selector}}.Encode())
}

// checkAlerts fires and resolves every default rule.
func checkAlerts(t *testing.T, p *e2e.Platform, siteID, groupID string) {
	ctx := t.Context()
	scenarios := m4Scenarios()

	// Every default rule has a scenario, and every scenario a rule.
	rules := map[string]client.AlertRule{}
	listed, err := p.Client.ListAlertRulesWithResponse(ctx, &client.ListAlertRulesParams{Limit: new(100)})
	if err != nil {
		t.Fatal(err)
	}
	if listed.JSON200 == nil {
		t.Fatalf("list alert rules: status %d: %s", listed.StatusCode(), listed.Body)
	}
	for _, r := range listed.JSON200.Items {
		if r.Builtin {
			rules[r.Name] = r
		}
	}
	for _, d := range alerts.DefaultRules() {
		if _, ok := rules[d.Name]; !ok {
			t.Fatalf("default rule %q (%s) is missing; rules: %v", d.Name, d.BuiltinKey, slices.Sorted(maps.Keys(rules)))
		}
		if !slices.ContainsFunc(scenarios, func(s m4Scenario) bool { return s.rule == d.Name && s.key == d.BuiltinKey }) {
			t.Fatalf("default rule %q (%s) has no scenario", d.Name, d.BuiltinKey)
		}
	}
	if len(scenarios) != len(alerts.DefaultRules()) {
		t.Fatalf("%d scenarios for %d default rules", len(scenarios), len(alerts.DefaultRules()))
	}
	for _, sc := range scenarios {
		r := rules[sc.rule]
		if !r.Enabled {
			t.Fatalf("default rule %q is disabled", sc.rule)
		}
		forSeconds := m4RuleFor
		if _, err := p.Client.UpdateAlertRuleWithResponse(ctx, r.ID, client.UpdateAlertRuleJSONRequestBody{ForSeconds: &forSeconds}); err != nil {
			t.Fatal(err)
		}
	}

	// A webhook channel that accepts any POST: VictoriaMetrics' health
	// endpoint, on the platform network (on Kubernetes, its Service in the
	// control plane's namespace). What a delivery carries is the
	// alertchannels package's unit tests' business; here it matters that
	// the engine's events arrive.
	channelURL := "http://victoria-metrics:8428/health"
	created, err := p.Client.CreateAlertChannelWithResponse(ctx, client.CreateAlertChannelJSONRequestBody{
		Name:    "M4 webhook",
		Type:    client.AlertChannelTypeWebhook,
		Webhook: &client.AlertChannelWebhookCreate{URL: &channelURL},
	})
	if err != nil {
		t.Fatal(err)
	}
	if created.JSON201 == nil {
		t.Fatalf("create alert channel: status %d: %s", created.StatusCode(), created.Body)
	}
	channelID := created.JSON201.ID

	w := newM4Writer(t, p, siteID, groupID, scenarios)
	// Registered first, so it runs last: stop writing, then leave nothing
	// behind in the metrics store.
	runCtx, stop := context.WithCancel(ctx)
	t.Cleanup(func() {
		stop()
		w.wg.Wait()
		cctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_ = w.deleteSeries(cctx, `{source="e2e"}`)
	})
	w.run(runCtx)

	// find returns the alert a scenario raised, if any: of the scenario's
	// rule, for the site, with the scenario's label unless the rule
	// aggregates it away.
	find := func(sc m4Scenario, scenario string) (client.Alert, bool) {
		ruleID := rules[sc.rule].ID
		resp, err := p.Client.ListAlertsWithResponse(ctx, &client.ListAlertsParams{RuleID: &ruleID, Limit: new(100)})
		if err != nil {
			t.Fatal(err)
		}
		if resp.JSON200 == nil {
			t.Fatalf("list alerts: status %d: %s", resp.StatusCode(), resp.Body)
		}
		for _, a := range resp.JSON200.Items {
			switch {
			case sc.aggregated && sc.site && a.Labels["site_id"] == siteID && a.Labels["scenario"] == "":
				return a, true
			case a.Labels["scenario"] == scenario && a.Labels["source"] == "e2e":
				return a, true
			}
		}
		return client.Alert{}, false
	}
	// waitFor polls until every scenario's alert is in the wanted state.
	waitFor := func(state client.AlertState) map[string]client.Alert {
		t.Helper()
		got := map[string]client.Alert{}
		deadline := time.Now().Add(m4AlertWait)
		for {
			var waiting []string
			for _, sc := range scenarios {
				if _, done := got[sc.key]; done {
					continue
				}
				if a, ok := find(sc, sc.key); ok && a.State == state {
					got[sc.key] = a
				} else {
					waiting = append(waiting, sc.key)
				}
			}
			if len(waiting) == 0 {
				return got
			}
			if time.Now().After(deadline) {
				t.Errorf("after %s, still not %s: %s", m4AlertWait, state, strings.Join(waiting, ", "))
				return got
			}
			select {
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			case <-time.After(5 * time.Second):
			}
		}
	}
	// delivered waits for a delivered event of each alert on the channel.
	delivered := func(alertsByKey map[string]client.Alert, event client.AlertEventType) {
		t.Helper()
		deadline := time.Now().Add(m4AlertWait)
		pending := map[string]string{}
		for key, a := range alertsByKey {
			pending[key] = a.ID
		}
		for len(pending) > 0 {
			for key, id := range pending {
				resp, err := p.Client.ListAlertDeliveriesWithResponse(ctx, &client.ListAlertDeliveriesParams{AlertID: &id, ChannelID: &channelID})
				if err != nil {
					t.Fatal(err)
				}
				if resp.JSON200 == nil {
					t.Fatalf("list deliveries: status %d: %s", resp.StatusCode(), resp.Body)
				}
				for _, d := range resp.JSON200.Items {
					if d.Event == event && d.State == client.AlertDeliveryStateDelivered {
						delete(pending, key)
					}
				}
			}
			if len(pending) == 0 {
				return
			}
			if time.Now().After(deadline) {
				t.Errorf("after %s, no %s event delivered for: %v", m4AlertWait, event, slices.Sorted(maps.Keys(pending)))
				return
			}
			select {
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			case <-time.After(3 * time.Second):
			}
		}
	}

	firing := waitFor(client.AlertStateFiring)
	for _, sc := range scenarios {
		if a, ok := firing[sc.key]; ok {
			if a.FiredAt == nil || a.ResolvedAt != nil || a.Summary == "" {
				t.Errorf("%s: firing alert is malformed: %+v", sc.key, a)
			}
		}
	}
	// The healthy twins never alerted, in any state.
	for _, sc := range scenarios {
		if sc.aggregated {
			continue
		}
		if a, ok := find(sc, sc.key+"-ok"); ok {
			t.Errorf("%s: the healthy twin alerted: %+v", sc.key, a)
		}
	}
	delivered(firing, client.AlertEventTypeFiring)

	w.heal(ctx)
	resolved := waitFor(client.AlertStateResolved)
	for key, a := range resolved {
		if a.ResolvedAt == nil || a.ID != firing[key].ID {
			t.Errorf("%s: resolved alert %s isn't the one that fired (%s) or has no resolution time", key, a.ID, firing[key].ID)
		}
	}
	delivered(resolved, client.AlertEventTypeResolved)
}
