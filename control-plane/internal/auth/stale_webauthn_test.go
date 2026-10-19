package auth_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/auth"
	"github.com/rchouinard/pressfleet/control-plane/internal/auth/authtest"
	"github.com/rchouinard/pressfleet/control-plane/internal/secrets/secretstest"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
)

// Credentials registered before the dashboard domain changed are bound to
// another relying party ID and can't be used, so they aren't a second
// factor anywhere the question is asked (#157).

// newStaleService is a service for rpID with real TOTP and WebAuthn
// verifiers.
func newStaleService(t *testing.T, db *store.DB, policy auth.TwoFactorPolicy) (*auth.Service, *clock) {
	t.Helper()
	c := &clock{t: time.Now().Truncate(30 * time.Second).Add(10 * time.Second)}
	return auth.NewService(db, auth.Options{
		Logger: storetest.Logger(), Now: c.Now, PasswordParams: cheap, TwoFactor: policy,
		Factors:      []auth.FactorVerifier{auth.NewTOTPVerifier(secretstest.Keyring(t, 1), rpID)},
		RelyingParty: auth.RelyingPartyFor(rpID),
	}), c
}

// addStaleKey gives the user a credential registered under a previous
// dashboard domain and returns it.
func addStaleKey(t *testing.T, db *store.DB, userID string) store.WebAuthnCredential {
	t.Helper()
	c := store.WebAuthnCredential{
		UserID: userID, CredentialID: []byte("stale-" + userID), PublicKey: []byte("key"), Algorithm: -7,
		AAGUID: make([]byte, 16), RPID: "old.example.com", Name: "Old key",
	}
	if err := store.NewWebAuthnCredentials(db).Create(context.Background(), &c); err != nil {
		t.Fatal(err)
	}
	return c
}

// checkTwoFactorEnabled checks TwoFactorEnabled and UsersWithTwoFactor
// (two_factor_enabled in User and /auth/me) for the user.
func checkTwoFactorEnabled(t *testing.T, svc *auth.Service, userID string, want bool) {
	t.Helper()
	ctx := context.Background()
	if on, err := svc.TwoFactorEnabled(ctx, userID); err != nil || on != want {
		t.Errorf("TwoFactorEnabled = %v, %v; want %v", on, err, want)
	}
	if ids, err := svc.UsersWithTwoFactor(ctx); err != nil || ids[userID] != want {
		t.Errorf("UsersWithTwoFactor = %v, %v; want %s: %v", ids, err, userID, want)
	}
}

func TestStaleWebAuthnWithoutPolicy(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		ctx := context.Background()
		svc, _ := newStaleService(t, db, auth.TwoFactorPolicy{})
		u := mustCreate(t, svc, "a@example.com", store.RoleAdmin)
		stale := addStaleKey(t, db, u.ID)
		if _, err := store.NewRecoveryCodes(db).Replace(ctx, u.ID, []string{"h1"}, time.Now()); err != nil {
			t.Fatal(err)
		}

		checkTwoFactorEnabled(t, svc, u.ID, false)
		// The password alone logs in, unrestricted.
		res := login(t, svc, "a@example.com")
		p, err := svc.AuthenticateSession(ctx, res.Token)
		if err != nil || p.EnrollmentRequired {
			t.Errorf("session = %+v, %v; want unrestricted", p, err)
		}
		// Reauthentication takes the password.
		opts, err := svc.StartReauthentication(ctx, p)
		if err != nil || !slices.Equal(opts.Methods, []auth.FactorMethod{auth.ReauthenticatePassword}) {
			t.Errorf("reauthentication methods = %v, %v; want password", opts.Methods, err)
		}
		// Recovery codes stand in for a factor the user doesn't have.
		if _, err := svc.RegenerateRecoveryCodes(ctx, u.ID); !errors.Is(err, auth.ErrNoSecondFactor) {
			t.Errorf("RegenerateRecoveryCodes = %v, want ErrNoSecondFactor", err)
		}
		// The stale key stays listed, so it can be removed.
		creds, err := svc.ListWebAuthnCredentials(ctx, u.ID)
		if err != nil || len(creds) != 1 || creds[0].ID != stale.ID || creds[0].RPID != "old.example.com" {
			t.Errorf("ListWebAuthnCredentials = %+v, %v; want the stale key", creds, err)
		}
		if err := svc.RemoveWebAuthnCredential(ctx, p, stale.ID); err != nil {
			t.Errorf("removing the stale key = %v", err)
		}

		// A key for the current domain is a factor again, and its
		// registration hands out recovery codes as for a first factor.
		addStaleKey(t, db, u.ID)
		a := authtest.NewAuthenticator(rpOrigin)
		if reg := registerKey(t, svc, u.ID, a, "New key"); len(reg.RecoveryCodes) == 0 {
			t.Error("registering the first usable key returned no recovery codes")
		}
		checkTwoFactorEnabled(t, svc, u.ID, true)
		webAuthnLogin(t, svc, "a@example.com", a)
		if _, err := svc.RegenerateRecoveryCodes(ctx, u.ID); err != nil {
			t.Errorf("RegenerateRecoveryCodes with a usable key = %v", err)
		}
	})
}

