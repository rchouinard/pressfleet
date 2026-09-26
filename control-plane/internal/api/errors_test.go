package api

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rchouinard/pressfleet/control-plane/internal/httpserver"
)

func TestProblemFor(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		status   int
		expected bool
		detail   string
	}{
		{"unauthenticated", ErrUnauthenticated, http.StatusUnauthorized, true, "authentication required"},
		{"forbidden", fmt.Errorf("delete site: %w", ErrForbidden), http.StatusForbidden, true, "delete site: forbidden"},
		{"not found", fmt.Errorf("site %q: %w", "abc", ErrNotFound), http.StatusNotFound, true, `site "abc": not found`},
		{"conflict", ErrConflict, http.StatusConflict, true, "conflict"},
		{"gone", ErrGone, http.StatusGone, true, "no longer available"},
		{"not implemented", notImplemented(14), http.StatusNotImplemented, true, "not implemented yet (see https://github.com/rchouinard/pressfleet/issues/14)"},
		{"validation", &ValidationError{Fields: []httpserver.FieldError{{Field: "/x", Detail: "bad"}}}, http.StatusUnprocessableEntity, true, "The request failed validation."},
		{"problem", httpserver.NewProblem(http.StatusTeapot, "short and stout"), http.StatusTeapot, true, "short and stout"},
		{"too large", fmt.Errorf("decode: %w", &http.MaxBytesError{Limit: 10}), http.StatusRequestEntityTooLarge, true, "The request body exceeds the limit of 10 bytes."},
		{"unexpected", errors.New("db password=hunter2 rejected"), http.StatusInternalServerError, false, "An unexpected error occurred."},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, expected := problemFor(tt.err)
			if p.Status != tt.status || expected != tt.expected || p.Detail != tt.detail {
				t.Errorf("problemFor = (%d, %v, %q), want (%d, %v, %q)", p.Status, expected, p.Detail, tt.status, tt.expected, tt.detail)
			}
		})
	}
}

func TestUnexpectedHandlerErrorIsLoggedNotLeaked(t *testing.T) {
	var logs strings.Builder
	log := slog.New(slog.NewTextHandler(&logs, nil))
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/v1/sites", nil)
	responseErrorHandler(log)(w, r, errors.New("internal detail xyzzy"))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d", w.Code)
	}
	if strings.Contains(w.Body.String(), "xyzzy") {
		t.Errorf("response leaks the internal error: %s", w.Body)
	}
	if !strings.Contains(logs.String(), "xyzzy") {
		t.Errorf("internal error not logged: %s", logs.String())
	}
}

func TestAuthenticationRunsBeforeValidation(t *testing.T) {
	h, err := NewHandler(Options{Logger: slog.New(slog.DiscardHandler), Authenticator: rejectAll{}})
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, "/api/v1/sites", strings.NewReader(`{not json`))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 before any body validation; body: %s", w.Code, w.Body)
	}
}
