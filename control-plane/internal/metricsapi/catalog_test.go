package metricsapi

import (
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

// expectedSite and expectedPlatform list every metric of design doc §7.1
// and §7.2. A metric dropped from the catalog fails the test, which is what
// "every metric is queryable" means.
var expectedSite = map[string]string{
	// Availability
	"uptime": groupAvailability, "http_check_status": groupAvailability, "response_time": groupAvailability,
	// Traffic
	"request_rate": groupTraffic, "status_codes": groupTraffic, "latency": groupTraffic,
	// Page cache
	"page_cache_ratio": groupPageCache,
	// PHP
	"php_workers": groupPHP, "php_max_children_reached": groupPHP, "php_slow_requests": groupPHP,
	// Resources
	"cpu": groupResources, "memory": groupResources, "restarts": groupResources,
	// Storage
	"wp_content_size": groupStorage, "database_size": groupStorage,
	// Object cache
	"object_cache_keys": groupObjectCache, "object_cache_memory": groupObjectCache, "object_cache_hit_ratio": groupObjectCache,
	// TLS
	"tls_expiry": groupTLS,
	// Backups
	"backup_last_success": groupBackups, "backup_size": groupBackups, "backup_duration": groupBackups,
}

var expectedPlatform = map[string]string{
	"host_cpu": groupHost, "host_memory": groupHost, "host_disk_usage": groupHost, "host_disk_io": groupHost,
	"mariadb_connections": groupMariaDB, "mariadb_slow_queries": groupMariaDB, "mariadb_buffer_pool": groupMariaDB,
	"redis_memory": groupRedis, "redis_hit_ratio": groupRedis, "redis_evictions": groupRedis,
	"job_queue_depth": groupJobs, "job_failure_rate": groupJobs,
	"restic_repository_size": groupBackups,
}

func TestCatalogCoversDesignDoc(t *testing.T) {
	for id, group := range expectedSite {
		e, ok := Lookup(id)
		if !ok || e.Scope != ScopeSite || e.Group != group {
			t.Errorf("site metric %q: found %v, scope %q, group %q; want scope site, group %q", id, ok, e.Scope, e.Group, group)
		}
	}
	for id, group := range expectedPlatform {
		e, ok := Lookup(id)
		if !ok || (e.Scope != ScopePlatform && e.Scope != ScopeClientGroup) || e.Group != group {
			t.Errorf("platform metric %q: found %v, scope %q, group %q; want platform, group %q", id, ok, e.Scope, e.Group, group)
		}
	}
	if got, want := len(Entries()), len(expectedSite)+len(expectedPlatform); got != want {
		t.Errorf("catalog has %d entries, the design doc lists %d: add the new ones to the test", got, want)
	}
	// Restic is per client group.
	if e, _ := Lookup("restic_repository_size"); e.Scope != ScopeClientGroup {
		t.Errorf("restic_repository_size scope = %q", e.Scope)
	}
}

func TestCatalogEntriesAreWellFormed(t *testing.T) {
	units := []Unit{UnitSeconds, UnitBytes, UnitRatio, UnitCount, UnitPerSecond, UnitBytesPerSecond, UnitCores, UnitTimestamp, UnitCode}
	kinds := []Kind{KindGauge, KindRate, KindRatio, KindPercentile}
	idFormat := regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
	for _, e := range Entries() {
		t.Run(e.ID, func(t *testing.T) {
			if !idFormat.MatchString(e.ID) || e.Title == "" || e.Description == "" || e.Group == "" {
				t.Errorf("incomplete entry: %+v", e)
			}
			if !slices.Contains(units, e.Unit) || !slices.Contains(kinds, e.Kind) {
				t.Errorf("unit %q or kind %q is unknown", e.Unit, e.Kind)
			}
			if (e.Kind == KindRatio) != (e.Unit == UnitRatio) {
				t.Errorf("kind %q and unit %q disagree about ratios", e.Kind, e.Unit)
			}
			if len(e.Queries) == 0 || len(e.Metrics()) == 0 {
				t.Fatalf("no queries or no metrics")
			}
			// Several queries must be told apart by a label, or their series merge.
			if len(e.Queries) > 1 {
				for _, q := range e.Queries {
					if len(q.Labels) == 0 {
						t.Errorf("query %q of a multi-query entry has no labels", q.Template)
					}
				}
			}
			for _, m := range e.Metrics() {
				if !strings.HasPrefix(m, "pressfleet_") {
					t.Errorf("metric %q lacks the pressfleet_ prefix", m)
				}
			}
			for _, r := range e.Render(Selector{SiteID: "S1", ClientGroupID: "G1"}, 5*time.Minute) {
				if strings.Contains(r.Expr, "$") || strings.Contains(r.Expr, "{}") || strings.Contains(r.Expr, "{,") {
					t.Errorf("rendered query has a leftover token or empty matcher: %s", r.Expr)
				}
			}
		})
	}
}

// TestSiteQueriesAreScoped checks that every selector of a site entry names
// the site, so no template can read another site's series.
func TestSiteQueriesAreScoped(t *testing.T) {
	selector := regexp.MustCompile(`pressfleet_[a-z0-9_]+(\{[^}]*\})?`)
	for _, e := range SiteEntries() {
		for _, r := range e.Render(Selector{SiteID: "SITE1"}, time.Minute) {
			for _, m := range selector.FindAllString(r.Expr, -1) {
				if !strings.Contains(m, `{site_id="SITE1"`) {
					t.Errorf("%s: selector %q doesn't select on site_id; query %s", e.ID, m, r.Expr)
				}
			}
		}
	}
}

// TestDocumentedMetrics checks that the table in doc.go names exactly what
// the catalog reads, so collectors written from it match the queries.
func TestDocumentedMetrics(t *testing.T) {
	src, err := os.ReadFile("doc.go")
	if err != nil {
		t.Fatal(err)
	}
	documented := map[string]string{}
	for line := range strings.SplitSeq(string(src), "\n") {
		if f := strings.Fields(strings.TrimPrefix(line, "//")); len(f) > 3 && strings.HasPrefix(f[0], "pressfleet_") {
			documented[f[0]] = line
		}
	}
	used := map[string]bool{}
	for _, e := range Entries() {
		for _, m := range e.Metrics() {
			used[m] = true
			line, ok := documented[m]
			if !ok {
				t.Errorf("metric %s (entry %s) isn't in the table of doc.go", m, e.ID)
			} else if !strings.Contains(line, e.ID) {
				t.Errorf("the table row of %s doesn't list entry %s", m, e.ID)
			}
		}
	}
	for m := range documented {
		if !used[m] {
			t.Errorf("doc.go documents %s, which no entry reads", m)
		}
	}
}

func TestMetricNamesFollowConvention(t *testing.T) {
	for _, e := range Entries() {
		for _, m := range e.Metrics() {
			unitOK := strings.HasSuffix(m, "_seconds") || strings.HasSuffix(m, "_bytes") || strings.HasSuffix(m, "_total") ||
				strings.HasSuffix(m, "_bucket") || strings.HasSuffix(m, "_ratio") || strings.HasSuffix(m, "_timestamp_seconds")
			counter := strings.Contains(e.Queries[0].Template, "rate("+m) || strings.Contains(e.Queries[0].Template, "increase("+m)
			if counter && !strings.HasSuffix(m, "_total") && !strings.HasSuffix(m, "_bucket") {
				t.Errorf("%s is used as a counter but doesn't end in _total", m)
			}
			_ = unitOK
		}
	}
}

func TestRender(t *testing.T) {
	const siteID = "01J9ZQ3V6T8K4M2N5P7R9S1X3Y"
	tests := []struct {
		id   string
		sel  Selector
		win  time.Duration
		want []string
	}{
		{"uptime", Selector{SiteID: siteID}, 2 * time.Minute,
			[]string{`avg_over_time(pressfleet_site_up{site_id="01J9ZQ3V6T8K4M2N5P7R9S1X3Y"}[120s])`}},
		{"http_check_status", Selector{SiteID: siteID}, time.Minute,
			[]string{`pressfleet_site_http_check_status_code{site_id="01J9ZQ3V6T8K4M2N5P7R9S1X3Y"}`}},
		{"request_rate", Selector{SiteID: siteID}, 5 * time.Minute,
			[]string{`sum(rate(pressfleet_site_http_requests_total{site_id="01J9ZQ3V6T8K4M2N5P7R9S1X3Y"}[300s]))`}},
		{"status_codes", Selector{SiteID: siteID}, 2 * time.Minute,
			[]string{`sum by (status_class) (rate(pressfleet_site_http_requests_total{site_id="01J9ZQ3V6T8K4M2N5P7R9S1X3Y"}[120s]))`}},
		{"latency", Selector{SiteID: siteID}, 2 * time.Minute, []string{
			`histogram_quantile(0.5, sum by (le) (rate(pressfleet_site_http_request_duration_seconds_bucket{site_id="01J9ZQ3V6T8K4M2N5P7R9S1X3Y"}[120s])))`,
			`histogram_quantile(0.95, sum by (le) (rate(pressfleet_site_http_request_duration_seconds_bucket{site_id="01J9ZQ3V6T8K4M2N5P7R9S1X3Y"}[120s])))`,
			`histogram_quantile(0.99, sum by (le) (rate(pressfleet_site_http_request_duration_seconds_bucket{site_id="01J9ZQ3V6T8K4M2N5P7R9S1X3Y"}[120s])))`,
		}},
		{"page_cache_ratio", Selector{SiteID: "s"}, 2 * time.Minute, []string{
			`sum by (cache_status) (rate(pressfleet_site_page_cache_requests_total{site_id="s"}[120s])) / ignoring (cache_status) group_left sum(rate(pressfleet_site_page_cache_requests_total{site_id="s"}[120s]))`}},
		{"php_workers", Selector{SiteID: "s"}, time.Minute, []string{
			`pressfleet_site_php_fpm_active_processes{site_id="s"}`,
			`pressfleet_site_php_fpm_idle_processes{site_id="s"}`,
			`pressfleet_site_php_fpm_max_children{site_id="s"}`,
		}},
		{"php_max_children_reached", Selector{SiteID: "s"}, 10 * time.Minute,
			[]string{`increase(pressfleet_site_php_fpm_max_children_reached_total{site_id="s"}[600s])`}},
		{"cpu", Selector{SiteID: "s"}, 2 * time.Minute,
			[]string{`sum by (container) (rate(pressfleet_container_cpu_usage_seconds_total{site_id="s"}[120s]))`}},
		{"memory", Selector{SiteID: "s"}, time.Minute,
			[]string{`sum by (container) (pressfleet_container_memory_usage_bytes{site_id="s"})`}},
		{"restarts", Selector{SiteID: "s"}, 2 * time.Minute,
			[]string{`sum by (container) (increase(pressfleet_container_restarts_total{site_id="s"}[120s]))`}},
		{"object_cache_hit_ratio", Selector{SiteID: "s"}, 2 * time.Minute, []string{
			`sum(rate(pressfleet_site_object_cache_hits_total{site_id="s"}[120s])) / (sum(rate(pressfleet_site_object_cache_hits_total{site_id="s"}[120s])) + sum(rate(pressfleet_site_object_cache_misses_total{site_id="s"}[120s])))`}},
		{"tls_expiry", Selector{SiteID: "s"}, time.Minute,
			[]string{`pressfleet_site_tls_certificate_expiry_timestamp_seconds{site_id="s"}`}},
		{"backup_duration", Selector{SiteID: "s"}, time.Minute,
			[]string{`pressfleet_site_backup_duration_seconds{site_id="s"}`}},
		// Platform entries have no selector, and a site ID is ignored.
		{"host_cpu", Selector{SiteID: "s"}, 2 * time.Minute, []string{
			`1 - sum(rate(pressfleet_host_cpu_seconds_total{mode="idle"}[120s])) / sum(rate(pressfleet_host_cpu_seconds_total[120s]))`}},
		{"host_memory", Selector{}, time.Minute, []string{
			`pressfleet_host_memory_total_bytes - pressfleet_host_memory_available_bytes`,
			`pressfleet_host_memory_total_bytes`,
		}},
		{"host_disk_usage", Selector{}, time.Minute,
			[]string{`1 - pressfleet_host_filesystem_avail_bytes / pressfleet_host_filesystem_size_bytes`}},
		{"host_disk_io", Selector{}, 2 * time.Minute, []string{
			`sum(rate(pressfleet_host_disk_read_bytes_total[120s]))`,
			`sum(rate(pressfleet_host_disk_written_bytes_total[120s]))`,
		}},
		{"mariadb_slow_queries", Selector{}, 2 * time.Minute,
			[]string{`rate(pressfleet_mariadb_slow_queries_total[120s])`}},
		{"redis_hit_ratio", Selector{}, 2 * time.Minute, []string{
			`rate(pressfleet_redis_keyspace_hits_total[120s]) / (rate(pressfleet_redis_keyspace_hits_total[120s]) + rate(pressfleet_redis_keyspace_misses_total[120s]))`}},
		{"redis_evictions", Selector{}, 2 * time.Minute,
			[]string{`rate(pressfleet_redis_evicted_keys_total[120s])`}},
		{"job_queue_depth", Selector{}, time.Minute,
			[]string{`sum by (state) (pressfleet_jobs_queue_depth)`}},
		{"job_failure_rate", Selector{}, 2 * time.Minute, []string{
			`sum(rate(pressfleet_jobs_finished_total{result="failed"}[120s])) / sum(rate(pressfleet_jobs_finished_total[120s]))`}},
		{"restic_repository_size", Selector{}, time.Minute,
			[]string{`sum by (client_group_id) (pressfleet_restic_repository_size_bytes)`}},
		{"restic_repository_size", Selector{ClientGroupID: "G-1"}, time.Minute,
			[]string{`sum by (client_group_id) (pressfleet_restic_repository_size_bytes{client_group_id="G-1"})`}},
		// A window is rounded up to whole seconds.
		{"redis_evictions", Selector{}, 90*time.Second + time.Millisecond,
			[]string{`rate(pressfleet_redis_evicted_keys_total[91s])`}},
	}
	for _, tt := range tests {
		t.Run(tt.id, func(t *testing.T) {
			e, ok := Lookup(tt.id)
			if !ok {
				t.Fatal("no such entry")
			}
			var got []string
			for _, r := range e.Render(tt.sel, tt.win) {
				got = append(got, r.Expr)
			}
			if !slices.Equal(got, tt.want) {
				t.Errorf("queries:\n got  %q\n want %q", got, tt.want)
			}
		})
	}
}

// TestRenderEscapesSelectors checks that a hostile ID stays inside its
// string literal even if validation were bypassed.
func TestRenderEscapesSelectors(t *testing.T) {
	e, _ := Lookup("request_rate")
	got := e.Render(Selector{SiteID: "a\"} or vector(1) # \\\n"}, time.Minute)[0].Expr
	want := `sum(rate(pressfleet_site_http_requests_total{site_id="a\"} or vector(1) # \\\n"}[60s]))`
	if got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

func TestValidID(t *testing.T) {
	for _, s := range []string{"01J9ZQ3V6T8K4M2N5P7R9S1X3Y", "a", "A_b-9", strings.Repeat("x", 64)} {
		if !ValidID(s) {
			t.Errorf("ValidID(%q) = false", s)
		}
	}
	for _, s := range []string{"", strings.Repeat("x", 65), `a"b`, "a b", "a}", "a\\", "a\n", "a,b", "é", `x"} or up{a="`} {
		if ValidID(s) {
			t.Errorf("ValidID(%q) = true", s)
		}
	}
}
