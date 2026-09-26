package api_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/api"
	"github.com/rchouinard/pressfleet/control-plane/internal/auth"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
)

func TestEveryProtectedOperationDeclaresAnAction(t *testing.T) {
	ops, err := api.Operations()
	if err != nil {
		t.Fatal(err)
	}
	for _, op := range ops {
		switch {
		case op.Public && op.Action != "":
			t.Errorf("public operation %s declares action %s", op.ID, op.Action)
		case !op.Public && !op.Action.Known():
			t.Errorf("operation %s has no known authorization action (%q)", op.ID, op.Action)
		}
	}
}

// TestEveryOperationChecksAuthorization calls every protected operation as
// a caller whose role allows nothing. A handler that forgets the
// authorization check answers something other than 403.
func TestEveryOperationChecksAuthorization(t *testing.T) {
	ops, err := api.Operations()
	if err != nil {
		t.Fatal(err)
	}
	svc, _ := newAuthService(t)
	nobody := &auth.Principal{UserID: "01j9zq3v6t8k4m2n5p7r9s1x3y", Role: "nobody", Method: auth.MethodSession, SessionID: "s"}
	h := newHandler(t, api.Options{Authenticator: &fakeAuth{principal: nobody}, Auth: svc, DB: storetest.Open(t, store.SQLite)})
	for _, op := range ops {
		if op.Public {
			continue
		}
		t.Run(op.ID, func(t *testing.T) {
			w := do(t, h, request{method: op.Method, target: samplePath(op.Path), body: sampleBodies[op.ID], authed: true})
			if w.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403: the handler must call authorize first; body: %s", w.Code, w.Body)
			}
			if p := decodeProblem(t, w); !strings.Contains(p.Detail, "nobody") {
				t.Errorf("403 detail doesn't say why: %q", p.Detail)
			}
		})
	}
}

// createUser creates a user with the admin session and returns its ID.
func (e *authEnv) createUser(admin session, email string, role store.Role) string {
	e.t.Helper()
	w := e.do(http.MethodPost, "/api/v1/users", `{"email":"`+email+`","password":"`+testPassword+`","role":"`+string(role)+`"}`, admin.creds())
	if w.Code != http.StatusCreated {
		e.t.Fatalf("create user %s: %d %s", email, w.Code, w.Body)
	}
	var u struct{ ID string }
	_ = json.Unmarshal(w.Body.Bytes(), &u)
	return u.ID
}

func (e *authEnv) mustLoginAs(email string) session {
	e.t.Helper()
	s, w := e.login(email, testPassword, nil)
	if s.cookie == nil {
		e.t.Fatalf("login %s: %d %s", email, w.Code, w.Body)
	}
	return s
}

// allowed reports whether the authorization check let the request through:
// anything a handler answers after it, such as 501 from a stub or 409 from a
// duplicate, means it did.
func allowed(t *testing.T, w interface{ Result() *http.Response }, what string) bool {
	t.Helper()
	switch code := w.Result().StatusCode; code {
	case http.StatusForbidden:
		return false
	case http.StatusOK, http.StatusCreated, http.StatusAccepted, http.StatusNoContent, http.StatusNotImplemented,
		http.StatusNotFound, http.StatusConflict, http.StatusGone, http.StatusUnprocessableEntity:
		return true
	default:
		t.Fatalf("%s: unexpected status %d", what, code)
		return false
	}
}

func TestRolesOverHTTP(t *testing.T) {
	e := newAuthEnv(t, api.Options{})
	admin := e.mustLogin()
	e.createUser(admin, "op@example.com", store.RoleOperator)
	e.createUser(admin, "viewer@example.com", store.RoleViewer)
	op, viewer := e.mustLoginAs("op@example.com"), e.mustLoginAs("viewer@example.com")
	_, opFull := e.createToken(op, `{"name":"full","scope":"full"}`)
	_, adminRO := e.createToken(admin, `{"name":"ro","scope":"read-only"}`)

	group := `{"name":"Acme"}`
	tests := []struct {
		name                 string
		method, target, body string
		c                    creds
		allowed              bool
	}{
		{"viewer lists sites", http.MethodGet, "/api/v1/sites", "", viewer.creds(), true},
		{"viewer creates client group", http.MethodPost, "/api/v1/client-groups", group, viewer.creds(), false},
		{"viewer lists users", http.MethodGet, "/api/v1/users", "", viewer.creds(), false},
		{"operator creates client group", http.MethodPost, "/api/v1/client-groups", group, op.creds(), true},
		{"operator token creates client group", http.MethodPost, "/api/v1/client-groups", group, creds{bearer: opFull}, true},
		{"operator deletes site", http.MethodDelete, "/api/v1/sites/abc123", "", op.creds(), false},
		{"operator creates user", http.MethodPost, "/api/v1/users", `{"email":"x@example.com","password":"` + testPassword + `","role":"admin"}`, op.creds(), false},
		{"admin deletes site", http.MethodDelete, "/api/v1/sites/abc123", "", admin.creds(), true},
		{"admin read-only token lists users", http.MethodGet, "/api/v1/users", "", creds{bearer: adminRO}, true},
		{"admin read-only token deletes site", http.MethodDelete, "/api/v1/sites/abc123", "", creds{bearer: adminRO}, false},
		{"admin read-only token creates token", http.MethodPost, "/api/v1/auth/tokens", `{"name":"x","scope":"read-only"}`, creds{bearer: adminRO}, false},
		{"admin read-only token changes password", http.MethodPost, "/api/v1/auth/password",
			`{"current_password":"` + testPassword + `","new_password":"another password"}`, creds{bearer: adminRO}, false},
		{"admin read-only token reads itself", http.MethodGet, "/api/v1/auth/me", "", creds{bearer: adminRO}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := e.do(tt.method, tt.target, tt.body, tt.c)
			if got := allowed(t, w, tt.name); got != tt.allowed {
				t.Fatalf("allowed = %v, want %v; status %d: %s", got, tt.allowed, w.Code, w.Body)
			}
			if !tt.allowed {
				decodeProblem(t, w)
			}
		})
	}
}

