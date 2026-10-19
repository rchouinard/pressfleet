package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/api"
	"github.com/rchouinard/pressfleet/control-plane/internal/auth"
	"github.com/rchouinard/pressfleet/control-plane/internal/auth/authtest"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
)

const (
	webAuthnDomain = "pf.example.com"
	webAuthnOrigin = "https://pf.example.com"
)

// newWebAuthnEnv is newAuthEnv with a dashboard domain and a two-factor
// policy.
func newWebAuthnEnv(t *testing.T, policy auth.TwoFactorPolicy) *authEnv {
	t.Helper()
	db := storetest.Open(t, store.SQLite)
	clk := &testClock{t: time.Now()}
	svc := auth.NewService(db, auth.Options{
		Logger: storetest.Logger(), Now: clk.now,
		PasswordParams: auth.Params{Memory: 64, Time: 1, Threads: 1, SaltLen: 16, KeyLen: 32},
		TwoFactor:      policy, RelyingParty: auth.RelyingPartyFor(webAuthnDomain),
	})
	u, err := svc.CreateUser(context.Background(), auth.NewUser{Email: "admin@example.com", Password: testPassword, Role: store.RoleAdmin})
	if err != nil {
		t.Fatal(err)
	}
	opts := api.Options{DB: db, Sites: newSiteEnv(t, db, testPreviewBase).svc, Auth: svc, Authenticator: api.NewAuthenticator(svc)}
	return &authEnv{t: t, h: newHandler(t, opts), db: db, svc: svc, clock: clk, user: u}
}

type webAuthnCredentialBody struct {
	ID             string   `json:"id"`
	Name           string   `json:"name"`
	RPID           string   `json:"rp_id"`
	AAGUID         string   `json:"aaguid"`
	Transports     []string `json:"transports"`
	UserVerified   bool     `json:"user_verified"`
	BackupEligible bool     `json:"backup_eligible"`
	SignCount      int64    `json:"sign_count"`
	LastUsedAt     *string  `json:"last_used_at"`
}

// registerOverHTTP registers a's credential with session s and returns the
// response.
func (e *authEnv) registerOverHTTP(s creds, a *authtest.Authenticator, name string) *httptest.ResponseRecorder {
	e.t.Helper()
	w := e.do(http.MethodPost, "/api/v1/users/me/two-factor/webauthn/options", "", s)
	expectStatus(e.t, w, http.StatusOK, "registration options")
	resp, err := a.Register(w.Body.Bytes())
	if err != nil {
		e.t.Fatal(err)
	}
	return e.do(http.MethodPost, "/api/v1/users/me/two-factor/webauthn", `{"name":"`+name+`","credential":`+string(resp)+`}`, s)
}

