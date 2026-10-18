package metricsapi

import (
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Scope says what an [Entry] describes, and so which endpoint serves it and
// which labels select its series.
type Scope string

// Scopes.
const (
	// ScopeSite entries are selected by the site_id label.
	ScopeSite Scope = "site"
	// ScopePlatform entries describe the host and shared services; nothing
	// selects their series.
	ScopePlatform Scope = "platform"
	// ScopeClientGroup entries are served by the platform endpoints and
	// carry a client_group_id label, which an optional filter selects on.
	ScopeClientGroup Scope = "client_group"
)

// Kind is how a value is derived, which tells a chart how to read it.
type Kind string

// Kinds.
const (
	// KindGauge is a value sampled as it is.
	KindGauge Kind = "gauge"
	// KindRate is derived from a counter: a per-second rate, or an increase
	// over the rate window.
	KindRate Kind = "rate"
	// KindRatio is a fraction from 0 to 1.
	KindRatio Kind = "ratio"
	// KindPercentile is a quantile of a histogram; its series carry a
	// percentile label.
	KindPercentile Kind = "percentile"
)

// Unit is what a value counts. Values are always in base units; a chart
// decides how to display them.
type Unit string

// Units.
const (
	UnitSeconds        Unit = "seconds"
	UnitBytes          Unit = "bytes"
	UnitRatio          Unit = "ratio"
	UnitCount          Unit = "count"
	UnitPerSecond      Unit = "per_second"
	UnitBytesPerSecond Unit = "bytes_per_second"
	UnitCores          Unit = "cores"
	// UnitTimestamp is a Unix time in seconds.
	UnitTimestamp Unit = "timestamp"
	// UnitCode is an HTTP status code.
	UnitCode Unit = "code"
)

// Query is one PromQL template of an [Entry]. An entry has several when its
// series are different metrics shown together, such as active and idle
// PHP-FPM workers.
//
// Templates are parameterized only by tokens that [Entry.Render] replaces:
//
//	{$SEL}   the label matchers that scope the query: {site_id="..."},
//	         {client_group_id="..."}, or nothing for the platform
//	{$SEL,   the same with more matchers after it: {$SEL,mode="idle"}
//	$WIN     the rate window, such as 120s
//
// Nothing else is substituted, so a request can't change a query's shape.
type Query struct {
	Template string
	// Labels are added to every series the query returns, to tell the
	// queries of one entry apart.
	Labels map[string]string
}

// Entry is one curated series of the catalog: what the dashboard may ask for
// by id.
type Entry struct {
	// ID is stable and unique across the catalog.
	ID          string
	Title       string
	Description string
	Unit        Unit
	Kind        Kind
	// Group is the category of design doc §7.1 or §7.2 the entry belongs
	// to, such as "traffic".
	Group string
	Scope Scope
	// Labels names the labels (besides the site) that distinguish an
	// entry's series, e.g. status_class. Empty means one series.
	Labels  []string
	Queries []Query
	// Summary marks the entries returned by a summary request that names
	// none: a small set the overview and site list can show cheaply.
	Summary bool
}

var metricName = regexp.MustCompile(`pressfleet_[a-z0-9_]+`)

// Metrics returns the names of the stored metrics the entry reads, sorted.
// Collectors are written against this list. It is derived from the
// templates, so it can't drift from them.
func (e Entry) Metrics() []string {
	seen := map[string]bool{}
	for _, q := range e.Queries {
		for _, m := range metricName.FindAllString(q.Template, -1) {
			seen[m] = true
		}
	}
	out := make([]string, 0, len(seen))
	for m := range seen {
		out = append(out, m)
	}
	slices.Sort(out)
	return out
}

// Selector scopes a query. SiteID is required for [ScopeSite] entries and
// ClientGroupID optionally narrows [ScopeClientGroup] ones. Both must have
// been validated with [ValidID]; Render escapes them anyway.
type Selector struct {
	SiteID        string
	ClientGroupID string
}

// Rendered is a query ready to send to VictoriaMetrics.
type Rendered struct {
	Expr   string
	Labels map[string]string
}

// Render replaces the tokens of the entry's templates. window is the rate
// window; it is rounded up to whole seconds.
func (e Entry) Render(sel Selector, window time.Duration) []Rendered {
	var matchers string
	switch e.Scope {
	case ScopeSite:
		matchers = `site_id="` + escapeLabelValue(sel.SiteID) + `"`
	case ScopeClientGroup:
		if sel.ClientGroupID != "" {
			matchers = `client_group_id="` + escapeLabelValue(sel.ClientGroupID) + `"`
		}
	case ScopePlatform:
	}
	win := strconv.FormatInt(int64((window+time.Second-1)/time.Second), 10) + "s"
	out := make([]Rendered, len(e.Queries))
	for i, q := range e.Queries {
		expr := q.Template
		if matchers == "" {
			expr = strings.ReplaceAll(expr, "{$SEL,", "{")
			expr = strings.ReplaceAll(expr, "{$SEL}", "")
		} else {
			expr = strings.ReplaceAll(expr, "{$SEL,", "{"+matchers+",")
			expr = strings.ReplaceAll(expr, "{$SEL}", "{"+matchers+"}")
		}
		out[i] = Rendered{Expr: strings.ReplaceAll(expr, "$WIN", win), Labels: q.Labels}
	}
	return out
}

// escapeLabelValue escapes s for use inside a double-quoted PromQL string.
func escapeLabelValue(s string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`, "\r", `\r`).Replace(s)
}

// ValidID reports whether s can be used as a site or client group ID: the
// characters of the API's ID schema, which need no escaping in a query.
func ValidID(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for _, c := range []byte(s) {
		letter := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
		if !letter && (c < '0' || c > '9') && c != '_' && c != '-' {
			return false
		}
	}
	return true
}

// Lookup returns the entry with the id.
func Lookup(id string) (Entry, bool) {
	e, ok := byID[id]
	return e, ok
}

// Entries returns the whole catalog in a stable order: site entries, then
// platform ones, in the order of design doc §7.1 and §7.2.
func Entries() []Entry { return slices.Clone(catalog) }

// SiteEntries returns the entries of [ScopeSite], which the site endpoints
// serve.
func SiteEntries() []Entry { return entriesOf(true) }

// PlatformEntries returns the entries of [ScopePlatform] and
// [ScopeClientGroup], which the platform endpoints serve.
func PlatformEntries() []Entry { return entriesOf(false) }

func entriesOf(site bool) []Entry {
	var out []Entry
	for _, e := range catalog {
		if (e.Scope == ScopeSite) == site {
			out = append(out, e)
		}
	}
	return out
}

var byID = func() map[string]Entry {
	m := make(map[string]Entry, len(catalog))
	for _, e := range catalog {
		if _, dup := m[e.ID]; dup {
			panic("metricsapi: duplicate catalog id " + e.ID)
		}
		m[e.ID] = e
	}
	return m
}()

// q is shorthand for a single query without extra labels.
func q(template string) []Query { return []Query{{Template: template}} }

// lq is shorthand for a query that adds one label.
func lq(template, label, value string) Query {
	return Query{Template: template, Labels: map[string]string{label: value}}
}

const (
	groupAvailability = "availability"
	groupTraffic      = "traffic"
	groupPageCache    = "page_cache"
	groupPHP          = "php"
	groupResources    = "resources"
	groupStorage      = "storage"
	groupObjectCache  = "object_cache"
	groupTLS          = "tls"
	groupBackups      = "backups"

	groupHost    = "host"
	groupMariaDB = "mariadb"
	groupRedis   = "redis"
	groupJobs    = "jobs"
)

// catalog is every metric of design doc §7.1 (per site) and §7.2
// (platform), in order. Add an entry here and nowhere else: the API and the
// dependency list of the collectors follow.
var catalog = []Entry{
	// §7.1 Availability: control plane HTTP checks.
	{ID: "uptime", Title: "Uptime", Description: "Share of HTTP checks that succeeded.",
		Unit: UnitRatio, Kind: KindRatio, Group: groupAvailability, Scope: ScopeSite, Summary: true,
		Queries: q(`avg_over_time(pressfleet_site_up{$SEL}[$WIN])`)},
	{ID: "http_check_status", Title: "HTTP check status", Description: "Status code the HTTP check got (0 if it got no response).",
		Unit: UnitCode, Kind: KindGauge, Group: groupAvailability, Scope: ScopeSite, Summary: true,
		Queries: q(`pressfleet_site_http_check_status_code{$SEL}`)},
	{ID: "response_time", Title: "Response time", Description: "How long the HTTP check took.",
		Unit: UnitSeconds, Kind: KindGauge, Group: groupAvailability, Scope: ScopeSite, Summary: true,
		Queries: q(`pressfleet_site_http_check_duration_seconds{$SEL}`)},

	// §7.1 Traffic: Traefik.
	{ID: "request_rate", Title: "Request rate", Description: "Requests served per second.",
		Unit: UnitPerSecond, Kind: KindRate, Group: groupTraffic, Scope: ScopeSite, Summary: true,
		Queries: q(`sum(rate(pressfleet_site_http_requests_total{$SEL}[$WIN]))`)},
	{ID: "status_codes", Title: "Status codes", Description: "Requests served per second, by status class (1xx to 5xx).",
		Unit: UnitPerSecond, Kind: KindRate, Group: groupTraffic, Scope: ScopeSite, Labels: []string{"status_class"},
		Queries: q(`sum by (status_class) (rate(pressfleet_site_http_requests_total{$SEL}[$WIN]))`)},
	{ID: "latency", Title: "Latency", Description: "Request duration at the 50th, 95th, and 99th percentile.",
		Unit: UnitSeconds, Kind: KindPercentile, Group: groupTraffic, Scope: ScopeSite, Labels: []string{"percentile"},
		Queries: []Query{
			lq(`histogram_quantile(0.5, sum by (le) (rate(pressfleet_site_http_request_duration_seconds_bucket{$SEL}[$WIN])))`, "percentile", "p50"),
			lq(`histogram_quantile(0.95, sum by (le) (rate(pressfleet_site_http_request_duration_seconds_bucket{$SEL}[$WIN])))`, "percentile", "p95"),
			lq(`histogram_quantile(0.99, sum by (le) (rate(pressfleet_site_http_request_duration_seconds_bucket{$SEL}[$WIN])))`, "percentile", "p99"),
		}},

	// §7.1 Page cache: Nginx $upstream_cache_status.
	{ID: "page_cache_ratio", Title: "Page cache", Description: "Share of requests by FastCGI cache status: hit, miss, or bypass.",
		Unit: UnitRatio, Kind: KindRatio, Group: groupPageCache, Scope: ScopeSite, Labels: []string{"cache_status"},
		Queries: q(`sum by (cache_status) (rate(pressfleet_site_page_cache_requests_total{$SEL}[$WIN])) / ignoring (cache_status) group_left sum(rate(pressfleet_site_page_cache_requests_total{$SEL}[$WIN]))`)},

	// §7.1 PHP: PHP-FPM status endpoint.
	{ID: "php_workers", Title: "PHP-FPM workers", Description: "Active and idle workers, and the pool's limit (pm.max_children).",
		Unit: UnitCount, Kind: KindGauge, Group: groupPHP, Scope: ScopeSite, Labels: []string{"state"},
		Queries: []Query{
			lq(`pressfleet_site_php_fpm_active_processes{$SEL}`, "state", "active"),
			lq(`pressfleet_site_php_fpm_idle_processes{$SEL}`, "state", "idle"),
			lq(`pressfleet_site_php_fpm_max_children{$SEL}`, "state", "max"),
		}},
	{ID: "php_max_children_reached", Title: "PHP-FPM max children reached", Description: "Times the pool ran out of workers.",
		Unit: UnitCount, Kind: KindRate, Group: groupPHP, Scope: ScopeSite,
		Queries: q(`increase(pressfleet_site_php_fpm_max_children_reached_total{$SEL}[$WIN])`)},
	{ID: "php_slow_requests", Title: "PHP-FPM slow requests", Description: "Requests that exceeded the slow log threshold.",
		Unit: UnitCount, Kind: KindRate, Group: groupPHP, Scope: ScopeSite,
		Queries: q(`increase(pressfleet_site_php_fpm_slow_requests_total{$SEL}[$WIN])`)},

	// §7.1 Resources: Docker stats API.
	{ID: "cpu", Title: "CPU", Description: "CPU time used, in cores, per container.",
		Unit: UnitCores, Kind: KindRate, Group: groupResources, Scope: ScopeSite, Labels: []string{"container"},
		Queries: q(`sum by (container) (rate(pressfleet_container_cpu_usage_seconds_total{$SEL}[$WIN]))`)},
	{ID: "memory", Title: "Memory", Description: "Memory in use per container.",
		Unit: UnitBytes, Kind: KindGauge, Group: groupResources, Scope: ScopeSite, Labels: []string{"container"},
		Queries: q(`sum by (container) (pressfleet_container_memory_usage_bytes{$SEL})`)},
	{ID: "restarts", Title: "Restarts", Description: "Container restarts per rate window.",
		Unit: UnitCount, Kind: KindRate, Group: groupResources, Scope: ScopeSite, Labels: []string{"container"},
		Queries: q(`sum by (container) (increase(pressfleet_container_restarts_total{$SEL}[$WIN]))`)},

	// §7.1 Storage: control plane collector.
	{ID: "wp_content_size", Title: "wp-content size", Description: "Size of the site's wp-content volume.",
		Unit: UnitBytes, Kind: KindGauge, Group: groupStorage, Scope: ScopeSite, Summary: true,
		Queries: q(`pressfleet_site_wp_content_size_bytes{$SEL}`)},
	{ID: "database_size", Title: "Database size", Description: "Size of the site's database.",
		Unit: UnitBytes, Kind: KindGauge, Group: groupStorage, Scope: ScopeSite, Summary: true,
		Queries: q(`pressfleet_site_database_size_bytes{$SEL}`)},

	// §7.1 Object cache: Redis.
	{ID: "object_cache_keys", Title: "Object cache keys", Description: "Keys the site has in the object cache.",
		Unit: UnitCount, Kind: KindGauge, Group: groupObjectCache, Scope: ScopeSite,
		Queries: q(`pressfleet_site_object_cache_keys{$SEL}`)},
	{ID: "object_cache_memory", Title: "Object cache memory", Description: "Memory the site's object cache keys use.",
		Unit: UnitBytes, Kind: KindGauge, Group: groupObjectCache, Scope: ScopeSite,
		Queries: q(`pressfleet_site_object_cache_memory_bytes{$SEL}`)},
	{ID: "object_cache_hit_ratio", Title: "Object cache hit ratio", Description: "Share of object cache lookups that found the key.",
		Unit: UnitRatio, Kind: KindRatio, Group: groupObjectCache, Scope: ScopeSite,
		Queries: q(`sum(rate(pressfleet_site_object_cache_hits_total{$SEL}[$WIN])) / (sum(rate(pressfleet_site_object_cache_hits_total{$SEL}[$WIN])) + sum(rate(pressfleet_site_object_cache_misses_total{$SEL}[$WIN])))`)},

	// §7.1 TLS: control plane checks.
	{ID: "tls_expiry", Title: "TLS certificate expiry", Description: "When the certificate served for each domain expires.",
		Unit: UnitTimestamp, Kind: KindGauge, Group: groupTLS, Scope: ScopeSite, Labels: []string{"domain"}, Summary: true,
		Queries: q(`pressfleet_site_tls_certificate_expiry_timestamp_seconds{$SEL}`)},

	// §7.1 Backups: backup jobs.
	{ID: "backup_last_success", Title: "Last successful backup", Description: "When the latest backup that succeeded finished.",
		Unit: UnitTimestamp, Kind: KindGauge, Group: groupBackups, Scope: ScopeSite, Summary: true,
		Queries: q(`pressfleet_site_backup_last_success_timestamp_seconds{$SEL}`)},
	{ID: "backup_size", Title: "Backup size", Description: "Size of the latest backup.",
		Unit: UnitBytes, Kind: KindGauge, Group: groupBackups, Scope: ScopeSite,
		Queries: q(`pressfleet_site_backup_size_bytes{$SEL}`)},
	{ID: "backup_duration", Title: "Backup duration", Description: "How long the latest backup took.",
		Unit: UnitSeconds, Kind: KindGauge, Group: groupBackups, Scope: ScopeSite,
		Queries: q(`pressfleet_site_backup_duration_seconds{$SEL}`)},

	// §7.2 Host.
	{ID: "host_cpu", Title: "Host CPU", Description: "Share of CPU time the host spent busy, across all cores.",
		Unit: UnitRatio, Kind: KindRatio, Group: groupHost, Scope: ScopePlatform, Summary: true,
		Queries: q(`1 - sum(rate(pressfleet_host_cpu_seconds_total{$SEL,mode="idle"}[$WIN])) / sum(rate(pressfleet_host_cpu_seconds_total{$SEL}[$WIN]))`)},
	{ID: "host_memory", Title: "Host memory", Description: "Memory in use and total memory of the host.",
		Unit: UnitBytes, Kind: KindGauge, Group: groupHost, Scope: ScopePlatform, Labels: []string{"type"}, Summary: true,
		Queries: []Query{
			lq(`pressfleet_host_memory_total_bytes{$SEL} - pressfleet_host_memory_available_bytes{$SEL}`, "type", "used"),
			lq(`pressfleet_host_memory_total_bytes{$SEL}`, "type", "total"),
		}},
	{ID: "host_disk_usage", Title: "Host disk usage", Description: "Share of each monitored filesystem that is in use.",
		Unit: UnitRatio, Kind: KindRatio, Group: groupHost, Scope: ScopePlatform, Labels: []string{"mountpoint"}, Summary: true,
		Queries: q(`1 - pressfleet_host_filesystem_avail_bytes{$SEL} / pressfleet_host_filesystem_size_bytes{$SEL}`)},
	{ID: "host_disk_io", Title: "Host disk I/O", Description: "Bytes the host reads from and writes to disks per second.",
		Unit: UnitBytesPerSecond, Kind: KindRate, Group: groupHost, Scope: ScopePlatform, Labels: []string{"direction"},
		Queries: []Query{
			lq(`sum(rate(pressfleet_host_disk_read_bytes_total{$SEL}[$WIN]))`, "direction", "read"),
			lq(`sum(rate(pressfleet_host_disk_written_bytes_total{$SEL}[$WIN]))`, "direction", "write"),
		}},

	// §7.2 MariaDB.
	{ID: "mariadb_connections", Title: "MariaDB connections", Description: "Open connections and the server's limit.",
		Unit: UnitCount, Kind: KindGauge, Group: groupMariaDB, Scope: ScopePlatform, Labels: []string{"state"}, Summary: true,
		Queries: []Query{
			lq(`pressfleet_mariadb_threads_connected{$SEL}`, "state", "connected"),
			lq(`pressfleet_mariadb_max_connections{$SEL}`, "state", "max"),
		}},
	{ID: "mariadb_slow_queries", Title: "MariaDB slow queries", Description: "Queries slower than long_query_time, per second.",
		Unit: UnitPerSecond, Kind: KindRate, Group: groupMariaDB, Scope: ScopePlatform,
		Queries: q(`rate(pressfleet_mariadb_slow_queries_total{$SEL}[$WIN])`)},
	{ID: "mariadb_buffer_pool", Title: "MariaDB buffer pool", Description: "InnoDB buffer pool in use and its size.",
		Unit: UnitBytes, Kind: KindGauge, Group: groupMariaDB, Scope: ScopePlatform, Labels: []string{"type"},
		Queries: []Query{
			lq(`pressfleet_mariadb_innodb_buffer_pool_used_bytes{$SEL}`, "type", "used"),
			lq(`pressfleet_mariadb_innodb_buffer_pool_size_bytes{$SEL}`, "type", "total"),
		}},

	// §7.2 Redis.
	{ID: "redis_memory", Title: "Redis memory", Description: "Memory the object cache server uses.",
		Unit: UnitBytes, Kind: KindGauge, Group: groupRedis, Scope: ScopePlatform, Summary: true,
		Queries: q(`pressfleet_redis_memory_used_bytes{$SEL}`)},
	{ID: "redis_hit_ratio", Title: "Redis hit ratio", Description: "Share of lookups that found the key.",
		Unit: UnitRatio, Kind: KindRatio, Group: groupRedis, Scope: ScopePlatform,
		Queries: q(`rate(pressfleet_redis_keyspace_hits_total{$SEL}[$WIN]) / (rate(pressfleet_redis_keyspace_hits_total{$SEL}[$WIN]) + rate(pressfleet_redis_keyspace_misses_total{$SEL}[$WIN]))`)},
	{ID: "redis_evictions", Title: "Redis evictions", Description: "Keys evicted per second.",
		Unit: UnitPerSecond, Kind: KindRate, Group: groupRedis, Scope: ScopePlatform,
		Queries: q(`rate(pressfleet_redis_evicted_keys_total{$SEL}[$WIN])`)},

	// §7.2 Jobs.
	{ID: "job_queue_depth", Title: "Job queue depth", Description: "Jobs waiting to run and running.",
		Unit: UnitCount, Kind: KindGauge, Group: groupJobs, Scope: ScopePlatform, Labels: []string{"state"}, Summary: true,
		Queries: q(`sum by (state) (pressfleet_jobs_queue_depth{$SEL})`)},
	{ID: "job_failure_rate", Title: "Job failure rate", Description: "Share of finished jobs that failed.",
		Unit: UnitRatio, Kind: KindRatio, Group: groupJobs, Scope: ScopePlatform, Summary: true,
		Queries: q(`sum(rate(pressfleet_jobs_finished_total{$SEL,result="failed"}[$WIN])) / sum(rate(pressfleet_jobs_finished_total{$SEL}[$WIN]))`)},

	// §7.2 Restic repositories: one per client group.
	{ID: "restic_repository_size", Title: "Backup repository size", Description: "Size of each client group's Restic repository.",
		Unit: UnitBytes, Kind: KindGauge, Group: groupBackups, Scope: ScopeClientGroup, Labels: []string{"client_group_id"}, Summary: true,
		Queries: q(`sum by (client_group_id) (pressfleet_restic_repository_size_bytes{$SEL})`)},
}
