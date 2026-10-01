// Package alerts evaluates alert rules against metrics and tracks the
// alerts they raise (design doc §4.3, §7.3).
//
// # Rules
//
// A [store.AlertRule] is a PromQL-style expression that VictoriaMetrics
// evaluates, plus what makes its result a condition:
//
//   - A threshold rule compares the value of every series the expression
//     returns with the rule's threshold. The expression yields a number
//     (a ratio, a count, seconds) and the operator and threshold are
//     separate fields, so the thresholds of the default rules can be edited
//     without writing PromQL.
//   - An expression rule treats every series the expression returns as a
//     true condition. The expression filters, as in "pressfleet_site_up == 0".
//
// A rule's scope is "site" (every series carries a site_id label; an alert
// is raised per site, and the rule may be limited to one client group) or
// "platform" (the host and shared services; series may or may not name a
// site). The rule's duration is how long a condition must hold before its
// alert fires, which keeps a single failed check from paging anyone.
//
// # Alerts and events
//
// The [Engine] evaluates every enabled rule on an interval. Each series a
// condition holds for becomes an alert, identified by the rule and a hash
// of the series' labels, and moves through three states:
//
//	pending   the condition holds, but not yet for the rule's duration
//	firing    it has held for the duration, and still does
//	resolved  it stopped holding
//
// Each alert emits at most two events, one firing and one resolved, to a
// [Notifier]. A condition that keeps holding updates its alert, and emits
// nothing; a pending alert whose condition stops holding is dropped
// without a trace, because nothing was ever sent for it. The database
// allows only one unresolved alert per rule and series, so evaluation
// can't create duplicates even if two evaluations overlap.
//
// A series that stops existing counts as resolved, but a metrics query that
// fails changes nothing: alerts neither fire nor resolve while VictoriaMetrics
// can't be asked, so an outage of the metrics store doesn't resolve every
// alert at once.
//
// # Restarts
//
// Alert state lives in the database. After a restart a firing alert is
// still firing, and its condition is checked again at the next evaluation,
// without a second firing event. Events are delivered from the database:
// an alert records when the notifier accepted each of its events, and the
// engine delivers every event not yet accepted, in order, after each
// evaluation. An event the notifier didn't accept (it returned an error,
// or the process stopped first) is therefore delivered again, which makes
// delivery at least once; a [Notifier] that needs exactly once can
// deduplicate on [Event.Key].
//
// # Metrics
//
// The engine reaches VictoriaMetrics only through [Querier]. The default
// rules depend on exactly the metrics below, and nothing else: whoever
// implements a collector (#68 to #74) matches it to this table. Names follow
// the platform's conventions: the pressfleet_ prefix, snake_case, base
// units (seconds, bytes, ratios between 0 and 1), counters ending in
// _total, and the labels site_id and client_group_id on everything that
// belongs to a site.
//
//	Metric                                                       Type     Labels                                   Used by
//	pressfleet_site_up                                           gauge    site_id, client_group_id                 site_down
//	pressfleet_site_http_requests_total                          counter  site_id, client_group_id, status_class   elevated_5xx_rate
//	pressfleet_site_tls_certificate_expiry_timestamp_seconds     gauge    site_id, client_group_id, domain         tls_certificate_expiring, tls_certificate_renewal_failing
//	pressfleet_site_backup_last_run_success                      gauge    site_id, client_group_id                 backup_failed
//	pressfleet_site_backup_last_success_timestamp_seconds        gauge    site_id, client_group_id                 backup_stale
//	pressfleet_site_php_fpm_active_processes                     gauge    site_id, client_group_id                 php_fpm_saturation
//	pressfleet_site_php_fpm_max_children                         gauge    site_id, client_group_id                 php_fpm_saturation
//	pressfleet_container_restarts_total                          counter  container, [site_id, client_group_id]    container_restart_loop
//	pressfleet_host_filesystem_size_bytes                        gauge    mountpoint                               disk_nearly_full
//	pressfleet_host_filesystem_avail_bytes                       gauge    mountpoint                               disk_nearly_full
//	pressfleet_redis_evicted_keys_total                          counter  (none)                                   redis_eviction_spike
//
// What each one means:
//
//   - pressfleet_site_up is 1 while the control plane's HTTP check of the
//     site succeeds and 0 while it fails. Only sites that should be serving
//     emit it: a stopped site, or one being provisioned or deleted, emits
//     nothing, so it never alerts.
//   - pressfleet_site_http_requests_total counts requests the site served;
//     status_class is "1xx" to "5xx".
//   - ..._tls_certificate_expiry_timestamp_seconds is the Unix time at which
//     the certificate served for domain expires.
//   - pressfleet_site_backup_last_run_success is 1 if the site's latest
//     backup succeeded and 0 if it failed. ..._last_success_timestamp_seconds
//     is the Unix time of the latest backup that succeeded. Both are absent
//     for a site that has never been backed up, and for one whose backup
//     schedule is disabled.
//   - pressfleet_site_php_fpm_active_processes is the number of busy PHP-FPM
//     workers and ..._max_children the pool's pm.max_children.
//   - pressfleet_container_restarts_total is the container's restart count.
//     Site containers carry site_id and client_group_id; platform containers
//     (Traefik, MariaDB, Redis, and the control plane itself) don't.
//   - pressfleet_host_filesystem_*_bytes are the size and the space available
//     to unprivileged users of each mounted filesystem the platform cares
//     about (the Docker data root and the PressFleet data directory).
//   - pressfleet_redis_evicted_keys_total is the object cache server's
//     evicted_keys counter.
//
// [DefaultMetrics] lists the same names in code, and a test checks that the
// default rules use exactly those.
//
// # Wiring
//
// The control plane seeds the default rules with [SeedDefaults] at startup
// and runs the engine with [Engine.Run]. The engine needs a [Querier] for
// VictoriaMetrics and a [Notifier]: the control plane passes package
// alertchannels, which delivers events by email and webhook.
// [UnavailableQuerier] reports that metrics aren't available, and
// [NewLogNotifier] logs events instead of delivering them, for the engine
// without a notifier.
package alerts

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/store"
)

