package auth_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/auth"
	"github.com/rchouinard/pressfleet/control-plane/internal/auth/authtest"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
)

const (
	rpID     = "pf.example.com"
	rpOrigin = "https://pf.example.com"
)

func newWebAuthnService(t *testing.T, db *store.DB, policy auth.TwoFactorPolicy) (*auth.Service, *clock) {
	t.Helper()
	c := &clock{t: time.Now()}
	return auth.NewService(db, auth.Options{
		Logger: storetest.Logger(), Now: c.Now, PasswordParams: cheap,
		TwoFactor: policy, RelyingParty: auth.RelyingPartyFor(rpID),
	}), c
}

// registerKey registers a's credential for the user.
func registerKey(t *testing.T, svc *auth.Service, userID string, a *authtest.Authenticator, name string) auth.WebAuthnRegistration {
	t.Helper()
	ctx := context.Background()
	opts, err := svc.BeginWebAuthnRegistration(ctx, userID)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := a.Register(opts)
	if err != nil {
		t.Fatal(err)
	}
	reg, err := svc.FinishWebAuthnRegistration(ctx, userID, name, resp)
	if err != nil {
		t.Fatalf("FinishWebAuthnRegistration = %v", err)
	}
	return reg
}

// webAuthnStep passes the second step of a pending login with an
// assertion.
func webAuthnStep(svc *auth.Service, pending *auth.SecondFactorRequiredError, assertion json.RawMessage) (auth.LoginResult, error) {
	return svc.LoginSecondFactor(context.Background(), auth.SecondFactorRequest{
		PendingToken: pending.Token, ClientIP: "192.0.2.1",
		Proof: auth.FactorProof{Method: auth.FactorWebAuthn, Credential: assertion},
	})
}

// webAuthnLogin logs in with the password and a's assertion.
func webAuthnLogin(t *testing.T, svc *auth.Service, email string, a *authtest.Authenticator) auth.LoginResult {
	t.Helper()
	pending := startLogin(t, svc, email)
	assertion, err := a.Assert(pending.Challenges[auth.FactorWebAuthn])
	if err != nil {
		t.Fatal(err)
	}
	res, err := webAuthnStep(svc, pending, assertion)
	if err != nil {
		t.Fatalf("LoginSecondFactor = %v", err)
	}
	return res
}

func TestWebAuthnRegistrationOptions(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		ctx := context.Background()
		svc, _ := newWebAuthnService(t, db, auth.TwoFactorPolicy{})
		u := mustCreate(t, svc, "a@example.com", store.RoleViewer)
		first := authtest.NewAuthenticator(rpOrigin)
		registerKey(t, svc, u.ID, first, "Key")

		raw, err := svc.BeginWebAuthnRegistration(ctx, u.ID)
		if err != nil {
			t.Fatal(err)
		}
		var opts struct {
			RP struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"rp"`
			User struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"user"`
			Challenge              string `json:"challenge"`
			Attestation            string `json:"attestation"`
			AuthenticatorSelection struct {
				ResidentKey      string `json:"residentKey"`
				UserVerification string `json:"userVerification"`
			} `json:"authenticatorSelection"`
			ExcludeCredentials []struct {
				ID string `json:"id"`
			} `json:"excludeCredentials"`
		}
		if err := json.Unmarshal(raw, &opts); err != nil {
			t.Fatal(err)
		}
		handle, err := base64.RawURLEncoding.DecodeString(opts.User.ID)
		if err != nil || len(handle) != store.WebAuthnUserHandleSize || string(handle) != string(first.UserHandle()) {
			t.Errorf("user.id = %q (%v), want the 64-byte handle of the first registration", opts.User.ID, err)
		}
		if opts.RP.ID != rpID || opts.RP.Name != "PressFleet" || opts.User.Name != "a@example.com" || opts.Challenge == "" {
			t.Errorf("options: %s", raw)
		}
		if opts.Attestation != "none" || opts.AuthenticatorSelection.ResidentKey != "preferred" ||
			opts.AuthenticatorSelection.UserVerification != "preferred" {
			t.Errorf("attestation and authenticator selection: %s", raw)
		}
		if len(opts.ExcludeCredentials) != 1 || opts.ExcludeCredentials[0].ID != base64.RawURLEncoding.EncodeToString(first.CredentialID()) {
			t.Errorf("excludeCredentials = %+v, want the registered credential", opts.ExcludeCredentials)
		}
	})
}

