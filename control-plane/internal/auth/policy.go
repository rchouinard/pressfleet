package auth

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/store"
)

// TwoFactorRequirement says whose roles must have a second factor
// (two_factor.required, design doc §6.2).
type TwoFactorRequirement string

// Values of TwoFactorRequirement, as in the configuration.
const (
	RequireNone   TwoFactorRequirement = "off"
	RequireAdmins TwoFactorRequirement = "admins"
	RequireAll    TwoFactorRequirement = "all"
)

// TwoFactorPolicy is the platform's dashboard two-factor policy (the
// two_factor settings, design doc §6.2) as auth and authz apply it. The
// zero value requires nothing and asks for no reauthentication, so tests
// that don't set it see the behavior from before two-factor; the control
// plane always sets it from its configuration.
type TwoFactorPolicy struct {
	// Required is whose roles must have a second factor. Empty means
	// RequireNone.
	Required TwoFactorRequirement
	// Reauthentication says whether sensitive actions from a session need
	// a recent factor check.
	Reauthentication bool
	// ReauthenticationMaxAge is how long a check counts as recent.
	ReauthenticationMaxAge time.Duration
}

// Covers reports whether the policy requires a user with role to have a
// second factor. It is decided on every request from the user's current
// role, so a promotion to Admin under RequireAdmins applies at once.
func (p TwoFactorPolicy) Covers(role store.Role) bool {
	switch p.Required {
	case RequireAll:
		return true
	case RequireAdmins:
		return role == store.RoleAdmin
	}
	return false
}

// ErrLastSecondFactor means removing a second factor would leave a user
// whom the policy requires to have one without any. It isn't
// ErrSecondFactorRequired, which is a login waiting for its second step.
var ErrLastSecondFactor = errors.New("the account must keep at least one second factor")

// TwoFactorPolicy returns the policy the service applies.
func (s *Service) TwoFactorPolicy() TwoFactorPolicy { return s.twoFactor }

// ensureSecondFactorKept returns ErrLastSecondFactor if user must have a
// second factor under the policy and has none left. Every path that
// removes a user's own factor (TOTP, a WebAuthn credential) calls it in the
// same transaction, after the removal, so the removal rolls back when it
// fails. Recovery codes and credentials from another relying party ID
// don't count as a factor (Service.hasSecondFactor).
// Admin resets (#155) don't call it: they remove everything on purpose, and
// the user must enroll again at their next login.
func (s *Service) ensureSecondFactorKept(ctx context.Context, q store.Querier, user store.User) error {
	if !s.twoFactor.Covers(user.Role) {
		return nil
	}
	ok, err := s.hasSecondFactor(ctx, q, user.ID)
	if err != nil {
		return fmt.Errorf("check second factors of user %s: %w", user.ID, err)
	}
	if !ok {
		return ErrLastSecondFactor
	}
	return nil
}
