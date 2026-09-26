package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/api"
	"github.com/rchouinard/pressfleet/control-plane/internal/auth"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
)

const testPassword = "correct horse battery"

type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *testClock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// newAuthService returns an auth service on a fresh SQLite database, with
// cheap password hashing and a controllable clock. (The service's database
// code is tested on both dialects in packages store and auth.)
func newAuthService(t *testing.T) (*auth.Service, *testClock) {
	t.Helper()
	return newAuthServiceOn(storetest.Open(t, store.SQLite))
}

// newAuthServiceOn is newAuthService on db.
func newAuthServiceOn(db *store.DB) (*auth.Service, *testClock) {
	clk := &testClock{t: time.Now()}
	svc := auth.NewService(db, auth.Options{
		Logger:         storetest.Logger(),
		Now:            clk.now,
		PasswordParams: auth.Params{Memory: 64, Time: 1, Threads: 1, SaltLen: 16, KeyLen: 32},
	})
	return svc, clk
}

// authEnv is an API handler with real authentication and one admin user.
type authEnv struct {
	t     *testing.T
	h     http.Handler
	db    *store.DB
	svc   *auth.Service
	clock *testClock
	user  store.User
}

// newAuthEnv builds the environment on opts.DB, or a fresh SQLite database
// if it is nil.
func newAuthEnv(t *testing.T, opts api.Options) *authEnv {
	t.Helper()
	if opts.DB == nil {
		opts.DB = storetest.Open(t, store.SQLite)
	}
	svc, clk := newAuthServiceOn(opts.DB)
	u, err := svc.CreateUser(context.Background(), auth.NewUser{Email: "admin@example.com", Password: testPassword, Role: store.RoleAdmin})
	if err != nil {
		t.Fatal(err)
	}
	opts.Auth, opts.Authenticator = svc, api.NewAuthenticator(svc)
	return &authEnv{t: t, h: newHandler(t, opts), db: opts.DB, svc: svc, clock: clk, user: u}
}

// creds says how to authenticate a request.
type creds struct {
	cookie, csrf, bearer string
	header               map[string]string
}