func TestWebAuthnRegistration(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		ctx := context.Background()
		svc, _ := newWebAuthnService(t, db, auth.TwoFactorPolicy{})
		u := mustCreate(t, svc, "a@example.com", store.RoleViewer)

		a := authtest.NewAuthenticator(rpOrigin)
		a.BackupEligible, a.BackupState, a.UserVerified = true, true, false
		a.Transports = []string{"internal", "hybrid"}
		reg := registerKey(t, svc, u.ID, a, "  Laptop  ")
		c := reg.Credential
		if c.ID == "" || c.UserID != u.ID || c.Name != "Laptop" || c.RPID != rpID || c.Algorithm != -7 ||
			string(c.CredentialID) != string(a.CredentialID()) || c.UserVerified || !c.BackupEligible || !c.BackupState ||
			len(c.Transports) != 2 || c.Transports[0] != "internal" || string(c.AAGUID) != string(a.AAGUID[:]) {
			t.Errorf("credential = %+v", c)
		}
		if len(reg.RecoveryCodes) != auth.RecoveryCodeCount {
			t.Errorf("first factor issued %d recovery codes, want %d", len(reg.RecoveryCodes), auth.RecoveryCodeCount)
		}
		if on, err := svc.TwoFactorEnabled(ctx, u.ID); err != nil || !on {
			t.Errorf("TwoFactorEnabled = %v, %v; want true", on, err)
		}
		stored, err := svc.GetWebAuthnCredential(ctx, u.ID, c.ID)
		if err != nil || stored.Name != "Laptop" || !stored.BackupEligible {
			t.Errorf("GetWebAuthnCredential = %+v, %v", stored, err)
		}

		// A second key isn't the first factor: the codes stay as they are.
		second := registerKey(t, svc, u.ID, authtest.NewAuthenticator(rpOrigin), "YubiKey")
		if second.RecoveryCodes != nil || !second.Credential.UserVerified {
			t.Errorf("second registration = %+v, want a verified credential and no recovery codes", second)
		}
		if n, err := svc.RecoveryCodesRemaining(ctx, u.ID); err != nil || n != auth.RecoveryCodeCount {
			t.Errorf("RecoveryCodesRemaining = %d, %v", n, err)
		}
		creds, err := svc.ListWebAuthnCredentials(ctx, u.ID)
		if err != nil || len(creds) != 2 {
			t.Errorf("ListWebAuthnCredentials = %d credentials, %v; want 2", len(creds), err)
		}
	})
}

func TestWebAuthnDuplicateRegistration(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		ctx := context.Background()
		svc, _ := newWebAuthnService(t, db, auth.TwoFactorPolicy{})
		u := mustCreate(t, svc, "a@example.com", store.RoleViewer)
		a := authtest.NewAuthenticator(rpOrigin)
		registerKey(t, svc, u.ID, a, "Key")

		// The options exclude the registered credential, so a browser
		// refuses to register it again.
		opts, err := svc.BeginWebAuthnRegistration(ctx, u.ID)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := a.Register(opts); !errors.Is(err, authtest.ErrExcluded) {
			t.Fatalf("Register = %v, want ErrExcluded", err)
		}
		// A client that ignores the exclusion is refused by the server, for
		// this user or another.
		for _, userID := range []string{u.ID, mustCreate(t, svc, "b@example.com", store.RoleViewer).ID} {
			opts, err := svc.BeginWebAuthnRegistration(ctx, userID)
			if err != nil {
				t.Fatal(err)
			}
			resp, err := a.Register(withoutExclusions(t, opts))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := svc.FinishWebAuthnRegistration(ctx, userID, "Again", resp); !errors.Is(err, auth.ErrWebAuthnCredentialExists) {
				t.Errorf("registering again = %v, want ErrWebAuthnCredentialExists", err)
			}
		}
	})
}

