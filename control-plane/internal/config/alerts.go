package config

import (
	"fmt"
	"time"
)

// AlertsConfig configures alert rule evaluation (#75, design doc §7.3).
// The rules themselves are data, managed through the API.
type AlertsConfig struct {
	// Enabled turns rule evaluation on. The rules and alerts API works
	// either way; without evaluation nothing fires.
	Enabled bool `yaml:"enabled"`
	// EvaluationIntervalSeconds is the time between evaluations of every
	// rule.
	EvaluationIntervalSeconds int `yaml:"evaluation_interval_seconds"`
}

// Alert evaluation limits.
const (
	// DefaultAlertEvaluationIntervalSeconds matches how often metrics are
	// collected, so each evaluation sees new data.
	DefaultAlertEvaluationIntervalSeconds = 30
	// MinAlertEvaluationIntervalSeconds keeps evaluation from loading the
	// metrics store and the state database needlessly.
	MinAlertEvaluationIntervalSeconds = 5
	// MaxAlertEvaluationIntervalSeconds bounds the delay before an alert
	// can fire at 15 minutes.
	MaxAlertEvaluationIntervalSeconds = 900
)

// DefaultAlerts returns the default alert settings.
func DefaultAlerts() AlertsConfig {
	return AlertsConfig{Enabled: true, EvaluationIntervalSeconds: DefaultAlertEvaluationIntervalSeconds}
}

// EvaluationInterval returns EvaluationIntervalSeconds as a duration.
func (a AlertsConfig) EvaluationInterval() time.Duration {
	return time.Duration(a.EvaluationIntervalSeconds) * time.Second
}

// Validate reports settings out of range.
func (a AlertsConfig) Validate() error {
	if s := a.EvaluationIntervalSeconds; s < MinAlertEvaluationIntervalSeconds || s > MaxAlertEvaluationIntervalSeconds {
		return fmt.Errorf("alerts.evaluation_interval_seconds must be between %d and %d, got %d",
			MinAlertEvaluationIntervalSeconds, MaxAlertEvaluationIntervalSeconds, s)
	}
	return nil
}
