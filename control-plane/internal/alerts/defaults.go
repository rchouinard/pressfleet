package alerts

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/store"
)

// Metric names the default rules depend on. The table in the package
// documentation says what each one means; collectors emit exactly these.
const (
	MetricSiteUp                  = "pressfleet_site_up"
	MetricSiteHTTPRequests        = "pressfleet_site_http_requests_total"
	MetricSiteCertExpiry          = "pressfleet_site_tls_certificate_expiry_timestamp_seconds"
	MetricSiteBackupLastRunOK     = "pressfleet_site_backup_last_run_success"
	MetricSiteBackupLastSuccess   = "pressfleet_site_backup_last_success_timestamp_seconds"
	MetricSitePHPFPMActive        = "pressfleet_site_php_fpm_active_processes"
	MetricSitePHPFPMMaxChildren   = "pressfleet_site_php_fpm_max_children"
	MetricContainerRestarts       = "pressfleet_container_restarts_total"
	MetricHostFilesystemSizeBytes = "pressfleet_host_filesystem_size_bytes"
	MetricHostFilesystemAvail     = "pressfleet_host_filesystem_avail_bytes"
	MetricRedisEvictedKeys        = "pressfleet_redis_evicted_keys_total"
	MetricSitePendingUpdates      = "pressfleet_site_pending_updates"
)

// DefaultMetrics lists every metric the default rules use.
var DefaultMetrics = []string{
	MetricSiteUp,
	MetricSiteHTTPRequests,
	MetricSiteCertExpiry,
	MetricSiteBackupLastRunOK,
	MetricSiteBackupLastSuccess,
	MetricSitePHPFPMActive,
	MetricSitePHPFPMMaxChildren,
	MetricContainerRestarts,
	MetricHostFilesystemSizeBytes,
	MetricHostFilesystemAvail,
	MetricRedisEvictedKeys,
	MetricSitePendingUpdates,
}

// Keys of the default rules. They are stored as the rules' BuiltinKey and
// never change: seeding finds a rule by its key.
const (
	KeySiteDown             = "site_down"
	KeyElevated5xx          = "elevated_5xx_rate"
	KeyCertExpiring         = "tls_certificate_expiring"
	KeyCertRenewalFailing   = "tls_certificate_renewal_failing"
	KeyBackupFailed         = "backup_failed"
	KeyBackupStale          = "backup_stale"
	KeyDiskNearlyFull       = "disk_nearly_full"
	KeyContainerRestartLoop = "container_restart_loop"
	KeyPHPFPMSaturation     = "php_fpm_saturation"
	KeyRedisEvictionSpike   = "redis_eviction_spike"
	KeyUpdatesPending       = "updates_pending"
)

// Expressions and parts of them.
const (
	site5xxSeries              = `sum by (site_id, client_group_id) (rate(` + MetricSiteHTTPRequests + `{status_class="5xx"}[5m]))`
	siteRequestSeries          = `sum by (site_id, client_group_id) (rate(` + MetricSiteHTTPRequests + `[5m]))`
	minRequestsPerSecond       = "0.05"
	certSecondsRemainingExpr   = MetricSiteCertExpiry + ` - time()`
	backupSecondsSinceSuccess  = `time() - ` + MetricSiteBackupLastSuccess
	diskUsedRatioExpr          = `1 - (` + MetricHostFilesystemAvail + ` / ` + MetricHostFilesystemSizeBytes + `)`
	phpFPMSaturationRatioExpr  = MetricSitePHPFPMActive + ` / ` + MetricSitePHPFPMMaxChildren
	containerRestartsExpr      = `increase(` + MetricContainerRestarts + `[15m])`
	redisEvictionsPerSecondExp = `rate(` + MetricRedisEvictedKeys + `[5m])`
)

