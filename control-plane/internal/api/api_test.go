package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/rchouinard/pressfleet/control-plane/internal/api"
	"github.com/rchouinard/pressfleet/control-plane/internal/auth"
	"github.com/rchouinard/pressfleet/control-plane/internal/httpserver"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
)

// problem is the decoded Problem Details body.
type problem struct {
	Type      string `json:"type"`
	Title     string `json:"title"`
	Status    int    `json:"status"`
	Detail    string `json:"detail"`
	Instance  string `json:"instance"`
	RequestID string `json:"request_id"`
	Errors    []struct {
		Field  string `json:"field"`
		Detail string `json:"detail"`
	} `json:"errors"`
}

// fakeAuth records calls and rejects requests without the magic header.
// Accepted requests carry principal, or an admin session if it is nil.
type fakeAuth struct {
	calls     []string
	principal *auth.Principal
}

func (f *fakeAuth) Authenticate(r *http.Request) (context.Context, error) {
	f.calls = append(f.calls, r.Pattern)
	if r.Header.Get("X-Test-Auth") != "ok" {
		return nil, api.ErrUnauthenticated
	}
	p := auth.Principal{UserID: "01j9zq3v6t8k4m2n5p7r9s1x3y", Role: store.RoleAdmin, Method: auth.MethodSession, SessionID: "test"}
	if f.principal != nil {
		p = *f.principal
	}
	return auth.WithPrincipal(r.Context(), p), nil
}

func newHandler(t *testing.T, opts api.Options) http.Handler {
	t.Helper()
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	h, err := api.NewHandler(opts)
	if err != nil {
		t.Fatal(err)
	}
	// Serve through the real middleware so request IDs and body limits apply.
	return httpserver.New(httpserver.Options{Handler: h, Logger: opts.Logger, MaxBodyBytes: 4096}).Handler()
}

type request struct {
	method, target, body, contentType string
	authed                            bool
}

