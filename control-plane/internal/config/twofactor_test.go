package config_test

import (
	"strings"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/config"
)

func TestTwoFactorDefaults(t *testing.T) {
	// The §6.2 defaults.
	want := config.TwoFactorConfig{Required: "off", Reauthentication: "on", ReauthenticationMaxAge: 10 * time.Minute}
	cfg, err := config.Load("", env(noDefaultFile(t, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.TwoFactor != want {
		t.Errorf("defaults = %+v, want %+v", cfg.TwoFactor, want)
	}
	if !cfg.TwoFactor.ReauthenticationEnabled() {
		t.Error("reauthentication is off by default")
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate: %v", err)
	}
}

func TestTwoFactorSettings(t *testing.T) {
	for _, tc := range []struct {
		name string
		file string
		env  map[string]string
		want config.TwoFactorConfig
	}{
		{
			// A partial file section keeps the defaults of what it
			// doesn't name.
			name: "file",
			file: "two_factor:\n  required: admins\n  reauthentication_max_age: 30m\n",
			want: config.TwoFactorConfig{Required: "admins", Reauthentication: "on", ReauthenticationMaxAge: 30 * time.Minute},
		},
		{
			// `on` and `off` work unquoted, although YAML 1.1 would read
			// them as booleans.
			name: "unquoted on and off",
			file: "two_factor:\n  required: off\n  reauthentication: off\n",
			want: config.TwoFactorConfig{Required: "off", Reauthentication: "off", ReauthenticationMaxAge: 10 * time.Minute},
		},
		{
			name: "environment",
			env: map[string]string{
				"PRESSFLEET_TWO_FACTOR_REQUIRED":                 "all",
				"PRESSFLEET_TWO_FACTOR_REAUTHENTICATION":         "off",
				"PRESSFLEET_TWO_FACTOR_REAUTHENTICATION_MAX_AGE": "1h",
			},
			want: config.TwoFactorConfig{Required: "all", Reauthentication: "off", ReauthenticationMaxAge: time.Hour},
		},
		{
			name: "environment over file",
			file: "two_factor:\n  required: all\n  reauthentication: off\n  reauthentication_max_age: 5m\n",
			env: map[string]string{
				"PRESSFLEET_TWO_FACTOR_REQUIRED":                 "admins",
				"PRESSFLEET_TWO_FACTOR_REAUTHENTICATION_MAX_AGE": "90s",
			},
			want: config.TwoFactorConfig{Required: "admins", Reauthentication: "off", ReauthenticationMaxAge: 90 * time.Second},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := ""
			if tc.file != "" {
				path = writeFile(t, tc.file)
			}
			cfg, err := config.Load(path, env(noDefaultFile(t, tc.env)))
			if err != nil {
				t.Fatal(err)
			}
			if cfg.TwoFactor != tc.want {
				t.Errorf("TwoFactor = %+v, want %+v", cfg.TwoFactor, tc.want)
			}
			if err := cfg.Validate(); err != nil {
				t.Errorf("Validate: %v", err)
			}
		})
	}
}

func TestTwoFactorLoadErrors(t *testing.T) {
	for _, tc := range []struct {
		name, file string
		env        map[string]string
		want       string
	}{
		{name: "env duration", env: map[string]string{"PRESSFLEET_TWO_FACTOR_REAUTHENTICATION_MAX_AGE": "10"},
			want: "PRESSFLEET_TWO_FACTOR_REAUTHENTICATION_MAX_AGE"},
		{name: "file duration", file: "two_factor:\n  reauthentication_max_age: ten minutes\n", want: "time.Duration"},
		{name: "unknown key", file: "two_factor:\n  require: all\n", want: "require"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := ""
			if tc.file != "" {
				path = writeFile(t, tc.file)
			}
			_, err := config.Load(path, env(noDefaultFile(t, tc.env)))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want one naming %s", err, tc.want)
			}
		})
	}
}

func TestValidateTwoFactor(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*config.TwoFactorConfig)
		want   string // substring of the error; empty means valid
	}{
		{"defaults", func(*config.TwoFactorConfig) {}, ""},
		{"admins", func(c *config.TwoFactorConfig) { c.Required = "admins" }, ""},
		{"all", func(c *config.TwoFactorConfig) { c.Required = "all" }, ""},
		{"reauthentication off", func(c *config.TwoFactorConfig) { c.Reauthentication = "off" }, ""},
		{"shortest max age", func(c *config.TwoFactorConfig) { c.ReauthenticationMaxAge = time.Minute }, ""},
		{"longest max age", func(c *config.TwoFactorConfig) { c.ReauthenticationMaxAge = time.Hour }, ""},
		{"unknown required", func(c *config.TwoFactorConfig) { c.Required = "required_for_admins" }, "two_factor.required"},
		{"empty required", func(c *config.TwoFactorConfig) { c.Required = "" }, "two_factor.required"},
		{"boolean reauthentication", func(c *config.TwoFactorConfig) { c.Reauthentication = "true" }, "two_factor.reauthentication must"},
		{"empty reauthentication", func(c *config.TwoFactorConfig) { c.Reauthentication = "" }, "two_factor.reauthentication must"},
		{"max age too short", func(c *config.TwoFactorConfig) { c.ReauthenticationMaxAge = 59 * time.Second }, "two_factor.reauthentication_max_age"},
		{"max age too long", func(c *config.TwoFactorConfig) { c.ReauthenticationMaxAge = time.Hour + time.Second }, "two_factor.reauthentication_max_age"},
		{"zero max age", func(c *config.TwoFactorConfig) { c.ReauthenticationMaxAge = 0 }, "two_factor.reauthentication_max_age"},
		{"max age checked while off", func(c *config.TwoFactorConfig) {
			c.Reauthentication, c.ReauthenticationMaxAge = "off", 2*time.Hour
		}, "two_factor.reauthentication_max_age"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Default()
			tc.mutate(&cfg.TwoFactor)
			err := cfg.Validate()
			switch {
			case tc.want == "" && err != nil:
				t.Errorf("Validate: %v", err)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
				t.Errorf("Validate: %v, want an error about %s", err, tc.want)
			}
		})
	}
}