// DefaultRules returns the built-in rules (design doc §7.3). Every rule is
// enabled and has a BuiltinKey; thresholds and durations can be edited
// afterwards.
func DefaultRules() []store.AlertRule {
	return []store.AlertRule{
		{
			BuiltinKey:  KeySiteDown,
			Name:        "Site down",
			Description: "The control plane's HTTP check of the site has been failing.",
			Scope:       store.AlertScopeSite,
			Kind:        store.AlertKindExpression,
			Expr:        MetricSiteUp + ` == 0`,
			For:         2 * time.Minute,
			Severity:    store.SeverityCritical,
			Annotations: summary(
				"Site {{ .Labels.site_id }} is down",
				"The HTTP check of site {{ .Labels.site_id }} has been failing for at least 2 minutes."),
		},
		{
			BuiltinKey: KeyElevated5xx,
			Name:       "Elevated 5xx error rate",
			Description: "The share of a site's responses that are 5xx errors is above the threshold. " +
				"Sites with almost no traffic are ignored. The threshold is a ratio: 0.05 is 5%.",
			Scope: store.AlertScopeSite,
			Kind:  store.AlertKindThreshold,
			// Dividing by a filtered vector leaves out sites with less than
			// minRequestsPerSecond requests per second, where one error
			// would be a huge share.
			Expr:      site5xxSeries + ` / (` + siteRequestSeries + ` > ` + minRequestsPerSecond + `)`,
			Operator:  store.OpGreater,
			Threshold: 0.05,
			For:       5 * time.Minute,
			Severity:  store.SeverityWarning,
			Annotations: summary(
				"Site {{ .Labels.site_id }}: {{ humanizePercentage .Value }} of responses are 5xx errors",
				"The 5xx error rate of site {{ .Labels.site_id }} has been above {{ humanizePercentage .Threshold }} for at least 5 minutes."),
		},
		{
			BuiltinKey: KeyCertExpiring,
			Name:       "TLS certificate expiring",
			Description: "A site's TLS certificate expires soon. Certificates are renewed well before then, so this " +
				"is an early warning. The threshold is in seconds: 1209600 is 14 days.",
			Scope:     store.AlertScopeSite,
			Kind:      store.AlertKindThreshold,
			Expr:      certSecondsRemainingExpr,
			Operator:  store.OpLess,
			Threshold: (14 * 24 * time.Hour).Seconds(),
			For:       15 * time.Minute,
			Severity:  store.SeverityWarning,
			Annotations: summary(
				"The certificate for {{ .Labels.domain }} expires in {{ humanizeDuration .Value }}",
				"The TLS certificate for {{ .Labels.domain }} (site {{ .Labels.site_id }}) expires in {{ humanizeDuration .Value }}."),
		},
		{
			BuiltinKey: KeyCertRenewalFailing,
			Name:       "TLS certificate renewal failing",
			Description: "A site's TLS certificate is about to expire (or has), which means renewal has been failing for weeks. " +
				"The threshold is in seconds: 259200 is 3 days.",
			Scope:     store.AlertScopeSite,
			Kind:      store.AlertKindThreshold,
			Expr:      certSecondsRemainingExpr,
			Operator:  store.OpLess,
			Threshold: (3 * 24 * time.Hour).Seconds(),
			For:       15 * time.Minute,
			Severity:  store.SeverityCritical,
			Annotations: summary(
				"The certificate for {{ .Labels.domain }} wasn't renewed and expires in {{ humanizeDuration .Value }}",
				"The TLS certificate for {{ .Labels.domain }} (site {{ .Labels.site_id }}) should have been renewed long ago. "+
					"Check that its DNS points at this server and that Traefik can reach the certificate authority."),
		},
		{
			BuiltinKey:  KeyBackupFailed,
			Name:        "Backup failed",
			Description: "The latest backup of a site failed.",
			Scope:       store.AlertScopeSite,
			Kind:        store.AlertKindExpression,
			Expr:        MetricSiteBackupLastRunOK + ` == 0`,
			Severity:    store.SeverityCritical,
			Annotations: summary(
				"The latest backup of site {{ .Labels.site_id }} failed",
				"The latest backup of site {{ .Labels.site_id }} failed; see its backup history for the error."),
		},
		{
			BuiltinKey: KeyBackupStale,
			Name:       "Backup stale",
			Description: "A site hasn't been backed up successfully for too long. The threshold is in seconds: " +
				"129600 is 36 hours, a day of daily backups and some slack.",
			Scope:     store.AlertScopeSite,
			Kind:      store.AlertKindThreshold,
			Expr:      backupSecondsSinceSuccess,
			Operator:  store.OpGreater,
			Threshold: (36 * time.Hour).Seconds(),
			For:       15 * time.Minute,
			Severity:  store.SeverityWarning,
			Annotations: summary(
				"Site {{ .Labels.site_id }} hasn't been backed up for {{ humanizeDuration .Value }}",
				"The last successful backup of site {{ .Labels.site_id }} was {{ humanizeDuration .Value }} ago."),
		},
		{
			BuiltinKey: KeyDiskNearlyFull,
			Name:       "Disk nearly full",
			Description: "A filesystem the platform uses is nearly full. The threshold is the used share: " +
				"0.9 is 90%. On Kubernetes this rule has nothing to evaluate, because the platform reports no " +
				"host disk metrics there; the cluster's own monitoring covers the nodes' disks.",
			Scope:     store.AlertScopePlatform,
			Kind:      store.AlertKindThreshold,
			Expr:      diskUsedRatioExpr,
			Operator:  store.OpGreater,
			Threshold: 0.9,
			For:       10 * time.Minute,
			Severity:  store.SeverityWarning,
			Annotations: summary(
				"{{ .Labels.mountpoint }} is {{ humanizePercentage .Value }} full",
				"The filesystem mounted at {{ .Labels.mountpoint }} has been more than {{ humanizePercentage .Threshold }} full for at least 10 minutes."),
		},
		{
			BuiltinKey: KeyContainerRestartLoop,
			Name:       "Container restart loop",
			Description: "A container restarted more than the threshold number of times in 15 minutes. It covers site " +
				"containers, whose alerts carry the site, and platform containers, which don't.",
			Scope:     store.AlertScopePlatform,
			Kind:      store.AlertKindThreshold,
			Expr:      containerRestartsExpr,
			Operator:  store.OpGreater,
			Threshold: 3,
			Severity:  store.SeverityCritical,
			Annotations: summary(
				"Container {{ .Labels.container }} keeps restarting",
				"Container {{ .Labels.container }} restarted about {{ printf \"%.0f\" .Value }} times in the last 15 minutes."),
		},
		{
			BuiltinKey: KeyPHPFPMSaturation,
			Name:       "PHP-FPM worker saturation",
			Description: "A site's PHP-FPM workers are almost all busy, so new requests will queue. The threshold is the " +
				"busy share of pm.max_children: 0.9 is 90%.",
			Scope:     store.AlertScopeSite,
			Kind:      store.AlertKindThreshold,
			Expr:      phpFPMSaturationRatioExpr,
			Operator:  store.OpGreater,
			Threshold: 0.9,
			For:       5 * time.Minute,
			Severity:  store.SeverityWarning,
			Annotations: summary(
				"Site {{ .Labels.site_id }}: {{ humanizePercentage .Value }} of PHP-FPM workers are busy",
				"The PHP-FPM workers of site {{ .Labels.site_id }} have been more than {{ humanizePercentage .Threshold }} busy for at least 5 minutes."),
		},
		{
			BuiltinKey: KeyRedisEvictionSpike,
			Name:       "Redis eviction spike",
			Description: "The object cache is evicting keys faster than the threshold, which means it is out of memory. " +
				"The threshold is evictions per second.",
			Scope:     store.AlertScopePlatform,
			Kind:      store.AlertKindThreshold,
			Expr:      redisEvictionsPerSecondExp,
			Operator:  store.OpGreater,
			Threshold: 1,
			For:       5 * time.Minute,
			Severity:  store.SeverityWarning,
			Annotations: summary(
				"The object cache is evicting {{ printf \"%.1f\" .Value }} keys per second",
				"The object cache has been evicting more than {{ printf \"%.1f\" .Threshold }} keys per second for at least 5 minutes; give it more memory."),
		},
		{
			// Pending updates make a site's health a warning, never a
			// failure (#125, design doc §9 Q2). The duration leaves
			// WordPress's own auto-updates, which run twice a day, time to
			// apply them, so only updates nobody is applying alert.
			BuiltinKey: KeyUpdatesPending,
			Name:       "Plugin or theme updates pending",
			Description: "A site has had plugin or theme updates available for a while. WordPress applies them " +
				"(auto-updates or wp-admin); the platform only reports them. The threshold is a number of updates.",
			Scope:     store.AlertScopeSite,
			Kind:      store.AlertKindThreshold,
			Expr:      MetricSitePendingUpdates,
			Operator:  store.OpGreater,
			Threshold: 0,
			For:       72 * time.Hour,
			Severity:  store.SeverityWarning,
			Annotations: summary(
				"Site {{ .Labels.site_id }} has {{ printf \"%.0f\" .Value }} {{ .Labels.kind }} updates pending",
				"Site {{ .Labels.site_id }} has had {{ .Labels.kind }} updates available for at least 3 days. "+
					"The site's page in the dashboard lists them; apply them in wp-admin."),
		},
	}
}

func summary(summary, description string) map[string]string {
	return map[string]string{AnnotationSummary: summary, AnnotationDescription: description}
}

// SeedDefaults inserts the default rules that don't exist yet, and returns
// how many it inserted. It is safe to call on every start: a rule that
// exists, however the user has changed it or disabled it, is left alone,
// and one that has been added by a newer version appears. Rules are found
// by BuiltinKey, so renaming a default rule doesn't make it reappear.
func SeedDefaults(ctx context.Context, q store.Querier, log *slog.Logger) (int, error) {
	rules := store.NewAlertRules(q)
	inserted := 0
	for _, r := range DefaultRules() {
		if err := ValidateRule(r); err != nil {
			return inserted, fmt.Errorf("default alert rule %s: %w", r.BuiltinKey, err)
		}
		r.Enabled = true
		ok, err := rules.EnsureBuiltin(ctx, &r)
		if err != nil {
			return inserted, err
		}
		if ok {
			inserted++
			log.LogAttrs(ctx, slog.LevelInfo, "added default alert rule", slog.String("key", r.BuiltinKey), slog.String("name", r.Name))
		}
	}
	return inserted, nil
}