func withoutExclusions(t *testing.T, opts json.RawMessage) json.RawMessage {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(opts, &m); err != nil {
		t.Fatal(err)
	}
	delete(m, "excludeCredentials")
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestWebAuthnRegistrationRefused(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		ctx := context.Background()
		svc, clk := newWebAuthnService(t, db, auth.TwoFactorPolicy{})
		u := mustCreate(t, svc, "a@example.com", store.RoleViewer)
		other := mustCreate(t, svc, "b@example.com", store.RoleViewer)

		tests := []struct {
			name string
			// change prepares the authenticator, and may answer options
			// that aren't the user's own.
			change func(a *authtest.Authenticator) (optionsFor string)
			// after runs between the response and its check.
			after func()
		}{
			{name: "wrong origin", change: func(a *authtest.Authenticator) string { a.Origin = "https://evil.example.com"; return u.ID }},
			{name: "wrong RP ID", change: func(a *authtest.Authenticator) string { a.RPID = "evil.example.com"; return u.ID }},
			{name: "no user presence", change: func(a *authtest.Authenticator) string { a.UserPresent = false; return u.ID }},
			{name: "unknown challenge", change: func(a *authtest.Authenticator) string { a.Challenge = "bm90LWEtY2hhbGxlbmdlLWZyb20tdXM"; return u.ID }},
			{name: "another user's challenge", change: func(*authtest.Authenticator) string { return other.ID }},
			{name: "expired challenge", change: func(*authtest.Authenticator) string { return u.ID }, after: func() { clk.Add(5*time.Minute + time.Second) }},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				a := authtest.NewAuthenticator(rpOrigin)
				optsUser := tt.change(a)
				opts, err := svc.BeginWebAuthnRegistration(ctx, optsUser)
				if err != nil {
					t.Fatal(err)
				}
				resp, err := a.Register(opts)
				if err != nil {
					t.Fatal(err)
				}
				if tt.after != nil {
					tt.after()
				}
				if _, err := svc.FinishWebAuthnRegistration(ctx, u.ID, "Key", resp); !errors.Is(err, auth.ErrInvalidWebAuthnResponse) {
					t.Errorf("FinishWebAuthnRegistration = %v, want ErrInvalidWebAuthnResponse", err)
				}
			})
		}

		t.Run("replayed challenge", func(t *testing.T) {
			opts, err := svc.BeginWebAuthnRegistration(ctx, u.ID)
			if err != nil {
				t.Fatal(err)
			}
			resp, err := authtest.NewAuthenticator(rpOrigin).Register(opts)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := svc.FinishWebAuthnRegistration(ctx, u.ID, "Key", resp); err != nil {
				t.Fatal(err)
			}
			// Another authenticator answering the same challenge.
			again, err := authtest.NewAuthenticator(rpOrigin).Register(opts)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := svc.FinishWebAuthnRegistration(ctx, u.ID, "Key", again); !errors.Is(err, auth.ErrInvalidWebAuthnResponse) {
				t.Errorf("replayed registration = %v, want ErrInvalidWebAuthnResponse", err)
			}
		})

		t.Run("garbage", func(t *testing.T) {
			if _, err := svc.FinishWebAuthnRegistration(ctx, u.ID, "Key", json.RawMessage(`{"id":"x"}`)); !errors.Is(err, auth.ErrInvalidWebAuthnResponse) {
				t.Errorf("FinishWebAuthnRegistration = %v, want ErrInvalidWebAuthnResponse", err)
			}
		})

		t.Run("bad name", func(t *testing.T) {
			for _, name := range []string{"", "   ", "a\nb", strings.Repeat("x", 101)} {
				if _, err := svc.FinishWebAuthnRegistration(ctx, u.ID, name, nil); !errors.Is(err, auth.ErrInvalidWebAuthnCredentialName) {
					t.Errorf("name %q: %v, want ErrInvalidWebAuthnCredentialName", name, err)
				}
			}
		})
	})
}

