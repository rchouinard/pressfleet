// Package api implements the control plane's HTTP API (api/openapi.yaml).
//
// The router, request types, and response types are generated into package
// gen. This package provides the handler implementations (the Handlers
// methods, one file per area) and the per-operation middleware that runs
// between routing and the handler:
//
//  1. authentication, for every operation that isn't Public (see
//     Authenticator; implemented in #13);
//  2. request validation against the OpenAPI document;
//  3. JSON decoding (generated), then the handler.
//
// # Writing a handler
//
// Handlers implement gen.StrictServerInterface. Adding or implementing an
// endpoint never touches the router: edit the spec, run `make generate`, and
// fill in the method. Every handler must start with the single authorization
// check (design doc §4.6), before reading or changing anything. #103 adds it;
// the intended shape is:
//
//	if err := h.authorize(ctx, action, target); err != nil {
//		return nil, err // wraps ErrForbidden -> 403
//	}
//
// Handlers report failures by returning errors that wrap the sentinels in
// errors.go (or a *httpserver.Problem, or a *ValidationError). They are
// rendered as RFC 9457 Problem Details; any other error becomes a logged 500.
package api

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/rchouinard/pressfleet/control-plane/internal/api/gen"
	"github.com/rchouinard/pressfleet/control-plane/internal/httpserver"
)

// Authenticator identifies the caller of an operation that requires
// authentication (#13: session cookies and bearer API tokens).
type Authenticator interface {
	// Authenticate returns a context carrying the caller's identity, or an
	// error wrapping ErrUnauthenticated if the request has no valid
	// credentials.
	Authenticate(r *http.Request) (context.Context, error)
}

// Options configures the API handler.
type Options struct {
	// Logger is required.
	Logger *slog.Logger
	// Readiness holds the checks behind GET /readyz. Nil means no checks.
	Readiness *httpserver.Readiness
	// Authenticator authenticates every non-public operation. If nil,
	// requests reach handlers unauthenticated; this is only acceptable until
	// #13 lands, and NewHandler logs a warning.
	Authenticator Authenticator
}

// Handlers implements every API operation.
type Handlers struct {
	log       *slog.Logger
	readiness *httpserver.Readiness
}

var _ gen.StrictServerInterface = (*Handlers)(nil)

// NewHandler returns the http.Handler for every route in the OpenAPI
// document, including /healthz and /readyz. Unmatched requests get 404 or
// 405 Problem Details.
func NewHandler(opts Options) (http.Handler, error) {
	if opts.Logger == nil {
		return nil, fmt.Errorf("api: Logger is required")
	}
	if opts.Readiness == nil {
		opts.Readiness = &httpserver.Readiness{}
	}
	doc, err := loadSpec()
	if err != nil {
		return nil, err
	}
	ops := newOperationTable(doc)
	if opts.Authenticator == nil {
		opts.Authenticator = unauthenticated{}
		opts.Logger.Warn("API authentication is not configured; protected operations are open")
	}

	h := &Handlers{log: opts.Logger, readiness: opts.Readiness}
	strict := gen.NewStrictHandlerWithOptions(h, nil, gen.StrictHTTPServerOptions{
		RequestErrorHandlerFunc:  requestErrorHandler(opts.Logger),
		ResponseErrorHandlerFunc: responseErrorHandler(opts.Logger),
	})
	mux := http.NewServeMux()
	gen.HandlerWithOptions(strict, gen.StdHTTPServerOptions{
		BaseRouter: mux,
		// The generated wrapper applies these in order, so the last one is
		// the outermost: operation lookup, then authentication, then
		// validation.
		Middlewares: []gen.MiddlewareFunc{
			validateRequests(opts.Logger),
			authenticate(opts.Logger, opts.Authenticator),
			withOperation(ops),
		},
		ErrorHandlerFunc: requestErrorHandler(opts.Logger),
	})
	return httpserver.ProblemRouter(mux), nil
}

// withOperation stores the matched operation in the request context.
func withOperation(ops operationTable) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if op, ok := ops.forRequest(r); ok {
				r = r.WithContext(context.WithValue(r.Context(), operationKey{}, op))
			}
			next.ServeHTTP(w, r)
		})
	}
}

// authenticate runs the Authenticator for every operation that isn't
// public. It runs before validation and decoding so unauthenticated callers
// learn nothing about the request format.
func authenticate(log *slog.Logger, auth Authenticator) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			op, ok := OperationFromContext(r.Context())
			if ok && op.Public {
				next.ServeHTTP(w, r)
				return
			}
			// Fail closed: a route missing from the operation table is
			// treated as protected.
			ctx, err := auth.Authenticate(r)
			if err != nil {
				writeError(log, w, r, err)
				return
			}
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// unauthenticated lets every request through. It stands in until #13.
type unauthenticated struct{}

func (unauthenticated) Authenticate(r *http.Request) (context.Context, error) {
	return r.Context(), nil
}
