package platform

import (
	"bytes"
	"crypto/rand"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"text/template"

	"golang.org/x/crypto/bcrypt"

	"github.com/rchouinard/pressfleet/control-plane/internal/config"
	"github.com/rchouinard/pressfleet/control-plane/internal/ingress"
	"github.com/rchouinard/pressfleet/control-plane/internal/redact"
	"github.com/rchouinard/pressfleet/templates"
)

// Public metrics access (metrics.public_domain, #117).
//
// The agency's Grafana reaches VictoriaMetrics through a Traefik route on a
// dedicated domain with HTTP basic auth. The route is a file of Traefik's
// file provider, which Traefik reloads by itself, so rotating the credential
// touches no container. The host CLI owns the credential (the control plane
// never needs it, and §4.7 keeps the host CLI away from the state
// database): pressfleet init creates it and shows the password once,
// pressfleet metrics-access rotate replaces it, and only its bcrypt hash is
// kept, in MetricsAccessHashSecret, so a re-render keeps it.

// MetricsAccessUsername is the basic auth user name of the public metrics
// route.
const MetricsAccessUsername = "grafana"

// Files of the public metrics route, relative to the project directory.
const (
	// MetricsAccessHashSecret holds the credential's bcrypt hash.
	MetricsAccessHashSecret = "secrets/metrics_access_hash"
	// TraefikMetricsConfigFile is the route, rendered only with
	// Config.MetricsPublicDomain.
	TraefikMetricsConfigFile = TraefikDynamicDir + "/metrics.yaml"
)

// Traefik names of the public metrics route.
const (
	metricsRouter         = "pf-metrics"
	metricsAuthMiddleware = "pf-metrics-auth"
)

// MetricsPublicPaths are the VictoriaMetrics paths the public route serves:
// the Prometheus query API Grafana's data source uses, and /federate. They
// are all read-only; the write paths (/api/v1/write, /api/v1/import, ...),
// the admin paths (/api/v1/admin/..., /internal/..., /snapshot/...), and
// everything else are never routed. A path ending in * has one segment in
// its place.
var MetricsPublicPaths = []string{
	"/api/v1/query",
	"/api/v1/query_range",
	"/api/v1/series",
	"/api/v1/labels",
	"/api/v1/label/*/values",
	"/api/v1/metadata",
	"/api/v1/status/buildinfo",
	"/federate",
}

// metricsRule is the Traefik rule of the public route for domain.
func metricsRule(domain string) string {
	paths := make([]string, len(MetricsPublicPaths))
	for i, p := range MetricsPublicPaths {
		if strings.Contains(p, "*") {
			// Paths hold only letters, digits, slashes, and underscores, so
			// the asterisk is the only character to translate.
			re := strings.ReplaceAll(p, "*", "[^/]+")
			paths[i] = "PathRegexp(" + backquote("^"+re+"$") + ")"
		} else {
			paths[i] = "Path(" + backquote(p) + ")"
		}
	}
	return "Host(" + backquote(domain) + ") && (" + strings.Join(paths, " || ") + ")"
}

// NewMetricsAccess generates a credential for the public metrics route: a
// random password and its bcrypt hash. The hash uses bcrypt's lowest cost,
// because Traefik checks it on every request and a random password has
// 130 bits that no stretching would make harder to guess.
func NewMetricsAccess() (password redact.Secret, hash string, err error) {
	password = redact.Secret(rand.Text())
	h, err := bcrypt.GenerateFromPassword([]byte(password.Reveal()), bcrypt.MinCost)
	if err != nil {
		return "", "", fmt.Errorf("hashing the metrics password: %w", err)
	}
	return password, string(h), nil
}

// ErrNoMetricsAccess means the project has no metrics credential yet.
var ErrNoMetricsAccess = errors.New("no metrics credential")

// ReadMetricsAccessHash returns the credential hash rendered into dir, or
// ErrNoMetricsAccess.
func ReadMetricsAccessHash(dir string) (string, error) {
	data, err := os.ReadFile(filepath.Join(dir, MetricsAccessHashSecret)) //nolint:gosec // G304: a file in the data directory
	switch {
	case errors.Is(err, fs.ErrNotExist) || (err == nil && strings.TrimSpace(string(data)) == ""):
		return "", ErrNoMetricsAccess
	case err != nil:
		return "", fmt.Errorf("reading the metrics credential: %w", err)
	}
	return strings.TrimSpace(string(data)), nil
}

// RotateMetricsAccess replaces the public metrics credential in the project
// rendered into dir and returns the new password. Traefik picks the change up
// without a restart; the old password stops working as soon as it does.
func RotateMetricsAccess(dir, domain string) (redact.Secret, error) {
	if !config.ValidDomain(domain) {
		return "", fmt.Errorf("metrics.public_domain must be a domain name like metrics.example.com, got %q", domain)
	}
	password, hash, err := NewMetricsAccess()
	if err != nil {
		return "", err
	}
	if err := writeMetricsAccess(dir, domain, hash); err != nil {
		return "", err
	}
	return password, nil
}

// writeMetricsAccess stores hash and renders the public route that checks it.
func writeMetricsAccess(dir, domain, hash string) error {
	tmpl, err := parseTemplates()
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	data := metricsAccessData{
		EntryPointHTTPS:        ingress.EntryPointHTTPS,
		CertResolverHTTP:       ingress.CertResolverHTTP,
		VictoriaMetricsService: VictoriaMetricsService,
		VictoriaMetricsPort:    VictoriaMetricsPort,
		MetricsRouter:          metricsRouter,
		MetricsAuthMiddleware:  metricsAuthMiddleware,
		MetricsRule:            metricsRule(domain),
		MetricsUsername:        MetricsAccessUsername,
		MetricsPasswordHash:    hash,
	}
	if err := tmpl.ExecuteTemplate(&buf, "traefik-metrics.yaml.tmpl", data); err != nil {
		return fmt.Errorf("rendering %s: %w", TraefikMetricsConfigFile, err)
	}
	// The hash first: a route without one is never rendered.
	if err := writeFile(filepath.Join(dir, MetricsAccessHashSecret), []byte(hash+"\n"), 0o600); err != nil {
		return err
	}
	if err := writeFile(filepath.Join(dir, TraefikMetricsConfigFile), buf.Bytes(), 0o644); err != nil {
		return err
	}
	return os.Chmod(filepath.Join(dir, TraefikDynamicDir), 0o755) //nolint:gosec // G302: Traefik must list it
}

// removeMetricsAccess turns the public route off, forgetting the credential.
func removeMetricsAccess(dir string) error {
	for _, p := range []string{TraefikMetricsConfigFile, MetricsAccessHashSecret} {
		if err := os.Remove(filepath.Join(dir, p)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("removing %s: %w", p, err)
		}
	}
	return nil
}

// metricsAccessData is what traefik-metrics.yaml.tmpl is rendered with.
type metricsAccessData struct {
	EntryPointHTTPS        string
	CertResolverHTTP       string
	VictoriaMetricsService string
	VictoriaMetricsPort    int
	MetricsRouter          string
	MetricsAuthMiddleware  string
	MetricsRule            string
	MetricsUsername        string
	MetricsPasswordHash    string
}

// parseTemplates parses the platform templates.
func parseTemplates() (*template.Template, error) {
	tmpl, err := template.New("").Option("missingkey=error").
		Funcs(template.FuncMap{"q": quote, "yq": yamlQuote, "backquote": backquote}).
		ParseFS(templates.Platform(), "*.tmpl")
	if err != nil {
		return nil, fmt.Errorf("parsing platform templates: %w", err)
	}
	return tmpl, nil
}
