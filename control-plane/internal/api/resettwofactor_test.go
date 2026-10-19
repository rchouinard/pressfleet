package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/api"
	"github.com/rchouinard/pressfleet/control-plane/internal/auth"
	"github.com/rchouinard/pressfleet/control-plane/internal/redact"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
)

// resetTarget is a user whose second factors an Admin resets.
type resetTarget struct {
	id, email string
	codes     []redact.Secret
	session   session
	token     string
}

// giveFactor gives a user a second factor, a WebAuthn credential for the
// env's relying party (no authenticator holds it, so it can't log in), and
// returns their recovery codes.
func (e *authEnv) giveFactor(userID string) []redact.Secret {
	e.t.Helper()
	ctx := context.Background()
	c := store.WebAuthnCredential{UserID: userID, CredentialID: []byte("cred-" + userID), PublicKey: []byte("key"), Algorithm: -7, RPID: "localhost", Name: "key"}
	if err := store.NewWebAuthnCredentials(e.db).Create(ctx, &c); err != nil {
		e.t.Fatal(err)
	}
	var codes []redact.Secret
	if err := e.db.InTx(ctx, func(q store.Querier) error {
		var err error
		codes, err = e.svc.IssueRecoveryCodes(ctx, q, userID)
		return err
	}); err != nil {
		e.t.Fatal(err)
	}
	return codes
}

// newResetTarget creates a user with role and gives them a second factor
// (giveFactor), a session logged in with a recovery code, and an API
// token.
func (e *authEnv) newResetTarget(email string, role store.Role) resetTarget {
	e.t.Helper()
	ctx := context.Background()
	u, err := e.svc.CreateUser(ctx, auth.NewUser{Email: email, Password: testPassword, Role: role})
	if err != nil {
		e.t.Fatal(err)
	}
	target := resetTarget{id: u.ID, email: email, codes: e.giveFactor(u.ID)}
	target.session = e.recoveryCodeLogin(email, target.codes[0])
	_, tok, err := e.svc.CreateAPIToken(ctx, u.ID, auth.NewAPIToken{Name: "ci", Scope: store.ScopeFull})
	if err != nil {
		e.t.Fatal(err)
	}
	target.token = tok.Reveal()
	return target
}

// webAuthnCredentials counts a user's WebAuthn credentials.
func (e *authEnv) webAuthnCredentials(userID string) int {
	e.t.Helper()
	creds, err := store.NewWebAuthnCredentials(e.db).ListForUser(context.Background(), userID)
	if err != nil {
		e.t.Fatal(err)
	}
	return len(creds)
}

// passwordStep enters a user's password and returns the response.
func (e *authEnv) passwordStep(email string) (session, string, int) {
	e.t.Helper()
	s, w := e.login(email, testPassword, nil)
	for _, c := range w.Result().Cookies() {
		if c.Name == api.LoginCookieName {
			return s, c.Value, w.Code
		}
	}
	return s, "", w.Code
}

