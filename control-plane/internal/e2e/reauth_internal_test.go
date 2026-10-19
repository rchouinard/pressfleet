package e2e

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/rchouinard/pressfleet/control-plane/internal/api"
	"github.com/rchouinard/pressfleet/control-plane/internal/api/client"
)

// fakeControlPlane answers login, refuses the first client group creation
// with reauthentication-required, and accepts reauthentication with the
// right password and CSRF token.
type fakeControlPlane struct {
	mu      sync.Mutex
	fresh   bool
	reauths int
	bodies  []string
}

const (
	fakePassword = "correct horse battery"
	fakeCSRF     = "csrf-token"
	fakeSession  = "session-token"
)

func (f *fakeControlPlane) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	authed := func() bool {
		c, err := r.Cookie(api.SessionCookieName)
		return err == nil && c.Value == fakeSession && r.Header.Get("X-CSRF-Token") == fakeCSRF
	}
	switch r.Method + " " + r.URL.Path {
	case "POST /api/v1/auth/login":
		http.SetCookie(w, &http.Cookie{Name: api.SessionCookieName, Value: fakeSession, Path: "/"})
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"u","auth_method":"session","csrf_token":"`+fakeCSRF+`"}`)
	case "POST /api/v1/auth/reauthenticate":
		var body struct{ Method, Password string }
		if !authed() || json.NewDecoder(r.Body).Decode(&body) != nil || body.Method != "password" || body.Password != fakePassword {
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = io.WriteString(w, `{"type":"about:blank","status":422}`)
			return
		}
		f.reauths++
		f.fresh = true
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"u","auth_method":"session","csrf_token":"`+fakeCSRF+`"}`)
	case "POST /api/v1/client-groups":
		b, _ := io.ReadAll(r.Body)
		f.bodies = append(f.bodies, string(b))
		if !authed() {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if !f.fresh {
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, `{"type":"`+api.ReauthenticationRequiredProblemType+`","status":403}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"id":"g","name":"Acme"}`)
	case "DELETE /api/v1/client-groups/plain":
		// A refusal that isn't about reauthentication passes through.
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"type":"about:blank","status":403,"detail":"the viewer role doesn't allow client_group.delete"}`)
	default:
		http.NotFound(w, r)
	}
}

func TestLoginClientReauthenticatesOnDemand(t *testing.T) {
	fake := &fakeControlPlane{}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	p := &Platform{BaseURL: srv.URL}
	c := p.Login(t, "admin@example.com", fakePassword)

	resp, err := c.CreateClientGroupWithResponse(t.Context(), client.CreateClientGroupJSONRequestBody{Name: "Acme"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode() != http.StatusCreated {
		t.Fatalf("create after reauthenticating: status %d: %s", resp.StatusCode(), resp.Body)
	}
	if fake.reauths != 1 {
		t.Errorf("reauthenticated %d times, want once", fake.reauths)
	}
	if len(fake.bodies) != 2 || fake.bodies[0] == "" || fake.bodies[1] != fake.bodies[0] {
		t.Errorf("request bodies = %q, want the same body twice", fake.bodies)
	}

	// Other refusals reach the caller whole, without reauthenticating.
	del, err := c.DeleteClientGroupWithResponse(t.Context(), "plain", nil)
	if err != nil {
		t.Fatal(err)
	}
	if del.StatusCode() != http.StatusForbidden || del.ApplicationProblemJSON403 == nil || del.ApplicationProblemJSON403.Detail == nil {
		t.Errorf("plain refusal: status %d: %s", del.StatusCode(), del.Body)
	}
	if fake.reauths != 1 {
		t.Errorf("a plain 403 reauthenticated (%d times)", fake.reauths)
	}
}

func TestReauthTransportGivesUpAfterOneRetry(t *testing.T) {
	var calls, reauths int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/problem+json; charset=utf-8")
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"type":"`+api.ReauthenticationRequiredProblemType+`","status":403}`)
	}))
	defer srv.Close()
	rt := &reauthTransport{base: http.DefaultTransport, reauthenticate: func(_ context.Context) error { reauths++; return nil }}
	resp, err := (&http.Client{Transport: rt}).Post(srv.URL, "application/json", strings.NewReader(`{"x":1}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(string(b), api.ReauthenticationRequiredProblemType) {
		t.Errorf("after the retry: %d %s, want the second refusal whole", resp.StatusCode, b)
	}
	if calls != 2 || reauths != 1 {
		t.Errorf("%d requests and %d reauthentications, want 2 and 1", calls, reauths)
	}
}
