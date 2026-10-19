package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/api"
	"github.com/rchouinard/pressfleet/control-plane/internal/auth"
	"github.com/rchouinard/pressfleet/control-plane/internal/authz"
	"github.com/rchouinard/pressfleet/control-plane/internal/sites"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
)

// reauthPolicy is the default policy: reauthentication on, 10 minutes.
var reauthPolicy = auth.TwoFactorPolicy{Reauthentication: true, ReauthenticationMaxAge: 10 * time.Minute}

// altSensitiveSamples are requests that make an operation's handler check
// one of its alternative actions (operationAltActions).
var altSensitiveSamples = map[authz.Action]request{
	authz.OpenReadWriteDatabaseSession: {method: http.MethodPost, target: "/api/v1/sites/abc123/database/sessions", body: `{"access":"read_write"}`},
	authz.DeleteSiteBackups:            {method: http.MethodDelete, target: "/api/v1/sites/abc123?delete_backups=true"},
	authz.OffboardClientGroup:          {method: http.MethodDelete, target: "/api/v1/client-groups/abc123?delete_backup_repository=true"},
}

func isReauthProblem(t *testing.T, w *httptest.ResponseRecorder) bool {
	t.Helper()
	if w.Code != http.StatusForbidden {
		return false
	}
	return decodeProblem(t, w).Type == api.ReauthenticationRequiredProblemType
}

// TestSensitiveOperationsAskForReauthentication sends a request for every
// sensitive action an operation can check, from fresh and stale sessions,
// from a token, and with the setting off. Only the stale session with the
// setting on is asked to reauthenticate.
func TestSensitiveOperationsAskForReauthentication(t *testing.T) {
	ops, err := api.Operations()
	if err != nil {
		t.Fatal(err)
	}
	type sample struct {
		name string
		req  request
	}
	var samples []sample
	covered := map[authz.Action]bool{}
	for _, op := range ops {
		if op.Public {
			continue
		}
		if op.Action.Sensitive() {
			samples = append(samples, sample{op.ID, sampleRequest(op, true)})
			covered[op.Action] = true
		}
		for _, a := range op.AltActions {
			if !a.Sensitive() {
				continue
			}
			req, ok := altSensitiveSamples[a]
			if !ok {
				t.Errorf("%s may check sensitive action %s, which altSensitiveSamples lacks", op.ID, a)
				continue
			}
			req.authed = true
			samples = append(samples, sample{op.ID + "/" + string(a), req})
			covered[a] = true
		}
	}
	// Every sensitive action with an endpoint is covered; the rest have
	// none yet.
	for _, a := range authz.Actions() {
		if a.Sensitive() && !covered[a] && a != authz.ManageOthersTokens {
			t.Errorf("sensitive action %s isn't reached by any operation", a)
		}
	}

	now := time.Now()
	callers := []struct {
		name   string
		p      auth.Principal
		policy auth.TwoFactorPolicy
		asked  bool
	}{
		{"fresh session", auth.Principal{Method: auth.MethodSession, SessionID: "s", VerifiedAt: now.Add(-time.Minute)}, reauthPolicy, false},
		{"stale session", auth.Principal{Method: auth.MethodSession, SessionID: "s", VerifiedAt: now.Add(-11 * time.Minute)}, reauthPolicy, true},
		{"stale session, setting off", auth.Principal{Method: auth.MethodSession, SessionID: "s", VerifiedAt: now.Add(-11 * time.Minute)},
			auth.TwoFactorPolicy{ReauthenticationMaxAge: 10 * time.Minute}, false},
		{"full token", auth.Principal{Method: auth.MethodToken, TokenID: "01j9zq3v6t8k4m2n5p7r9s1x3z", Scope: store.ScopeFull}, reauthPolicy, false},
	}
	for _, c := range callers {
		t.Run(c.name, func(t *testing.T) {
			db := storetest.Open(t, store.SQLite)
			svc, _ := newAuthServiceWith(db, c.policy)
			u, err := svc.CreateUser(context.Background(), auth.NewUser{Email: "admin@example.com", Password: testPassword, Role: store.RoleAdmin})
			if err != nil {
				t.Fatal(err)
			}
			p := c.p
			p.UserID, p.Email, p.Role = u.ID, u.Email, u.Role
			h := newHandler(t, api.Options{Authenticator: &fakeAuth{principal: &p}, Auth: svc, DB: db, Sites: newSiteEnv(t, db, testPreviewBase).svc})
			for _, s := range samples {
				t.Run(s.name, func(t *testing.T) {
					w := do(t, h, s.req)
					if got := isReauthProblem(t, w); got != c.asked {
						t.Errorf("asked to reauthenticate = %v, want %v; status %d: %s", got, c.asked, w.Code, w.Body)
					}
				})
			}
		})
	}
}