func do(t *testing.T, h http.Handler, req request) *httptest.ResponseRecorder {
	t.Helper()
	var body io.Reader
	if req.body != "" {
		body = strings.NewReader(req.body)
	}
	r := httptest.NewRequest(req.method, req.target, body)
	if req.body != "" {
		ct := req.contentType
		if ct == "" {
			ct = "application/json"
		}
		r.Header.Set("Content-Type", ct)
	}
	if req.authed {
		r.Header.Set("X-Test-Auth", "ok")
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func decodeProblem(t *testing.T, w *httptest.ResponseRecorder) problem {
	t.Helper()
	if ct := w.Header().Get("Content-Type"); ct != httpserver.ContentTypeProblem {
		t.Fatalf("Content-Type = %q, want %q; body: %s", ct, httpserver.ContentTypeProblem, w.Body)
	}
	var p problem
	if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
		t.Fatalf("decoding problem: %v; body: %s", err, w.Body)
	}
	if p.Status != w.Code {
		t.Errorf("problem status %d != response status %d", p.Status, w.Code)
	}
	if p.Type == "" || p.Title == "" {
		t.Errorf("problem missing type or title: %+v", p)
	}
	if p.RequestID == "" || p.RequestID != w.Header().Get(httpserver.HeaderRequestID) {
		t.Errorf("problem request_id = %q, header = %q", p.RequestID, w.Header().Get(httpserver.HeaderRequestID))
	}
	return p
}

// samplePath fills in path parameters with a valid ID.
func samplePath(path string) string {
	for _, p := range []string{"{token_id}", "{user_id}", "{client_group_id}", "{site_id}", "{job_id}"} {
		path = strings.ReplaceAll(path, p, "abc123")
	}
	return path
}

// sampleBodies are minimal valid request bodies for operations that take one.
var sampleBodies = map[string]string{
	"login":             `{"email":"a@example.com","password":"x"}`,
	"createApiToken":    `{"name":"ci","scope":"read-only","expires_at":null}`,
	"createUser":        `{"email":"a@example.com","password":"correct horse battery","role":"viewer"}`,
	"changePassword":    `{"current_password":"wrong","new_password":"correct horse battery"}`,
	"updateUser":        `{"role":"operator"}`,
	"createClientGroup": `{"name":"Acme"}`,
	"updateClientGroup": `{"notes":"hello"}`,
	"createSite":        `{"client_group_id":"abc123","name":"Acme","admin_email":"a@example.com"}`,
	"updateSite":        `{"php_version":"8.4","resources":{"memory_limit_mb":256}}`,
}

func TestOpenAPIDocumentIsValid(t *testing.T) {
	// Operations loads and validates the embedded document.
	ops, err := api.Operations()
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, op := range ops {
		if op.ID == "" || seen[op.ID] {
			t.Errorf("%s: missing or duplicate operationId %q", op.Pattern(), op.ID)
		}
		seen[op.ID] = true
	}
}

func TestOperationsRequireAuthExceptLoginAndHealth(t *testing.T) {
	ops, err := api.Operations()
	if err != nil {
		t.Fatal(err)
	}
	public := []string{"getHealth", "getReadiness", "login"}
	var gotPublic []string
	for _, op := range ops {
		if op.Public {
			gotPublic = append(gotPublic, op.ID)
		}
		if strings.HasPrefix(op.Path, "/api/v1/") == slices.Contains([]string{"getHealth", "getReadiness"}, op.ID) {
			t.Errorf("%s (%s): only health probes may live outside /api/v1", op.ID, op.Path)
		}
	}
	slices.Sort(gotPublic)
	if !slices.Equal(gotPublic, public) {
		t.Errorf("public operations = %v, want %v", gotPublic, public)
	}
}

func TestEveryOperationIsRoutedAndAuthenticated(t *testing.T) {
	ops, err := api.Operations()
	if err != nil {
		t.Fatal(err)
	}
	svc, _ := newAuthService(t)
	u, err := svc.CreateUser(context.Background(), auth.NewUser{Email: "admin@example.com", Password: testPassword, Role: store.RoleAdmin})
	if err != nil {
		t.Fatal(err)
	}
	fa := &fakeAuth{principal: &auth.Principal{UserID: u.ID, Email: u.Email, Role: u.Role, Method: auth.MethodSession, SessionID: "none"}}
	db := storetest.Open(t, store.SQLite)
	h := newHandler(t, api.Options{Authenticator: fa, Auth: svc, DB: db, Sites: newSiteEnv(t, db, testPreviewBase).svc})
	for _, op := range ops {
		t.Run(op.ID, func(t *testing.T) {
			req := request{method: op.Method, target: samplePath(op.Path), body: sampleBodies[op.ID]}

			// Without credentials, only public operations get through.
			fa.calls = nil
			w := do(t, h, req)
			if op.Public {
				if len(fa.calls) != 0 {
					t.Errorf("authenticator called for public operation")
				}
				// Login answers 401 for the sample's wrong credentials.
				if w.Code == http.StatusUnauthorized && op.ID != "login" {
					t.Errorf("public operation returned 401")
				}
			} else {
				if len(fa.calls) != 1 || fa.calls[0] != op.Pattern() {
					t.Errorf("authenticator calls = %v, want [%s]", fa.calls, op.Pattern())
				}
				if w.Code != http.StatusUnauthorized {
					t.Fatalf("status = %d, want 401; body: %s", w.Code, w.Body)
				}
				decodeProblem(t, w)
				if w.Header().Get("WWW-Authenticate") == "" {
					t.Error("401 without WWW-Authenticate")
				}
			}

			// With credentials, every operation reaches its handler: health
			// and authentication work, everything else is a 501 until
			// implemented.
			req.authed = true
			w = do(t, h, req)
			switch {
			case op.ID == "getHealth" || op.ID == "getReadiness" || op.ID == "listJobs" || op.ID == "listAuditEvents":
				if w.Code != http.StatusOK {
					t.Errorf("status = %d, want 200; body: %s", w.Code, w.Body)
				}
			case op.ID == "getJob":
				if w.Code != http.StatusNotFound {
					t.Errorf("status = %d, want 404; body: %s", w.Code, w.Body)
				}
			case op.ID == "login":
				// The sample credentials are wrong.
				if w.Code != http.StatusUnauthorized {
					t.Errorf("status = %d, want 401; body: %s", w.Code, w.Body)
				}
			case strings.HasPrefix(op.Path, "/api/v1/auth/") || strings.HasPrefix(op.Path, "/api/v1/users") ||
				strings.HasPrefix(op.Path, "/api/v1/client-groups") || implementedSiteOps[op.ID]:
				if w.Code == http.StatusUnauthorized || w.Code >= 500 {
					t.Errorf("status = %d; body: %s", w.Code, w.Body)
				}
			default:
				if w.Code != http.StatusNotImplemented {
					t.Fatalf("status = %d, want 501; body: %s", w.Code, w.Body)
				}
				p := decodeProblem(t, w)
				if !strings.Contains(p.Detail, "github.com/rchouinard/pressfleet/issues/") {
					t.Errorf("501 detail doesn't reference an issue: %q", p.Detail)
				}
			}
		})
	}
}

func TestUnmatchedRoutes(t *testing.T) {
	h := newHandler(t, api.Options{})
	tests := []struct {
		name   string
		method string
		target string
		status int
		allow  string
	}{
		{"unknown path", http.MethodGet, "/nope", http.StatusNotFound, ""},
		{"unknown api path", http.MethodGet, "/api/v1/nope", http.StatusNotFound, ""},
		{"api v2", http.MethodGet, "/api/v2/sites", http.StatusNotFound, ""},
		{"trailing slash", http.MethodGet, "/api/v1/sites/", http.StatusNotFound, ""},
		{"wrong method", http.MethodPut, "/api/v1/sites", http.StatusMethodNotAllowed, "GET, HEAD, POST"},
		{"wrong method on health", http.MethodPost, "/healthz", http.StatusMethodNotAllowed, "GET, HEAD"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := do(t, h, request{method: tt.method, target: tt.target})
			if w.Code != tt.status {
				t.Fatalf("status = %d, want %d; body: %s", w.Code, tt.status, w.Body)
			}
			p := decodeProblem(t, w)
			if p.Instance != tt.target {
				t.Errorf("instance = %q, want %q", p.Instance, tt.target)
			}
			if got := w.Header().Get("Allow"); got != tt.allow {
				t.Errorf("Allow = %q, want %q", got, tt.allow)
			}
		})
	}
}