// webAuthnLoginOverHTTP logs in with the password and a's assertion and
// returns the session.
func (e *authEnv) webAuthnLoginOverHTTP(a *authtest.Authenticator) session {
	e.t.Helper()
	_, w := e.login("admin@example.com", testPassword, nil)
	expectStatus(e.t, w, http.StatusUnauthorized, "password step")
	var problem struct {
		Methods  []string        `json:"methods"`
		WebAuthn json.RawMessage `json:"webauthn"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &problem); err != nil {
		e.t.Fatal(err)
	}
	if len(problem.Methods) != 2 || problem.Methods[0] != "webauthn" || problem.Methods[1] != "recovery_code" || problem.WebAuthn == nil {
		e.t.Fatalf("second-factor problem: %s", w.Body)
	}
	var loginCookie string
	for _, c := range w.Result().Cookies() {
		if c.Name == api.LoginCookieName {
			loginCookie = c.Value
		}
	}
	assertion, err := a.Assert(problem.WebAuthn)
	if err != nil {
		e.t.Fatal(err)
	}
	w = e.secondStep(loginCookie, `{"method":"webauthn","credential":`+string(assertion)+`}`)
	expectStatus(e.t, w, http.StatusOK, "WebAuthn second step")
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
	e.t.Fatalf("no session cookie: %v", w.Result().Cookies())
	return session{}
}

// lastLogin returns the newest login audit event.
func lastLogin(t *testing.T, db *store.DB) store.AuditEvent {
	t.Helper()
	for _, ev := range allEvents(t, db) {
		if ev.Action == "self.login" {
			return ev
		}
	}
	t.Fatal("no login event")
	return store.AuditEvent{}
}

func TestWebAuthnOverHTTP(t *testing.T) {
	e := newWebAuthnEnv(t, auth.TwoFactorPolicy{})
	s := e.mustLogin()
	a := authtest.NewAuthenticator(webAuthnOrigin)

	// Register: the first factor comes with the recovery codes.
	w := e.registerOverHTTP(s.creds(), a, "YubiKey")
	expectStatus(t, w, http.StatusCreated, "register")
	var reg struct {
		Credential    webAuthnCredentialBody `json:"credential"`
		RecoveryCodes []string               `json:"recovery_codes"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &reg); err != nil {
		t.Fatal(err)
	}
	c := reg.Credential
	if c.ID == "" || c.Name != "YubiKey" || c.RPID != webAuthnDomain || c.AAGUID != "50462d74-6573-7400-0000-000000000000" ||
		len(c.Transports) != 1 || !c.UserVerified || c.LastUsedAt != nil || len(reg.RecoveryCodes) != auth.RecoveryCodeCount {
		t.Errorf("registration: %s", w.Body)
	}
	if loc := w.Header().Get("Location"); loc != "/api/v1/users/me/two-factor/webauthn/"+c.ID {
		t.Errorf("Location = %q", loc)
	}

	// The same key again: the options exclude it, and a client that
	// ignores that gets 409. A response for another origin is a 422.
	w = e.do(http.MethodPost, "/api/v1/users/me/two-factor/webauthn/options", "", s.creds())
	expectStatus(t, w, http.StatusOK, "options")
	if _, err := a.Register(w.Body.Bytes()); err == nil {
		t.Error("the options don't exclude the registered key")
	}
	var opts map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &opts); err != nil {
		t.Fatal(err)
	}
	delete(opts, "excludeCredentials")
	stripped, _ := json.Marshal(opts)
	resp, err := a.Register(stripped)
	if err != nil {
		t.Fatal(err)
	}
	expectStatus(t, e.do(http.MethodPost, "/api/v1/users/me/two-factor/webauthn", `{"name":"Again","credential":`+string(resp)+`}`, s.creds()),
		http.StatusConflict, "duplicate registration")
	evil := authtest.NewAuthenticator("https://evil.example.com")
	w = e.registerOverHTTP(s.creds(), evil, "Evil")
	expectStatus(t, w, http.StatusUnprocessableEntity, "wrong origin")
	if p := decodeProblem(t, w); len(p.Errors) != 1 || p.Errors[0].Field != "/credential" {
		t.Errorf("wrong origin: %s", w.Body)
	}

	// Tokens can list and read, but not manage.
	_, token := e.createToken(s, `{"name":"ci","scope":"full"}`)
	expectStatus(t, e.do(http.MethodPost, "/api/v1/users/me/two-factor/webauthn/options", "", creds{bearer: token}), http.StatusForbidden, "token registration")
	expectStatus(t, e.do(http.MethodDelete, "/api/v1/users/me/two-factor/webauthn/"+c.ID, "", creds{bearer: token}), http.StatusForbidden, "token removal")
	w = e.do(http.MethodGet, "/api/v1/users/me/two-factor/webauthn", "", creds{bearer: token})
	expectStatus(t, w, http.StatusOK, "list")
	var list struct {
		Items []webAuthnCredentialBody `json:"items"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil || len(list.Items) != 1 || list.Items[0].ID != c.ID {
		t.Errorf("list: %s (%v)", w.Body, err)
	}

	// Log out, and log in again with the key.
	expectStatus(t, e.do(http.MethodPost, "/api/v1/auth/logout", "", s.creds()), http.StatusNoContent, "logout")
	s1 := e.webAuthnLoginOverHTTP(a)
	if ev := lastLogin(t, e.db); ev.Result != store.AuditSuccess || ev.ActorUserID != e.user.ID ||
		ev.Details["second_factor"] != "webauthn" || ev.Details["webauthn_credential_id"] != c.ID || ev.Details["webauthn_sign_count"] != "" {
		t.Errorf("login event = %+v", ev)
	}
	w = e.do(http.MethodGet, "/api/v1/users/me/two-factor/webauthn/"+c.ID, "", s1.creds())
	expectStatus(t, w, http.StatusOK, "get")
	var got webAuthnCredentialBody
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || got.SignCount != 1 || got.LastUsedAt == nil {
		t.Errorf("after login: %s (%v)", w.Body, err)
	}

	// A counter that goes backwards still logs in, and is audited.
	a.SignCount = 0
	s2 := e.webAuthnLoginOverHTTP(a)
	if ev := lastLogin(t, e.db); ev.Result != store.AuditSuccess || ev.Details["webauthn_sign_count"] != "regressed" {
		t.Errorf("login event = %+v, want a sign count warning", ev)
	}

	// Rename.
	w = e.do(http.MethodPatch, "/api/v1/users/me/two-factor/webauthn/"+c.ID, `{"name":"Desk key"}`, s2.creds())
	expectStatus(t, w, http.StatusOK, "rename")
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || got.Name != "Desk key" {
		t.Errorf("rename: %s (%v)", w.Body, err)
	}
	w = e.do(http.MethodPatch, "/api/v1/users/me/two-factor/webauthn/"+c.ID, `{"name":"   "}`, s2.creds())
	expectStatus(t, w, http.StatusUnprocessableEntity, "blank name")
	expectStatus(t, e.do(http.MethodPatch, "/api/v1/users/me/two-factor/webauthn/nope", `{"name":"x"}`, s2.creds()), http.StatusNotFound, "rename unknown")

	// Remove: the other session ends, this one and the token stay, and
	// the user is back to one-step logins.
	expectStatus(t, e.do(http.MethodDelete, "/api/v1/users/me/two-factor/webauthn/"+c.ID, "", s2.creds()), http.StatusNoContent, "remove")
	expectStatus(t, e.do(http.MethodGet, "/api/v1/auth/me", "", s1.creds()), http.StatusUnauthorized, "other session after removal")
	expectStatus(t, e.do(http.MethodGet, "/api/v1/auth/me", "", s2.creds()), http.StatusOK, "current session after removal")
	expectStatus(t, e.do(http.MethodGet, "/api/v1/auth/me", "", creds{bearer: token}), http.StatusOK, "token after removal")
	expectStatus(t, e.do(http.MethodDelete, "/api/v1/users/me/two-factor/webauthn/"+c.ID, "", s2.creds()), http.StatusNotFound, "remove again")
	e.mustLogin()

	// Audit: each change, attributed and targeted.
	want := map[string]bool{
		"self.two_factor.webauthn.register": false, "self.two_factor.webauthn.rename": false, "self.two_factor.webauthn.remove": false,
	}
	for _, ev := range allEvents(t, e.db) {
		if _, ok := want[ev.Action]; ok && ev.Result == store.AuditSuccess && ev.TargetType == "webauthn_credential" &&
			ev.TargetID == c.ID && ev.ActorUserID == e.user.ID {
			want[ev.Action] = true
		}
	}
	for action, seen := range want {
		if !seen {
			t.Errorf("no successful %s event for the credential", action)
		}
	}
}

func TestWebAuthnLastFactorOverHTTP(t *testing.T) {
	e := newWebAuthnEnv(t, auth.TwoFactorPolicy{Required: auth.RequireAll})
	s := e.mustLogin()
	w := e.registerOverHTTP(s.creds(), authtest.NewAuthenticator(webAuthnOrigin), "Key")
	expectStatus(t, w, http.StatusCreated, "register")
	var reg struct {
		Credential webAuthnCredentialBody `json:"credential"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &reg); err != nil {
		t.Fatal(err)
	}
	expectStatus(t, e.do(http.MethodDelete, "/api/v1/users/me/two-factor/webauthn/"+reg.Credential.ID, "", s.creds()),
		http.StatusConflict, "removing the last factor")

	// A second key isn't the first factor: no recovery codes this time.
	w = e.registerOverHTTP(s.creds(), authtest.NewAuthenticator(webAuthnOrigin), "Spare")
	expectStatus(t, w, http.StatusCreated, "second key")
	var second map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &second); err != nil {
		t.Fatal(err)
	}
	if _, ok := second["recovery_codes"]; ok {
		t.Errorf("second registration returned recovery codes: %s", w.Body)
	}
	expectStatus(t, e.do(http.MethodDelete, "/api/v1/users/me/two-factor/webauthn/"+reg.Credential.ID, "", s.creds()),
		http.StatusNoContent, "removing one of two")
}
