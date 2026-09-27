// Package maintenance serves the page visitors of a site in maintenance
// mode see (design doc §5.4): 503 Service Unavailable with Retry-After and
// a small, self-contained HTML page.
//
// It runs as its own platform service, `pressfleet maintenance-page` in the
// control plane image, rather than inside the control plane: it serves
// public traffic for sites under maintenance, and the control plane holds
// the Docker socket and the state database. The service needs neither, no
// configuration, and no writable filesystem. Traefik reaches it on the
// platform network as ingress.MaintenanceService; the Compose driver's
// SetMaintenance points a site's domains at it.
package maintenance

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"time"
)

// RetryAfter is what the page tells clients, in seconds, to wait before
// trying again. Maintenance (a restore, an update) usually takes minutes;
// clients and crawlers retrying every two minutes find the site back soon
// after it is.
const RetryAfter = 120

// DefaultListen is the address the service listens on in its container.
const DefaultListen = ":8080"

// page is the maintenance page. It loads nothing from anywhere: no images,
// fonts, or scripts, since the site's own files may be what is being
// restored.
//
//go:embed page.html
var page []byte

// ErrUnexpectedResponse is returned by Check when the service answers with
// something other than the maintenance page.
var ErrUnexpectedResponse = errors.New("maintenance: unexpected response")

// Handler answers every request, whatever the host, path, or method, with
// the maintenance page and status 503.
func Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Type", "text/html; charset=utf-8")
		h.Set("Content-Length", strconv.Itoa(len(page)))
		h.Set("Retry-After", strconv.Itoa(RetryAfter))
		// Never cache the page: the site is back as soon as maintenance
		// ends, and a CDN in front of Traefik mustn't keep serving this.
		h.Set("Cache-Control", "no-store")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; frame-ancestors 'none'")
		w.WriteHeader(http.StatusServiceUnavailable)
		if r.Method != http.MethodHead {
			_, _ = w.Write(page)
		}
	})
}

// Serve runs the service on listen until ctx is canceled, then shuts down
// gracefully.
func Serve(ctx context.Context, listen string, log *slog.Logger) error {
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return fmt.Errorf("maintenance: %w", err)
	}
	srv := &http.Server{
		Handler:           Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    64 << 10,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	log.InfoContext(ctx, "maintenance page listening", "addr", ln.Addr().String())
	select {
	case err := <-errc:
		return fmt.Errorf("maintenance: %w", err)
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdown); err != nil {
		return fmt.Errorf("maintenance: shutting down: %w", err)
	}
	return nil
}

// Check asks the service at url for the page, for the container's health
// check. It succeeds only on a 503 with Retry-After.
func Check(ctx context.Context, url string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, url, http.NoBody)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("maintenance: %w", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("Retry-After") == "" {
		return fmt.Errorf("%w: HEAD %s returned %s", ErrUnexpectedResponse, url, resp.Status)
	}
	return nil
}
