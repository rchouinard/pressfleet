package config_test

import (
	"strings"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/config"
)

func TestAlertsSettings(t *testing.T) {
	def := config.Default().Alerts
	if !def.Enabled || def.EvaluationInterval() != 30*time.Second {
		t.Errorf("default alerts = %+v", def)
	}
	path := writeFile(t, "alerts:\n  enabled: false\n  evaluation_interval_seconds: 60\n")
	cfg, err := config.Load(path, env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if want := (config.AlertsConfig{Enabled: false, EvaluationIntervalSeconds: 60}); cfg.Alerts != want {
		t.Errorf("Alerts = %+v, want %+v", cfg.Alerts, want)
	}
	cfg, err = config.Load(path, env(map[string]string{
		"PRESSFLEET_ALERTS_ENABLED":                     "true",
		"PRESSFLEET_ALERTS_EVALUATION_INTERVAL_SECONDS": "15",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Alerts.Enabled || cfg.Alerts.EvaluationInterval() != 15*time.Second {
		t.Errorf("Alerts with the environment = %+v", cfg.Alerts)
	}
	for k, v := range map[string]string{"PRESSFLEET_ALERTS_ENABLED": "maybe", "PRESSFLEET_ALERTS_EVALUATION_INTERVAL_SECONDS": "soon"} {
		if _, err := config.Load(path, env(map[string]string{k: v})); err == nil || !strings.Contains(err.Error(), k) {
			t.Errorf("Load with %s=%s = %v", k, v, err)
		}
	}

	for _, tt := range []struct {
		name    string
		seconds int
		want    string // empty: valid
	}{
		{"default", 30, ""},
		{"smallest", 5, ""},
		{"largest", 900, ""},
		{"too small", 4, "alerts.evaluation_interval_seconds"},
		{"zero", 0, "alerts.evaluation_interval_seconds"},
		{"too large", 901, "alerts.evaluation_interval_seconds"},
	} {
		cfg := config.Default()
		cfg.Alerts.EvaluationIntervalSeconds = tt.seconds
		err := cfg.Validate()
		switch {
		case tt.want == "" && err != nil:
			t.Errorf("%s: Validate() = %v, want nil", tt.name, err)
		case tt.want != "" && (err == nil || !strings.Contains(err.Error(), tt.want)):
			t.Errorf("%s: Validate() = %v, want error mentioning %q", tt.name, err, tt.want)
		}
	}
}
