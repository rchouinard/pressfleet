package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/rchouinard/pressfleet/control-plane/internal/api"
	"github.com/rchouinard/pressfleet/control-plane/internal/auth"
	"github.com/rchouinard/pressfleet/control-plane/internal/redact"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
)

// enrollAdmin gives the env's admin a second factor (a WebAuthn
// credential for the current relying party, localhost, whose key the tests
// don't hold, so they use recovery codes) and returns their recovery codes.
func (e *authEnv) enrollAdmin() []redact.Secret {
	e.t.Helper()
	ctx := context.Background()
	c := store.WebAuthnCredential{UserID: e.user.ID, CredentialID: []byte("cred"), PublicKey: []byte("key"), Algorithm: -7, RPID: auth.DefaultRelyingPartyID, Name: "key"}
	if err := store.NewWebAuthnCredentials(e.db).Create(ctx, &c); err != nil {
		e.t.Fatal(err)
	}
	var codes []redact.Secret
	if err := e.db.InTx(ctx, func(q store.Querier) error {
		var err error
		codes, err = e.svc.IssueRecoveryCodes(ctx, q, e.user.ID)
		return err
	}); err != nil {
		e.t.Fatal(err)
	}
	return codes
}

// secondStep posts the second step with the pending-login cookie.
func (e *authEnv) secondStep(loginCookie, body string) *httptest.ResponseRecorder {
	e.t.Helper()
	c := creds{}
	if loginCookie != "" {
		c.header = map[string]string{"Cookie": api.LoginCookieName + "=" + loginCookie}
	}
	return e.do(http.MethodPost, "/api/v1/auth/login/second-factor", body, c)
}

// startTwoStepLogin enters the admin's password and returns the
// pending-login cookie.
func (e *authEnv) startTwoStepLogin() string {
	e.t.Helper()
	_, w := e.login("admin@example.com", testPassword, nil)
	expectStatus(e.t, w, http.StatusUnauthorized, "password step")
	for _, c := range w.Result().Cookies() {
		if c.Name == api.LoginCookieName {
			return c.Value
		}
	}
	e.t.Fatalf("no %s cookie: %v", api.LoginCookieName, w.Result().Cookies())
	return ""
}

type meBody struct {
	ID                     string  `json:"id"`
	TwoFactorEnabled       bool    `json:"two_factor_enabled"`
	VerifiedAt             *string `json:"verified_at"`
	RecoveryCodesRemaining int     `json:"recovery_codes_remaining"`
	CSRFToken              *string `json:"csrf_token"`

	ReauthenticationMaxAgeSeconds *int `json:"reauthentication_max_age_seconds"`
}

func decodeMe(t *testing.T, w *httptest.ResponseRecorder) meBody {
	t.Helper()
	var me meBody
	if err := json.Unmarshal(w.Body.Bytes(), &me); err != nil {
		t.Fatal(err)
	}
	return me
}

