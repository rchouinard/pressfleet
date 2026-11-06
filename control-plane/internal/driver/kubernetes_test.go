package driver_test

import (
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/rchouinard/pressfleet/control-plane/internal/catalog"
	"github.com/rchouinard/pressfleet/control-plane/internal/config"
	"github.com/rchouinard/pressfleet/control-plane/internal/driver"
)

// kubernetesConfig configures the Kubernetes driver for a fake API server
// that answers /version, through a kubeconfig file.
func kubernetesConfig(t *testing.T, up bool) *config.Config {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/version" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"major":"1","minor":"35","gitVersion":"v1.35.1"}`)
	}))
	if up {
		t.Cleanup(srv.Close)
	} else {
		srv.Close()
	}
	ca := base64.StdEncoding.EncodeToString(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}))
	kubeconfig := filepath.Join(t.TempDir(), "kubeconfig")
	content := fmt.Sprintf(`apiVersion: v1
kind: Config
clusters: [{name: dev, cluster: {server: %q, certificate-authority-data: %q}}]
users: [{name: dev, user: {token: dev-token}}]
contexts: [{name: dev, context: {cluster: dev, user: dev}}]
current-context: dev
`, srv.URL, ca)
	if err := os.WriteFile(kubeconfig, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := testConfig(t)
	cfg.Driver = config.SiteDriverKubernetes
	cfg.Database = config.DatabaseConfig{Driver: config.DriverPostgres, PostgresDSN: "postgres://pressfleet@db/pressfleet"}
	cfg.Kubernetes.Kubeconfig = kubeconfig
	cfg.Kubernetes.PodCIDRs = []string{"10.244.0.0/16"}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestNewKubernetes(t *testing.T) {
	cfg := kubernetesConfig(t, true)
	rt, err := driver.New(t.Context(), driver.Options{Config: cfg, Secrets: noSecrets{}, Images: &catalog.Catalog{}, Logger: discard()})
	if err != nil {
		t.Fatal(err)
	}
	if rt.Driver == nil || rt.Templates == nil || rt.HostProbe == nil {
		t.Errorf("the Kubernetes driver left a capability out: %+v", rt)
	}
	// Off until the driver has them (#216, #218), which turns their
	// features off rather than failing them.
	if rt.Snapshots != nil || rt.FPMStatus != nil || rt.TraefikMetrics != nil {
		t.Errorf("the Kubernetes driver claims capabilities it lacks: %+v", rt)
	}
	if fp := rt.Templates.TemplateFingerprint(); fp == "" {
		t.Error("empty template fingerprint")
	}
	if _, err := rt.Driver.GetSite(t.Context(), "01hzy3k5q7m9c2v4b6n8r0t1wx"); err == nil {
		t.Error("GetSite succeeded on a driver that doesn't implement it yet")
	}
	if _, err := rt.HostProbe.Engine(t.Context()); err == nil {
		t.Error("the Kubernetes driver's host probe found a Docker engine")
	}
}

// The control plane doesn't start without its cluster.
func TestNewKubernetesUnreachable(t *testing.T) {
	cfg := kubernetesConfig(t, false)
	if _, err := driver.New(t.Context(), driver.Options{Config: cfg, Secrets: noSecrets{}, Images: &catalog.Catalog{}, Logger: discard()}); err == nil {
		t.Error("New succeeded without a cluster")
	}
}

func TestKubernetesRepositoryTasksAndHostProbe(t *testing.T) {
	cfg := kubernetesConfig(t, false)
	// Neither contacts the cluster.
	if r, err := driver.NewRepositoryTasks(cfg, discard()); err != nil || r == nil {
		t.Errorf("NewRepositoryTasks = %v, %v", r, err)
	}
	p, err := driver.NewHostProbe(cfg)
	if err != nil || p == nil {
		t.Fatalf("NewHostProbe = %v, %v", p, err)
	}
	if _, err := p.ComposeVersion(t.Context()); err == nil {
		t.Error("the Kubernetes driver's host probe found Docker Compose")
	}
}