// TestDeleteSiteAfterReauthentication is the acceptance case of #154: with
// the defaults, deleting a site 11 minutes after login asks for a fresh
// check, and succeeds right after one.
func TestDeleteSiteAfterReauthentication(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		se := newSiteEnv(t, db, testPreviewBase)
		e := newAuthEnvWith(t, api.Options{DB: db, Sites: se.svc}, reauthPolicy)
		group := &store.ClientGroup{Name: "Acme"}
		if err := store.NewClientGroups(db).Create(ctx, group); err != nil {
			t.Fatal(err)
		}
		site, _, err := se.svc.Create(ctx, sites.NewSite{ClientGroupID: group.ID, Name: "Doomed", AdminEmail: "o@example.com", PHPVersion: "8.4"})
		if err != nil {
			t.Fatal(err)
		}
		se.runJobs(t)

		s := e.mustLogin()
		me := decodeMe(t, e.do(http.MethodGet, "/api/v1/auth/me", "", s.creds()))
		if me.ReauthenticationMaxAgeSeconds == nil || *me.ReauthenticationMaxAgeSeconds != 600 {
			t.Errorf("reauthentication_max_age_seconds = %v, want 600", me.ReauthenticationMaxAgeSeconds)
		}
		loggedIn := *me.VerifiedAt

		e.clock.add(11 * time.Minute)
		target := "/api/v1/sites/" + site.ID
		w := e.do(http.MethodDelete, target, "", s.creds())
		if !isReauthProblem(t, w) {
			t.Fatalf("delete 11 minutes after login: %d %s, want 403 %s", w.Code, w.Body, api.ReauthenticationRequiredProblemType)
		}
		if got := allEvents(t, db)[0]; got.Action != string(authz.DeleteSite) || got.Result != store.AuditDenied {
			t.Errorf("refusal audited as %s %s", got.Action, got.Result)
		}

		w = e.do(http.MethodPost, "/api/v1/auth/reauthenticate", `{"method":"password","password":"`+testPassword+`"}`, s.creds())
		expectStatus(t, w, http.StatusOK, "reauthenticate")
		if me := decodeMe(t, w); me.VerifiedAt == nil || *me.VerifiedAt == loggedIn {
			t.Errorf("verified_at after reauthenticating = %v, was %s", me.VerifiedAt, loggedIn)
		}
		ev := allEvents(t, db)[0]
		if ev.Action != "self.reauthenticate" || ev.Result != store.AuditSuccess || ev.ActorUserID != e.user.ID || ev.Details["method"] != "password" {
			t.Errorf("reauthentication audited as %+v", ev)
		}

		expectStatus(t, e.do(http.MethodDelete, target, "", s.creds()), http.StatusAccepted, "delete right after reauthenticating")
	})
}