func TestTwoStepLoginOverHTTP(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newAuthEnv(t, api.Options{DB: db})
		codes := e.enrollAdmin()

		// The password step: 401 with the problem type, the methods, and the
		// pending-login cookie, but no session.
		_, w := e.login("admin@example.com", testPassword, nil)
		expectStatus(t, w, http.StatusUnauthorized, "password step")
		var problem struct {
			Type    string   `json:"type"`
			Status  int      `json:"status"`
			Methods []string `json:"methods"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &problem); err != nil {
			t.Fatal(err)
		}
		if problem.Type != "https://github.com/rchouinard/pressfleet/blob/main/docs/configuration.md#logging-in-with-a-second-factor" ||
			problem.Status != 401 || !slices.Equal(problem.Methods, []string{"webauthn", "recovery_code"}) {
			t.Errorf("problem: %s", w.Body)
		}
		cookies := w.Result().Cookies()
		if len(cookies) != 1 || cookies[0].Name != api.LoginCookieName {
			t.Fatalf("cookies = %v, want only %s", cookies, api.LoginCookieName)
		}
		lc := cookies[0]
		if !lc.HttpOnly || lc.SameSite != http.SameSiteStrictMode || lc.Path != "/api/v1/auth/login" || lc.Secure ||
			lc.Expires.IsZero() || len(lc.Value) < 43 {
			t.Errorf("pending-login cookie: %+v", lc)
		}
		// The pending-login token is no session.
		expectStatus(t, e.do(http.MethodGet, "/api/v1/auth/me", "", creds{cookie: lc.Value}), http.StatusUnauthorized, "pending token as session")

		// Mistakes that aren't guesses.
		expectStatus(t, e.secondStep("", `{"method":"recovery_code","code":"`+codes[0].Reveal()+`"}`), http.StatusUnauthorized, "no cookie")
		expectStatus(t, e.secondStep(lc.Value, `{"method":"recovery_code"}`), http.StatusUnprocessableEntity, "no code")
		w = e.secondStep(lc.Value, `{"method":"totp","code":"123456"}`)
		expectStatus(t, w, http.StatusUnprocessableEntity, "unenrolled method")
		if p := decodeProblem(t, w); len(p.Errors) != 1 || p.Errors[0].Field != "/method" {
			t.Errorf("unenrolled method: %s", w.Body)
		}
		// A wrong code is a 422 on /code, and the login stays pending.
		w = e.secondStep(lc.Value, `{"method":"recovery_code","code":"AAAA-AAAA-AAAA-AAAA"}`)
		expectStatus(t, w, http.StatusUnprocessableEntity, "wrong code")
		if p := decodeProblem(t, w); len(p.Errors) != 1 || p.Errors[0].Field != "/code" {
			t.Errorf("wrong code: %s", w.Body)
		}

		w = e.secondStep(lc.Value, `{"method":"recovery_code","code":"`+codes[0].Reveal()+`"}`)
		expectStatus(t, w, http.StatusOK, "second step")
		var sc *http.Cookie
		for _, c := range w.Result().Cookies() {
			if c.Name == api.SessionCookieName {
				sc = c
			}
		}
		me := decodeMe(t, w)
		if sc == nil || !sc.HttpOnly || sc.SameSite != http.SameSiteLaxMode || me.ID != e.user.ID || !me.TwoFactorEnabled ||
			me.VerifiedAt == nil || me.RecoveryCodesRemaining != 9 || me.CSRFToken == nil {
			t.Fatalf("second step response: cookie %+v, body %s", sc, w.Body)
		}
		w = e.do(http.MethodGet, "/api/v1/auth/me", "", creds{cookie: sc.Value})
		expectStatus(t, w, http.StatusOK, "me after two-step login")
		if me := decodeMe(t, w); !me.TwoFactorEnabled || me.VerifiedAt == nil || me.RecoveryCodesRemaining != 9 {
			t.Errorf("me: %s", w.Body)
		}

		// The pending login and the code work once.
		expectStatus(t, e.secondStep(lc.Value, `{"method":"recovery_code","code":"`+codes[1].Reveal()+`"}`), http.StatusUnauthorized, "reused login")
		w = e.secondStep(e.startTwoStepLogin(), `{"method":"recovery_code","code":"`+codes[0].Reveal()+`"}`)
		expectStatus(t, w, http.StatusUnprocessableEntity, "reused code")

		// Audit: the password step, the wrong code, the login, and the reused
		// code, all with the user as actor; requests that had no pending
		// login or made no guess aren't recorded as factor checks.
		var logins []store.AuditEvent
		for _, ev := range allEvents(t, e.db) {
			if ev.Action == "self.login" {
				logins = append(logins, ev)
			}
		}
		type summary struct {
			actor, result, factor, remaining string
		}
		got := make([]summary, len(logins))
		for i, ev := range logins {
			got[i] = summary{ev.ActorUserID, string(ev.Result), ev.Details["second_factor"], ev.Details["recovery_codes_remaining"]}
		}
		u := e.user.ID
		// Newest first.
		want := []summary{
			{u, "failure", "recovery_code", ""},
			{u, "failure", "required", ""},
			{u, "success", "recovery_code", "9"},
			{u, "failure", "recovery_code", ""},
			{u, "failure", "required", ""},
		}
		if len(got) != len(want) {
			t.Fatalf("login events = %+v, want %+v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("login event %d = %+v, want %+v", i, got[i], want[i])
			}
		}
	})
}

func TestOneStepLoginUnchangedOverHTTP(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newAuthEnv(t, api.Options{DB: db})
		s := e.mustLogin()
		w := e.do(http.MethodGet, "/api/v1/auth/me", "", creds{cookie: s.cookie.Value})
		expectStatus(t, w, http.StatusOK, "me")
		if me := decodeMe(t, w); me.TwoFactorEnabled || me.VerifiedAt == nil || me.RecoveryCodesRemaining != 0 {
			t.Errorf("me: %s", w.Body)
		}
		// A wrong password keeps the old 401: about:blank, no extension
		// members, no cookie.
		_, w = e.login("admin@example.com", "wrong password", nil)
		expectStatus(t, w, http.StatusUnauthorized, "wrong password")
		var body map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if body["type"] != "about:blank" || body["methods"] != nil || len(w.Result().Cookies()) != 0 {
			t.Errorf("wrong password: %s, cookies %v", w.Body, w.Result().Cookies())
		}
		// Tokens report no verification time.
		_, tok := e.createToken(s, `{"name":"ci","scope":"read-only","expires_at":null}`)
		w = e.do(http.MethodGet, "/api/v1/auth/me", "", creds{bearer: tok})
		expectStatus(t, w, http.StatusOK, "me with a token")
		if me := decodeMe(t, w); me.VerifiedAt != nil {
			t.Errorf("token me: %s", w.Body)
		}
	})
}

func TestSecondFactorThrottledOverHTTP(t *testing.T) {
	e := newAuthEnv(t, api.Options{})
	codes := e.enrollAdmin()
	lc := e.startTwoStepLogin()
	for range 5 {
		e.secondStep(lc, `{"method":"recovery_code","code":"AAAA-AAAA-AAAA-AAAA"}`)
	}
	w := e.secondStep(lc, `{"method":"recovery_code","code":"`+codes[0].Reveal()+`"}`)
	expectStatus(t, w, http.StatusTooManyRequests, "throttled second step")
	if w.Header().Get("Retry-After") == "" {
		t.Error("429 without Retry-After")
	}
}

func TestRegenerateRecoveryCodesOverHTTP(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newAuthEnv(t, api.Options{DB: db})
		s := e.mustLogin()
		const path = "/api/v1/users/me/two-factor/recovery-codes"
		expectStatus(t, e.do(http.MethodPost, path, "", s.creds()), http.StatusConflict, "without a second factor")

		codes := e.enrollAdmin()
		w := e.do(http.MethodPost, path, "", s.creds())
		expectStatus(t, w, http.StatusOK, "regenerate")
		var body struct{ Codes []string }
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if len(body.Codes) != 10 || body.Codes[0] == codes[0].Reveal() {
			t.Fatalf("regenerated: %s", w.Body)
		}
		// The old set is gone; a new code logs in.
		expectStatus(t, e.secondStep(e.startTwoStepLogin(), `{"method":"recovery_code","code":"`+codes[0].Reveal()+`"}`),
			http.StatusUnprocessableEntity, "old code")
		expectStatus(t, e.secondStep(e.startTwoStepLogin(), `{"method":"recovery_code","code":"`+body.Codes[0]+`"}`),
			http.StatusOK, "new code")

		// API tokens can't regenerate them, even with full scope.
		_, tok := e.createToken(s, `{"name":"ci","scope":"full","expires_at":null}`)
		expectStatus(t, e.do(http.MethodPost, path, "", creds{bearer: tok}), http.StatusForbidden, "token caller")

		// Audit: each attempt, with the user as actor and no codes.
		type summary struct{ actor, result string }
		var got []summary
		for _, ev := range allEvents(t, db) {
			if ev.Action != "self.two_factor.recovery_codes.regenerate" {
				continue
			}
			got = append(got, summary{ev.ActorUserID, string(ev.Result)})
			for k, v := range ev.Details {
				if slices.Contains(body.Codes, v) {
					t.Errorf("regeneration event details %s hold a recovery code", k)
				}
			}
		}
		u := e.user.ID
		// Newest first.
		want := []summary{{u, "denied"}, {u, "success"}, {u, "failure"}}
		if !slices.Equal(got, want) {
			t.Errorf("regeneration events = %+v, want %+v", got, want)
		}
	})
}

func TestUsersReportTwoFactor(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newAuthEnv(t, api.Options{DB: db})
		s := e.mustLogin()
		viewer, err := e.svc.CreateUser(context.Background(), auth.NewUser{Email: "viewer@example.com", Password: testPassword, Role: store.RoleViewer})
		if err != nil {
			t.Fatal(err)
		}
		e.enrollAdmin()
		// The viewer's only key is from a previous dashboard domain, so it
		// isn't a second factor.
		stale := store.WebAuthnCredential{UserID: viewer.ID, CredentialID: []byte("stale"), PublicKey: []byte("key"), Algorithm: -7, RPID: "old.example.com", Name: "key"}
		if err := store.NewWebAuthnCredentials(e.db).Create(context.Background(), &stale); err != nil {
			t.Fatal(err)
		}
		w := e.do(http.MethodGet, "/api/v1/users", "", s.creds())
		expectStatus(t, w, http.StatusOK, "list users")
		var list struct {
			Items []struct {
				Email            string `json:"email"`
				TwoFactorEnabled bool   `json:"two_factor_enabled"`
			}
		}
		if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
			t.Fatal(err)
		}
		got := map[string]bool{}
		for _, u := range list.Items {
			got[u.Email] = u.TwoFactorEnabled
		}
		if len(got) != 2 || !got["admin@example.com"] || got["viewer@example.com"] {
			t.Errorf("two_factor_enabled: %v", got)
		}
		w = e.do(http.MethodGet, "/api/v1/users/"+e.user.ID, "", s.creds())
		expectStatus(t, w, http.StatusOK, "get user")
		if me := decodeMe(t, w); !me.TwoFactorEnabled {
			t.Errorf("get user: %s", w.Body)
		}
		vs := e.mustLoginAs("viewer@example.com")
		if me := decodeMe(t, e.do(http.MethodGet, "/api/v1/auth/me", "", vs.creds())); me.TwoFactorEnabled {
			t.Errorf("viewer's /auth/me with only a stale key: two_factor_enabled = true")
		}
	})
}
