package platform_test

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
	"golang.org/x/crypto/bcrypt"

	"github.com/rchouinard/pressfleet/control-plane/internal/platform"
)

type metricsRoute struct {
	HTTP struct {
		Routers map[string]struct {
			Rule        string   `yaml:"rule"`
			Middlewares []string `yaml:"middlewares"`
			Service     string   `yaml:"service"`
			EntryPoints []string `yaml:"entryPoints"`
		} `yaml:"routers"`
		Middlewares map[string]struct {
			BasicAuth struct {
				Users        []string `yaml:"users"`
				RemoveHeader bool     `yaml:"removeHeader"`
			} `yaml:"basicAuth"`
		} `yaml:"middlewares"`
		Services map[string]struct {
			LoadBalancer struct {
				Servers []struct {
					URL string `yaml:"url"`
				} `yaml:"servers"`
			} `yaml:"loadBalancer"`
		} `yaml:"services"`
	} `yaml:"http"`
}

func metricsConfig(t *testing.T) (platform.Config, string) {
	t.Helper()
	password, hash, err := platform.NewMetricsAccess()
	if err != nil {
		t.Fatal(err)
	}
	cfg := testConfig()
	cfg.MetricsPublicDomain = "metrics.agency.example"
	cfg.MetricsAccessHash = hash
	return cfg, password.Reveal()
}

func readMetricsRoute(t *testing.T, dir string) metricsRoute {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, platform.TraefikMetricsConfigFile))
	if err != nil {
		t.Fatal(err)
	}
	var r metricsRoute
	if err := yaml.Unmarshal(data, &r); err != nil {
		t.Fatalf("%v\n%s", err, data)
	}
	return r
}

func TestRenderedMetricsAccess(t *testing.T) {
	cfg, password := metricsConfig(t)
	dir := render(t, cfg)
	route := readMetricsRoute(t, dir)

	if len(route.HTTP.Routers) != 1 {
		t.Fatalf("routers = %v", route.HTTP.Routers)
	}
	for name, r := range route.HTTP.Routers {
		if !strings.HasPrefix(r.Rule, "Host(`metrics.agency.example`) && (") {
			t.Errorf("rule = %s", r.Rule)
		}
		if len(r.EntryPoints) != 1 || r.EntryPoints[0] != "websecure" {
			t.Errorf("entry points = %v", r.EntryPoints)
		}
		if r.Service != name || len(route.HTTP.Services[name].LoadBalancer.Servers) != 1 ||
			route.HTTP.Services[name].LoadBalancer.Servers[0].URL != "http://victoria-metrics:8428" {
			t.Errorf("service = %q, %+v", r.Service, route.HTTP.Services)
		}
		// Requests are authenticated before they reach VictoriaMetrics.
		if len(r.Middlewares) != 1 {
			t.Fatalf("middlewares = %v", r.Middlewares)
		}
		auth := route.HTTP.Middlewares[r.Middlewares[0]].BasicAuth
		if len(auth.Users) != 1 || !auth.RemoveHeader {
			t.Fatalf("basic auth = %+v", auth)
		}
		user, hash, _ := strings.Cut(auth.Users[0], ":")
		if user != platform.MetricsAccessUsername || bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) != nil {
			t.Errorf("user %q does not accept the generated password", user)
		}
		if bcrypt.CompareHashAndPassword([]byte(hash), []byte("wrong")) == nil {
			t.Error("the hash accepts a wrong password")
		}
	}

	// The plaintext never reaches the project, and the hash is kept for the
	// next render.
	filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error { //nolint:errcheck // test
		if err == nil && !d.IsDir() {
			if b, _ := os.ReadFile(path); strings.Contains(string(b), password) {
				t.Errorf("%s holds the plaintext password", path)
			}
		}
		return nil
	})
	got, err := platform.ReadMetricsAccessHash(dir)
	if err != nil || got != cfg.MetricsAccessHash {
		t.Errorf("ReadMetricsAccessHash() = %q, %v", got, err)
	}
	info, err := os.Stat(filepath.Join(dir, platform.MetricsAccessHashSecret))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("hash secret = %v, %v", info, err)
	}
}

