package auth

import (
	"context"
	"errors"
	"fmt"

	"github.com/rchouinard/pressfleet/control-plane/internal/store"
)

// TwoFactorReset says what ResetTwoFactor removed. It holds counts only,
// so it is safe to log, audit, and print.
type TwoFactorReset struct {
	// TOTP is whether the user had an authenticator app, confirmed or
	// still being enrolled.
	TOTP bool
	// WebAuthnCredentials is how many security keys and passkeys were
	// removed.
	WebAuthnCredentials int
	// RecoveryCodes is how many recovery codes were removed, used ones
	// included.
	RecoveryCodes int
	// Sessions is how many sessions were ended.
	Sessions int
}

// ResetTwoFactor removes all of a user's second factors, for a user who
// lost theirs (design doc §6.2): their TOTP and its sealed secret, every
// WebAuthn credential, and every recovery code. It also ends all of their
// sessions, so a session opened with a lost factor ends with it; API
// tokens are kept, as when a user removes a factor themselves. It is one
// transaction, so a failure removes nothing.
//
// Unlike removing one's own factors, it doesn't keep a last factor that
// the policy requires (ensureSecondFactorKept): removing everything is the
// point, and a user the policy covers is restricted to enrolling again at
// their next login. It applies to disabled users too, so they can be
// re-enabled afterwards.
//
// It returns an error wrapping ErrUserNotFound for an unknown user, and
// ErrNoSecondFactor, changing nothing, if the user has no TOTP, WebAuthn
// credential, or recovery code to remove. Callers decide who may reset
// whom: the API refuses an Admin's own account (authz), and the command
// line, the recovery path for a locked-out Admin, allows any user.
func (s *Service) ResetTwoFactor(ctx context.Context, userID string) (TwoFactorReset, error) {
	var reset TwoFactorReset
	err := s.db.InTx(ctx, func(q store.Querier) error {
		if _, err := store.NewUsers(q).Get(ctx, userID); err != nil {
			return err
		}
		switch err := store.NewUserTOTPs(q).Delete(ctx, userID); {
		case err == nil:
			reset.TOTP = true
		case !errors.Is(err, store.ErrNotFound):
			return err
		}
		n, err := store.NewWebAuthnCredentials(q).DeleteAllForUser(ctx, userID)
		if err != nil {
			return err
		}
		reset.WebAuthnCredentials = int(n)
		n, err = store.NewRecoveryCodes(q).DeleteAllForUser(ctx, userID)
		if err != nil {
			return err
		}
		reset.RecoveryCodes = int(n)
		if !reset.TOTP && reset.WebAuthnCredentials == 0 && reset.RecoveryCodes == 0 {
			return ErrNoSecondFactor
		}
		// The WebAuthn user handle stays. It is an identifier, not a
		// credential: nothing logs in with it once the credentials are
		// gone. Keeping it means a key or passkey the user registers again
		// on the same authenticator replaces the stale discoverable
		// credential stored there, instead of leaving a second one for
		// the account behind.
		n, err = store.NewSessions(q).DeleteForUser(ctx, userID, "")
		reset.Sessions = int(n)
		return err
	})
	switch {
	case errors.Is(err, store.ErrNotFound):
		return TwoFactorReset{}, ErrUserNotFound
	case errors.Is(err, ErrNoSecondFactor):
		return TwoFactorReset{}, ErrNoSecondFactor
	case err != nil:
		return TwoFactorReset{}, fmt.Errorf("reset two-factor: %w", err)
	}
	s.log.Info("two-factor reset", "user_id", userID, "totp", reset.TOTP,
		"webauthn_credentials", reset.WebAuthnCredentials, "recovery_codes", reset.RecoveryCodes, "sessions", reset.Sessions)
	return reset, nil
}
