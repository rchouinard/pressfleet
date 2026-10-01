package config_test

import (
	"strings"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/config"
)

func TestMetricsDefaults(t *testing.T) {
	m := config.Default().Metrics
	if !m.Enabled || m.URL != "http://victoria-metrics:8428" || m.Interval() != 30*time.Second {
		t.Errorf("defaults = %+v", m)
	}
}

func TestMetricsFromEnvAndFile(t *testing.T) {
	cfg, err := config.Load(writeFile(t, "metrics:\n  url: http://vm.example:8428\n  interval_seconds: 60\n"), env(map[string]string{
		"PRESSFLEET_METRICS_INTERVAL_SECONDS": "15",
		"PRESSFLEET_METRICS_ENABLED":          "false",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Metrics.Enabled || cfg.Metrics.URL != "http://vm.example:8428" || cfg.Metrics.IntervalSeconds != 15 {
		t.Errorf("Metrics = %+v", cfg.Metrics)
	}
	if _, err := config.Load(writeFile(t, ""), env(map[string]string{"PRESSFLEET_METRICS_ENABLED": "maybe"})); err == nil {
		t.Error("invalid PRESSFLEET_METRICS_ENABLED accepted")
	}
}

func TestValidateMetrics(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*config.MetricsConfig)
		want   string
	}{
		{"default", func(*config.MetricsConfig) {}, ""},
		{"https", func(m *config.MetricsConfig) { m.URL = "https://vm.example" }, ""},
		{"no scheme", func(m *config.MetricsConfig) { m.URL = "victoria-metrics:8428" }, "metrics.url"},
		{"empty URL", func(m *config.MetricsConfig) { m.URL = "" }, "metrics.url"},
		{"too fast", func(m *config.MetricsConfig) { m.IntervalSeconds = 1 }, "metrics.interval_seconds"},
		{"too slow", func(m *config.MetricsConfig) { m.IntervalSeconds = 3601 }, "metrics.interval_seconds"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config.Default()
			tt.mutate(&cfg.Metrics)
			err := cfg.Validate()
			switch {
			case tt.want == "" && err != nil:
				t.Errorf("Validate() = %v, want nil", err)
			case tt.want != "" && (err == nil || !strings.Contains(err.Error(), tt.want)):
				t.Errorf("Validate() = %v, want error mentioning %s", err, tt.want)
			}
		})
	}
}