func TestWebAuthnLogin(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		ctx := context.Background()
		svc, clk := newWebAuthnService(t, db, auth.TwoFactorPolicy{})
		u := mustCreate(t, svc, "a@example.com", store.RoleViewer)
		a := authtest.NewAuthenticator(rpOrigin)
		reg := registerKey(t, svc, u.ID, a, "Key")

		pending := startLogin(t, svc, "a@example.com")
		if len(pending.Methods) != 2 || pending.Methods[0] != auth.FactorWebAuthn || pending.Methods[1] != auth.FactorRecoveryCode {
			t.Errorf("methods = %v, want webauthn and recovery_code", pending.Methods)
		}
		var opts struct {
			Challenge        string `json:"challenge"`
			RPID             string `json:"rpId"`
			UserVerification string `json:"userVerification"`
			AllowCredentials []struct {
				ID string `json:"id"`
			} `json:"allowCredentials"`
		}
		if err := json.Unmarshal(pending.Challenges[auth.FactorWebAuthn], &opts); err != nil {
			t.Fatal(err)
		}
		if opts.Challenge == "" || opts.RPID != rpID || opts.UserVerification != "preferred" || len(opts.AllowCredentials) != 1 {
			t.Errorf("request options: %s", pending.Challenges[auth.FactorWebAuthn])
		}

		clk.Add(time.Minute)
		assertion, err := a.Assert(pending.Challenges[auth.FactorWebAuthn])
		if err != nil {
			t.Fatal(err)
		}
		res, err := webAuthnStep(svc, pending, assertion)
		if err != nil {
			t.Fatalf("LoginSecondFactor = %v", err)
		}
		if res.SecondFactor != auth.FactorWebAuthn || res.FactorDetails["webauthn_credential_id"] != reg.Credential.ID ||
			res.FactorDetails["webauthn_sign_count"] != "" {
			t.Errorf("login result: factor %q, details %v", res.SecondFactor, res.FactorDetails)
		}
		c, err := svc.GetWebAuthnCredential(ctx, u.ID, reg.Credential.ID)
		if err != nil || c.SignCount != 1 || c.LastUsedAt == nil || !c.LastUsedAt.Equal(clk.Now().UTC().Truncate(time.Microsecond)) {
			t.Errorf("after login: %+v, %v; want sign count 1 and last used now", c, err)
		}

		// The assertion's challenge is used up: replaying it on a new
		// login fails.
		next := startLogin(t, svc, "a@example.com")
		if _, err := webAuthnStep(svc, next, assertion); !errors.Is(err, auth.ErrInvalidSecondFactor) {
			t.Errorf("replayed assertion = %v, want ErrInvalidSecondFactor", err)
		}
		// The login's own challenge still works after the failure.
		assertion, err = a.Assert(next.Challenges[auth.FactorWebAuthn])
		if err != nil {
			t.Fatal(err)
		}
		if _, err := webAuthnStep(svc, next, assertion); err != nil {
			t.Errorf("LoginSecondFactor after a failure = %v", err)
		}
	})
}

