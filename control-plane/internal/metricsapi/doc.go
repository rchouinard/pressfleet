// Package metricsapi is the catalog of metrics the dashboard can chart, and
// the service that answers queries for them from VictoriaMetrics (design doc
// §7.1, §7.2, issue #77).
//
// # Why a catalog
//
// The dashboard never sends PromQL. It names metrics by id (GET
// /api/v1/metrics/catalog lists them) and a time range, and this package
// turns that into queries. A catalog [Entry] has a stable id, a title, a
// [Unit], a [Kind], a group, a [Scope], and one or more PromQL templates.
// Templates take only three parameters, and every one is validated or
// computed here:
//
//   - the site: site_id is checked with [ValidID] and escaped, and every
//     site query selects on it, so a request can't see another site's series;
//   - the client group: an optional client_group_id filter, validated and
//     escaped the same way;
//   - the rate window, derived from the step (never shorter than two
//     minutes, which is four of the default 30 second collection
//     intervals).
//
// A metric with no data returns no series, not an error. That is what the
// API answers for every entry whose collector (#68 to #74) isn't there yet.
//
// # Metrics the catalog reads
//
// Collectors are matched against this table. Names follow the conventions
// of package metrics: the pressfleet_ prefix, snake_case, base units, ratios
// from 0 to 1, counters ending in _total. "site" metrics carry site_id and
// client_group_id (the API selects on site_id); "platform" ones carry
// neither. Labels listed are the ones the catalog groups or filters by.
//
//	Metric                                                       Type       Labels                      Entries
//	pressfleet_site_up                                           gauge      site                        uptime
//	pressfleet_site_http_check_status_code                       gauge      site                        http_check_status
//	pressfleet_site_http_check_duration_seconds                  gauge      site                        response_time
//	pressfleet_site_http_requests_total                          counter    site, status_class          request_rate, status_codes
//	pressfleet_site_http_request_duration_seconds_bucket         histogram  site, le                    latency
//	pressfleet_site_page_cache_requests_total                    counter    site, cache_status          page_cache_ratio
//	pressfleet_site_php_fpm_active_processes                     gauge      site                        php_workers
//	pressfleet_site_php_fpm_idle_processes                       gauge      site                        php_workers
//	pressfleet_site_php_fpm_max_children                         gauge      site                        php_workers
//	pressfleet_site_php_fpm_max_children_reached_total           counter    site                        php_max_children_reached
//	pressfleet_site_php_fpm_slow_requests_total                  counter    site                        php_slow_requests
//	pressfleet_container_cpu_usage_seconds_total                 counter    site, container             cpu
//	pressfleet_container_memory_usage_bytes                      gauge      site, container             memory
//	pressfleet_container_restarts_total                          counter    site, container             restarts
//	pressfleet_site_wp_content_size_bytes                        gauge      site                        wp_content_size
//	pressfleet_site_database_size_bytes                          gauge      site                        database_size
//	pressfleet_site_object_cache_keys                            gauge      site                        object_cache_keys
//	pressfleet_site_object_cache_memory_bytes                    gauge      site                        object_cache_memory
//	pressfleet_site_object_cache_hits_total                      counter    site                        object_cache_hit_ratio
//	pressfleet_site_object_cache_misses_total                    counter    site                        object_cache_hit_ratio
//	pressfleet_site_tls_certificate_expiry_timestamp_seconds     gauge      site, domain                tls_expiry
//	pressfleet_site_backup_last_success_timestamp_seconds        gauge      site                        backup_last_success
//	pressfleet_site_backup_size_bytes                            gauge      site                        backup_size
//	pressfleet_site_backup_duration_seconds                      gauge      site                        backup_duration
//	pressfleet_host_cpu_seconds_total                            counter    cpu, mode                   host_cpu
//	pressfleet_host_memory_total_bytes                           gauge      (none)                      host_memory
//	pressfleet_host_memory_available_bytes                       gauge      (none)                      host_memory
//	pressfleet_host_filesystem_size_bytes                        gauge      mountpoint                  host_disk_usage
//	pressfleet_host_filesystem_avail_bytes                       gauge      mountpoint                  host_disk_usage
//	pressfleet_host_disk_read_bytes_total                        counter    device (summed)             host_disk_io
//	pressfleet_host_disk_written_bytes_total                     counter    device (summed)             host_disk_io
//	pressfleet_mariadb_threads_connected                         gauge      (none)                      mariadb_connections
//	pressfleet_mariadb_max_connections                           gauge      (none)                      mariadb_connections
//	pressfleet_mariadb_slow_queries_total                        counter    (none)                      mariadb_slow_queries
//	pressfleet_mariadb_innodb_buffer_pool_used_bytes             gauge      (none)                      mariadb_buffer_pool
//	pressfleet_mariadb_innodb_buffer_pool_size_bytes             gauge      (none)                      mariadb_buffer_pool
//	pressfleet_redis_memory_used_bytes                           gauge      (none)                      redis_memory
//	pressfleet_redis_keyspace_hits_total                         counter    (none)                      redis_hit_ratio
//	pressfleet_redis_keyspace_misses_total                       counter    (none)                      redis_hit_ratio
//	pressfleet_redis_evicted_keys_total                          counter    (none)                      redis_evictions
//	pressfleet_jobs_queue_depth                                  gauge      state                       job_queue_depth
//	pressfleet_jobs_finished_total                               counter    result ("failed", ...)      job_failure_rate
//	pressfleet_restic_repository_size_bytes                      gauge      client_group_id             restic_repository_size
//
// Conventions the queries rely on, which a collector must follow:
//
//   - pressfleet_site_up is 1 or 0 per check. The page cache's cache_status
//     is "hit", "miss", or "bypass"; status_class is "1xx" to "5xx".
//   - pressfleet_site_http_check_status_code is 0 when the check got no
//     response.
//   - Traefik's per-router series are relabeled to site_id (and
//     client_group_id) by the collector; the histogram's le buckets are in
//     seconds.
//   - pressfleet_container_* series of site containers carry site_id and
//     container (the component, such as "php" or "nginx"); platform
//     containers carry neither, so a site never sees them.
//   - pressfleet_jobs_queue_depth has one series per job state ("pending",
//     "running"); pressfleet_jobs_finished_total counts finished jobs by
//     result ("succeeded", "failed").
//   - pressfleet_host_cpu_seconds_total has a mode label whose values include
//     "idle"; the query sums over cpu, so per-core series are fine.
//   - The disk I/O counters may have a device label; the query sums over it.
//
// The dependency list of each entry is derived from its templates
// ([Entry.Metrics]), and a test checks that this table names exactly the
// metrics the catalog reads.
//
// # Limits
//
// A range query is limited to [MaxRange], a step of at least [MinStep],
// [MaxPoints] points per series, and [MaxMetrics] metrics. Without a step,
// one is chosen from a list of round steps to give about 300 points. Start
// and end are aligned down to the step, so repeated requests ask
// VictoriaMetrics for the same points, and end is capped at the current
// time. A request's queries run concurrently, at most eight at a time. If
// VictoriaMetrics can't be reached the error wraps [metrics.ErrUnavailable],
// which the API answers with 503.
package metricsapi