func TestStaleWebAuthnUnderPolicy(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		ctx := context.Background()
		svc, _ := newStaleService(t, db, auth.TwoFactorPolicy{Required: auth.RequireAdmins})
		admin := mustCreate(t, svc, "admin@example.com", store.RoleAdmin)
		operator := mustCreate(t, svc, "operator@example.com", store.RoleOperator)
		addStaleKey(t, db, admin.ID)
		addStaleKey(t, db, operator.ID)

		// The covered Admin is restricted to enrollment.
		session := login(t, svc, "admin@example.com").Token
		p, err := svc.AuthenticateSession(ctx, session)
		if err != nil || !p.EnrollmentRequired {
			t.Errorf("admin session = %+v, %v; want restricted to enrollment", p, err)
		}
		checkTwoFactorEnabled(t, svc, admin.ID, false)
		// The Operator isn't covered: a one-step, unrestricted login.
		q, err := svc.AuthenticateSession(ctx, login(t, svc, "operator@example.com").Token)
		if err != nil || q.EnrollmentRequired {
			t.Errorf("operator session = %+v, %v; want unrestricted", q, err)
		}

		// Registering a key for the current domain lifts the restriction
		// and makes the next login two steps.
		a := authtest.NewAuthenticator(rpOrigin)
		registerKey(t, svc, admin.ID, a, "New key")
		if p, err = svc.AuthenticateSession(ctx, session); err != nil || p.EnrollmentRequired {
			t.Errorf("admin session after registering = %+v, %v; want unrestricted", p, err)
		}
		webAuthnLogin(t, svc, "admin@example.com", a)
	})
}

func TestStaleWebAuthnDoesNotKeepLastFactor(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		ctx := context.Background()
		svc, clk := newStaleService(t, db, auth.TwoFactorPolicy{Required: auth.RequireAll})

		// A TOTP beside a stale key is the last usable factor.
		u := mustCreate(t, svc, "a@example.com", store.RoleViewer)
		secret := enrollTOTP(t, svc, clk, u.ID)
		stale := addStaleKey(t, db, u.ID)
		checkTwoFactorEnabled(t, svc, u.ID, true)
		res, err := secondStep(svc, startLogin(t, svc, "a@example.com").Token, auth.FactorTOTP, totpCode(secret, clk).Reveal())
		if err != nil {
			t.Fatal(err)
		}
		p, err := svc.AuthenticateSession(ctx, res.Token)
		if err != nil || p.EnrollmentRequired {
			t.Fatalf("session = %+v, %v; want unrestricted", p, err)
		}
		if err := svc.RemoveTOTP(ctx, p); !errors.Is(err, auth.ErrLastSecondFactor) {
			t.Errorf("removing the TOTP beside a stale key = %v, want ErrLastSecondFactor", err)
		}
		// The stale key itself can go.
		if err := svc.RemoveWebAuthnCredential(ctx, p, stale.ID); err != nil {
			t.Errorf("removing the stale key = %v", err)
		}

		// So is a key for the current domain beside a stale one.
		v := mustCreate(t, svc, "b@example.com", store.RoleViewer)
		addStaleKey(t, db, v.ID)
		a := authtest.NewAuthenticator(rpOrigin)
		current := registerKey(t, svc, v.ID, a, "New key").Credential
		checkTwoFactorEnabled(t, svc, v.ID, true)
		vp, err := svc.AuthenticateSession(ctx, webAuthnLogin(t, svc, "b@example.com", a).Token)
		if err != nil {
			t.Fatal(err)
		}
		if err := svc.RemoveWebAuthnCredential(ctx, vp, current.ID); !errors.Is(err, auth.ErrLastSecondFactor) {
			t.Errorf("removing the current key beside a stale one = %v, want ErrLastSecondFactor", err)
		}
	})
}
