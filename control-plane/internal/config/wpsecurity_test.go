package config_test

import (
	"strings"
	"testing"

	"github.com/rchouinard/pressfleet/control-plane/internal/config"
)

func TestWordPressSecurityDefaults(t *testing.T) {
	// The §6.1 defaults.
	want := config.WordPressSecurityConfig{
		BlockUserEnumeration: true,
		PasswordPolicy:       config.PasswordPolicyConfig{Enabled: true, MinLength: 12},
		PasswordHashing:      "bcrypt",
		LoginRateLimit:       config.LoginRateLimitConfig{Enabled: true, RequestsPerMinute: 10, Burst: 10},
		DisableFileEdit:      true,
		Headers:              true,
		TwoFactor:            "off",
	}
	if got := config.Default().WordPressSecurity; got != want {
		t.Errorf("defaults = %+v, want %+v", got, want)
	}
}

func TestWordPressSecuritySettings(t *testing.T) {
	// A partial file section keeps the defaults of what it doesn't name.
	path := writeFile(t, `wordpress_security:
  password_policy:
    min_length: 16
  password_hashing: argon2id
  headers: false
  two_factor: optional
`)
	cfg, err := config.Load(path, env(map[string]string{
		"PRESSFLEET_WORDPRESS_SECURITY_BLOCK_USER_ENUMERATION":               "false",
		"PRESSFLEET_WORDPRESS_SECURITY_PASSWORD_POLICY_ENABLED":              "0",
		"PRESSFLEET_WORDPRESS_SECURITY_LOGIN_RATE_LIMIT_ENABLED":             "true",
		"PRESSFLEET_WORDPRESS_SECURITY_LOGIN_RATE_LIMIT_REQUESTS_PER_MINUTE": "30",
		"PRESSFLEET_WORDPRESS_SECURITY_LOGIN_RATE_LIMIT_BURST":               "0",
		"PRESSFLEET_WORDPRESS_SECURITY_DISABLE_FILE_EDIT":                    "false",
		"PRESSFLEET_WORDPRESS_SECURITY_DISABLE_APPLICATION_PASSWORDS":        "true",
		"PRESSFLEET_WORDPRESS_SECURITY_TWO_FACTOR":                           "required_for_admins",
	}))
	if err != nil {
		t.Fatal(err)
	}
	want := config.WordPressSecurityConfig{
		BlockUserEnumeration:        false,
		PasswordPolicy:              config.PasswordPolicyConfig{Enabled: false, MinLength: 16},
		PasswordHashing:             "argon2id",
		LoginRateLimit:              config.LoginRateLimitConfig{Enabled: true, RequestsPerMinute: 30, Burst: 0},
		DisableFileEdit:             false,
		Headers:                     false,
		TwoFactor:                   "required_for_admins",
		DisableApplicationPasswords: true,
	}
	if cfg.WordPressSecurity != want {
		t.Errorf("WordPressSecurity = %+v, want %+v", cfg.WordPressSecurity, want)
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate: %v", err)
	}
}

// TestWordPressSecurityUnquotedOff checks that `two_factor: off` works
// unquoted, although YAML 1.1 would read it as a boolean.
func TestWordPressSecurityUnquotedOff(t *testing.T) {
	cfg, err := config.Load(writeFile(t, "wordpress_security:\n  two_factor: off\n"), env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.WordPressSecurity.TwoFactor != config.TwoFactorOff {
		t.Errorf("two_factor = %q", cfg.WordPressSecurity.TwoFactor)
	}
}

func TestWordPressSecurityEnvErrors(t *testing.T) {
	for _, tc := range []struct{ name, value string }{
		{"PRESSFLEET_WORDPRESS_SECURITY_HEADERS", "yes please"},
		{"PRESSFLEET_WORDPRESS_SECURITY_PASSWORD_POLICY_MIN_LENGTH", "twelve"},
		{"PRESSFLEET_WORDPRESS_SECURITY_LOGIN_RATE_LIMIT_BURST", "1.5"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := config.Load("", env(noDefaultFile(t, map[string]string{tc.name: tc.value})))
			if err == nil || !strings.Contains(err.Error(), tc.name) {
				t.Errorf("err = %v, want one naming %s", err, tc.name)
			}
		})
	}
}

func TestValidateWordPressSecurity(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*config.WordPressSecurityConfig)
		want   string // substring of the error; empty means valid
	}{
		{"defaults", func(*config.WordPressSecurityConfig) {}, ""},
		{"shortest minimum", func(c *config.WordPressSecurityConfig) { c.PasswordPolicy.MinLength = 8 }, ""},
		{"minimum too short", func(c *config.WordPressSecurityConfig) { c.PasswordPolicy.MinLength = 7 }, "min_length"},
		{"minimum too long", func(c *config.WordPressSecurityConfig) { c.PasswordPolicy.MinLength = 65 }, "min_length"},
		{"minimum checked while off", func(c *config.WordPressSecurityConfig) {
			c.PasswordPolicy = config.PasswordPolicyConfig{Enabled: false, MinLength: 0}
		}, "min_length"},
		{"unknown hashing", func(c *config.WordPressSecurityConfig) { c.PasswordHashing = "md5" }, "password_hashing"},
		{"empty hashing", func(c *config.WordPressSecurityConfig) { c.PasswordHashing = "" }, "password_hashing"},
		{"zero rate", func(c *config.WordPressSecurityConfig) { c.LoginRateLimit.RequestsPerMinute = 0 }, "requests_per_minute"},
		{"rate too high", func(c *config.WordPressSecurityConfig) { c.LoginRateLimit.RequestsPerMinute = 601 }, "requests_per_minute"},
		{"negative burst", func(c *config.WordPressSecurityConfig) { c.LoginRateLimit.Burst = -1 }, "burst"},
		{"burst too high", func(c *config.WordPressSecurityConfig) { c.LoginRateLimit.Burst = 101 }, "burst"},
		{"unknown two factor", func(c *config.WordPressSecurityConfig) { c.TwoFactor = "required" }, "two_factor"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Default()
			tc.mutate(&cfg.WordPressSecurity)
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
