package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/redact"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
)

// Reauthentication (design doc §6.2): a session proves again who its user
// is before sensitive actions, with the user's second factor, or with the
// password for users without one. Package authz decides when a session
// must; this is how it does.

// ReauthenticatePassword is the reauthentication method of users without a
// second factor. It is never a login's second step.
const ReauthenticatePassword FactorMethod = "password"

// ReauthenticationOptions are the ways a user can reauthenticate.
type ReauthenticationOptions struct {
	// Methods are the methods the user can use: their second factors in
	// factorOrder, or only ReauthenticatePassword for a user without one.
	Methods []FactorMethod
	// Challenges holds what a method needs the client to sign, keyed by
	// method, such as WebAuthn request options (FactorChallenger).
	Challenges map[FactorMethod]json.RawMessage
}

// ReauthenticationRequest is a fresh check of who a session's user is.
type ReauthenticationRequest struct {
	// Proof is the second factor, or for ReauthenticatePassword, the
	// password in Password.
	Proof    FactorProof
	Password redact.Secret
	ClientIP string
}

// Now returns the service's current time, which decides whether a
// session's last check is recent enough (authz.CheckReauthentication).
func (s *Service) Now() time.Time { return s.now() }

// StartReauthentication returns the ways the session's user can
// reauthenticate, starting the challenges of the methods that need one.
func (s *Service) StartReauthentication(ctx context.Context, p Principal) (ReauthenticationOptions, error) {
	if p.Method != MethodSession {
		return ReauthenticationOptions{}, ErrUnauthenticated
	}
	user, err := s.activeUser(ctx, p.UserID)
	if err != nil {
		return ReauthenticationOptions{}, err
	}
	has, err := store.NewUsers(s.db).HasSecondFactor(ctx, user.ID)
	if err != nil {
		return ReauthenticationOptions{}, fmt.Errorf("reauthenticate: %w", err)
	}
	if !has {
		return ReauthenticationOptions{Methods: []FactorMethod{ReauthenticatePassword}, Challenges: map[FactorMethod]json.RawMessage{}}, nil
	}
	methods, challenges, err := s.factorMethods(ctx, user)
	if err != nil {
		return ReauthenticationOptions{}, fmt.Errorf("reauthenticate: %w", err)
	}
	return ReauthenticationOptions{Methods: methods, Challenges: challenges}, nil
}

// Reauthenticate checks req for the session's user and, if it passes,
// records that the session has just proved who its user is (VerifiedAt),
// returning when. A user with a second factor must use it (any verifier
// registered in Options.Factors, or a recovery code); a user without one
// gives their password.
//
// Failures count against the per-account and per-address throttle of
// logins, so a stolen session allows no more guesses than the login page.
// It returns ErrFactorUnavailable if the user can't use the method, a
// *SecondFactorError for a wrong factor, ErrWrongPassword for a wrong
// password, a *ThrottledError after too many recent failures, and
// ErrUnauthenticated if the session or user is gone.
func (s *Service) Reauthenticate(ctx context.Context, p Principal, req ReauthenticationRequest) (time.Time, error) {
	if p.Method != MethodSession || p.SessionID == "" {
		return time.Time{}, ErrUnauthenticated
	}
	user, err := s.activeUser(ctx, p.UserID)
	if err != nil {
		return time.Time{}, err
	}
	keys := []throttleKey{accountKey(NormalizeEmail(user.Email)), addressKey(req.ClientIP)}
	if err := s.throttle.check(keys...); err != nil {
		return time.Time{}, err
	}
	has, err := store.NewUsers(s.db).HasSecondFactor(ctx, user.ID)
	if err != nil {
		return time.Time{}, fmt.Errorf("reauthenticate: %w", err)
	}
	method := req.Proof.Method
	switch {
	case !has && method != ReauthenticatePassword, has && method == ReauthenticatePassword:
		return time.Time{}, ErrFactorUnavailable
	case !has:
		ok, _, err := s.hasher.verify(ctx, user.PasswordHash, req.Password)
		if err != nil {
			return time.Time{}, fmt.Errorf("reauthenticate: verifying password of user %s: %w", user.ID, err)
		}
		if !ok {
			s.throttle.fail(keys...)
			s.log.Info("reauthentication failed", "user_id", user.ID, "method", string(method))
			return time.Time{}, ErrWrongPassword
		}
	default:
		v, ok := s.factors[method]
		if !ok {
			return time.Time{}, ErrFactorUnavailable
		}
		if enrolled, err := v.Enrolled(ctx, user.ID); err != nil {
			return time.Time{}, fmt.Errorf("reauthenticate: %w", err)
		} else if !enrolled {
			return time.Time{}, ErrFactorUnavailable
		}
		if err := v.Verify(ctx, user, req.Proof); errors.Is(err, ErrInvalidSecondFactor) {
			s.throttle.fail(keys...)
			s.log.Info("reauthentication failed", "user_id", user.ID, "method", string(method))
			return time.Time{}, &SecondFactorError{UserID: user.ID, Method: method}
		} else if err != nil {
			return time.Time{}, fmt.Errorf("reauthenticate: checking %s: %w", method, err)
		}
	}
	s.throttle.reset(keys[0])
	now := s.now()
	if err := store.NewSessions(s.db).SetVerified(ctx, p.SessionID, now); errors.Is(err, store.ErrNotFound) {
		return time.Time{}, ErrUnauthenticated
	} else if err != nil {
		return time.Time{}, fmt.Errorf("reauthenticate: %w", err)
	}
	s.log.Info("user reauthenticated", "user_id", user.ID, "session_id", p.SessionID, "method", string(method))
	return now, nil
}