// TestReauthenticateOverHTTP covers users with and without a second
// factor, wrong codes and passwords, the throttle, and token callers.
func TestReauthenticateOverHTTP(t *testing.T) {
	const path = "/api/v1/auth/reauthenticate"
	t.Run("without a second factor", func(t *testing.T) {
		e := newAuthEnvWith(t, api.Options{}, reauthPolicy)
		s := e.mustLogin()
		w := e.do(http.MethodPost, path+"/options", "", s.creds())
		expectStatus(t, w, http.StatusOK, "options")
		if got := decodeOptions(t, w); !slices.Equal(got.Methods, []string{"password"}) || got.WebAuthn != nil {
			t.Errorf("options = %+v, want only the password", got)
		}
		for _, tt := range []struct {
			body, field string
		}{
			{`{"method":"password","password":"wrong password"}`, "/password"},
			{`{"method":"password"}`, "/password"},
			{`{"method":"recovery_code","code":"AAAA-BBBB-CCCC-DDDD"}`, "/method"},
		} {
			w := e.do(http.MethodPost, path, tt.body, s.creds())
			expectStatus(t, w, http.StatusUnprocessableEntity, tt.body)
			if p := decodeProblem(t, w); len(p.Errors) != 1 || p.Errors[0].Field != tt.field {
				t.Errorf("%s: errors %+v, want one on %s", tt.body, p.Errors, tt.field)
			}
		}
		if ev := allEvents(t, e.db)[0]; ev.Action != "self.reauthenticate" || ev.Result != store.AuditFailure {
			t.Errorf("failure audited as %+v", ev)
		}
		expectStatus(t, e.do(http.MethodPost, path, `{"method":"password","password":"`+testPassword+`"}`, s.creds()), http.StatusOK, "right password")
	})

	t.Run("with a second factor", func(t *testing.T) {
		e := newAuthEnvWith(t, api.Options{}, reauthPolicy)
		codes := e.enrollAdmin()
		s := e.secondFactorSession(codes[0].Reveal())
		w := e.do(http.MethodPost, path+"/options", "", s.creds())
		expectStatus(t, w, http.StatusOK, "options")
		// The WebAuthn credential has no verifier in this control plane,
		// so only recovery codes are offered.
		if got := decodeOptions(t, w); !slices.Equal(got.Methods, []string{"recovery_code"}) {
			t.Errorf("options = %+v, want recovery codes", got)
		}
		w = e.do(http.MethodPost, path, `{"method":"password","password":"`+testPassword+`"}`, s.creds())
		expectStatus(t, w, http.StatusUnprocessableEntity, "the password instead of the factor")
		if p := decodeProblem(t, w); len(p.Errors) != 1 || p.Errors[0].Field != "/method" {
			t.Errorf("password with a factor: errors %+v, want one on /method", p.Errors)
		}
		w = e.do(http.MethodPost, path, `{"method":"recovery_code","code":"`+codes[0].Reveal()+`"}`, s.creds())
		expectStatus(t, w, http.StatusUnprocessableEntity, "a used recovery code")
		if p := decodeProblem(t, w); len(p.Errors) != 1 || p.Errors[0].Field != "/code" {
			t.Errorf("used code: errors %+v, want one on /code", p.Errors)
		}
		expectStatus(t, e.do(http.MethodPost, path, `{"method":"recovery_code","code":"`+codes[1].Reveal()+`"}`, s.creds()), http.StatusOK, "a fresh recovery code")
		if ev := allEvents(t, e.db)[0]; ev.Action != "self.reauthenticate" || ev.Result != store.AuditSuccess || ev.Details["method"] != "recovery_code" {
			t.Errorf("reauthentication audited as %+v", ev)
		}
	})

	t.Run("throttled like login", func(t *testing.T) {
		e := newAuthEnvWith(t, api.Options{}, reauthPolicy)
		s := e.mustLogin()
		// One failed login and four wrong passwords: the account's five
		// failures.
		if _, w := e.login("admin@example.com", "wrong password", nil); w.Code != http.StatusUnauthorized {
			t.Fatalf("wrong login: %d", w.Code)
		}
		for range 4 {
			expectStatus(t, e.do(http.MethodPost, path, `{"method":"password","password":"wrong password"}`, s.creds()), http.StatusUnprocessableEntity, "wrong password")
		}
		w := e.do(http.MethodPost, path, `{"method":"password","password":"`+testPassword+`"}`, s.creds())
		expectStatus(t, w, http.StatusTooManyRequests, "right password while throttled")
		if w.Header().Get("Retry-After") == "" {
			t.Error("429 without Retry-After")
		}
		if _, w := e.login("admin@example.com", testPassword, nil); w.Code != http.StatusTooManyRequests {
			t.Errorf("login after failed reauthentications: %d, want 429", w.Code)
		}
	})

	t.Run("token callers", func(t *testing.T) {
		e := newAuthEnvWith(t, api.Options{}, reauthPolicy)
		s := e.mustLogin()
		_, token := e.createToken(s, `{"name":"ci","scope":"full"}`)
		expectStatus(t, e.do(http.MethodPost, path+"/options", "", creds{bearer: token}), http.StatusForbidden, "token options")
		expectStatus(t, e.do(http.MethodPost, path, `{"method":"password","password":"`+testPassword+`"}`, creds{bearer: token}), http.StatusForbidden, "token reauthenticates")
		if me := decodeMe(t, e.do(http.MethodGet, "/api/v1/auth/me", "", creds{bearer: token})); me.ReauthenticationMaxAgeSeconds != nil {
			t.Errorf("token reauthentication_max_age_seconds = %d, want null", *me.ReauthenticationMaxAgeSeconds)
		}
		// Long after the login, the token still takes sensitive actions.
		e.clock.add(time.Hour)
		expectStatus(t, e.do(http.MethodPost, "/api/v1/users",
			`{"email":"new@example.com","password":"`+testPassword+`","role":"viewer"}`, creds{bearer: token}), http.StatusCreated, "token creates a user")
	})

	t.Run("setting off", func(t *testing.T) {
		e := newAuthEnvWith(t, api.Options{}, auth.TwoFactorPolicy{ReauthenticationMaxAge: 10 * time.Minute})
		s := e.mustLogin()
		if me := decodeMe(t, e.do(http.MethodGet, "/api/v1/auth/me", "", s.creds())); me.ReauthenticationMaxAgeSeconds != nil {
			t.Errorf("reauthentication_max_age_seconds = %d with the setting off, want null", *me.ReauthenticationMaxAgeSeconds)
		}
		e.clock.add(time.Hour)
		e.createToken(s, `{"name":"ci","scope":"full"}`)
	})
}

// secondFactorSession logs the admin in with a recovery code.
func (e *authEnv) secondFactorSession(code string) session {
	e.t.Helper()
	w := e.secondStep(e.startTwoStepLogin(), `{"method":"recovery_code","code":"`+code+`"}`)
	expectStatus(e.t, w, http.StatusOK, "second step")
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

type optionsBody struct {
	Methods  []string        `json:"methods"`
	WebAuthn json.RawMessage `json:"webauthn"`
}

func decodeOptions(t *testing.T, w *httptest.ResponseRecorder) optionsBody {
	t.Helper()
	var o optionsBody
	if err := json.Unmarshal(w.Body.Bytes(), &o); err != nil {
		t.Fatal(err)
	}
	return o
}
