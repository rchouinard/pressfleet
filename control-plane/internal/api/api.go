// Package api implements the control plane's HTTP API (api/openapi.yaml).
//
// The router, request types, and response types are generated into package
// gen. This package provides the handler implementations (the Handlers
// methods, one file per area) and the per-operation middleware that runs
// between routing and the handler:
//
//  1. the cross-origin check for unsafe requests without a bearer token,
//     including login (see crossOrigin);
//  2. authentication, for every operation that isn't Public (see
//     Authenticator and NewAuthenticator). Without an Authenticator every
//     protected operation is rejected: the API fails closed;
//  3. the audit log, which records the request once it is done if it
//     changed state or was denied (see audit.go);
//  4. request validation against the OpenAPI document;
//  5. JSON decoding (generated), then the handler.
//
// # Writing a handler
//
// Handlers implement gen.StrictServerInterface. Adding or implementing an
// endpoint never touches the router: edit the spec, run `make generate`,
// declare the operation's authorization action in operationActions, and
// fill in the method. Every protected handler must start with the single
// authorization check (design doc §4.6, package authz), before reading or
// changing anything:
//
//	p, err := h.authorize(ctx, authz.UpdateSite, authz.Site(req.SiteID))
//	if err != nil {
//		return nil, err // ErrForbidden -> 403
//	}
//
// It returns the caller. NewHandler fails if a protected operation has no
// declared action, authorize fails if the handler checks a different action
// than the declared one, and a test calls every operation as a caller with
// no permissions and expects 403.
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
	"net/netip"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/adminlogin"
	"github.com/rchouinard/pressfleet/control-plane/internal/alertchannels"
	"github.com/rchouinard/pressfleet/control-plane/internal/api/gen"
	"github.com/rchouinard/pressfleet/control-plane/internal/auth"
	"github.com/rchouinard/pressfleet/control-plane/internal/authz"
	"github.com/rchouinard/pressfleet/control-plane/internal/backup/offboard"
	"github.com/rchouinard/pressfleet/control-plane/internal/backup/sitebackup"
	"github.com/rchouinard/pressfleet/control-plane/internal/buildinfo"
	"github.com/rchouinard/pressfleet/control-plane/internal/dbsession"
	"github.com/rchouinard/pressfleet/control-plane/internal/deploy"
	"github.com/rchouinard/pressfleet/control-plane/internal/export"
	"github.com/rchouinard/pressfleet/control-plane/internal/httpserver"
	"github.com/rchouinard/pressfleet/control-plane/internal/metricsapi"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
	"github.com/rchouinard/pressfleet/control-plane/internal/sites"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/uploads"
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
	// Authenticator authenticates every non-public operation, normally
	// NewAuthenticator(Auth). If nil, every protected operation is rejected
	// with 401.
	Authenticator Authenticator
	// DB is the state database. Handlers that need it fail with a 500
	// without it.
	DB *store.DB
	// Auth backs login, the current user, API tokens, and user management.
	// If nil, those operations fail with 500.
	Auth *auth.Service
	// TrustedProxies are the networks whose X-Forwarded-For and
	// X-Forwarded-Proto headers are believed. Nil means
	// DefaultTrustedProxies; use an empty, non-nil slice to trust none.
	TrustedProxies []netip.Prefix
	// Sites runs the site workflows. If nil, the site operations fail with
	// 500.
	Sites *sites.Service
	// Backups takes and lists site backups. If nil, the backup operations
	// fail with 500.
	Backups *sitebackup.Service
	// Offboarding deletes client groups' backup repositories. If nil,
	// those deletions fail with 500.
	Offboarding *offboard.Service
	// Uploads receives large files for site imports and deployments. If
	// nil, the upload operations fail with 500.
	Uploads *uploads.Service
	// Exports exports site databases and whole sites for download. If
	// nil, the export operations fail with 500.
	Exports *export.Service
	// Deployments deploys theme and plugin packages. If nil, the deploy
	// operation fails with 500.
	Deployments *deploy.Service
	// DatabaseSessions opens and ends temporary database sessions. If nil,
	// the database session operations fail with 500.
	DatabaseSessions *dbsession.Service
	// AdminLogin issues one-click WordPress admin login tokens and lists
	// the administrators to choose from. If nil, those operations fail with
	// 500.
	AdminLogin *adminlogin.Service
	// Logs reads the site containers' logs through the orchestrator
	// driver. If nil, the site log operations fail with 500.
	Logs provisioner.LogReader
	// Metrics answers the metrics queries. If nil, they fail with 503, as
	// when VictoriaMetrics can't be reached.
	Metrics *metricsapi.Service
	// AlertChannels manages alert channels and sends test notifications. If
	// nil, the alert channel operations fail with 500.
	AlertChannels *alertchannels.Service
	// Doctor runs the platform's preflight checks for GET
	// /api/v1/platform/doctor. If nil, that operation fails with 500.
	Doctor DoctorFunc
	// SecureCookies forces the Secure flag on the session cookie. Otherwise
	// it is set when the request arrived over HTTPS, directly or through a
	// trusted proxy, so plain-HTTP development on localhost still works.
	SecureCookies bool
	// SFTP is what GET /api/v1/platform reports about the gateway.
	SFTP SFTPInfo
	// Build is what GET /api/v1/platform reports about the running binary.
	Build buildinfo.Info
}

