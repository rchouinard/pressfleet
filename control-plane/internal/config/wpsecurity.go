package config

import (
	"errors"
	"fmt"
	"strconv"
)

// WordPressSecurityConfig is the install-wide WordPress security set (design
// doc §6.1, decision #42). Every feature is on or off for all sites; there
// are no per-site overrides. The control plane applies a change to existing
// sites when it restarts.
type WordPressSecurityConfig struct {
	// BlockUserEnumeration hides which user names exist: ?author=N
	// redirects, anonymous /wp/v2/users, author data in oEmbed, the users
	// sitemap, and login errors that tell a wrong user from a wrong password.
	BlockUserEnumeration bool `yaml:"block_user_enumeration"`
	// PasswordPolicy is checked on the server whenever a password is set.
	PasswordPolicy PasswordPolicyConfig `yaml:"password_policy"`
	// PasswordHashing is the algorithm for new password hashes:
	// PasswordHashingBcrypt (WordPress's default) or PasswordHashingArgon2id.
	PasswordHashing string `yaml:"password_hashing"`
	// LoginRateLimit limits requests to wp-login.php per client IP (nginx).
	LoginRateLimit LoginRateLimitConfig `yaml:"login_rate_limit"`
	// DisableFileEdit turns off the theme and plugin file editor
	// (DISALLOW_FILE_EDIT). Plugin and theme updates keep working.
	DisableFileEdit bool `yaml:"disable_file_edit"`
	// Headers sends Referrer-Policy, X-Frame-Options, and Permissions-Policy
	// from nginx (and HSTS on primary domains, from Traefik).
	Headers bool `yaml:"headers"`
	// TwoFactor controls the Two Factor plugin, installed at provisioning:
	// TwoFactorOff, TwoFactorOptional, or TwoFactorRequiredForAdmins.
	TwoFactor string `yaml:"two_factor"`
	// DisableApplicationPasswords turns off WordPress application
	// passwords. Off by default because integrations rely on them.
	DisableApplicationPasswords bool `yaml:"disable_application_passwords"`
}

// PasswordPolicyConfig is the server-side password policy.
type PasswordPolicyConfig struct {
	Enabled bool `yaml:"enabled"`
	// MinLength is the minimum password length in characters.
	MinLength int `yaml:"min_length"`
}

// LoginRateLimitConfig is the per-IP rate limit on wp-login.php.
type LoginRateLimitConfig struct {
	Enabled bool `yaml:"enabled"`
	// RequestsPerMinute is the sustained rate one client IP may reach.
	RequestsPerMinute int `yaml:"requests_per_minute"`
	// Burst is how many requests above the rate are let through at once
	// before nginx answers 429.
	Burst int `yaml:"burst"`
}

// Values of wordpress_security.password_hashing.
const (
	PasswordHashingBcrypt   = "bcrypt"
	PasswordHashingArgon2id = "argon2id"
)

// Values of wordpress_security.two_factor.
const (
	TwoFactorOff               = "off"
	TwoFactorOptional          = "optional"
	TwoFactorRequiredForAdmins = "required_for_admins"
)

// Bounds of the security set's numeric settings. The common-password list
// in the site images only holds passwords of at least MinPasswordMinLength
// characters, so a shorter minimum would let listed passwords through.
const (
	MinPasswordMinLength = 8
	MaxPasswordMinLength = 64
	MaxLoginRatePerMin   = 600
	MaxLoginBurst        = 100
)

// DefaultWordPressSecurity returns the §6.1 defaults.
func DefaultWordPressSecurity() WordPressSecurityConfig {
	return WordPressSecurityConfig{
		BlockUserEnumeration: true,
		PasswordPolicy:       PasswordPolicyConfig{Enabled: true, MinLength: 12},
		PasswordHashing:      PasswordHashingBcrypt,
		// Loading the form and posting it are two requests, so ten a
		// minute with a burst of ten leaves room for a few typos, even
		// behind a shared office address, while stopping brute force.
		LoginRateLimit:  LoginRateLimitConfig{Enabled: true, RequestsPerMinute: 10, Burst: 10},
		DisableFileEdit: true,
		Headers:         true,
		TwoFactor:       TwoFactorOff,
	}
}

const envWPSecurity = "PRESSFLEET_WORDPRESS_SECURITY_"

