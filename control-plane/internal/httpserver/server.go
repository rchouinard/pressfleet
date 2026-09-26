// Package httpserver runs the control plane's HTTP server: timeouts, graceful
// shutdown, the middleware every request passes through (request IDs, access
// logging, panic recovery, body size limits), RFC 9457 Problem Details
// responses, and readiness checks.
//
// It knows nothing about the API itself; package api builds the handler that
// is served here.
package httpserver

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"
)

// Defaults for Options fields left zero.
const (
	DefaultReadHeaderTimeout = 10 * time.Second
	DefaultReadTimeout       = 30 * time.Second
	DefaultWriteTimeout      = 60 * time.Second
	DefaultIdleTimeout       = 120 * time.Second
	DefaultShutdownTimeout   = 15 * time.Second
	DefaultMaxBodyBytes      = 1 << 20 // 1 MiB; API bodies are small JSON documents
)

// Options configures a Server. Zero durations and sizes use the defaults.
type Options struct {
	// Addr is the TCP listen address, e.g. ":8080".
	Addr string
	// Handler serves requests after the standard middleware.
	Handler http.Handler
	// Logger receives access logs, panics, and server errors.
	Logger *slog.Logger

	ReadHeaderTimeout time.Duration
	ReadTimeout       time.Duration
	WriteTimeout      time.Duration
	IdleTimeout       time.Duration
	// ShutdownTimeout bounds how long in-flight requests may take to finish
	// once shutdown starts.
	ShutdownTimeout time.Duration
	// MaxBodyBytes limits request body size.
	MaxBodyBytes int64
}

func (o *Options) setDefaults() {
	def := func(d *time.Duration, v time.Duration) {
		if *d <= 0 {
			*d = v
		}
	}
	def(&o.ReadHeaderTimeout, DefaultReadHeaderTimeout)
	def(&o.ReadTimeout, DefaultReadTimeout)
	def(&o.WriteTimeout, DefaultWriteTimeout)
	def(&o.IdleTimeout, DefaultIdleTimeout)
	def(&o.ShutdownTimeout, DefaultShutdownTimeout)
	if o.MaxBodyBytes <= 0 {
		o.MaxBodyBytes = DefaultMaxBodyBytes
	}
}

// Server is the control plane HTTP server.
type Server struct {
	opts    Options
	log     *slog.Logger
	handler http.Handler
}

// New returns a server for opts. It doesn't listen until Run or Serve.
func New(opts Options) *Server {
	opts.setDefaults()
	log := opts.Logger
	return &Server{
		opts: opts,
		log:  log,
		handler: Chain(opts.Handler,
			RequestID,
			AccessLog(log),
			Recover(log),
			MaxBodyBytes(opts.MaxBodyBytes),
		),
	}
}

// Handler returns the handler with all standard middleware applied.
func (s *Server) Handler() http.Handler { return s.handler }

// Run listens on the configured address and serves until ctx is cancelled,
// then shuts down gracefully. It returns an error if listening or serving
// fails, or if in-flight requests don't finish within the shutdown timeout.
func (s *Server) Run(ctx context.Context) error {
	var lc net.ListenConfig
	// A context cancelled before we start still gets a clean listen-then-
	// shutdown cycle rather than a spurious listen error.
	ln, err := lc.Listen(context.WithoutCancel(ctx), "tcp", s.opts.Addr)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", s.opts.Addr, err)
	}
	return s.Serve(ctx, ln)
}

// Serve is like Run but uses an existing listener, which it closes.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	srv := &http.Server{
		Handler:           s.handler,
		ReadHeaderTimeout: s.opts.ReadHeaderTimeout,
		ReadTimeout:       s.opts.ReadTimeout,
		WriteTimeout:      s.opts.WriteTimeout,
		IdleTimeout:       s.opts.IdleTimeout,
		ErrorLog:          slog.NewLogLogger(s.log.Handler(), slog.LevelWarn),
		// Requests must not be cancelled the moment shutdown begins; Shutdown
		// lets them finish within the shutdown timeout.
		BaseContext: func(net.Listener) context.Context { return context.WithoutCancel(ctx) },
	}

	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	s.log.Info("http server listening", "addr", ln.Addr().String())

	select {
	case err := <-errc:
		return fmt.Errorf("http server: %w", err)
	case <-ctx.Done():
	}

	s.log.Info("http server shutting down", "timeout", s.opts.ShutdownTimeout)
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.opts.ShutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		_ = srv.Close()
		return fmt.Errorf("shutting down http server: %w", err)
	}
	if err := <-errc; !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("http server: %w", err)
	}
	s.log.Info("http server stopped")
	return nil
}
