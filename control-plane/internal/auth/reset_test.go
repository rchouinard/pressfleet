package auth_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/auth"
	"github.com/rchouinard/pressfleet/control-plane/internal/auth/authtest"
	"github.com/rchouinard/pressfleet/control-plane/internal/id"
	"github.com/rchouinard/pressfleet/control-plane/internal/redact"
	"github.com/rchouinard/pressfleet/control-plane/internal/secrets/secretstest"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
)

// newResetService returns a service with every factor, TOTP and WebAuthn
// included, under policy, and its clock, set 10 seconds into a TOTP step.
func newResetService(t *testing.T, db *store.DB, policy auth.TwoFactorPolicy) (*auth.Service, *clock) {
	t.Helper()
	c := &clock{t: time.Now().Truncate(30 * time.Second).Add(10 * time.Second)}
	return auth.NewService(db, auth.Options{
		Logger: storetest.Logger(), Now: c.Now, PasswordParams: cheap, TwoFactor: policy,
		RelyingParty: auth.RelyingPartyFor(rpID),
		Factors:      []auth.FactorVerifier{auth.NewTOTPVerifier(secretstest.Keyring(t, 1), rpID)},
	}), c
}

// enrollTOTPWithCodes is enrollTOTP for a user's first factor, also
// returning the recovery codes it issues.
func enrollTOTPWithCodes(t *testing.T, svc *auth.Service, clk *clock, userID string) (redact.Secret, []redact.Secret) {
	t.Helper()
	ctx := context.Background()
	e, err := svc.StartTOTPEnrollment(ctx, userID)
	if err != nil {
		t.Fatal(err)
	}
	codes, err := svc.ConfirmTOTP(ctx, userID, totpCode(e.Secret, clk))
	if err != nil || len(codes) != auth.RecoveryCodeCount {
		t.Fatalf("ConfirmTOTP = %d codes, %v", len(codes), err)
	}
	clk.Add(30 * time.Second)
	return e.Secret, codes
}

// countRows runs a COUNT query.
func countRows(t *testing.T, db *store.DB, query string, args ...any) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// refused reports whether err is a refused second factor: one the user
// doesn't have, or a wrong proof.
func refused(err error) bool {
	return errors.Is(err, auth.ErrFactorUnavailable) || errors.Is(err, auth.ErrInvalidSecondFactor)
}

