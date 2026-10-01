package config

import (
	"errors"
	"fmt"
	"net/url"
	"time"
)

// DefaultMetricsRetentionDays is metrics.retention_days unless configured.
const DefaultMetricsRetentionDays = 90

// MaxMetricsRetentionDays bounds metrics.retention_days at about ten years.
const MaxMetricsRetentionDays = 3660

// MetricsConfig configures metrics storage and collection (design doc §3,
// §7): the control plane's built-in collectors write to VictoriaMetrics, and
// the alert engine and metrics API read from it.
type MetricsConfig struct {
	// RetentionDays is how long metrics are kept (decision #65). The host
	// CLI renders it into the platform project, so a change applies the next
	// time the project is rendered (pressfleet upgrade).
	RetentionDays int `yaml:"retention_days"`
	// Enabled runs the collector service. Turn it off on hosts without
	// VictoriaMetrics, or to stop collection without removing the platform
	// service.
	Enabled bool `yaml:"enabled"`
	// URL is VictoriaMetrics' base URL, for writes and queries.
	URL string `yaml:"url"`
	// IntervalSeconds is how often collectors run by default, 5 to 3600.
	// A collector may ask for a longer interval of its own.
	IntervalSeconds int `yaml:"interval_seconds"`
}

// DefaultMetrics collects every 30 seconds into the platform's
// VictoriaMetrics (templates/platform).
func DefaultMetrics() MetricsConfig {
	return MetricsConfig{
		RetentionDays:   DefaultMetricsRetentionDays,
		Enabled:         true,
		URL:             "http://victoria-metrics:8428",
		IntervalSeconds: 30,
	}
}

// Interval returns IntervalSeconds as a duration.
func (m MetricsConfig) Interval() time.Duration {
	return time.Duration(m.IntervalSeconds) * time.Second
}

// Validate reports every problem with the metrics settings, or nil.
func (m MetricsConfig) Validate() error {
	var errs []error
	if m.RetentionDays < 1 || m.RetentionDays > MaxMetricsRetentionDays {
		errs = append(errs, fmt.Errorf("metrics.retention_days must be between 1 and %d, got %d", MaxMetricsRetentionDays, m.RetentionDays))
	}
	if u, err := url.Parse(m.URL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		errs = append(errs, fmt.Errorf("metrics.url must be an http or https URL, got %q", m.URL))
	}
	if m.IntervalSeconds < 5 || m.IntervalSeconds > 3600 {
		errs = append(errs, fmt.Errorf("metrics.interval_seconds must be from 5 to 3600, got %d", m.IntervalSeconds))
	}
	return errors.Join(errs...)
}
