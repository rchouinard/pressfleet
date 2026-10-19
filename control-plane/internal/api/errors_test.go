package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rchouinard/pressfleet/control-plane/internal/auth"
	"github.com/rchouinard/pressfleet/control-plane/internal/authz"
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
	h, err := newHandler(Options{Logger: slog.New(slog.DiscardHandler), Authenticator: rejectAll{}}, false)
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

// TestTypedProblems checks the problem types of errors that callers branch
// on, also when a handler wraps them in a sentinel.
func TestTypedProblems(t *testing.T) {
	tests := []struct {
		name   string
		err    error
		status int
		typ    string
	}{
		{"last second factor", fmt.Errorf("remove TOTP: %w", auth.ErrLastSecondFactor), http.StatusConflict, LastSecondFactorProblemType},
		{"last second factor as a conflict", fmt.Errorf("%w: %w", ErrConflict, auth.ErrLastSecondFactor), http.StatusConflict, LastSecondFactorProblemType},
		{"enrollment required", forbidden(fmt.Errorf("check: %w", authz.ErrEnrollmentRequired)), http.StatusForbidden, TwoFactorEnrollmentRequiredProblemType},
		{"other denial", forbidden(authz.ErrDenied), http.StatusForbidden, "about:blank"},
		{"other conflict", fmt.Errorf("%w: %w", ErrConflict, auth.ErrLastAdmin), http.StatusConflict, "about:blank"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			responseErrorHandler(slog.New(slog.DiscardHandler))(w, httptest.NewRequest(http.MethodDelete, "/api/v1/users/me/two-factor/totp", nil), tt.err)
			var p httpserver.Problem
			if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
				t.Fatal(err)
			}
			if w.Code != tt.status || p.Status != tt.status || p.Type != tt.typ || w.Header().Get("Content-Type") != httpserver.ContentTypeProblem {
				t.Errorf("response %d %s, want %d with type %s", w.Code, w.Body, tt.status, tt.typ)
			}
		})
	}
}