// wpSecurityBools and wpSecurityInts map environment variables to the
// security set's fields; the names follow the YAML keys.
var (
	wpSecurityBools = []struct {
		name  string
		field func(*WordPressSecurityConfig) *bool
	}{
		{"BLOCK_USER_ENUMERATION", func(c *WordPressSecurityConfig) *bool { return &c.BlockUserEnumeration }},
		{"PASSWORD_POLICY_ENABLED", func(c *WordPressSecurityConfig) *bool { return &c.PasswordPolicy.Enabled }},
		{"LOGIN_RATE_LIMIT_ENABLED", func(c *WordPressSecurityConfig) *bool { return &c.LoginRateLimit.Enabled }},
		{"DISABLE_FILE_EDIT", func(c *WordPressSecurityConfig) *bool { return &c.DisableFileEdit }},
		{"HEADERS", func(c *WordPressSecurityConfig) *bool { return &c.Headers }},
		{"DISABLE_APPLICATION_PASSWORDS", func(c *WordPressSecurityConfig) *bool { return &c.DisableApplicationPasswords }},
	}
	wpSecurityInts = []struct {
		name  string
		field func(*WordPressSecurityConfig) *int
	}{
		{"PASSWORD_POLICY_MIN_LENGTH", func(c *WordPressSecurityConfig) *int { return &c.PasswordPolicy.MinLength }},
		{"LOGIN_RATE_LIMIT_REQUESTS_PER_MINUTE", func(c *WordPressSecurityConfig) *int { return &c.LoginRateLimit.RequestsPerMinute }},
		{"LOGIN_RATE_LIMIT_BURST", func(c *WordPressSecurityConfig) *int { return &c.LoginRateLimit.Burst }},
	}
	wpSecurityStrings = []struct {
		name  string
		field func(*WordPressSecurityConfig) *string
	}{
		{"PASSWORD_HASHING", func(c *WordPressSecurityConfig) *string { return &c.PasswordHashing }},
		{"TWO_FACTOR", func(c *WordPressSecurityConfig) *string { return &c.TwoFactor }},
	}
)

// applyEnv overrides c with PRESSFLEET_WORDPRESS_SECURITY_* variables.
func (c *WordPressSecurityConfig) applyEnv(env LookupEnv) error {
	var errs []error
	for _, v := range wpSecurityBools {
		if val, ok := env(envWPSecurity + v.name); ok {
			b, err := strconv.ParseBool(val)
			if err != nil {
				errs = append(errs, fmt.Errorf("%s%s must be true or false, got %q", envWPSecurity, v.name, val))
				continue
			}
			*v.field(c) = b
		}
	}
	for _, v := range wpSecurityInts {
		if val, ok := env(envWPSecurity + v.name); ok {
			n, err := strconv.Atoi(val)
			if err != nil {
				errs = append(errs, fmt.Errorf("%s%s must be a whole number, got %q", envWPSecurity, v.name, val))
				continue
			}
			*v.field(c) = n
		}
	}
	for _, v := range wpSecurityStrings {
		if val, ok := env(envWPSecurity + v.name); ok {
			*v.field(c) = val
		}
	}
	return errors.Join(errs...)
}

// validate reports every problem with the security set. Numeric settings
// of a disabled feature are still checked, so turning it on later can't
// reveal a mistake made long before.
func (c WordPressSecurityConfig) validate() error {
	var errs []error
	if n := c.PasswordPolicy.MinLength; n < MinPasswordMinLength || n > MaxPasswordMinLength {
		errs = append(errs, fmt.Errorf("wordpress_security.password_policy.min_length must be between %d and %d, got %d",
			MinPasswordMinLength, MaxPasswordMinLength, n))
	}
	switch c.PasswordHashing {
	case PasswordHashingBcrypt, PasswordHashingArgon2id:
	default:
		errs = append(errs, fmt.Errorf("wordpress_security.password_hashing must be %q or %q, got %q",
			PasswordHashingBcrypt, PasswordHashingArgon2id, c.PasswordHashing))
	}
	if n := c.LoginRateLimit.RequestsPerMinute; n < 1 || n > MaxLoginRatePerMin {
		errs = append(errs, fmt.Errorf("wordpress_security.login_rate_limit.requests_per_minute must be between 1 and %d, got %d",
			MaxLoginRatePerMin, n))
	}
	if n := c.LoginRateLimit.Burst; n < 0 || n > MaxLoginBurst {
		errs = append(errs, fmt.Errorf("wordpress_security.login_rate_limit.burst must be between 0 and %d, got %d", MaxLoginBurst, n))
	}
	switch c.TwoFactor {
	case TwoFactorOff, TwoFactorOptional, TwoFactorRequiredForAdmins:
	default:
		errs = append(errs, fmt.Errorf("wordpress_security.two_factor must be %q, %q, or %q, got %q",
			TwoFactorOff, TwoFactorOptional, TwoFactorRequiredForAdmins, c.TwoFactor))
	}
	return errors.Join(errs...)
}