func TestWebAuthnLoginRefused(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		svc, clk := newWebAuthnService(t, db, auth.TwoFactorPolicy{})
		u := mustCreate(t, svc, "a@example.com", store.RoleViewer)
		a := authtest.NewAuthenticator(rpOrigin)
		registerKey(t, svc, u.ID, a, "Key")
		// Another user's key, registered to them.
		mustCreate(t, svc, "b@example.com", store.RoleViewer)
		b := authtest.NewAuthenticator(rpOrigin)
		bUser, err := svc.GetUserByEmail(context.Background(), "b@example.com")
		if err != nil {
			t.Fatal(err)
		}
		registerKey(t, svc, bUser.ID, b, "B's key")

		tests := []struct {
			name   string
			assert func(pending *auth.SecondFactorRequiredError) (json.RawMessage, error)
		}{
			{"wrong origin", func(p *auth.SecondFactorRequiredError) (json.RawMessage, error) {
				a.Origin = "https://evil.example.com"
				defer func() { a.Origin = rpOrigin }()
				return a.Assert(p.Challenges[auth.FactorWebAuthn])
			}},
			{"wrong RP ID", func(p *auth.SecondFactorRequiredError) (json.RawMessage, error) {
				a.RPID = "evil.example.com"
				defer func() { a.RPID = "" }()
				return a.Assert(p.Challenges[auth.FactorWebAuthn])
			}},
			{"no user presence", func(p *auth.SecondFactorRequiredError) (json.RawMessage, error) {
				a.UserPresent = false
				defer func() { a.UserPresent = true }()
				return a.Assert(p.Challenges[auth.FactorWebAuthn])
			}},
			{"another user's credential", func(p *auth.SecondFactorRequiredError) (json.RawMessage, error) {
				return b.Assert(withoutAllowList(t, p.Challenges[auth.FactorWebAuthn]))
			}},
			{"garbage", func(*auth.SecondFactorRequiredError) (json.RawMessage, error) {
				return json.RawMessage(`{"id":"eA","rawId":"eA","type":"public-key"}`), nil
			}},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				// Each case fails once; a new throttle window keeps the
				// failures from adding up to a lockout.
				clk.Add(16 * time.Minute)
				pending := startLogin(t, svc, "a@example.com")
				assertion, err := tt.assert(pending)
				if err != nil {
					t.Fatal(err)
				}
				_, err = webAuthnStep(svc, pending, assertion)
				var failed *auth.SecondFactorError
				if !errors.As(err, &failed) || failed.Method != auth.FactorWebAuthn {
					t.Errorf("LoginSecondFactor = %v, want a WebAuthn SecondFactorError", err)
				}
			})
		}

		t.Run("expired challenge", func(t *testing.T) {
			pending := startLogin(t, svc, "a@example.com")
			assertion, err := a.Assert(pending.Challenges[auth.FactorWebAuthn])
			if err != nil {
				t.Fatal(err)
			}
			clk.Add(5*time.Minute + time.Second)
			if _, err := webAuthnStep(svc, pending, assertion); !errors.Is(err, auth.ErrLoginExpired) {
				t.Errorf("LoginSecondFactor = %v, want ErrLoginExpired", err)
			}
		})
	})
}

func withoutAllowList(t *testing.T, opts json.RawMessage) json.RawMessage {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(opts, &m); err != nil {
		t.Fatal(err)
	}
	delete(m, "allowCredentials")
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestWebAuthnChallengeExpiresOnItsOwn(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		svc, clk := newWebAuthnService(t, db, auth.TwoFactorPolicy{})
		u := mustCreate(t, svc, "a@example.com", store.RoleViewer)
		a := authtest.NewAuthenticator(rpOrigin)
		registerKey(t, svc, u.ID, a, "Key")

		// A WebAuthn check outside a login (reauthentication, #154) goes
		// through the same verifier; its challenge expires on its own, and
		// a failed attempt, which puts it back, doesn't extend it.
		pending := startLogin(t, svc, "a@example.com")
		clk.Add(4 * time.Minute)
		a.Origin = "https://evil.example.com"
		bad, err := a.Assert(pending.Challenges[auth.FactorWebAuthn])
		if err != nil {
			t.Fatal(err)
		}
		if _, err := webAuthnStep(svc, pending, bad); !errors.Is(err, auth.ErrInvalidSecondFactor) {
			t.Fatalf("wrong origin = %v, want ErrInvalidSecondFactor", err)
		}
		a.Origin = rpOrigin
		assertion, err := a.Assert(pending.Challenges[auth.FactorWebAuthn])
		if err != nil {
			t.Fatal(err)
		}
		clk.Add(time.Minute + time.Second)
		fresh := startLogin(t, svc, "a@example.com")
		// The old challenge is gone, so its assertion fails on a fresh login.
		if _, err := webAuthnStep(svc, fresh, assertion); !errors.Is(err, auth.ErrInvalidSecondFactor) {
			t.Errorf("expired challenge = %v, want ErrInvalidSecondFactor", err)
		}
	})
}