// ErrMetricsUnavailable means the metrics store can't be queried (it isn't
// configured, is down, or the query timed out). Queriers wrap it so the
// engine can tell an outage from a bad expression, though it treats both
// the same way: nothing changes until the next evaluation.
var ErrMetricsUnavailable = errors.New("metrics unavailable")

// Sample is one series of an instant query result.
type Sample struct {
	// Labels are the series' labels. The engine ignores __name__.
	Labels map[string]string
	// Value is the series' value at the time of the query.
	Value float64
}

// Querier runs instant queries against the metrics store (VictoriaMetrics).
// It is the only way the engine reaches it.
type Querier interface {
	// Query evaluates the PromQL expression expr at time at and returns the
	// resulting vector: one Sample per series, none if the expression
	// matches nothing. Scalar results should be returned as one sample
	// without labels. An error wrapping ErrMetricsUnavailable means the
	// store can't be reached.
	Query(ctx context.Context, expr string, at time.Time) ([]Sample, error)
}

// UnavailableQuerier is a [Querier] for when there is no metrics store to
// ask: every query fails with ErrMetricsUnavailable, so no alert fires or
// resolves.
type UnavailableQuerier struct{}

// Query implements Querier.
func (UnavailableQuerier) Query(context.Context, string, time.Time) ([]Sample, error) {
	return nil, ErrMetricsUnavailable
}

// Event is an alert firing or resolving. It carries everything a notifier
// needs, so it never has to look anything up.
type Event struct {
	Type store.AlertEventType
	// Alert is the alert as it stands, with its rendered Summary and
	// Description, its Labels (the series' plus the rule's), its Value,
	// and its timestamps.
	Alert store.Alert
	// Rule is the alert's rule, as it stands.
	Rule store.AlertRule
}

// Key identifies the event: the same alert and type always give the same
// key, so a notifier can drop an event it has already handled.
func (e Event) Key() string { return e.Alert.ID + ":" + string(e.Type) }

// Notifier delivers alert events. Package alertchannels (#76) implements it
// with SMTP and webhook channels.
type Notifier interface {
	// Notify accepts an event for delivery. Returning nil means the
	// notifier has taken responsibility for it, and the engine won't send
	// it again; a notifier that retries delivery should do so itself.
	// Returning an error means the event wasn't accepted, and the engine
	// offers it again after its next evaluation. Notify is called from the
	// engine's goroutine, one event at a time, so it should return
	// promptly.
	Notify(ctx context.Context, e Event) error
}

// NewLogNotifier returns a [Notifier] that logs events and delivers
// nothing, for when no channel is configured.
func NewLogNotifier(log *slog.Logger) Notifier { return logNotifier{log: log} }

type logNotifier struct{ log *slog.Logger }

func (n logNotifier) Notify(ctx context.Context, e Event) error {
	n.log.LogAttrs(ctx, slog.LevelInfo, "alert event",
		slog.String("event", string(e.Type)),
		slog.String("alert_id", e.Alert.ID),
		slog.String("rule", e.Rule.Name),
		slog.String("severity", string(e.Alert.Severity)),
		slog.String("site_id", e.Alert.SiteID),
		slog.String("summary", e.Alert.Summary),
		slog.Float64("value", e.Alert.Value))
	return nil
}