// recoveryCodeLogin logs a user in with their password and a recovery
// code.
func (e *authEnv) recoveryCodeLogin(email string, code redact.Secret) session {
	e.t.Helper()
	_, cookie, status := e.passwordStep(email)
	if status != http.StatusUnauthorized || cookie == "" {
		e.t.Fatalf("password step of %s: %d", email, status)
	}
	w := e.secondStep(cookie, `{"method":"recovery_code","code":"`+code.Reveal()+`"}`)
	expectStatus(e.t, w, http.StatusOK, "second step of "+email)
	var body struct {
		CSRFToken string `json:"csrf_token"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		e.t.Fatal(err)
	}
	for _, c := range w.Result().Cookies() {
		if c.Name == api.SessionCookieName {
			return session{cookie: c, csrf: body.CSRFToken}
		}
	}
	e.t.Fatal("no session cookie")
	return session{}
}

func resetPath(userID string) string { return "/api/v1/users/" + userID + "/two-factor" }

// TestResetUserTwoFactorOverHTTP is the acceptance criterion over HTTP:
// an Admin's reset removes the user's factors and sessions, keeps their
// API tokens, is audited without secrets, and leaves them to log in with
// their password alone, or restricted to enrolling when the policy covers
// them; every old factor is refused.
func TestResetUserTwoFactorOverHTTP(t *testing.T) {
	for _, tt := range []struct {
		required auth.TwoFactorRequirement
		restrict bool
	}{
		{auth.RequireNone, false},
		{auth.RequireAll, true},
	} {
		t.Run(string(tt.required), func(t *testing.T) {
			storetest.Run(t, func(t *testing.T, db *store.DB) {
				e := newPolicyAuthEnv(t, db, tt.required)
				admin := e.recoveryCodeLogin("admin@example.com", e.giveFactor(e.user.ID)[0])
				target := e.newResetTarget("v@example.com", store.RoleViewer)
				// A login whose password step came before the reset.
				_, pending, _ := e.passwordStep(target.email)

				w := e.do(http.MethodDelete, resetPath(target.id), "", admin.creds())
				expectStatus(t, w, http.StatusNoContent, "reset")
				ev := allEvents(t, db)[0]
				if ev.Action != "user.two_factor.reset" || ev.ActorUserID != e.user.ID || ev.TargetType != "user" || ev.TargetID != target.id ||
					ev.Result != store.AuditSuccess {
					t.Errorf("audit event: %+v", ev)
				}
				want := map[string]string{"totp_removed": "false", "webauthn_credentials_removed": "1", "recovery_codes_removed": "10", "sessions_ended": "1"}
				if len(ev.Details) != len(want) {
					t.Errorf("audit details: %v, want %v", ev.Details, want)
				}
				for k, v := range want {
					if ev.Details[k] != v {
						t.Errorf("audit details: %v, want %v", ev.Details, want)
					}
				}

				// Signed out everywhere; tokens are kept.
				expectStatus(t, e.do(http.MethodGet, "/api/v1/auth/me", "", target.session.creds()), http.StatusUnauthorized, "session after reset")
				expectStatus(t, e.do(http.MethodGet, "/api/v1/auth/me", "", creds{bearer: target.token}), http.StatusOK, "token after reset")
				// The Admin's own session and factors are untouched.
				w = e.do(http.MethodGet, "/api/v1/users/"+target.id, "", admin.creds())
				expectStatus(t, w, http.StatusOK, "get user")
				if decodeMe(t, w).TwoFactorEnabled {
					t.Errorf("two_factor_enabled after reset: %s", w.Body)
				}
				w = e.do(http.MethodGet, "/api/v1/users", "", admin.creds())
				var list struct {
					Items []struct {
						ID               string `json:"id"`
						TwoFactorEnabled bool   `json:"two_factor_enabled"`
					}
				}
				if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
					t.Fatal(err)
				}
				for _, u := range list.Items {
					if u.TwoFactorEnabled != (u.ID == e.user.ID) {
						t.Errorf("user list two_factor_enabled: %s", w.Body)
					}
				}

				// The old recovery codes are refused, at the login that was
				// waiting and at a new one; the password alone now logs in.
				w = e.secondStep(pending, `{"method":"recovery_code","code":"`+target.codes[1].Reveal()+`"}`)
				if w.Code == http.StatusOK {
					t.Errorf("old recovery code at a pending login: %s", w.Body)
				}
				s, cookie, status := e.passwordStep(target.email)
				if status != http.StatusOK || cookie != "" {
					t.Fatalf("password login after reset: %d, login cookie %q", status, cookie)
				}

				// Under the policy, the session may only enroll.
				w = e.do(http.MethodGet, "/api/v1/auth/me", "", s.creds())
				expectStatus(t, w, http.StatusOK, "me after reset")
				if me := decodeEnrollmentMe(t, w, "me"); me.EnrollmentRequired != tt.restrict {
					t.Errorf("enrollment_required = %v, want %v", me.EnrollmentRequired, tt.restrict)
				}
				w = e.do(http.MethodGet, "/api/v1/sites", "", s.creds())
				if tt.restrict {
					expectEnrollmentRequired(t, w, "list sites")
				} else {
					expectStatus(t, w, http.StatusOK, "list sites")
				}

				// Nothing is left to reset.
				expectStatus(t, e.do(http.MethodDelete, resetPath(target.id), "", admin.creds()), http.StatusConflict, "second reset")
			})
		})
	}
}

// TestResetUserTwoFactorRefusedOverHTTP checks who may not reset: Operators
// and Viewers, API tokens (second factors are managed from sessions), and
// an Admin resetting themselves. Unknown users are 404.
func TestResetUserTwoFactorRefusedOverHTTP(t *testing.T) {
	e := newAuthEnv(t, api.Options{})
	admin := e.mustLogin()
	target := e.newResetTarget("v@example.com", store.RoleViewer)
	e.createUser(admin, "op@example.com", store.RoleOperator)
	e.createUser(admin, "viewer@example.com", store.RoleViewer)
	_, full := e.createToken(admin, `{"name":"full","scope":"full","expires_at":null}`)
	_, readOnly := e.createToken(admin, `{"name":"ro","scope":"read-only","expires_at":null}`)

	for _, c := range []struct {
		name  string
		creds creds
	}{
		{"operator", e.mustLoginAs("op@example.com").creds()},
		{"viewer", e.mustLoginAs("viewer@example.com").creds()},
		{"admin full token", creds{bearer: full}},
		{"admin read-only token", creds{bearer: readOnly}},
	} {
		w := e.do(http.MethodDelete, resetPath(target.id), "", c.creds)
		expectStatus(t, w, http.StatusForbidden, c.name)
		if ev := allEvents(t, e.db)[0]; ev.Action != "user.two_factor.reset" || ev.Result != store.AuditDenied || ev.TargetID != target.id {
			t.Errorf("%s: audit event %+v", c.name, ev)
		}
	}

	// An Admin can't reset their own factors here, even with nothing
	// required.
	e.giveFactor(e.user.ID)
	expectStatus(t, e.do(http.MethodDelete, resetPath(e.user.ID), "", admin.creds()), http.StatusForbidden, "own factors")
	if e.webAuthnCredentials(e.user.ID) != 1 {
		t.Error("refused self-reset removed the Admin's factors")
	}

	expectStatus(t, e.do(http.MethodDelete, resetPath("01j9zq3v6t8k4m2n5p7r9s1x3z"), "", admin.creds()), http.StatusNotFound, "unknown user")

	// Nothing was reset along the way.
	if e.webAuthnCredentials(target.id) != 1 {
		t.Error("refused resets removed the target's factors")
	}
	expectStatus(t, e.do(http.MethodGet, "/api/v1/auth/me", "", target.session.creds()), http.StatusOK, "target's session")
}

// TestResetUserTwoFactorAsksForReauthentication checks that a stale Admin
// session must reauthenticate before resetting, and then may.
func TestResetUserTwoFactorAsksForReauthentication(t *testing.T) {
	e := newAuthEnvWith(t, api.Options{}, reauthPolicy)
	admin := e.mustLogin()
	target := e.newResetTarget("v@example.com", store.RoleViewer)
	e.clock.add(11 * time.Minute)

	w := e.do(http.MethodDelete, resetPath(target.id), "", admin.creds())
	if !isReauthProblem(t, w) {
		t.Fatalf("stale session: %d %s, want reauthentication-required", w.Code, w.Body)
	}
	if e.webAuthnCredentials(target.id) != 1 {
		t.Error("refused reset removed the target's factors")
	}
	w = e.do(http.MethodPost, "/api/v1/auth/reauthenticate", `{"method":"password","password":"`+testPassword+`"}`, admin.creds())
	expectStatus(t, w, http.StatusOK, "reauthenticate")
	expectStatus(t, e.do(http.MethodDelete, resetPath(target.id), "", admin.creds()), http.StatusNoContent, "reset after reauthenticating")
}
