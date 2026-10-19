package httpserver_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/httpserver"
)

// syncBuffer is a bytes.Buffer safe for concurrent use.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func jsonLogger() (*slog.Logger, *syncBuffer) {
	var buf syncBuffer
	return slog.New(slog.NewJSONHandler(&buf, nil)), &buf
}

type problem struct {
	Type, Title, Detail, Instance string
	Status                        int
	RequestID                     string `json:"request_id"`
}

func decodeProblem(t *testing.T, w *httptest.ResponseRecorder) problem {
	t.Helper()
	if ct := w.Header().Get("Content-Type"); ct != httpserver.ContentTypeProblem {
		t.Fatalf("Content-Type = %q; body: %s", ct, w.Body)
	}
	var p problem
	if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
		t.Fatalf("decoding problem: %v; body: %s", err, w.Body)
	}
	return p
}

func serve(h http.Handler, r *http.Request) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestRequestID(t *testing.T) {
	var seen string
	h := httpserver.RequestID(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		seen = httpserver.RequestIDFromContext(r.Context())
	}))
	tests := []struct {
		name     string
		incoming string
		keep     bool
	}{
		{"generated when absent", "", false},
		{"accepted when well-formed", "edge-proxy.1234:abcd_EF", true},
		{"replaced when too long", strings.Repeat("a", 129), false},
		{"replaced when it has unsafe characters", "abc\ndef", false},
		{"replaced when it has spaces", "abc def", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			if tt.incoming != "" {
				r.Header.Set(httpserver.HeaderRequestID, tt.incoming)
			}
			w := serve(h, r)
			got := w.Header().Get(httpserver.HeaderRequestID)
			if got == "" || got != seen {
				t.Fatalf("response header %q, context %q", got, seen)
			}
			if (got == tt.incoming) != tt.keep {
				t.Errorf("request ID = %q, incoming %q, keep = %v", got, tt.incoming, tt.keep)
			}
		})
	}
}

func TestAccessLogNeverLogsCredentials(t *testing.T) {
	log, buf := jsonLogger()
	var handlerLog *slog.Logger
	h := httpserver.Chain(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handlerLog = httpserver.LoggerFromContext(r.Context(), nil)
		_, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("hello"))
	}), httpserver.RequestID, httpserver.AccessLog(log))

	r := httptest.NewRequest(http.MethodPost, "/api/v1/sites?token=querysecret", strings.NewReader(`{"password":"bodysecret"}`))
	r.Header.Set("Authorization", "Bearer headersecret")
	r.Header.Set("Cookie", "pressfleet_session=cookiesecret")
	r.Header.Set(httpserver.HeaderRequestID, "req-1")
	serve(h, r)

	out := buf.String()
	for _, secret := range []string{"querysecret", "bodysecret", "headersecret", "cookiesecret"} {
		if strings.Contains(out, secret) {
			t.Errorf("access log contains %q:\n%s", secret, out)
		}
	}
	var entry map[string]any
	if err := json.Unmarshal([]byte(out), &entry); err != nil {
		t.Fatalf("want exactly one JSON log line: %v\n%s", err, out)
	}
	for k, want := range map[string]any{
		"msg": "http request", "method": "POST", "path": "/api/v1/sites",
		"status": float64(201), "bytes": float64(5), "request_id": "req-1",
	} {
		if entry[k] != want {
			t.Errorf("log %s = %v, want %v", k, entry[k], want)
		}
	}
	if handlerLog == nil {
		t.Fatal("no request-scoped logger in context")
	}
}

func TestRecover(t *testing.T) {
	log, buf := jsonLogger()
	h := httpserver.Chain(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom")
	}), httpserver.RequestID, httpserver.AccessLog(log), httpserver.Recover(log))

	r := httptest.NewRequest(http.MethodGet, "/explode", nil)
	w := serve(h, r)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d", w.Code)
	}
	p := decodeProblem(t, w)
	if p.Status != 500 || p.Instance != "/explode" || p.RequestID != w.Header().Get(httpserver.HeaderRequestID) {
		t.Errorf("unexpected problem: %+v", p)
	}
	if strings.Contains(w.Body.String(), "boom") {
		t.Errorf("panic value leaked to client: %s", w.Body)
	}
	out := buf.String()
	for _, want := range []string{`"msg":"panic serving request"`, `"panic":"boom"`, `"stack":"goroutine`, `"request_id":"` + p.RequestID, `"status":500`} {
		if !strings.Contains(out, want) {
			t.Errorf("log missing %s:\n%s", want, out)
		}
	}
}

