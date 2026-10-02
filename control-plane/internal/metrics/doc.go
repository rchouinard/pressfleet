// Package metrics collects site and platform metrics into VictoriaMetrics and
// reads them back (design doc §3, §7). It owns all access to VictoriaMetrics:
// other packages depend on [Writer] and [Querier], never on HTTP details.
//
// # Pieces
//
//   - [Client] implements [Writer] (VictoriaMetrics' Prometheus text import,
//     /api/v1/import/prometheus) and [Querier] (the Prometheus-compatible
//     /api/v1/query and /api/v1/query_range), with batching, per-request
//     timeouts, and retries with backoff. It uses only the standard library:
//     the text import format needs no protobuf or snappy dependency, which
//     remote write would.
//   - [Service] runs [Collector]s, each on its own interval, with a timeout
//     per run. A collector that fails, hangs, or panics is recorded and
//     retried at its next interval; it never affects the others.
//   - [TargetSource] lists the sites to collect for. The service asks for
//     the list on every run of every collector, so created and deleted sites
//     are picked up without a restart. [NewSiteTargets] reads the site
//     records.
//
// # Metric naming convention
//
// Collectors written for #68 to #74 follow these rules, which match
// Prometheus's own conventions:
//
//   - Every metric starts with "pressfleet_", then the subsystem
//     ("site_", "php_", "redis_", "collector_", ...), then what it measures:
//     pressfleet_site_cpu_usage_seconds_total.
//   - Names and label names are snake_case ASCII. Label names starting with
//     "__" are reserved.
//   - Values use base units: seconds (never milliseconds), bytes (never MiB),
//     ratios from 0 to 1 (never percentages). The unit is the name's suffix:
//     _seconds, _bytes, _ratio, or _timestamp_seconds for Unix times.
//   - Counters, which only go up, end in "_total" (after the unit if there
//     is one: _seconds_total, _bytes_total). Everything else is a gauge. A
//     collector reports a counter's current cumulative value; it does not
//     compute deltas. Rates are query-time work.
//   - Boolean states are gauges with value 0 or 1 (pressfleet_site_up).
//     Info-style metrics with descriptive labels have the value 1 and end
//     in "_info".
//   - Label values are bounded: identifiers, enum-like states, never free
//     text, URLs with parameters, or anything that grows with traffic.
//
// # Labels
//
// Every site metric carries [LabelSiteID] and [LabelClientGroupID]. Collectors
// build such samples with [Target.Sample], which sets both. The service
// enforces the rule on whatever a collector returns: a sample with a
// site_id gets the client_group_id of that site's target (overwriting a
// wrong one), and a sample for a site that is not among the targets, for
// example one deleted mid-run, is dropped. Platform-wide metrics carry
// neither label; a per-group metric carries [LabelClientGroupID] alone, and
// is dropped like a site's when no target is in that group. Collector-specific labels (domain, component, state)
// come on top and must not reuse these two names.
//
// The collector's own metrics, pressfleet_collector_*, are documented on
// [Service].
package metrics