func TestRequestValidation(t *testing.T) {
	db := storetest.Open(t, store.SQLite)
	h := newHandler(t, api.Options{Authenticator: &fakeAuth{}, DB: db, Sites: newSiteEnv(t, db, testPreviewBase).svc})
	tests := []struct {
		name       string
		req        request
		status     int
		wantFields []string
	}{
		{
			// The handler answers: there is no such site.
			name:   "valid body reaches handler",
			req:    request{method: http.MethodPatch, target: "/api/v1/sites/abc123", body: sampleBodies["updateSite"]},
			status: http.StatusNotFound,
		},
		{
			name:   "malformed JSON",
			req:    request{method: http.MethodPost, target: "/api/v1/sites", body: `{"name":`},
			status: http.StatusBadRequest,
		},
		{
			name:   "missing body",
			req:    request{method: http.MethodPost, target: "/api/v1/client-groups"},
			status: http.StatusBadRequest,
		},
		{
			name:   "wrong content type",
			req:    request{method: http.MethodPost, target: "/api/v1/client-groups", body: `name=x`, contentType: "application/x-www-form-urlencoded"},
			status: http.StatusUnsupportedMediaType,
		},
		{
			name:       "schema violations",
			req:        request{method: http.MethodPost, target: "/api/v1/users", body: `{"email":"a@example.com","password":"s3cret-pw","role":"root"}`},
			status:     http.StatusUnprocessableEntity,
			wantFields: []string{"/password", "/role"},
		},
		{
			name:       "missing required field",
			req:        request{method: http.MethodPost, target: "/api/v1/client-groups", body: `{"notes":"x"}`},
			status:     http.StatusUnprocessableEntity,
			wantFields: []string{"/name"},
		},
		{
			name:       "unknown field",
			req:        request{method: http.MethodPatch, target: "/api/v1/sites/abc", body: `{"slug":"x"}`},
			status:     http.StatusUnprocessableEntity,
			wantFields: []string{"/slug"},
		},
		{
			name:       "nested field",
			req:        request{method: http.MethodPatch, target: "/api/v1/sites/abc", body: `{"resources":{"memory_limit_mb":1}}`},
			status:     http.StatusUnprocessableEntity,
			wantFields: []string{"/resources/memory_limit_mb"},
		},
		{
			name:       "query parameter out of range",
			req:        request{method: http.MethodGet, target: "/api/v1/jobs?limit=0"},
			status:     http.StatusBadRequest,
			wantFields: []string{"limit"},
		},
		{
			name:       "query parameter not in enum",
			req:        request{method: http.MethodGet, target: "/api/v1/sites?status=bogus"},
			status:     http.StatusBadRequest,
			wantFields: []string{"status"},
		},
		{
			name:       "path parameter pattern",
			req:        request{method: http.MethodGet, target: "/api/v1/sites/bad%20id"},
			status:     http.StatusBadRequest,
			wantFields: []string{"site_id"},
		},
		{
			name:   "body too large",
			req:    request{method: http.MethodPost, target: "/api/v1/client-groups", body: `{"name":"x","notes":"` + strings.Repeat("a", 5000) + `"}`},
			status: http.StatusRequestEntityTooLarge,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.req.authed = true
			w := do(t, h, tt.req)
			if w.Code != tt.status {
				t.Fatalf("status = %d, want %d; body: %s", w.Code, tt.status, w.Body)
			}
			p := decodeProblem(t, w)
			if strings.Contains(w.Body.String(), "s3cret-pw") || strings.Contains(w.Body.String(), "bad id'") {
				t.Errorf("problem echoes a submitted password: %s", w.Body)
			}
			var fields []string
			for _, e := range p.Errors {
				fields = append(fields, e.Field)
				if e.Detail == "" {
					t.Errorf("field %q has no detail", e.Field)
				}
			}
			slices.Sort(fields)
			if tt.wantFields != nil && !slices.Equal(fields, tt.wantFields) {
				t.Errorf("error fields = %q, want %q; body: %s", fields, tt.wantFields, w.Body)
			}
		})
	}
}

