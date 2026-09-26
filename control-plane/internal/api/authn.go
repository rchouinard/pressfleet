package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/auth"
	"github.com/rchouinard/pressfleet/control-plane/internal/redact"
)

// Names of the session cookie and the CSRF header (see api/openapi.yaml).
const (
	SessionCookieName = "pressfleet_session"
	HeaderCSRFToken   = "X-CSRF-Token" //nolint:gosec // G101: a header name, not a credential
)

// NewAuthenticator returns the Authenticator for the API: a bearer API
// token if the request has an Authorization header, otherwise the session
// cookie. Cookie-authenticated requests with unsafe methods must also carry
// the session's CSRF token in the X-CSRF-Token header; bearer requests are
// exempt, because browsers never attach bearer tokens on their own.
//
// It puts an auth.Principal in the request context.
func NewAuthenticator(svc *auth.Service) Authenticator {
	return &authenticator{svc: svc}
}

type authenticator struct {
	svc *auth.Service
}

func (a *authenticator) Authenticate(r *http.Request) (context.Context, error) {
	ctx := r.Context()
	if h := r.Header.Get("Authorization"); h != "" {
		// A request with an Authorization header is judged on it alone;
		// falling back to the cookie would make CSRF exemption depend on a
		// header an attacker could add.
		scheme, tok, ok := strings.Cut(h, " ")
		if !ok || !strings.EqualFold(scheme, "Bearer") {
			return nil, fmt.Errorf("%w: the Authorization header must use the Bearer scheme", ErrUnauthenticated)
		}
		p, err := a.svc.AuthenticateToken(ctx, redact.Secret(strings.TrimSpace(tok)))
		if err != nil {
			return nil, authnError(err)
		}
		return auth.WithPrincipal(ctx, p), nil
	}

	c, err := r.Cookie(SessionCookieName)
	if err != nil || c.Value == "" {
		return nil, ErrUnauthenticated
	}
	token := redact.Secret(c.Value)
	p, err := a.svc.AuthenticateSession(ctx, token)
	if err != nil {
		return nil, authnError(err)
	}
	if !safeMethod(r.Method) && !auth.ValidCSRFToken(token, redact.Secret(r.Header.Get(HeaderCSRFToken))) {
		return nil, fmt.Errorf("%w: missing or invalid %s header", ErrForbidden, HeaderCSRFToken)
	}
	ctx = auth.WithPrincipal(ctx, p)
	return context.WithValue(ctx, csrfKey{}, auth.CSRFToken(token)), nil
}

func authnError(err error) error {
	if errors.Is(err, auth.ErrUnauthenticated) {
		return ErrUnauthenticated
	}
	return err
}

// rejectAll is used when no Authenticator is configured: the API fails
// closed rather than serving protected operations to anyone.
type rejectAll struct{}

func (rejectAll) Authenticate(*http.Request) (context.Context, error) {
	return nil, fmt.Errorf("%w: authentication is not configured", ErrUnauthenticated)
}

type csrfKey struct{}

// csrfFromContext returns the CSRF token of a cookie-authenticated request.
func csrfFromContext(ctx context.Context) (redact.Secret, bool) {
	t, ok := ctx.Value(csrfKey{}).(redact.Secret)
	return t, ok
}

func safeMethod(m string) bool {
	return m == http.MethodGet || m == http.MethodHead || m == http.MethodOptions
}

// crossOrigin rejects unsafe browser requests from other origins, using
// Sec-Fetch-Site and Origin (http.CrossOriginProtection). It applies to
// every unsafe request without an Authorization header, including login,
// which has no session yet and so no CSRF token (login CSRF). Non-browser
// clients send neither header and are unaffected.
func crossOrigin(log *slog.Logger) func(http.Handler) http.Handler {
	cop := http.NewCrossOriginProtection()
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") == "" {
				if err := cop.Check(r); err != nil {
					writeError(log, w, r, fmt.Errorf("%w: cross-origin request rejected", ErrForbidden))
					return
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}

// DefaultTrustedProxies are the networks whose X-Forwarded-* headers are
// believed by default: loopback and private ranges. In the Compose
// deployment the control plane's port isn't published, so only containers
// on its Docker network, Traefik in particular, can reach it.
var DefaultTrustedProxies = []netip.Prefix{
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("::1/128"),
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("fc00::/7"),
}

// clientInfo describes where a request came from.
type clientInfo struct {
	// IP is the client address, after trusted proxies.
	IP string
	// HTTPS reports whether the client connected over HTTPS, directly or
	// to a trusted proxy.
	HTTPS     bool
	UserAgent string
}

type clientKey struct{}

func clientFromContext(ctx context.Context) clientInfo {
	c, _ := ctx.Value(clientKey{}).(clientInfo)
	return c
}

// withClient stores clientInfo in the request context. X-Forwarded-For and
// X-Forwarded-Proto are only believed from trusted proxies.
func withClient(trusted []netip.Prefix) func(http.Handler) http.Handler {
	isTrusted := func(a netip.Addr) bool {
		a = a.Unmap()
		for _, p := range trusted {
			if p.Contains(a) {
				return true
			}
		}
		return false
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			info := clientInfo{HTTPS: r.TLS != nil, UserAgent: r.UserAgent()}
			remote := remoteAddr(r)
			info.IP = remote.String()
			if remote.IsValid() && isTrusted(remote) {
				if strings.EqualFold(strings.TrimSpace(lastValue(r.Header.Values("X-Forwarded-Proto"))), "https") {
					info.HTTPS = true
				}
				// The client is the rightmost address not added by a trusted
				// proxy; anything further left could be forged by the client.
				hops := strings.Split(strings.Join(r.Header.Values("X-Forwarded-For"), ","), ",")
				for i := len(hops) - 1; i >= 0; i-- {
					a, err := netip.ParseAddr(strings.TrimSpace(hops[i]))
					if err != nil {
						break
					}
					info.IP = a.Unmap().String()
					if !isTrusted(a) {
						break
					}
				}
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), clientKey{}, info)))
		})
	}
}

func remoteAddr(r *http.Request) netip.Addr {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	a, _ := netip.ParseAddr(host)
	return a.Unmap()
}

func lastValue(vs []string) string {
	if len(vs) == 0 {
		return ""
	}
	parts := strings.Split(vs[len(vs)-1], ",")
	return parts[len(parts)-1]
}

// sessionCookie builds the session cookie. SameSite=Lax rather than Strict:
// Strict would drop the cookie when a user follows a link to the dashboard
// from an alert email or chat, forcing a new login, while Lax already
// withholds it from cross-site subrequests and unsafe cross-site requests;
// the CSRF token and origin check protect state changes regardless.
// Secure is set when the request arrived over HTTPS (or Options.SecureCookies
// forces it), so plain-HTTP development on localhost keeps working.
func (h *Handlers) sessionCookie(ctx context.Context, value string, expires time.Time) *http.Cookie {
	c := &http.Cookie{ //nolint:gosec // G124: Secure is deliberately conditional on HTTPS, see above
		Name:     SessionCookieName,
		Value:    value,
		Path:     "/",
		HttpOnly: true,
		Secure:   h.secureCookies || clientFromContext(ctx).HTTPS,
		SameSite: http.SameSiteLaxMode,
	}
	if value == "" {
		c.MaxAge = -1
	} else {
		c.Expires = expires
	}
	return c
}
