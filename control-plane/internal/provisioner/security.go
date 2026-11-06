package provisioner

import (
	"errors"
	"strconv"

	"github.com/rchouinard/pressfleet/control-plane/internal/config"
	"github.com/rchouinard/pressfleet/control-plane/internal/wpconfig"
)

// The install-wide WordPress security set (§6.1) reaches a site in two
// halves: constants in wp-config.php, and nginx's environment. Every driver
// renders both from the same configuration, so they live here.

// WordPressSecurity is the part of the security set that wp-config.php
// carries: DISALLOW_FILE_EDIT and the must-use plugin modules' constants.
func WordPressSecurity(s config.WordPressSecurityConfig) wpconfig.Security {
	sec := wpconfig.Security{
		DisallowFileEdit:            s.DisableFileEdit,
		BlockUserEnumeration:        s.BlockUserEnumeration,
		DisableApplicationPasswords: s.DisableApplicationPasswords,
		// Optional two-factor needs nothing here: the control plane just
		// installs the Two Factor plugin (package sites).
		RequireTwoFactor: s.TwoFactor == config.TwoFactorRequiredForAdmins,
	}
	if s.PasswordPolicy.Enabled {
		sec.PasswordMinLength = s.PasswordPolicy.MinLength
	}
	if s.PasswordHashing == config.PasswordHashingArgon2id {
		sec.PasswordHashing = wpconfig.HashArgon2id
	}
	return sec
}

// NginxSecurity is the nginx half of the security set, as the site-nginx
// image's environment variables (images/README.md).
type NginxSecurity struct {
	LoginRateLimit  bool
	LoginRate       string // NGINX_LOGIN_RATE, e.g. "10r/m"
	LoginBurst      int
	SecurityHeaders bool
}

// NginxSecuritySettings derives nginx's settings from the security set.
func NginxSecuritySettings(s config.WordPressSecurityConfig) (NginxSecurity, error) {
	n := NginxSecurity{LoginRateLimit: s.LoginRateLimit.Enabled, SecurityHeaders: s.Headers}
	if !n.LoginRateLimit {
		return n, nil
	}
	if s.LoginRateLimit.RequestsPerMinute < 1 || s.LoginRateLimit.Burst < 0 {
		return n, errors.New("security: the login rate limit needs a positive rate and a burst of at least 0")
	}
	n.LoginRate = strconv.Itoa(s.LoginRateLimit.RequestsPerMinute) + "r/m"
	n.LoginBurst = s.LoginRateLimit.Burst
	return n, nil
}