func TestDemotionRestrictsExistingSessionsAndTokens(t *testing.T) {
	e := newAuthEnv(t, api.Options{})
	admin := e.mustLogin()
	opID := e.createUser(admin, "op@example.com", store.RoleOperator)
	op := e.mustLoginAs("op@example.com")
	_, token := e.createToken(op, `{"name":"ci","scope":"full"}`)
	group := `{"name":"Acme"}`
	for name, c := range map[string]creds{"session": op.creds(), "token": {bearer: token}} {
		if !allowed(t, e.do(http.MethodPost, "/api/v1/client-groups", group, c), name) {
			t.Fatalf("operator %s can't create a client group before demotion", name)
		}
	}

	expectStatus(t, e.do(http.MethodPatch, "/api/v1/users/"+opID, `{"role":"viewer"}`, admin.creds()), http.StatusOK, "demote")

	for name, c := range map[string]creds{"session": op.creds(), "token": {bearer: token}} {
		expectStatus(t, e.do(http.MethodPost, "/api/v1/client-groups", group, c), http.StatusForbidden, name+" after demotion")
		expectStatus(t, e.do(http.MethodGet, "/api/v1/auth/me", "", c), http.StatusOK, name+" still authenticates")
	}
}

func TestLastAdminOverHTTP(t *testing.T) {
	e := newAuthEnv(t, api.Options{})
	admin := e.mustLogin()
	me := "/api/v1/users/" + e.user.ID
	expectStatus(t, e.do(http.MethodPatch, me, `{"role":"operator"}`, admin.creds()), http.StatusConflict, "demote the last admin")
	expectStatus(t, e.do(http.MethodPatch, me, `{"disabled":true}`, admin.creds()), http.StatusConflict, "disable the last admin")
	expectStatus(t, e.do(http.MethodDelete, me, "", admin.creds()), http.StatusConflict, "delete the last admin")

	e.createUser(admin, "second@example.com", store.RoleAdmin)
	expectStatus(t, e.do(http.MethodPatch, me, `{"role":"operator"}`, admin.creds()), http.StatusOK, "demote with another admin")
	// The demoted caller's own session can't manage users any more.
	expectStatus(t, e.do(http.MethodGet, "/api/v1/users", "", admin.creds()), http.StatusForbidden, "list users after self-demotion")
}

func TestTokenCantMintBroaderToken(t *testing.T) {
	e := newAuthEnv(t, api.Options{})
	s := e.mustLogin()
	exp := e.clock.now().Add(time.Hour).UTC()
	_, expiring := e.createToken(s, `{"name":"short","scope":"full","expires_at":"`+exp.Format(time.RFC3339)+`"}`)
	c := creds{bearer: expiring}

	expectStatus(t, e.do(http.MethodPost, "/api/v1/auth/tokens", `{"name":"forever","scope":"full"}`, c), http.StatusForbidden, "no expiry")
	later := exp.Add(time.Minute).Format(time.RFC3339)
	expectStatus(t, e.do(http.MethodPost, "/api/v1/auth/tokens", `{"name":"later","scope":"read-only","expires_at":"`+later+`"}`, c),
		http.StatusForbidden, "later expiry")
	sooner := exp.Add(-time.Minute).Format(time.RFC3339)
	expectStatus(t, e.do(http.MethodPost, "/api/v1/auth/tokens", `{"name":"sooner","scope":"read-only","expires_at":"`+sooner+`"}`, c),
		http.StatusCreated, "sooner expiry")
	// Sessions aren't limited.
	expectStatus(t, e.do(http.MethodPost, "/api/v1/auth/tokens", `{"name":"forever","scope":"full"}`, s.creds()), http.StatusCreated, "session")
}
