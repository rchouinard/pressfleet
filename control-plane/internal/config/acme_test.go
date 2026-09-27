package config_test

import (
	"strings"
	"testing"

	"github.com/rchouinard/pressfleet/control-plane/internal/config"
)

func TestACMESettings(t *testing.T) {
	if got := config.Default().ACME; got != (config.ACMEConfig{}) {
		t.Errorf("default acme = %+v", got)
	}
	path := writeFile(t, "acme:\n  email: ops@agency.example\n  ca_server: https://acme-staging-v02.api.letsencrypt.org/directory\n")
	cfg, err := config.Load(path, env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ACME.Email != "ops@agency.example" || cfg.ACME.CAServer != "https://acme-staging-v02.api.letsencrypt.org/directory" {
		t.Errorf("acme from file = %+v", cfg.ACME)
	}
	cfg, err = config.Load(path, env(map[string]string{
		"PRESSFLEET_ACME_EMAIL":     "certs@agency.example",
		"PRESSFLEET_ACME_CA_SERVER": "https://pebble:14000/dir",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ACME.Email != "certs@agency.example" || cfg.ACME.CAServer != "https://pebble:14000/dir" {
		t.Errorf("acme from env = %+v", cfg.ACME)
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate() = %v", err)
	}
}

func TestValidateACME(t *testing.T) {
	tests := []struct {
		name   string
		modify func(*config.ACMEConfig)
		want   string // substring of the error; empty for valid
	}{
		{"defaults", func(*config.ACMEConfig) {}, ""},
		{"email", func(a *config.ACMEConfig) { a.Email = "ops@agency.example" }, ""},
		{"no at", func(a *config.ACMEConfig) { a.Email = "ops" }, "acme.email"},
		{"no local part", func(a *config.ACMEConfig) { a.Email = "@agency.example" }, "acme.email"},
		{"bad domain", func(a *config.ACMEConfig) { a.Email = "ops@localhost" }, "acme.email"},
		{"display name", func(a *config.ACMEConfig) { a.Email = "Ops <ops@agency.example>" }, "acme.email"},
		{"pebble", func(a *config.ACMEConfig) { a.CAServer = "https://pebble:14000/dir" }, ""},
		{"plain http", func(a *config.ACMEConfig) { a.CAServer = "http://pebble:14000/dir" }, "acme.ca_server"},
		{"no scheme", func(a *config.ACMEConfig) { a.CAServer = "pebble:14000/dir" }, "acme.ca_server"},
		{"credentials", func(a *config.ACMEConfig) { a.CAServer = "https://u:p@pebble/dir" }, "acme.ca_server"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config.Default()
			tt.modify(&cfg.ACME)
			err := cfg.Validate()
			switch {
			case tt.want == "" && err != nil:
				t.Errorf("Validate() = %v", err)
			case tt.want != "" && (err == nil || !strings.Contains(err.Error(), tt.want)):
				t.Errorf("Validate() = %v, want an error about %s", err, tt.want)
			}
		})
	}
}