// SFTPInfo is where developers reach the SFTP gateway.
type SFTPInfo struct {
	Enabled bool
	// Host is the dashboard domain, where the gateway's port is published.
	Host string
	Port int
}

// Handlers implements every API operation.
type Handlers struct {
	log           *slog.Logger
	readiness     *httpserver.Readiness
	db            *store.DB
	auth          *auth.Service
	sites         *sites.Service
	backups       *sitebackup.Service
	offboarding   *offboard.Service
	uploads       *uploads.Service
	exports       *export.Service
	deploys       *deploy.Service
	dbSessions    *dbsession.Service
	adminLogin    *adminlogin.Service
	logs          provisioner.LogReader
	metrics       *metricsapi.Service
	channels      *alertchannels.Service
	doctor        DoctorFunc
	resolver      authz.Resolver
	secureCookies bool
	sftp          SFTPInfo
	build         buildinfo.Info
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
	if err := ops.checkActions(); err != nil {
		return nil, err
	}
	if opts.Authenticator == nil {
		opts.Authenticator = rejectAll{}
		opts.Logger.Warn("API authentication is not configured; protected operations are rejected")
	}
	if opts.TrustedProxies == nil {
		opts.TrustedProxies = DefaultTrustedProxies
	}

	h := &Handlers{log: opts.Logger, readiness: opts.Readiness, db: opts.DB, auth: opts.Auth, sites: opts.Sites, backups: opts.Backups,
		uploads: opts.Uploads, secureCookies: opts.SecureCookies, sftp: opts.SFTP, build: opts.Build}
	h.exports = opts.Exports
	h.offboarding = opts.Offboarding
	h.deploys = opts.Deployments
	h.dbSessions = opts.DatabaseSessions
	h.adminLogin = opts.AdminLogin
	h.logs = opts.Logs
	if h.metrics = opts.Metrics; h.metrics == nil {
		h.metrics = metricsapi.New(nil, nil)
	}
	h.channels = opts.AlertChannels
	h.doctor = opts.Doctor
	if opts.DB != nil {
		h.resolver = authz.NewResolver(opts.DB)
	}
	strict := gen.NewStrictHandlerWithOptions(h, []gen.StrictMiddlewareFunc{withRequest}, gen.StrictHTTPServerOptions{
		RequestErrorHandlerFunc:  requestErrorHandler(opts.Logger),
		ResponseErrorHandlerFunc: responseErrorHandler(opts.Logger),
	})
	mux := http.NewServeMux()
	gen.HandlerWithOptions(strict, gen.StdHTTPServerOptions{
		BaseRouter: mux,
		// The generated wrapper applies these in order, so the last one is
		// the outermost: operation lookup and client details, the
		// cross-origin check, authentication, the audit log, then
		// validation.
		Middlewares: []gen.MiddlewareFunc{
			validateRequests(opts.Logger),
			auditRequests(opts.Logger, opts.DB),
			authenticate(opts.Logger, opts.Authenticator),
			crossOrigin(opts.Logger),
			withClient(opts.TrustedProxies),
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
				if op.ID == "streamSiteLogs" {
					// The server's write timeout is for responses that end;
					// a followed log lasts as long as the client watches.
					// Keepalive comments make a vanished client's writes fail.
					_ = http.NewResponseController(w).SetWriteDeadline(time.Time{})
				}
			}
			// Responses are per-user and may carry credentials (tokens, CSRF
			// tokens); keep them out of shared and browser caches.
			w.Header().Set("Cache-Control", "no-store")
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
