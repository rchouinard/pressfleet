package config

import (
	"errors"
	"fmt"
	"time"
)

// TwoFactorConfig is the dashboard two-factor policy (design doc §6.2). It
// is separate from the Two Factor plugin in sites' wp-admin
// (WordPressSecurityConfig.TwoFactor, §6.1).
type TwoFactorConfig struct {
	// Required says whose roles must enroll a second factor:
	// TwoFactorRequiredOff, TwoFactorRequiredAdmins, or
	// TwoFactorRequiredAll.
	Required string `yaml:"required"`
	// Reauthentication is ReauthenticationOn or ReauthenticationOff: whether
	// sensitive dashboard actions need a recent factor check.
	Reauthentication string `yaml:"reauthentication"`
	// ReauthenticationMaxAge is how long a check counts as fresh.
	ReauthenticationMaxAge time.Duration `yaml:"reauthentication_max_age"`
}

// Values of two_factor.required.
const (
	TwoFactorRequiredOff    = "off"
	TwoFactorRequiredAdmins = "admins"
	TwoFactorRequiredAll    = "all"
)

// Values of two_factor.reauthentication. They are words rather than a
// boolean so the file and the environment variable take the same values.
const (
	ReauthenticationOn  = "on"
	ReauthenticationOff = "off"
)

// Bounds of two_factor.reauthentication_max_age (§6.2).
const (
	MinReauthenticationMaxAge = time.Minute
	MaxReauthenticationMaxAge = time.Hour
)

// DefaultTwoFactor returns the §6.2 defaults.
func DefaultTwoFactor() TwoFactorConfig {
	return TwoFactorConfig{
		Required:               TwoFactorRequiredOff,
		Reauthentication:       ReauthenticationOn,
		ReauthenticationMaxAge: 10 * time.Minute,
	}
}

// ReauthenticationEnabled reports whether sensitive actions need a fresh
// factor check.
func (c TwoFactorConfig) ReauthenticationEnabled() bool {
	return c.Reauthentication == ReauthenticationOn
}

// Environment variables of the two_factor settings.
const (
	envTwoFactorRequired               = "PRESSFLEET_TWO_FACTOR_REQUIRED"
	envTwoFactorReauthentication       = "PRESSFLEET_TWO_FACTOR_REAUTHENTICATION"
	envTwoFactorReauthenticationMaxAge = "PRESSFLEET_TWO_FACTOR_REAUTHENTICATION_MAX_AGE"
)

// twoFactorEnvVars lists the variables applyEnv reads, for the
// documentation test.
var twoFactorEnvVars = []string{envTwoFactorRequired, envTwoFactorReauthentication, envTwoFactorReauthenticationMaxAge}

// applyEnv overrides c with PRESSFLEET_TWO_FACTOR_* variables.
func (c *TwoFactorConfig) applyEnv(env LookupEnv) error {
	if val, ok := env(envTwoFactorRequired); ok {
		c.Required = val
	}
	if val, ok := env(envTwoFactorReauthentication); ok {
		c.Reauthentication = val
	}
	if val, ok := env(envTwoFactorReauthenticationMaxAge); ok {
		d, err := time.ParseDuration(val)
		if err != nil {
			return fmt.Errorf("%s must be a duration like 10m, got %q", envTwoFactorReauthenticationMaxAge, val)
		}
		c.ReauthenticationMaxAge = d
	}
	return nil
}

// validate reports every problem with the two-factor settings. The maximum
// age is checked while reauthentication is off, so turning it on later
// can't reveal a mistake made long before.
func (c TwoFactorConfig) validate() error {
	var errs []error
	switch c.Required {
	case TwoFactorRequiredOff, TwoFactorRequiredAdmins, TwoFactorRequiredAll:
	default:
		errs = append(errs, fmt.Errorf("two_factor.required must be %q, %q, or %q, got %q",
			TwoFactorRequiredOff, TwoFactorRequiredAdmins, TwoFactorRequiredAll, c.Required))
	}
	switch c.Reauthentication {
	case ReauthenticationOn, ReauthenticationOff:
	default:
		errs = append(errs, fmt.Errorf("two_factor.reauthentication must be %q or %q, got %q",
			ReauthenticationOn, ReauthenticationOff, c.Reauthentication))
	}
	if d := c.ReauthenticationMaxAge; d < MinReauthenticationMaxAge || d > MaxReauthenticationMaxAge {
		errs = append(errs, fmt.Errorf("two_factor.reauthentication_max_age must be between 1m and 1h, got %s", d))
	}
	return errors.Join(errs...)
}
