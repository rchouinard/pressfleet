package maintenance_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/maintenance"
)

func TestHandler(t *testing.T) {
	for _, tc := range []struct{ method, target string }{
		{http.MethodGet, "http://acme.preview.example/"},
		{http.MethodGet, "http://www.acme.example/wp-admin/?x=1"},
		{http.MethodPost, "http://www.acme.example/wp-login.php"},
		{http.MethodHead, "http://www.acme.example/"},
	} {
		t.Run(tc.method+" "+tc.target, func(t *testing.T) {
			w := httptest.NewRecorder()
			maintenance.Handler().ServeHTTP(w, httptest.NewRequest(tc.method, tc.target, nil))
			if w.Code != http.StatusServiceUnavailable {
				t.Errorf("status = %d, want 503", w.Code)
			}
			for header, want := range map[string]string{
				"Retry-After":   strconv.Itoa(maintenance.RetryAfter),
				"Cache-Control": "no-store",
				"Content-Type":  "text/html; charset=utf-8",
			} {
				if got := w.Header().Get(header); got != want {
					t.Errorf("%s = %q, want %q", header, got, want)
				}
			}
			body := w.Body.String()
			if tc.method == http.MethodHead {
				if body != "" {
					t.Errorf("HEAD body = %q", body)
				}
				return
			}
			if !strings.Contains(body, "<title>Down for maintenance</title>") || !strings.Contains(body, "PressFleet") {
				t.Errorf("body:\n%s", body)
			}
			if strconv.Itoa(len(body)) != w.Header().Get("Content-Length") {
				t.Errorf("Content-Length = %s, body is %d bytes", w.Header().Get("Content-Length"), len(body))
			}
			// Self-contained: nothing to load from the site or elsewhere.
			for _, ref := range []string{"src=", "href=", "url(", "@import"} {
				if strings.Contains(body, ref) {
					t.Errorf("the page loads something (%s)", ref)
				}
			}
		})
	}
}

func TestServeAndCheck(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- maintenance.Serve(ctx, addr, slog.New(slog.DiscardHandler)) }()

	url := "http://" + addr + "/"
	deadline := time.Now().Add(5 * time.Second)
	for {
		err := maintenance.Check(t.Context(), url)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("Check: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable || !strings.Contains(string(b), "maintenance") {
		t.Errorf("GET: %s %.100s", resp.Status, b)
	}

	cancel()
	if err := <-done; err != nil {
		t.Errorf("Serve after cancel: %v", err)
	}

	// Anything but the maintenance page fails the check.
	other := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer other.Close()
	if err := maintenance.Check(t.Context(), other.URL); !errors.Is(err, maintenance.ErrUnexpectedResponse) {
		t.Errorf("Check against another server: %v", err)
	}
}