func TestMaxBodyBytes(t *testing.T) {
	var readErr error
	h := httpserver.MaxBodyBytes(4)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		_, readErr = io.ReadAll(r.Body)
	}))
	serve(h, httptest.NewRequest(http.MethodPost, "/", strings.NewReader("12345")))
	var mbe *http.MaxBytesError
	if !errors.As(readErr, &mbe) {
		t.Errorf("read error = %v, want *http.MaxBytesError", readErr)
	}
}

func TestSetBodyLimit(t *testing.T) {
	for _, tt := range []struct {
		limit int64
		ok    bool
	}{{4, false}, {5, true}} {
		var (
			body    []byte
			readErr error
		)
		h := httpserver.MaxBodyBytes(2)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			httpserver.SetBodyLimit(w, r, tt.limit)
			body, readErr = io.ReadAll(r.Body)
		}))
		serve(h, httptest.NewRequest(http.MethodPost, "/", strings.NewReader("12345")))
		var mbe *http.MaxBytesError
		switch {
		case tt.ok && (readErr != nil || string(body) != "12345"):
			t.Errorf("limit %d: read %q, %v", tt.limit, body, readErr)
		case !tt.ok && !errors.As(readErr, &mbe):
			t.Errorf("limit %d: read error = %v, want *http.MaxBytesError", tt.limit, readErr)
		}
	}
}

func TestProblemRouter(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /things/{id}", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("id") == "missing" {
			// Handlers own their 404s; the router must not rewrite them.
			http.Error(w, "custom", http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte("thing"))
	})
	h := httpserver.RequestID(httpserver.ProblemRouter(mux))

	tests := []struct {
		name, method, target string
		status               int
		problem              bool
		allow                string
	}{
		{"match", http.MethodGet, "/things/1", http.StatusOK, false, ""},
		{"handler 404 untouched", http.MethodGet, "/things/missing", http.StatusNotFound, false, ""},
		{"unknown path", http.MethodGet, "/nope", http.StatusNotFound, true, ""},
		{"wrong method", http.MethodDelete, "/things/1", http.StatusMethodNotAllowed, true, "GET, HEAD"},
		{"path cleaning redirect", http.MethodGet, "/things/../things/1", http.StatusTemporaryRedirect, false, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest(tt.method, "/", nil)
			r.URL.Path = tt.target
			w := serve(h, r)
			if w.Code != tt.status {
				t.Fatalf("status = %d, want %d; body: %s", w.Code, tt.status, w.Body)
			}
			isProblem := w.Header().Get("Content-Type") == httpserver.ContentTypeProblem
			if isProblem != tt.problem {
				t.Fatalf("problem = %v, want %v; body: %s", isProblem, tt.problem, w.Body)
			}
			if tt.problem {
				p := decodeProblem(t, w)
				if p.Status != tt.status || p.RequestID == "" || strings.Contains(w.Body.String(), "404 page not found") {
					t.Errorf("unexpected problem body: %s", w.Body)
				}
			}
			if got := w.Header().Get("Allow"); got != tt.allow {
				t.Errorf("Allow = %q, want %q", got, tt.allow)
			}
		})
	}
}

func TestWriteProblem(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/x", nil)
	w := httptest.NewRecorder()
	p := &httpserver.Problem{Status: http.StatusConflict, Detail: "taken"}
	httpserver.WriteProblem(w, r, p)
	got := decodeProblem(t, w)
	if got.Type != "about:blank" || got.Title != "Conflict" || got.Status != 409 || got.Detail != "taken" || got.Instance != "/x" {
		t.Errorf("unexpected problem: %+v", got)
	}
	if p.Instance != "" {
		t.Error("WriteProblem modified its argument")
	}
}

func TestWriteProblemExtensions(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/login", nil)
	w := httptest.NewRecorder()
	p := &httpserver.Problem{Type: "second-factor-required", Status: http.StatusUnauthorized, Extensions: map[string]any{
		"methods": []string{"totp"},
		// Extensions can't replace standard members, set or not.
		"status": 200, "detail": "forged",
	}}
	httpserver.WriteProblem(w, r, p)
	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	methods, _ := got["methods"].([]any)
	if got["type"] != "second-factor-required" || got["status"] != float64(401) || got["instance"] != "/login" ||
		len(methods) != 1 || methods[0] != "totp" {
		t.Errorf("unexpected problem: %v", got)
	}
	if _, ok := got["detail"]; ok {
		t.Errorf("an extension set the detail: %v", got)
	}
	// Without extensions, the output is unchanged.
	plain, err := json.Marshal(httpserver.Problem{Status: 404, Title: "Not Found"})
	if err != nil || string(plain) != `{"type":"","title":"Not Found","status":404}` {
		t.Errorf("Marshal = %s, %v", plain, err)
	}
}

