package e2e_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/rchouinard/pressfleet/control-plane/internal/api"
	"github.com/rchouinard/pressfleet/control-plane/internal/api/client"
	"github.com/rchouinard/pressfleet/control-plane/internal/e2e"
	"github.com/rchouinard/pressfleet/control-plane/internal/ingress"
)

// TestIngress checks Traefik (#33): HTTP redirects to HTTPS, the dashboard
// domain reaches the control plane over HTTP/2 with HTTP/3 advertised, and
// Traefik reads the Docker API only through the socket proxy, which refuses
// anything but the read-only calls Traefik needs.
func TestIngress(t *testing.T) {
	p := e2e.Start(t)
	hc := p.IngressClient(t)
	dashboard := "https://" + e2e.DashboardDomain

	get := func(t *testing.T, url string) *http.Response {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := hc.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return resp
	}

	t.Run("redirects HTTP to HTTPS", func(t *testing.T) {
		resp := get(t, "http://"+e2e.DashboardDomain+"/readyz?x=1")
		if resp.StatusCode != http.StatusPermanentRedirect && resp.StatusCode != http.StatusMovedPermanently {
			t.Errorf("status = %d, want a permanent redirect", resp.StatusCode)
		}
		if loc := resp.Header.Get("Location"); loc != dashboard+"/readyz?x=1" {
			t.Errorf("Location = %q", loc)
		}
	})

	t.Run("dashboard domain", func(t *testing.T) {
		resp := get(t, dashboard+"/readyz")
		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET /readyz: status %d", resp.StatusCode)
		}
		if resp.ProtoMajor != 2 {
			t.Errorf("protocol = %s, want HTTP/2", resp.Proto)
		}
		if alt := resp.Header.Get("Alt-Svc"); !strings.Contains(alt, `h3=":443"`) {
			t.Errorf("Alt-Svc = %q, want HTTP/3 advertised", alt)
		}
	})

	t.Run("unknown host", func(t *testing.T) {
		if resp := get(t, "https://unknown.e2e.test/readyz"); resp.StatusCode != http.StatusNotFound {
			t.Errorf("status = %d, want 404", resp.StatusCode)
		}
	})

	// Traefik terminates TLS and tells the control plane, a trusted proxy,
	// that the request was HTTPS, so the session cookie is Secure.
	t.Run("login through traefik", func(t *testing.T) {
		c, err := client.NewClientWithResponses(dashboard, client.WithHTTPClient(hc))
		if err != nil {
			t.Fatal(err)
		}
		pw := p.AdminPassword.Reveal()
		resp, err := c.LoginWithResponse(t.Context(), client.LoginJSONRequestBody{Email: client.Email(p.AdminEmail), Password: &pw})
		if err != nil {
			t.Fatal(err)
		}
		if resp.JSON200 == nil {
			t.Fatalf("login: status %d: %s", resp.StatusCode(), resp.Body)
		}
		var secure bool
		for _, c := range resp.HTTPResponse.Cookies() {
			if c.Name == api.SessionCookieName {
				secure = c.Secure
			}
		}
		if !secure {
			t.Errorf("session cookie not Secure: %v", resp.HTTPResponse.Header.Values("Set-Cookie"))
		}
		me, err := c.GetCurrentUserWithResponse(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if me.JSON200 == nil || string(me.JSON200.Email) != p.AdminEmail {
			t.Errorf("current user: status %d: %s", me.StatusCode(), me.Body)
		}
	})

	t.Run("no docker socket in traefik", func(t *testing.T) {
		tr, err := p.Project.ServiceContainer(t.Context(), ingress.TraefikService)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range tr.Mounts {
			if strings.HasSuffix(m.Source, "docker.sock") || strings.HasSuffix(m.Destination, "docker.sock") {
				t.Errorf("traefik mounts the Docker socket: %+v", m)
			}
		}
		proxy, err := p.Project.ServiceContainer(t.Context(), "socket-proxy")
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range proxy.Mounts {
			if m.Destination == "/var/run/docker.sock" && m.RW {
				t.Errorf("socket proxy mounts the Docker socket read-write: %+v", m)
			}
		}
	})

	// Traefik's image has busybox wget, which exits non-zero and names the
	// status on an HTTP error.
	wget := func(t *testing.T, args ...string) (string, error) {
		t.Helper()
		out, err := p.Project.ExecService(t.Context(), ingress.TraefikService, append([]string{"wget", "-q", "-O-"}, args...)...)
		return string(out), err
	}

	t.Run("socket proxy is read-only", func(t *testing.T) {
		const api = "http://socket-proxy:2375/v1.47"
		if out, err := wget(t, api+"/containers/json"); err != nil || !strings.HasPrefix(out, "[") {
			t.Fatalf("listing containers: %v %.200s", err, out)
		}
		tr, err := p.Project.ServiceContainer(t.Context(), ingress.TraefikService)
		if err != nil {
			t.Fatal(err)
		}
		for _, tc := range []struct {
			name string
			args []string
		}{
			{"create a container", []string{"--post-data", "{}", "--header", "Content-Type: application/json", api + "/containers/create"}},
			{"stop a container", []string{"--post-data", "", api + "/containers/" + tr.ID + "/stop"}},
			{"read logs", []string{api + "/containers/" + tr.ID + "/logs?stdout=1"}},
			{"download files", []string{api + "/containers/" + tr.ID + "/archive?path=/etc"}},
			{"list images", []string{api + "/images/json"}},
		} {
			out, err := wget(t, tc.args...)
			if err == nil {
				t.Errorf("%s: allowed: %.200s", tc.name, out)
			} else if msg := err.Error(); !strings.Contains(msg, "403") && !strings.Contains(msg, "405") {
				t.Errorf("%s: %v, want 403 or 405 from the proxy", tc.name, err)
			}
		}
	})

	t.Run("metrics", func(t *testing.T) {
		out, err := wget(t, fmt.Sprintf("http://127.0.0.1:%d/metrics", ingress.InternalPort))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out, `traefik_router_requests_total`) {
			t.Errorf("no router metrics in:\n%.2000s", out)
		}
	})
}