func TestWebAuthnSignCount(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		ctx := context.Background()
		svc, _ := newWebAuthnService(t, db, auth.TwoFactorPolicy{})
		u := mustCreate(t, svc, "a@example.com", store.RoleViewer)
		a := authtest.NewAuthenticator(rpOrigin)
		reg := registerKey(t, svc, u.ID, a, "Key")

		webAuthnLogin(t, svc, "a@example.com", a)
		webAuthnLogin(t, svc, "a@example.com", a) // the stored count is now 2

		// A counter that goes backwards logs in, with a note for the audit
		// log, and leaves the stored count alone.
		a.SignCount = 0
		res := webAuthnLogin(t, svc, "a@example.com", a)
		if res.FactorDetails["webauthn_sign_count"] != "regressed" {
			t.Errorf("details = %v, want webauthn_sign_count regressed", res.FactorDetails)
		}
		c, err := svc.GetWebAuthnCredential(ctx, u.ID, reg.Credential.ID)
		if err != nil || c.SignCount != 2 {
			t.Errorf("stored sign count = %d, %v; want 2", c.SignCount, err)
		}

		// A synced passkey reports 0 every time: no warning.
		p := authtest.NewAuthenticator(rpOrigin)
		p.ZeroCounter, p.BackupEligible, p.BackupState = true, true, true
		registerKey(t, svc, u.ID, p, "Passkey")
		for range 2 {
			if res := webAuthnLogin(t, svc, "a@example.com", p); res.FactorDetails["webauthn_sign_count"] != "" {
				t.Errorf("synced passkey details = %v, want no warning", res.FactorDetails)
			}
		}
	})
}

func TestWebAuthnOtherRelyingParty(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		ctx := context.Background()
		svc, _ := newWebAuthnService(t, db, auth.TwoFactorPolicy{})
		u := mustCreate(t, svc, "a@example.com", store.RoleViewer)
		// Registered under another dashboard domain: still a second factor,
		// but it can't be used here, so the login doesn't offer it.
		c := store.WebAuthnCredential{
			UserID: u.ID, CredentialID: []byte("old"), PublicKey: []byte("key"), Algorithm: -7,
			RPID: "old.example.com", Name: "Old key",
		}
		if err := store.NewWebAuthnCredentials(db).Create(ctx, &c); err != nil {
			t.Fatal(err)
		}
		pending := startLogin(t, svc, "a@example.com")
		if len(pending.Methods) != 0 || pending.Challenges[auth.FactorWebAuthn] != nil {
			t.Errorf("methods = %v, challenges = %v; want none", pending.Methods, pending.Challenges)
		}
		// Registering a key for the current domain excludes only that
		// domain's credentials.
		opts, err := svc.BeginWebAuthnRegistration(ctx, u.ID)
		if err != nil {
			t.Fatal(err)
		}
		var o struct {
			ExcludeCredentials []any `json:"excludeCredentials"`
		}
		if err := json.Unmarshal(opts, &o); err != nil || len(o.ExcludeCredentials) != 0 {
			t.Errorf("excludeCredentials = %v, %v; want none", o.ExcludeCredentials, err)
		}
	})
}