// TestMetricsAccessServesOnlyReadPaths evaluates the rendered rule against
// the paths VictoriaMetrics serves.
func TestMetricsAccessServesOnlyReadPaths(t *testing.T) {
	cfg, _ := metricsConfig(t)
	route := readMetricsRoute(t, render(t, cfg))
	var rule string
	for _, r := range route.HTTP.Routers {
		rule = r.Rule
	}
	// The rule is Host(..) && (Path(..) || PathRegexp(..) ...): every
	// matcher, in order.
	matchers := regexp.MustCompile("(Path|PathRegexp)\\(`([^`]+)`\\)").FindAllStringSubmatch(rule, -1)
	if len(matchers) != len(platform.MetricsPublicPaths) {
		t.Fatalf("%d path matchers for %d paths in %s", len(matchers), len(platform.MetricsPublicPaths), rule)
	}
	served := func(path string) bool {
		for _, m := range matchers {
			if m[1] == "Path" && m[2] == path {
				return true
			}
			if m[1] == "PathRegexp" && regexp.MustCompile(m[2]).MatchString(path) {
				return true
			}
		}
		return false
	}
	for _, p := range []string{
		"/api/v1/query", "/api/v1/query_range", "/api/v1/series", "/api/v1/labels",
		"/api/v1/label/__name__/values", "/api/v1/label/site_id/values", "/federate",
		"/api/v1/metadata", "/api/v1/status/buildinfo",
	} {
		if !served(p) {
			t.Errorf("%s is not served", p)
		}
	}
	for _, p := range []string{
		// Writes.
		"/api/v1/write", "/api/v1/import", "/api/v1/import/prometheus", "/write", "/insert/0/prometheus/api/v1/write",
		"/prometheus/api/v1/write", "/influx/write", "/api/v1/push",
		// Admin and deletion.
		"/api/v1/admin/tsdb/delete_series", "/api/v1/admin/tsdb/snapshot", "/snapshot/create", "/snapshot/delete",
		"/internal/force_flush", "/internal/resetRollupResultCache", "/-/reload", "/config", "/flags", "/metrics",
		"/api/v1/status/tsdb", "/api/v1/export", "/vmui/", "/",
		// Near misses of served paths.
		"/api/v1/query/x", "/api/v1/queryx", "/api/v1/label/values", "/api/v1/label/a/b/values",
		"/api/v1/label//values", "/api/v1/label/x/values/", "/api/v1/series/delete", "/federate/x",
		"/x/api/v1/query", "/prometheus/api/v1/query",
	} {
		if served(p) {
			t.Errorf("%s is served", p)
		}
	}
}

func TestMetricsAccessOffByDefault(t *testing.T) {
	dir := render(t, testConfig())
	for _, f := range []string{platform.TraefikMetricsConfigFile, platform.MetricsAccessHashSecret} {
		if _, err := os.Stat(filepath.Join(dir, f)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s: %v", f, err)
		}
	}
	// VictoriaMetrics stays internal: no ports, and no router of its own.
	for _, f := range []string{platform.ComposeFile, platform.TraefikDynamicConfigFile} {
		b, err := os.ReadFile(filepath.Join(dir, f))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), "pf-metrics") || strings.Contains(string(b), "metrics.agency.example") {
			t.Errorf("%s routes metrics", f)
		}
	}
}

func TestMetricsAccessRemovedWhenTurnedOff(t *testing.T) {
	cfg, _ := metricsConfig(t)
	dir := render(t, cfg)
	cfg.MetricsPublicDomain, cfg.MetricsAccessHash = "", ""
	if err := platform.Render(dir, cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := platform.ReadMetricsAccessHash(dir); !errors.Is(err, platform.ErrNoMetricsAccess) {
		t.Errorf("ReadMetricsAccessHash() = %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, platform.TraefikMetricsConfigFile)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("route stat = %v", err)
	}
}

func TestRotateMetricsAccess(t *testing.T) {
	cfg, oldPassword := metricsConfig(t)
	dir := render(t, cfg)

	password, err := platform.RotateMetricsAccess(dir, cfg.MetricsPublicDomain)
	if err != nil {
		t.Fatal(err)
	}
	if password.Reveal() == oldPassword {
		t.Fatal("the password did not change")
	}
	hash, err := platform.ReadMetricsAccessHash(dir)
	if err != nil {
		t.Fatal(err)
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(password.Reveal())) != nil ||
		bcrypt.CompareHashAndPassword([]byte(hash), []byte(oldPassword)) == nil {
		t.Error("the stored hash doesn't accept exactly the new password")
	}
	route := readMetricsRoute(t, dir)
	for _, m := range route.HTTP.Middlewares {
		if !strings.HasSuffix(m.BasicAuth.Users[0], hash) {
			t.Errorf("the route's hash is not the stored one: %v", m.BasicAuth.Users)
		}
	}
	if _, err := platform.RotateMetricsAccess(dir, "metrics"); err == nil {
		t.Error("RotateMetricsAccess accepted a bad domain")
	}
}

func TestRenderMetricsAccessInvalid(t *testing.T) {
	for name, mutate := range map[string]func(*platform.Config){
		"no hash":          func(c *platform.Config) { c.MetricsAccessHash = "" },
		"plaintext":        func(c *platform.Config) { c.MetricsAccessHash = "hunter2" },
		"bad domain":       func(c *platform.Config) { c.MetricsPublicDomain = "metrics" },
		"dashboard domain": func(c *platform.Config) { c.MetricsPublicDomain = c.DashboardDomain },
	} {
		t.Run(name, func(t *testing.T) {
			cfg, _ := metricsConfig(t)
			mutate(&cfg)
			if err := platform.Render(filepath.Join(t.TempDir(), "platform"), cfg); err == nil {
				t.Error("Render accepted it")
			}
		})
	}
}
