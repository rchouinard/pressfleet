package kubernetes

import (
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rchouinard/pressfleet/control-plane/internal/config"
)

// apiServer answers /version as an API server does, over TLS, for requests
// with the bearer token "secret-token", and 401 otherwise. client-go sends
// bearer tokens over TLS only.
func apiServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret-token" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = fmt.Fprint(w, `{"kind":"Status","apiVersion":"v1","status":"Failure","message":"Unauthorized","reason":"Unauthorized","code":401}`)
			return
		}
		if r.URL.Path != "/version" {
			http.NotFound(w, r)
			return
		}
		if ua := r.Header.Get("User-Agent"); ua != "pressfleet" {
			t.Errorf("User-Agent = %q", ua)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"major":"1","minor":"35","gitVersion":"v1.35.1","platform":"linux/amd64"}`)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// writeKubeconfig writes a kubeconfig for srv with token.
func writeKubeconfig(t *testing.T, srv *httptest.Server, token string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "kubeconfig")
	ca := base64.StdEncoding.EncodeToString(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}))
	content := fmt.Sprintf(`apiVersion: v1
kind: Config
clusters:
- name: dev
  cluster: {server: %q, certificate-authority-data: %q}
users:
- name: dev
  user: {token: %q}
contexts:
- name: dev
  context: {cluster: dev, user: dev}
current-context: dev
`, srv.URL, ca, token)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestConnectWithKubeconfig(t *testing.T) {
	srv := apiServer(t)
	p, err := New(Config{
		Layout:     config.Layout{Root: t.TempDir()},
		Kubernetes: config.KubernetesConfig{Kubeconfig: writeKubeconfig(t, srv, "secret-token"), PlatformNamespace: "pressfleet"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Connect(t.Context()); err != nil {
		t.Fatal(err)
	}
	info, err := p.Ping(t.Context())
	if err != nil || info.GitVersion != "v1.35.1" {
		t.Errorf("Ping = %+v, %v", info, err)
	}
}

func TestConnectFails(t *testing.T) {
	srv := apiServer(t)
	unreachable := httptest.NewTLSServer(http.NotFoundHandler())
	unreachable.Close()

	for name, kubeconfig := range map[string]string{
		"wrong token": writeKubeconfig(t, srv, "wrong"),
		"unreachable": writeKubeconfig(t, unreachable, "secret-token"),
	} {
		t.Run(name, func(t *testing.T) {
			p, err := New(Config{Kubernetes: config.KubernetesConfig{Kubeconfig: kubeconfig}})
			if err != nil {
				t.Fatal(err)
			}
			if err := p.Connect(t.Context()); !errors.Is(err, ErrConnect) {
				t.Errorf("Connect = %v, want ErrConnect", err)
			}
		})
	}

	t.Run("missing kubeconfig", func(t *testing.T) {
		missing := filepath.Join(t.TempDir(), "kubeconfig")
		if _, err := New(Config{Kubernetes: config.KubernetesConfig{Kubeconfig: missing}}); !errors.Is(err, ErrConnect) || !strings.Contains(err.Error(), missing) {
			t.Errorf("New = %v, want ErrConnect naming the file", err)
		}
	})

	t.Run("not in a pod", func(t *testing.T) {
		t.Setenv("KUBERNETES_SERVICE_HOST", "")
		t.Setenv("KUBERNETES_SERVICE_PORT", "")
		if _, err := New(Config{}); !errors.Is(err, ErrConnect) || !strings.Contains(err.Error(), "kubernetes.kubeconfig") {
			t.Errorf("New = %v, want ErrConnect pointing at kubernetes.kubeconfig", err)
		}
	})
}

func TestPingWithoutREST(t *testing.T) {
	c, _ := fakeClient(t)
	if _, err := c.Ping(t.Context()); !errors.Is(err, ErrConnect) {
		t.Errorf("Ping on the fake clientset = %v, want ErrConnect", err)
	}
}

func TestWarningsAreLogged(t *testing.T) {
	var buf strings.Builder
	w := slogWarnings{slog.New(slog.NewTextHandler(&buf, nil))}
	w.HandleWarningHeaderWithContext(t.Context(), 299, "-", "policy/v1beta1 PodSecurityPolicy is deprecated")
	w.HandleWarningHeaderWithContext(t.Context(), 199, "-", "not for clients")
	if got := buf.String(); !strings.Contains(got, "PodSecurityPolicy is deprecated") || strings.Contains(got, "not for clients") {
		t.Errorf("logged %q", got)
	}
}
