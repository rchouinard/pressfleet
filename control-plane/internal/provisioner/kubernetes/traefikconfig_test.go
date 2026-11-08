package kubernetes

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rchouinard/pressfleet/control-plane/internal/ingress"
)

// get asks the provider handler for path.
func get(t *testing.T, p *Provisioner, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	p.TraefikConfigHandler().ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), method, path, nil))
	return rec
}

func TestTraefikConfigHandler(t *testing.T) {
	p := cluster(t, nil)
	// No site yet: an empty configuration, not an error.
	rec := get(t, p, http.MethodGet, TraefikConfigPath)
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != `{
  "http": {}
}` {
		t.Errorf("no sites: %d %s", rec.Code, rec.Body)
	}

	if _, err := p.CreateSite(t.Context(), liveSite()); err != nil {
		t.Fatal(err)
	}
	rec = get(t, p, http.MethodGet, TraefikConfigPath)
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("GET: %d %v", rec.Code, rec.Header())
	}
	var c traefikConfig
	if err := json.Unmarshal(rec.Body.Bytes(), &c); err != nil {
		t.Fatal(err)
	}
	if len(c.HTTP.Routers) != 5 || len(c.HTTP.Services) != 1 {
		t.Errorf("served %+v", c.HTTP)
	}
	// The bcrypt hash, never anything else of the credentials.
	if !strings.Contains(rec.Body.String(), liveSite().PreviewAuth.PasswordHash) {
		t.Error("the preview's basic auth is missing")
	}
	// The same routes make the same bytes, so Traefik reloads nothing.
	if again := get(t, p, http.MethodGet, TraefikConfigPath); again.Body.String() != rec.Body.String() {
		t.Error("the configuration changed between two requests")
	}

	for _, tc := range []struct {
		method, path string
		want         int
	}{
		{http.MethodPost, TraefikConfigPath, http.StatusMethodNotAllowed},
		{http.MethodGet, "/", http.StatusNotFound},
		{http.MethodGet, "/healthz", http.StatusNotFound},
	} {
		if got := get(t, p, tc.method, tc.path).Code; got != tc.want {
			t.Errorf("%s %s = %d, want %d", tc.method, tc.path, got, tc.want)
		}
	}
}

// Broken or foreign routes on disk fail the request, so that Traefik keeps
// the routes it has instead of losing every site's.
func TestTraefikConfigHandlerRefuses(t *testing.T) {
	tests := map[string]string{
		"malformed": `{"http":`,
		"another site's names": `{"http":{"routers":{"` + ingress.SiteName(otherSiteID, "preview") +
			`":{"entryPoints":["websecure"],"rule":"Host(` + "`a.example`" + `)","service":"x","tls":{"certResolver":"acme-http"}}}}}`,
		"a platform name": `{"http":{"services":{"pf-dashboard":{"loadBalancer":{"servers":[{"url":"http://x"}]}}}}}`,
	}
	for name, content := range tests {
		t.Run(name, func(t *testing.T) {
			p := cluster(t, nil)
			if _, err := p.CreateSite(t.Context(), testSite()); err != nil {
				t.Fatal(err)
			}
			siteDir, _ := p.layout.SiteDir(testSiteID)
			if err := os.WriteFile(filepath.Join(siteDir, MaintenanceRoutesFile), []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			if rec := get(t, p, http.MethodGet, TraefikConfigPath); rec.Code != http.StatusInternalServerError {
				t.Errorf("status %d: %s", rec.Code, rec.Body)
			}
		})
	}
}

// Directories that aren't sites' are skipped.
func TestServedRoutesSkipsOtherDirectories(t *testing.T) {
	p := cluster(t, nil)
	if _, err := p.CreateSite(t.Context(), testSite()); err != nil {
		t.Fatal(err)
	}
	stray := filepath.Join(p.layout.SitesDir(), "Not_A_Site", SiteManifestDir)
	if err := os.MkdirAll(stray, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stray, RoutesFile), []byte(`{"http":{"routers":{"x":{}}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := routerNames(served(t, p), ""); len(got) != 1 {
		t.Errorf("routes = %q", got)
	}
}
