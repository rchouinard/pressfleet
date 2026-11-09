package kubernetes

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/rchouinard/pressfleet/control-plane/internal/ingress"
)

// Traefik's dynamic configuration (#322, security review K1). Traefik has
// no access to the Kubernetes API: its routes come from the chart's file
// (the dashboard, SFTP, the maintenance page's service, the preview
// wildcard certificate) and from the control plane, which serves the
// sites' routes to Traefik's HTTP provider (TraefikConfigHandler). Traefik
// polls it and applies what changed, so it serves only routes PressFleet
// wrote, and it holds no ServiceAccount token.

// TraefikConfigPath is the path of the control plane's Traefik provider
// endpoint, on its own listener (kubernetes.traefik_provider_listen),
// which the chart's NetworkPolicy opens to Traefik alone.
const TraefikConfigPath = "/traefik/dynamic"

// ErrTraefikConfig means the routes on disk can't be served: a file is
// malformed or defines another site's names.
var ErrTraefikConfig = errors.New("kubernetes: invalid Traefik configuration")

// traefikConfig is the part of Traefik's dynamic configuration the
// driver writes, as Traefik's file and HTTP providers read it.
type traefikConfig struct {
	HTTP traefikHTTP `json:"http"`
}

type traefikHTTP struct {
	Routers     map[string]traefikRouter     `json:"routers,omitempty"`
	Middlewares map[string]traefikMiddleware `json:"middlewares,omitempty"`
	Services    map[string]traefikService    `json:"services,omitempty"`
}

type traefikRouter struct {
	EntryPoints []string `json:"entryPoints"`
	Rule        string   `json:"rule"`
	// Priority is left to Traefik (the length of the rule) when zero.
	Priority    int              `json:"priority,omitempty"`
	Middlewares []string         `json:"middlewares,omitempty"`
	Service     string           `json:"service"`
	TLS         traefikRouterTLS `json:"tls"`
}

type traefikRouterTLS struct {
	CertResolver string `json:"certResolver"`
}

// traefikMiddleware holds exactly one kind of middleware.
type traefikMiddleware struct {
	Headers       *traefikHeaders       `json:"headers,omitempty"`
	BasicAuth     *traefikBasicAuth     `json:"basicAuth,omitempty"`
	RedirectRegex *traefikRedirectRegex `json:"redirectRegex,omitempty"`
}

type traefikHeaders struct {
	CustomResponseHeaders map[string]string `json:"customResponseHeaders,omitempty"`
	STSSeconds            int               `json:"stsSeconds,omitempty"`
}

type traefikBasicAuth struct {
	// Users are htpasswd lines: the user name and the bcrypt hash of the
	// password, never the password.
	Users        []string `json:"users"`
	Realm        string   `json:"realm"`
	RemoveHeader bool     `json:"removeHeader"`
}

type traefikRedirectRegex struct {
	Regex       string `json:"regex"`
	Replacement string `json:"replacement"`
	Permanent   bool   `json:"permanent"`
}

type traefikService struct {
	LoadBalancer traefikLoadBalancer `json:"loadBalancer"`
}

type traefikLoadBalancer struct {
	Servers []traefikServer `json:"servers"`
}

type traefikServer struct {
	URL string `json:"url"`
}

// owns reports whether every router, middleware, and service the
// configuration defines is named after the site (ingress.SiteName), so
// one site's file can't take over another's names.
func (c traefikConfig) owns(siteID string) bool {
	prefix := ingress.SiteService(siteID)
	mine := func(name string) bool { return name == prefix || strings.HasPrefix(name, prefix+"-") }
	for n := range maps.Keys(c.HTTP.Routers) {
		if !mine(n) {
			return false
		}
	}
	for n := range maps.Keys(c.HTTP.Middlewares) {
		if !mine(n) {
			return false
		}
	}
	for n := range maps.Keys(c.HTTP.Services) {
		if !mine(n) {
			return false
		}
	}
	return true
}

// add merges o into c. Names don't collide: owns has checked them.
func (c *traefikConfig) add(o traefikConfig) {
	if c.HTTP.Routers == nil {
		c.HTTP.Routers = map[string]traefikRouter{}
		c.HTTP.Middlewares = map[string]traefikMiddleware{}
		c.HTTP.Services = map[string]traefikService{}
	}
	maps.Copy(c.HTTP.Routers, o.HTTP.Routers)
	maps.Copy(c.HTTP.Middlewares, o.HTTP.Middlewares)
	maps.Copy(c.HTTP.Services, o.HTTP.Services)
}

// encodeTraefikConfig is the form the driver writes routes in. Go sorts
// map keys, so the same routes always make the same bytes.
func encodeTraefikConfig(c traefikConfig) ([]byte, error) {
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// readTraefikConfig reads routes the driver wrote; ok is false if the
// file doesn't exist.
func readTraefikConfig(path string) (c traefikConfig, ok bool, err error) {
	b, err := os.ReadFile(path) //nolint:gosec // G304: a file the driver wrote into a site's directory
	if errors.Is(err, fs.ErrNotExist) {
		return c, false, nil
	} else if err != nil {
		return c, false, err
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return c, false, fmt.Errorf("%w: %s: %w", ErrTraefikConfig, path, err)
	}
	return c, true, nil
}

// servedRoutes returns what Traefik's HTTP provider reads: every site's
// routes (RoutesFile), and the maintenance routes of the sites in
// maintenance mode (MaintenanceRoutesFile).
func (p *Provisioner) servedRoutes() (traefikConfig, error) {
	var out traefikConfig
	out.add(traefikConfig{})
	sitesDir := p.layout.SitesDir()
	entries, err := os.ReadDir(sitesDir)
	if errors.Is(err, fs.ErrNotExist) {
		return out, nil
	} else if err != nil {
		return out, err
	}
	for _, e := range entries {
		siteID := e.Name()
		if !e.IsDir() || p.validSiteNamespace(siteID) != nil {
			continue
		}
		dir := filepath.Join(sitesDir, siteID)
		for _, path := range []string{filepath.Join(dir, SiteManifestDir, RoutesFile), filepath.Join(dir, MaintenanceRoutesFile)} {
			c, ok, err := readTraefikConfig(path)
			if err != nil {
				return out, err
			}
			if !ok {
				continue
			}
			if !c.owns(siteID) {
				return out, fmt.Errorf("%w: %s defines names that aren't site %s's", ErrTraefikConfig, path, siteID)
			}
			out.add(c)
		}
	}
	return out, nil
}

// TraefikConfigHandler serves the sites' routes at TraefikConfigPath to
// Traefik's HTTP provider. It authenticates nothing, and what it serves is
// sensitive: every site's host names, and the usernames and bcrypt hashes
// of preview passwords (basicAuth.users). The chart's NetworkPolicy, which
// admits Traefik's pod alone to the listener, is its only protection. When they can't be read it answers with an
// error, and Traefik keeps the routes it has rather than dropping every
// site's.
func (p *Provisioner) TraefikConfigHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+TraefikConfigPath, func(w http.ResponseWriter, r *http.Request) {
		c, err := p.servedRoutes()
		var body []byte
		if err == nil {
			body, err = encodeTraefikConfig(c)
		}
		if err != nil {
			p.log.ErrorContext(r.Context(), "serving Traefik's configuration failed; Traefik keeps its current routes", "error", err)
			http.Error(w, "the routes can't be read", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(body)
	})
	return mux
}