func TestReadiness(t *testing.T) {
	var ready httpserver.Readiness
	if res := ready.Check(context.Background()); len(res) != 0 {
		t.Errorf("zero value has checks: %v", res)
	}
	ready.Timeout = 20 * time.Millisecond
	ready.Register("ok", func(context.Context) error { return nil })
	ready.Register("slow", func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() })
	ready.Register("broken", func(context.Context) error { return errors.New("down") })

	res := ready.Check(context.Background())
	if len(res) != 3 || res[0].Name != "ok" || res[0].Err != nil ||
		res[1].Name != "slow" || !errors.Is(res[1].Err, context.DeadlineExceeded) ||
		res[2].Name != "broken" || res[2].Err == nil {
		t.Errorf("unexpected results: %+v", res)
	}
}

func newServer(t *testing.T, h http.Handler) (*httpserver.Server, net.Listener, *syncBuffer) {
	t.Helper()
	log, buf := jsonLogger()
	srv := httpserver.New(httpserver.Options{Handler: h, Logger: log, ShutdownTimeout: 2 * time.Second})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	return srv, ln, buf
}

func TestServerGracefulShutdown(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/slow" {
			close(started)
			<-release
		}
		_, _ = w.Write([]byte("done"))
	})
	srv, ln, logs := newServer(t, h)
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ctx, ln) }()

	// One connection per request: a pooled client may dial a spare connection
	// that sits unused, which Shutdown only reaps after 5s.
	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	url := "http://" + ln.Addr().String()
	resp, err := client.Get(url + "/fast")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.Header.Get(httpserver.HeaderRequestID) == "" {
		t.Error("standard middleware not applied")
	}

	// Start a slow request, then begin shutdown while it is in flight.
	slow := make(chan string, 1)
	go func() {
		resp, err := client.Get(url + "/slow")
		if err != nil {
			slow <- "error: " + err.Error()
			return
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		slow <- string(b)
	}()
	<-started
	cancel()
	// Give Shutdown a moment to begin, so the request is truly in flight
	// during shutdown rather than merely before it.
	time.Sleep(50 * time.Millisecond)

	close(release)
	if got := <-slow; got != "done" {
		t.Errorf("in-flight request = %q, want it to complete", got)
	}
	if err := <-served; err != nil {
		t.Errorf("Serve = %v, want nil", err)
	}
	if conn, err := net.Dial("tcp", ln.Addr().String()); err == nil {
		_ = conn.Close()
		t.Error("listener still accepting after shutdown")
	}
	for _, want := range []string{"http server listening", "http server shutting down", "http server stopped"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("logs missing %q:\n%s", want, logs.String())
		}
	}
}

func TestServerShutdownTimeout(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	h := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		close(started)
		<-release
	})
	log, _ := jsonLogger()
	srv := httpserver.New(httpserver.Options{Handler: h, Logger: log, ShutdownTimeout: 50 * time.Millisecond})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ctx, ln) }()
	go func() {
		if resp, err := http.Get("http://" + ln.Addr().String()); err == nil {
			_ = resp.Body.Close()
		}
	}()
	<-started
	cancel()
	if err := <-served; !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Serve = %v, want a shutdown deadline error", err)
	}
}

func TestRunFailsWhenAddressInUse(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	log, _ := jsonLogger()
	srv := httpserver.New(httpserver.Options{Addr: ln.Addr().String(), Handler: http.NotFoundHandler(), Logger: log})
	done := make(chan error, 1)
	go func() { done <- srv.Run(context.Background()) }()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "listening on") {
			t.Errorf("Run = %v, want a listen error", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not fail")
	}
}

func TestRunWithCancelledContext(t *testing.T) {
	log, _ := jsonLogger()
	srv := httpserver.New(httpserver.Options{Addr: "127.0.0.1:0", Handler: http.NotFoundHandler(), Logger: log})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := srv.Run(ctx); err != nil {
		t.Errorf("Run = %v, want nil", err)
	}
}