// TestResetTwoFactor is the acceptance criterion: a reset removes every
// factor, the TOTP's sealed secret included, ends every session and keeps
// API tokens, and afterwards every old factor is refused, at a login
// started before the reset and after the user enrolls again.
func TestResetTwoFactor(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		svc, clk := newResetService(t, db, auth.TwoFactorPolicy{})
		u := mustCreate(t, svc, "v@example.com", store.RoleViewer)
		other := mustCreate(t, svc, "o@example.com", store.RoleViewer)
		otherSession := login(t, svc, "o@example.com")
		enroll(t, db, other.ID)

		if _, err := svc.ResetTwoFactor(ctx, id.New()); !errors.Is(err, auth.ErrUserNotFound) {
			t.Errorf("unknown user: %v", err)
		}
		if _, err := svc.ResetTwoFactor(ctx, u.ID); !errors.Is(err, auth.ErrNoSecondFactor) {
			t.Errorf("user without factors: %v", err)
		}

		secret, codes := enrollTOTPWithCodes(t, svc, clk, u.ID)
		oldKey := authtest.NewAuthenticator(rpOrigin)
		registerKey(t, svc, u.ID, oldKey, "old key")
		totpLogin := func(secret redact.Secret) auth.LoginResult {
			t.Helper()
			clk.Add(30 * time.Second)
			res, err := secondStep(svc, startLogin(t, svc, "v@example.com").Token, auth.FactorTOTP, totpCode(secret, clk).Reveal())
			if err != nil {
				t.Fatal(err)
			}
			return res
		}
		s1, s2 := totpLogin(secret), totpLogin(secret)
		_, tok, err := svc.CreateAPIToken(ctx, u.ID, auth.NewAPIToken{Name: "ci", Scope: store.ScopeFull})
		if err != nil {
			t.Fatal(err)
		}
		totp, err := store.NewUserTOTPs(db).Get(ctx, u.ID)
		if err != nil {
			t.Fatal(err)
		}
		// A login whose password step came before the reset.
		pending := startLogin(t, svc, "v@example.com")

		reset, err := svc.ResetTwoFactor(ctx, u.ID)
		if err != nil {
			t.Fatal(err)
		}
		if want := (auth.TwoFactorReset{TOTP: true, WebAuthnCredentials: 1, RecoveryCodes: auth.RecoveryCodeCount, Sessions: 2}); reset != want {
			t.Errorf("ResetTwoFactor = %+v, want %+v", reset, want)
		}
		for what, n := range map[string]int{
			"TOTP":                 countRows(t, db, "SELECT COUNT(*) FROM user_totp WHERE user_id = ?", u.ID),
			"TOTP secret":          countRows(t, db, "SELECT COUNT(*) FROM secrets WHERE id = ?", totp.SecretID),
			"WebAuthn credentials": countRows(t, db, "SELECT COUNT(*) FROM webauthn_credentials WHERE user_id = ?", u.ID),
			"recovery codes":       countRows(t, db, "SELECT COUNT(*) FROM recovery_codes WHERE user_id = ?", u.ID),
			"sessions":             countSessions(t, db, u.ID),
		} {
			if n != 0 {
				t.Errorf("%d %s left", n, what)
			}
		}
		for _, s := range []auth.LoginResult{s1, s2} {
			if _, err := svc.AuthenticateSession(ctx, s.Token); !errors.Is(err, auth.ErrUnauthenticated) {
				t.Errorf("session after reset: %v", err)
			}
		}
		if _, err := svc.AuthenticateToken(ctx, tok); err != nil {
			t.Errorf("API token after reset: %v", err)
		}
		if on, _ := svc.TwoFactorEnabled(ctx, u.ID); on {
			t.Error("TwoFactorEnabled after reset")
		}
		if ids, _ := svc.UsersWithTwoFactor(ctx); ids[u.ID] || !ids[other.ID] {
			t.Errorf("UsersWithTwoFactor = %v", ids)
		}
		// Other users keep their factors and sessions.
		if _, err := svc.AuthenticateSession(ctx, otherSession.Token); err != nil {
			t.Errorf("other user's session: %v", err)
		}
		if _, err := svc.ResetTwoFactor(ctx, u.ID); !errors.Is(err, auth.ErrNoSecondFactor) {
			t.Errorf("second reset: %v", err)
		}

		// The pending login can't finish with any old factor.
		clk.Add(30 * time.Second)
		if _, err := secondStep(svc, pending.Token, auth.FactorTOTP, totpCode(secret, clk).Reveal()); !refused(err) {
			t.Errorf("old TOTP code at a pending login: %v", err)
		}
		if _, err := secondStep(svc, pending.Token, auth.FactorRecoveryCode, codes[0].Reveal()); !refused(err) {
			t.Errorf("old recovery code at a pending login: %v", err)
		}
		assertion, err := oldKey.Assert(pending.Challenges[auth.FactorWebAuthn])
		if err != nil {
			t.Fatal(err)
		}
		if _, err := webAuthnStep(svc, pending, assertion); !refused(err) {
			t.Errorf("old WebAuthn assertion at a pending login: %v", err)
		}

		// Without a requirement, the password alone logs in.
		if res := login(t, svc, "v@example.com"); res.SecondFactor != "" {
			t.Errorf("login after reset used %q", res.SecondFactor)
		}

		// Enrolling again, with a new app and key, doesn't bring the old
		// factors back.
		newSecret, newCodes := enrollTOTPWithCodes(t, svc, clk, u.ID)
		registerKey(t, svc, u.ID, authtest.NewAuthenticator(rpOrigin), "new key")
		pending = startLogin(t, svc, "v@example.com")
		if _, err := secondStep(svc, pending.Token, auth.FactorTOTP, totpCode(secret, clk).Reveal()); !refused(err) {
			t.Errorf("old TOTP code after enrolling again: %v", err)
		}
		if _, err := secondStep(svc, pending.Token, auth.FactorRecoveryCode, codes[1].Reveal()); !refused(err) {
			t.Errorf("old recovery code after enrolling again: %v", err)
		}
		assertion, err = oldKey.Assert(withoutAllowList(t, pending.Challenges[auth.FactorWebAuthn]))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := webAuthnStep(svc, pending, assertion); !refused(err) {
			t.Errorf("old WebAuthn assertion after enrolling again: %v", err)
		}
		if res, err := secondStep(svc, pending.Token, auth.FactorTOTP, totpCode(newSecret, clk).Reveal()); err != nil || res.SecondFactor != auth.FactorTOTP {
			t.Errorf("new TOTP code = %+v, %v", res, err)
		}
		if _, err := secondStep(svc, startLogin(t, svc, "v@example.com").Token, auth.FactorRecoveryCode, newCodes[0].Reveal()); err != nil {
			t.Errorf("new recovery code: %v", err)
		}
	})
}

// TestResetTwoFactorUnderPolicy checks the reset of a user's last factor
// whatever the policy (it isn't ErrLastSecondFactor), and what their next
// login gets: a session restricted to enrolling when the policy covers
// them, and a full one otherwise.
func TestResetTwoFactorUnderPolicy(t *testing.T) {
	ctx := context.Background()
	for _, tt := range []struct {
		required auth.TwoFactorRequirement
		restrict bool
	}{
		{auth.RequireNone, false},
		{auth.RequireAdmins, false}, // the user is an Operator
		{auth.RequireAll, true},
	} {
		t.Run(string(tt.required), func(t *testing.T) {
			storetest.Run(t, func(t *testing.T, db *store.DB) {
				svc, clk := newResetService(t, db, auth.TwoFactorPolicy{Required: tt.required})
				u := mustCreate(t, svc, "op@example.com", store.RoleOperator)
				enrollTOTPWithCodes(t, svc, clk, u.ID)

				if _, err := svc.ResetTwoFactor(ctx, u.ID); err != nil {
					t.Fatalf("ResetTwoFactor = %v", err)
				}
				res := login(t, svc, "op@example.com")
				p, err := svc.AuthenticateSession(ctx, res.Token)
				if err != nil {
					t.Fatal(err)
				}
				if p.EnrollmentRequired != tt.restrict {
					t.Errorf("EnrollmentRequired = %v, want %v", p.EnrollmentRequired, tt.restrict)
				}
			})
		})
	}
}