func (e *authEnv) do(method, target, body string, c creds) *httptest.ResponseRecorder {
	e.t.Helper()
	var r *http.Request
	if body != "" {
		r = httptest.NewRequest(method, target, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	} else {
		r = httptest.NewRequest(method, target, nil)
	}
	if c.cookie != "" {
		r.AddCookie(&http.Cookie{Name: api.SessionCookieName, Value: c.cookie})
	}
	if c.csrf != "" {
		r.Header.Set(api.HeaderCSRFToken, c.csrf)
	}
	if c.bearer != "" {
		r.Header.Set("Authorization", "Bearer "+c.bearer)
	}
	for k, v := range c.header {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	e.h.ServeHTTP(w, r)
	return w
}

type session struct {
	cookie *http.Cookie
	csrf   string
}

func (s session) creds() creds { return creds{cookie: s.cookie.Value, csrf: s.csrf} }

func (e *authEnv) login(email, password string, header map[string]string) (session, *httptest.ResponseRecorder) {
	e.t.Helper()
	w := e.do(http.MethodPost, "/api/v1/auth/login", `{"email":"`+email+`","password":"`+password+`"}`, creds{header: header})
	if w.Code != http.StatusOK {
		return session{}, w
	}
	var body struct {
		CSRFToken  string `json:"csrf_token"`
		AuthMethod string `json:"auth_method"`
		ID         string `json:"id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		e.t.Fatal(err)
	}
	cookies := w.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != api.SessionCookieName || body.CSRFToken == "" || body.AuthMethod != "session" {
		e.t.Fatalf("login response: cookies %v, body %s", cookies, w.Body)
	}
	return session{cookie: cookies[0], csrf: body.CSRFToken}, w
}

func (e *authEnv) mustLogin() session {
	e.t.Helper()
	s, w := e.login("admin@example.com", testPassword, nil)
	if s.cookie == nil {
		e.t.Fatalf("login failed: %d %s", w.Code, w.Body)
	}
	return s
}

// createToken creates an API token with session s and returns its plaintext.
func (e *authEnv) createToken(s session, body string) (string, string) {
	e.t.Helper()
	w := e.do(http.MethodPost, "/api/v1/auth/tokens", body, s.creds())
	if w.Code != http.StatusCreated {
		e.t.Fatalf("create token: %d %s", w.Code, w.Body)
	}
	var tok struct{ ID, Token, Prefix, Scope string }
	if err := json.Unmarshal(w.Body.Bytes(), &tok); err != nil {
		e.t.Fatal(err)
	}
	if !strings.HasPrefix(tok.Token, "pft_") || !strings.HasPrefix(tok.Token, tok.Prefix) || w.Header().Get("Location") != "/api/v1/auth/tokens/"+tok.ID {
		e.t.Errorf("created token: %s (Location %q)", w.Body, w.Header().Get("Location"))
	}
	return tok.ID, tok.Token
}

func expectStatus(t *testing.T, w *httptest.ResponseRecorder, want int, what string) {
	t.Helper()
	if w.Code != want {
		t.Errorf("%s: status = %d, want %d; body: %s", what, w.Code, want, w.Body)
		return
	}
	if want >= 400 {
		decodeProblem(t, w)
	}
	if want == http.StatusUnauthorized && w.Header().Get("WWW-Authenticate") == "" {
		t.Errorf("%s: 401 without WWW-Authenticate", what)
	}
}

func TestLoginAndSessionCookie(t *testing.T) {
	e := newAuthEnv(t, api.Options{})
	s, w := e.login("Admin@Example.com", testPassword, nil)
	if s.cookie == nil {
		t.Fatalf("login: %d %s", w.Code, w.Body)
	}
	c := s.cookie
	if !c.HttpOnly || c.SameSite != http.SameSiteLaxMode || c.Path != "/" || c.Secure || c.Expires.IsZero() {
		t.Errorf("cookie flags over plain HTTP: %+v", c)
	}
	if len(c.Value) < 43 || strings.Contains(w.Body.String(), c.Value) {
		t.Errorf("cookie value %q (must be long and not in the body)", c.Value)
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("Cache-Control = %q", w.Header().Get("Cache-Control"))
	}

	w = e.do(http.MethodGet, "/api/v1/auth/me", "", creds{cookie: c.Value})
	expectStatus(t, w, http.StatusOK, "me")
	var me struct {
		Email, Role, AuthMethod string
		CSRFToken               *string `json:"csrf_token"`
		LastLoginAt             *string `json:"last_login_at"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &me)
	if me.Email != "admin@example.com" || me.Role != "admin" || me.CSRFToken == nil || *me.CSRFToken != s.csrf || me.LastLoginAt == nil {
		t.Errorf("me: %s", w.Body)
	}

	// Unknown email and wrong password get the same response.
	w1 := e.do(http.MethodPost, "/api/v1/auth/login", `{"email":"nobody@example.com","password":"`+testPassword+`"}`, creds{})
	w2 := e.do(http.MethodPost, "/api/v1/auth/login", `{"email":"admin@example.com","password":"wrong password"}`, creds{})
	expectStatus(t, w1, http.StatusUnauthorized, "unknown email")
	expectStatus(t, w2, http.StatusUnauthorized, "wrong password")
	p1, p2 := decodeProblem(t, w1), decodeProblem(t, w2)
	if p1.Detail != p2.Detail || p1.Title != p2.Title || p1.Detail == "" {
		t.Errorf("login failures differ: %+v vs %+v", p1, p2)
	}
}

func TestSecureCookie(t *testing.T) {
	proxy := []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24")} // httptest's RemoteAddr
	https := map[string]string{"X-Forwarded-Proto": "https"}
	tests := []struct {
		name   string
		opts   api.Options
		header map[string]string
		secure bool
	}{
		{"plain HTTP", api.Options{}, nil, false},
		{"forwarded HTTPS from trusted proxy", api.Options{TrustedProxies: proxy}, https, true},
		{"forwarded HTTPS from untrusted address", api.Options{}, https, false},
		{"forced", api.Options{SecureCookies: true}, nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newAuthEnv(t, tt.opts)
			s, w := e.login("admin@example.com", testPassword, tt.header)
			if s.cookie == nil {
				t.Fatalf("login: %d %s", w.Code, w.Body)
			}
			if s.cookie.Secure != tt.secure {
				t.Errorf("Secure = %v, want %v", s.cookie.Secure, tt.secure)
			}
		})
	}
}

func TestCSRF(t *testing.T) {
	e := newAuthEnv(t, api.Options{})
	s := e.mustLogin()
	body := `{"name":"ci","scope":"full"}`

	// Cookie-authenticated unsafe requests need the session's CSRF token.
	expectStatus(t, e.do(http.MethodPost, "/api/v1/auth/tokens", body, creds{cookie: s.cookie.Value}), http.StatusForbidden, "no CSRF header")
	expectStatus(t, e.do(http.MethodPost, "/api/v1/auth/tokens", body, creds{cookie: s.cookie.Value, csrf: "wrong"}), http.StatusForbidden, "wrong CSRF header")
	other := e.mustLogin()
	expectStatus(t, e.do(http.MethodPost, "/api/v1/auth/tokens", body, creds{cookie: s.cookie.Value, csrf: other.csrf}), http.StatusForbidden, "another session's CSRF token")
	expectStatus(t, e.do(http.MethodPost, "/api/v1/auth/logout", "", creds{cookie: s.cookie.Value}), http.StatusForbidden, "logout without CSRF")
	// Safe methods don't.
	expectStatus(t, e.do(http.MethodGet, "/api/v1/auth/tokens", "", creds{cookie: s.cookie.Value}), http.StatusOK, "GET without CSRF")

	_, token := e.createToken(s, body)

	// Cross-site browser requests are refused even with the token.
	crossSite := s.creds()
	crossSite.header = map[string]string{"Sec-Fetch-Site": "cross-site"}
	expectStatus(t, e.do(http.MethodPost, "/api/v1/auth/tokens", body, crossSite), http.StatusForbidden, "cross-site")
	crossSite.header = map[string]string{"Origin": "https://evil.example"}
	expectStatus(t, e.do(http.MethodPost, "/api/v1/auth/tokens", body, crossSite), http.StatusForbidden, "cross-origin Origin")
	if _, w := e.login("admin@example.com", testPassword, map[string]string{"Sec-Fetch-Site": "cross-site"}); w.Code != http.StatusForbidden {
		t.Errorf("cross-site login: status %d", w.Code)
	}
	sameOrigin := s.creds()
	sameOrigin.header = map[string]string{"Sec-Fetch-Site": "same-origin"}
	expectStatus(t, e.do(http.MethodPost, "/api/v1/auth/tokens", body, sameOrigin), http.StatusCreated, "same-origin")

	// Bearer requests need no CSRF token, and ignore any cookie.
	expectStatus(t, e.do(http.MethodPost, "/api/v1/auth/tokens", body, creds{bearer: token}), http.StatusCreated, "bearer without CSRF")
	expectStatus(t, e.do(http.MethodPost, "/api/v1/auth/tokens", body, creds{bearer: "pft_bogus", cookie: s.cookie.Value, csrf: s.csrf}),
		http.StatusUnauthorized, "bad bearer with a good cookie")
}

func TestAPITokensOverHTTP(t *testing.T) {
	e := newAuthEnv(t, api.Options{})
	s := e.mustLogin()

	id, token := e.createToken(s, `{"name":"metrics","scope":"read-only"}`)
	w := e.do(http.MethodGet, "/api/v1/auth/me", "", creds{bearer: token})
	expectStatus(t, w, http.StatusOK, "me with bearer")
	if !strings.Contains(w.Body.String(), `"auth_method":"token"`) || !strings.Contains(w.Body.String(), `"csrf_token":null`) {
		t.Errorf("me with bearer: %s", w.Body)
	}

	w = e.do(http.MethodGet, "/api/v1/auth/tokens", "", creds{bearer: token})
	expectStatus(t, w, http.StatusOK, "list")
	var list struct {
		Items []struct {
			ID, Scope, Prefix string
			LastUsedAt        *string `json:"last_used_at"`
		} `json:"items"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &list)
	if len(list.Items) != 1 || list.Items[0].ID != id || list.Items[0].Scope != "read-only" || list.Items[0].LastUsedAt == nil {
		t.Errorf("list: %s", w.Body)
	}
	if strings.Contains(w.Body.String(), token) {
		t.Error("list reveals the token")
	}

	// Expired tokens are rejected.
	exp := e.clock.now().Add(time.Hour).UTC().Format(time.RFC3339)
	_, expiring := e.createToken(s, `{"name":"short","scope":"full","expires_at":"`+exp+`"}`)
	expectStatus(t, e.do(http.MethodGet, "/api/v1/auth/me", "", creds{bearer: expiring}), http.StatusOK, "before expiry")
	expectStatus(t, e.do(http.MethodPost, "/api/v1/auth/tokens", `{"name":"x","scope":"full","expires_at":"2000-01-01T00:00:00Z"}`, s.creds()),
		http.StatusUnprocessableEntity, "expiry in the past")

	// Revoked tokens are rejected.
	expectStatus(t, e.do(http.MethodDelete, "/api/v1/auth/tokens/"+id, "", s.creds()), http.StatusNoContent, "revoke")
	expectStatus(t, e.do(http.MethodGet, "/api/v1/auth/me", "", creds{bearer: token}), http.StatusUnauthorized, "revoked token")
	expectStatus(t, e.do(http.MethodDelete, "/api/v1/auth/tokens/"+id, "", s.creds()), http.StatusNotFound, "revoke twice")

	e.clock.add(2 * time.Hour)
	expectStatus(t, e.do(http.MethodGet, "/api/v1/auth/me", "", creds{bearer: expiring}), http.StatusUnauthorized, "expired token")
	expectStatus(t, e.do(http.MethodGet, "/api/v1/auth/me", "", creds{bearer: "not-a-token"}), http.StatusUnauthorized, "garbage token")
	w = e.do(http.MethodGet, "/api/v1/auth/me", "", creds{header: map[string]string{"Authorization": "Basic YTpi"}})
	expectStatus(t, w, http.StatusUnauthorized, "basic auth")
}

func TestSessionLifecycle(t *testing.T) {
	e := newAuthEnv(t, api.Options{})
	s := e.mustLogin()

	// Idle expiry.
	e.clock.add(auth.DefaultSessionIdleTimeout + time.Minute)
	expectStatus(t, e.do(http.MethodGet, "/api/v1/auth/me", "", creds{cookie: s.cookie.Value}), http.StatusUnauthorized, "idle session")

	// Logout clears the cookie and ends the session.
	s = e.mustLogin()
	w := e.do(http.MethodPost, "/api/v1/auth/logout", "", s.creds())
	expectStatus(t, w, http.StatusNoContent, "logout")
	if cs := w.Result().Cookies(); len(cs) != 1 || cs[0].Name != api.SessionCookieName || cs[0].MaxAge >= 0 || cs[0].Value != "" {
		t.Errorf("logout cookie: %v", cs)
	}
	expectStatus(t, e.do(http.MethodGet, "/api/v1/auth/me", "", creds{cookie: s.cookie.Value}), http.StatusUnauthorized, "after logout")
}

func TestPasswordChangeRevokesOtherSessions(t *testing.T) {
	e := newAuthEnv(t, api.Options{})
	current, other := e.mustLogin(), e.mustLogin()
	_, token := e.createToken(current, `{"name":"ci","scope":"full"}`)

	w := e.do(http.MethodPost, "/api/v1/auth/password", `{"current_password":"wrong password","new_password":"new password 123"}`, current.creds())
	expectStatus(t, w, http.StatusUnprocessableEntity, "wrong current password")
	if p := decodeProblem(t, w); len(p.Errors) != 1 || p.Errors[0].Field != "/current_password" {
		t.Errorf("wrong current password: %s", w.Body)
	}
	expectStatus(t, e.do(http.MethodPost, "/api/v1/auth/password", `{"current_password":"`+testPassword+`","new_password":"short"}`, current.creds()),
		http.StatusUnprocessableEntity, "short new password")
	expectStatus(t, e.do(http.MethodPost, "/api/v1/auth/password", `{"current_password":"`+testPassword+`","new_password":"new password 123"}`, current.creds()),
		http.StatusNoContent, "change password")

	expectStatus(t, e.do(http.MethodGet, "/api/v1/auth/me", "", creds{cookie: current.cookie.Value}), http.StatusOK, "current session")
	expectStatus(t, e.do(http.MethodGet, "/api/v1/auth/me", "", creds{cookie: other.cookie.Value}), http.StatusUnauthorized, "other session")
	expectStatus(t, e.do(http.MethodGet, "/api/v1/auth/me", "", creds{bearer: token}), http.StatusUnauthorized, "token")
	if s, _ := e.login("admin@example.com", "new password 123", nil); s.cookie == nil {
		t.Error("can't log in with the new password")
	}
}

func TestDisabledUserRejectedEverywhere(t *testing.T) {
	e := newAuthEnv(t, api.Options{})
	admin := e.mustLogin()

	w := e.do(http.MethodPost, "/api/v1/users", `{"email":"op@example.com","password":"`+testPassword+`","role":"operator"}`, admin.creds())
	expectStatus(t, w, http.StatusCreated, "create user")
	var u struct{ ID, Role string }
	_ = json.Unmarshal(w.Body.Bytes(), &u)
	if u.Role != "operator" || w.Header().Get("Location") != "/api/v1/users/"+u.ID {
		t.Errorf("create user: %s", w.Body)
	}
	op, lw := e.login("op@example.com", testPassword, nil)
	if op.cookie == nil {
		t.Fatalf("operator login: %s", lw.Body)
	}
	_, token := e.createToken(op, `{"name":"ci","scope":"full"}`)

	expectStatus(t, e.do(http.MethodPatch, "/api/v1/users/"+u.ID, `{"disabled":true}`, admin.creds()), http.StatusOK, "disable")
	expectStatus(t, e.do(http.MethodGet, "/api/v1/auth/me", "", creds{cookie: op.cookie.Value}), http.StatusUnauthorized, "disabled user's session")
	expectStatus(t, e.do(http.MethodGet, "/api/v1/auth/me", "", creds{bearer: token}), http.StatusUnauthorized, "disabled user's token")
	if _, w := e.login("op@example.com", testPassword, nil); w.Code != http.StatusUnauthorized {
		t.Errorf("disabled user login: %d", w.Code)
	}

	// Other user management.
	expectStatus(t, e.do(http.MethodPatch, "/api/v1/users/"+u.ID, `{"role":"root"}`, admin.creds()), http.StatusUnprocessableEntity, "invalid role")
	expectStatus(t, e.do(http.MethodPatch, "/api/v1/users/"+u.ID, `{"email":"admin@example.com"}`, admin.creds()), http.StatusConflict, "duplicate email")
	expectStatus(t, e.do(http.MethodPost, "/api/v1/users", `{"email":"admin@example.com","password":"`+testPassword+`","role":"viewer"}`, admin.creds()),
		http.StatusConflict, "create duplicate")
	w = e.do(http.MethodGet, "/api/v1/users?role=operator&limit=1", "", admin.creds())
	expectStatus(t, w, http.StatusOK, "list users")
	if !strings.Contains(w.Body.String(), `"disabled":true`) || !strings.Contains(w.Body.String(), `"next_cursor":null`) || strings.Contains(w.Body.String(), "argon2") {
		t.Errorf("list users: %s", w.Body)
	}
	expectStatus(t, e.do(http.MethodGet, "/api/v1/users/"+u.ID, "", admin.creds()), http.StatusOK, "get user")
	expectStatus(t, e.do(http.MethodDelete, "/api/v1/users/"+u.ID, "", admin.creds()), http.StatusNoContent, "delete user")
	expectStatus(t, e.do(http.MethodGet, "/api/v1/users/"+u.ID, "", admin.creds()), http.StatusNotFound, "get deleted user")
}

func TestLoginThrottledOverHTTP(t *testing.T) {
	e := newAuthEnv(t, api.Options{})
	for range 5 {
		e.login("admin@example.com", "wrong password", nil)
	}
	_, w := e.login("admin@example.com", testPassword, nil)
	expectStatus(t, w, http.StatusTooManyRequests, "throttled login")
	if w.Header().Get("Retry-After") != "900" {
		t.Errorf("Retry-After = %q", w.Header().Get("Retry-After"))
	}
}

func TestFailsClosedWithoutAuthenticator(t *testing.T) {
	svc, _ := newAuthService(t)
	h := newHandler(t, api.Options{Auth: svc})
	ops, err := api.Operations()
	if err != nil {
		t.Fatal(err)
	}
	for _, op := range ops {
		if op.Public {
			continue
		}
		// Even requests that would satisfy a test authenticator are refused.
		w := do(t, h, request{method: op.Method, target: samplePath(op.Path), body: sampleBodies[op.ID], authed: true})
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s: status = %d, want 401", op.ID, w.Code)
			continue
		}
		decodeProblem(t, w)
		if w.Header().Get("WWW-Authenticate") == "" {
			t.Errorf("%s: 401 without WWW-Authenticate", op.ID)
		}
	}
	if w := do(t, h, request{method: http.MethodGet, target: "/healthz"}); w.Code != http.StatusOK {
		t.Errorf("healthz: %d", w.Code)
	}
}
