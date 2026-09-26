package api

import (
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"strconv"

	"github.com/rchouinard/pressfleet/control-plane/internal/auth"
	"github.com/rchouinard/pressfleet/control-plane/internal/httpserver"
)

// Sentinel errors that handlers return (wrapped with %w for context) to
// select the response status. The wrapped message becomes the problem's
// detail, so it must not contain secrets. Any other error is a 500 whose
// details are logged but not sent to the client.
var (
	// ErrUnauthenticated: the caller isn't authenticated (401).
	ErrUnauthenticated = errors.New("authentication required")
	// ErrForbidden: the caller's role or token scope doesn't allow the
	// action (403). The authorization check (#103) returns it.
	ErrForbidden = errors.New("forbidden")
	// ErrNotFound: the target doesn't exist (404).
	ErrNotFound = errors.New("not found")
	// ErrConflict: the request conflicts with current state (409).
	ErrConflict = errors.New("conflict")
	// ErrGone: a one-time resource has already been consumed (410).
	ErrGone = errors.New("no longer available")
	// ErrNotImplemented: the endpoint isn't built yet (501).
	ErrNotImplemented = errors.New("not implemented")
)

// ValidationError reports semantic validation failures that the OpenAPI
// schema can't express, such as a memory limit below the reservation (422).
type ValidationError struct {
	Fields []httpserver.FieldError
}

func (e *ValidationError) Error() string {
	return fmt.Sprintf("validation failed on %d field(s)", len(e.Fields))
}

// notImplemented is returned by handlers whose feature lands in a later
// issue.
func notImplemented(issue int) error {
	return fmt.Errorf("%w yet (see https://github.com/rchouinard/pressfleet/issues/%d)", ErrNotImplemented, issue)
}

var sentinelStatus = []struct {
	err    error
	status int
}{
	{ErrUnauthenticated, http.StatusUnauthorized},
	{ErrForbidden, http.StatusForbidden},
	{ErrNotFound, http.StatusNotFound},
	{ErrConflict, http.StatusConflict},
	{ErrGone, http.StatusGone},
	{ErrNotImplemented, http.StatusNotImplemented},
	{auth.ErrThrottled, http.StatusTooManyRequests},
}

// problemFor converts a handler error to Problem Details. The second result
// is false for unexpected errors, which should be logged.
func problemFor(err error) (*httpserver.Problem, bool) {
	var p *httpserver.Problem
	if errors.As(err, &p) {
		return p, true
	}
	var ve *ValidationError
	if errors.As(err, &ve) {
		p := httpserver.NewProblem(http.StatusUnprocessableEntity, "The request failed validation.")
		p.Errors = ve.Fields
		return p, true
	}
	var mbe *http.MaxBytesError
	if errors.As(err, &mbe) {
		return httpserver.NewProblem(http.StatusRequestEntityTooLarge,
			fmt.Sprintf("The request body exceeds the limit of %d bytes.", mbe.Limit)), true
	}
	for _, s := range sentinelStatus {
		if errors.Is(err, s.err) {
			return httpserver.NewProblem(s.status, err.Error()), true
		}
	}
	return httpserver.NewProblem(http.StatusInternalServerError, "An unexpected error occurred."), false
}

// writeError writes err as Problem Details, logging unexpected errors.
func writeError(log *slog.Logger, w http.ResponseWriter, r *http.Request, err error) {
	p, expected := problemFor(err)
	if !expected {
		httpserver.LoggerFromContext(r.Context(), log).Error("request failed",
			"method", r.Method, "path", r.URL.Path, "error", err)
	}
	if p.Status == http.StatusUnauthorized {
		// RFC 9110 requires a challenge on 401; bearer is the scheme
		// automation clients can act on.
		w.Header().Set("WWW-Authenticate", `Bearer realm="pressfleet"`)
	}
	var te *auth.ThrottledError
	if errors.As(err, &te) {
		w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(te.RetryAfter.Seconds()))))
	}
	httpserver.WriteProblem(w, r, p)
}

// requestErrorHandler handles errors from parameter binding and body
// decoding in the generated code: the request is malformed (400), or too
// large (413).
func requestErrorHandler(log *slog.Logger) func(http.ResponseWriter, *http.Request, error) {
	return func(w http.ResponseWriter, r *http.Request, err error) {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeError(log, w, r, err)
			return
		}
		writeError(log, w, r, httpserver.NewProblem(http.StatusBadRequest, err.Error()))
	}
}

// responseErrorHandler handles errors returned by handlers.
func responseErrorHandler(log *slog.Logger) func(http.ResponseWriter, *http.Request, error) {
	return func(w http.ResponseWriter, r *http.Request, err error) {
		writeError(log, w, r, err)
	}
}