func TestReadiness(t *testing.T) {
	ready := &httpserver.Readiness{}
	h := newHandler(t, api.Options{Readiness: ready})

	w := do(t, h, request{method: http.MethodGet, target: "/readyz"})
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"status":"ok"`) {
		t.Fatalf("no checks: %d %s", w.Code, w.Body)
	}

	ready.Register("good", func(context.Context) error { return nil })
	ready.Register("database", func(context.Context) error { return errors.New("connection refused") })
	w = do(t, h, request{method: http.MethodGet, target: "/readyz"})
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body: %s", w.Code, w.Body)
	}
	var body struct {
		Status string `json:"status"`
		Checks []struct {
			Name, Status, Error string
		} `json:"checks"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Status != "unavailable" || len(body.Checks) != 2 ||
		body.Checks[0].Status != "ok" ||
		body.Checks[1].Name != "database" || body.Checks[1].Status != "failed" || body.Checks[1].Error != "connection refused" {
		t.Errorf("unexpected body: %s", w.Body)
	}

	// Liveness doesn't depend on readiness.
	if w := do(t, h, request{method: http.MethodGet, target: "/healthz"}); w.Code != http.StatusOK {
		t.Errorf("healthz status = %d", w.Code)
	}
}

func TestNewHandlerRequiresLogger(t *testing.T) {
	if _, err := api.NewHandler(api.Options{}); err == nil {
		t.Error("expected an error without a logger")
	}
}