func TestWebAuthnRenameAndRemove(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		ctx := context.Background()
		svc, _ := newWebAuthnService(t, db, auth.TwoFactorPolicy{Required: auth.RequireAll})
		u := mustCreate(t, svc, "a@example.com", store.RoleViewer)
		other := mustCreate(t, svc, "b@example.com", store.RoleViewer)
		a := authtest.NewAuthenticator(rpOrigin)
		first := registerKey(t, svc, u.ID, a, "Key").Credential

		renamed, err := svc.RenameWebAuthnCredential(ctx, u.ID, first.ID, " Office key ")
		if err != nil || renamed.Name != "Office key" {
			t.Errorf("RenameWebAuthnCredential = %+v, %v", renamed, err)
		}
		if _, err := svc.RenameWebAuthnCredential(ctx, u.ID, first.ID, ""); !errors.Is(err, auth.ErrInvalidWebAuthnCredentialName) {
			t.Errorf("empty name: %v, want ErrInvalidWebAuthnCredentialName", err)
		}
		if _, err := svc.RenameWebAuthnCredential(ctx, other.ID, first.ID, "Mine"); !errors.Is(err, auth.ErrWebAuthnCredentialNotFound) {
			t.Errorf("another user's credential: %v, want ErrWebAuthnCredentialNotFound", err)
		}
		if _, err := svc.GetWebAuthnCredential(ctx, other.ID, first.ID); !errors.Is(err, auth.ErrWebAuthnCredentialNotFound) {
			t.Errorf("GetWebAuthnCredential of another user's = %v, want ErrWebAuthnCredentialNotFound", err)
		}

		current := webAuthnLogin(t, svc, "a@example.com", a)
		webAuthnLogin(t, svc, "a@example.com", a)
		p, err := svc.AuthenticateSession(ctx, current.Token)
		if err != nil {
			t.Fatal(err)
		}

		// The last factor stays while the policy requires one.
		if err := svc.RemoveWebAuthnCredential(ctx, p, first.ID); !errors.Is(err, auth.ErrLastSecondFactor) {
			t.Fatalf("removing the last factor = %v, want ErrLastSecondFactor", err)
		}
		if _, err := svc.GetWebAuthnCredential(ctx, u.ID, first.ID); err != nil {
			t.Errorf("refused removal deleted the credential: %v", err)
		}
		if n := countSessions(t, db, u.ID); n != 2 {
			t.Errorf("refused removal left %d sessions, want 2", n)
		}

		// With another factor, it goes, and signs out the other sessions.
		registerKey(t, svc, u.ID, authtest.NewAuthenticator(rpOrigin), "Spare")
		if err := svc.RemoveWebAuthnCredential(ctx, auth.Principal{UserID: other.ID}, first.ID); !errors.Is(err, auth.ErrWebAuthnCredentialNotFound) {
			t.Errorf("removing another user's credential = %v, want ErrWebAuthnCredentialNotFound", err)
		}
		if err := svc.RemoveWebAuthnCredential(ctx, p, first.ID); err != nil {
			t.Fatalf("RemoveWebAuthnCredential = %v", err)
		}
		if _, err := svc.GetWebAuthnCredential(ctx, u.ID, first.ID); !errors.Is(err, auth.ErrWebAuthnCredentialNotFound) {
			t.Errorf("removed credential: %v, want ErrWebAuthnCredentialNotFound", err)
		}
		if n := countSessions(t, db, u.ID); n != 1 {
			t.Errorf("%d sessions after removal, want only the current one", n)
		}
		if _, err := svc.AuthenticateSession(ctx, current.Token); err != nil {
			t.Errorf("current session after removal: %v", err)
		}
		if err := svc.RemoveWebAuthnCredential(ctx, p, first.ID); !errors.Is(err, auth.ErrWebAuthnCredentialNotFound) {
			t.Errorf("removing it again = %v, want ErrWebAuthnCredentialNotFound", err)
		}
	})
}

func TestWebAuthnRemoveLastWithoutPolicy(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		ctx := context.Background()
		svc, _ := newWebAuthnService(t, db, auth.TwoFactorPolicy{Required: auth.RequireAdmins})
		u := mustCreate(t, svc, "a@example.com", store.RoleOperator)
		c := registerKey(t, svc, u.ID, authtest.NewAuthenticator(rpOrigin), "Key").Credential
		if err := svc.RemoveWebAuthnCredential(ctx, auth.Principal{UserID: u.ID}, c.ID); err != nil {
			t.Fatalf("removing an operator's last factor under admins = %v", err)
		}
		if on, err := svc.TwoFactorEnabled(ctx, u.ID); err != nil || on {
			t.Errorf("TwoFactorEnabled = %v, %v; want false", on, err)
		}
		// Back to a one-step login.
		login(t, svc, "a@example.com")
	})
}
