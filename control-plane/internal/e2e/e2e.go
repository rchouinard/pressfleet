// Package e2e is the end-to-end test harness (design doc §10, item 3). It
// boots a real platform, the control plane from a locally built image plus
// MariaDB and Traefik, as an isolated Compose project on the local Docker
// daemon, and drives it through the generated API client.
//
// Tests using it are skipped unless PRESSFLEET_DOCKER_TESTS=1 and
// PRESSFLEET_TEST_CONTROL_PLANE_IMAGE names a control plane image. Run them
// with `make test-e2e`, which builds the image first; see docs/testing.md.
//
// The harness never runs the docker CLI itself: everything goes through
// compose.Project, so the rule that only the Compose driver talks to Docker
// (§4.2) holds for tests too.
package e2e

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"fmt"
	"maps"
	"net"
	"net/http"
	"net/http/cookiejar"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/api/client"
	"github.com/rchouinard/pressfleet/control-plane/internal/config"
	"github.com/rchouinard/pressfleet/control-plane/internal/ingress"
	"github.com/rchouinard/pressfleet/control-plane/internal/platform"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner/compose"
	"github.com/rchouinard/pressfleet/control-plane/internal/redact"
	"github.com/rchouinard/pressfleet/control-plane/internal/secrets"
)

// Environment variables that enable and configure the harness.
const (
	EnvDockerTests       = "PRESSFLEET_DOCKER_TESTS"
	EnvControlPlaneImage = "PRESSFLEET_TEST_CONTROL_PLANE_IMAGE"
)

// Service is the Compose service name of the control plane.
const Service = "control-plane"

// PreviewBaseDomain is the preview base domain of platforms Start boots.
// Nothing resolves it: tests reach sites over their networks, from the
// sites' containers or a client container (compose.RunContainer), until
// Traefik routes preview domains (#36).
const PreviewBaseDomain = "preview.e2e.test"

// DashboardDomain is the dashboard domain of platforms Start boots. Nothing
// resolves it either: tests send it as the Host header (and TLS server
// name) to Traefik's published port (Platform.HTTPSAddr).
const DashboardDomain = "fleet.e2e.test"

// PublicIP is the public_ip of platforms Start boots, an address from
// TEST-NET-1 (RFC 5737) that nothing resolves to. So the go-live DNS check
// (package domains) runs, knows where domains should point, and refuses
// every test domain, which don't resolve at all; tests that go live skip
// it.
const PublicIP = "192.0.2.10"

// ACMECAServer is the ACME directory of platforms the harness boots. The
// .invalid top-level domain never resolves (RFC 6761), so Traefik can't
// reach a certificate authority and serves its self-signed default
// certificate: tests never ask Let's Encrypt for certificates. A test that
// needs real issuance runs Pebble and points the platform at it instead.
const ACMECAServer = "https://acme.invalid/directory"

// Container ports: the control plane's platform.DefaultListen, and
// Traefik's entry points.
const (
	controlPlanePort = 8080
	traefikHTTPPort  = 80
	traefikHTTPSPort = 443
)

// Timeouts for booting and tearing down the platform. Pulling MariaDB on a
// cold CI runner dominates the boot time.
const (
	bootTimeout    = 5 * time.Minute
	cleanupTimeout = 3 * time.Minute
	pullTimeout    = 10 * time.Minute
)

// overrideFile publishes the control plane API on an ephemeral loopback port,
// so tests reach it the same way on Linux and Docker Desktop (where container
// IPs aren't routable from the host). It also moves Traefik from the host's
// ports 80 and 443 to ephemeral loopback ports (TCP only), so platforms can
// run side by side on one host, and next to anything else using those
// ports. Compose merges compose.override.yaml automatically, which keeps
// these test-only changes out of the rendered platform project; !override
// replaces the rendered port list instead of adding to it.
//
// The control plane comes last: Restart appends its environment.
const overrideFile = `# Written by the e2e test harness (control-plane/internal/e2e).
services:
  traefik:
    ports: !override
      - "127.0.0.1::80"
      - "127.0.0.1::443"
  control-plane:
    ports:
      - "127.0.0.1::8080"
`

// Platform is a running platform started by Start.
type Platform struct {
	// Project is the platform's Compose project.
	Project compose.Project
	// DataDir is the data directory, mounted into the control plane at the
	// same path.
	DataDir string
	// BaseURL is the control plane's URL on the host, e.g. http://127.0.0.1:49153.
	BaseURL string
	// HTTPAddr and HTTPSAddr are the host addresses ("127.0.0.1:port") of
	// Traefik's HTTP and HTTPS entry points.
	HTTPAddr, HTTPSAddr string
	// AdminEmail and AdminPassword are the credentials of the admin user
	// Start created; empty for adopted projects.
	AdminEmail    string
	AdminPassword redact.Secret
	// Client is logged in as the admin with a session cookie, and sends the
	// session's CSRF token with unsafe requests.
	Client *client.ClientWithResponses
}

// Start boots the platform under a unique Compose project name with a
// temporary data directory, waits until the control plane is ready, creates
// an admin user, and logs in. Everything is torn down, including volumes and
// networks, when the test ends, whether it passed or not; if it failed, the
// container logs are written to the test log first.
//
// Start skips the test unless the environment enables Docker tests.
func Start(t *testing.T) *Platform {
	t.Helper()
	image := Image(t)

	// Pre-create the layout so the control plane (root in its container)
	// doesn't create root-owned directories the test can't remove.
	layout := config.Layout{Root: t.TempDir()}
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	images, err := platform.DefaultImages("e2e")
	if err != nil {
		t.Fatal(err)
	}
	images.ControlPlane = image
	// serve requires the master key outside the data directory (§4.1).
	keyFile := filepath.Join(t.TempDir(), "master.key")
	if err := os.WriteFile(keyFile, []byte(secrets.GenerateMasterKey().Encode().Reveal()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := platform.Config{
		ProjectName:         "pfe2e-" + randomToken(10),
		DataDir:             layout.Root,
		Images:              images,
		MariaDBRootPassword: redact.Secret(randomToken(26)),
		RedisAdminPassword:  redact.Secret(randomToken(26)),
		MasterKeyFile:       keyFile,
		// Development machines and CI runners often lack Docker address pools.
		SkipPreflight:     true,
		PreviewBaseDomain: PreviewBaseDomain,
		DashboardDomain:   DashboardDomain,
		PublicIP:          PublicIP,
		ACME:              platform.ACMEConfig{CAServer: ACMECAServer},
	}
	dir := layout.PlatformDir()
	if err := platform.Render(dir, cfg); err != nil {
		t.Fatal(err)
	}
	WriteOverride(t, dir)

	p := Adopt(t, compose.Project{Name: cfg.ProjectName, Dir: dir}, layout.Root)
	p.AdminEmail, p.AdminPassword = "admin@e2e.test", redact.Secret(randomToken(26))

	ctx, cancel := context.WithTimeout(t.Context(), bootTimeout)
	defer cancel()
	t.Logf("starting platform project %s in %s", p.Project.Name, p.DataDir)
	// --wait returns once MariaDB and the control plane pass their
	// healthchecks, and fails if either exits or turns unhealthy.
	if err := p.Project.Up(ctx, compose.UpOptions{Wait: true, WaitTimeout: bootTimeout - 30*time.Second}); err != nil {
		t.Fatalf("starting platform: %v", err)
	}
	p.Connect(ctx, t)
	p.createAdmin(ctx, t)
	p.Client = p.Login(t, p.AdminEmail, p.AdminPassword)
	return p
}

// Image returns the control plane image to test, skipping the test unless
// the environment enables Docker tests.
func Image(t *testing.T) string {
	t.Helper()
	if os.Getenv(EnvDockerTests) != "1" {
		t.Skip("set " + EnvDockerTests + "=1 to run tests against Docker")
	}
	image := os.Getenv(EnvControlPlaneImage)
	if image == "" {
		t.Skip("set " + EnvControlPlaneImage + " to a locally built control plane image (see make test-e2e)")
	}
	return image
}

// WriteOverride writes the compose.override.yaml that publishes the control
// plane API and Traefik on ephemeral loopback ports into a platform project
// directory. Every platform a test boots needs it: without it, Traefik
// takes the host's ports 80 and 443.
func WriteOverride(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "compose.override.yaml"), []byte(overrideFile), 0o600); err != nil {
		t.Fatal(err)
	}
}

// Restart recreates the control plane with extra PRESSFLEET_* environment
// variables, the way an operator changes its configuration and restarts it,
// then reconnects and logs in again. Only the control plane is recreated;
// MariaDB and the sites keep running.
func (p *Platform) Restart(t *testing.T, env map[string]string) {
	t.Helper()
	override := overrideFile
	if len(env) > 0 {
		override += "    environment:\n"
		for _, k := range slices.Sorted(maps.Keys(env)) {
			override += "      " + k + ": " + strconv.Quote(env[k]) + "\n"
		}
	}
	if err := os.WriteFile(filepath.Join(p.Project.Dir, "compose.override.yaml"), []byte(override), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), bootTimeout)
	defer cancel()
	t.Logf("restarting the control plane of %s", p.Project.Name)
	if err := p.Project.Up(ctx, compose.UpOptions{Wait: true, WaitTimeout: bootTimeout - 30*time.Second}); err != nil {
		t.Fatalf("restarting the control plane: %v", err)
	}
	p.Connect(ctx, t)
	p.Client = p.Login(t, p.AdminEmail, p.AdminPassword)
}

// Adopt tears down a platform project like Start does when the test ends,
// for projects something else brings up, such as `pressfleet init` in its
// own end-to-end test. Call it after creating dataDir with t.TempDir, so
// the teardown runs before the directory is removed, and before starting
// the project, so a failed start is cleaned up too.
func Adopt(t *testing.T, project compose.Project, dataDir string) *Platform {
	t.Helper()
	p := &Platform{Project: project, DataDir: dataDir}
	t.Cleanup(func() { p.teardown(t) })
	return p
}

// Connect finds the published ports of the control plane and Traefik (see
// WriteOverride) and waits until the control plane reports ready.
func (p *Platform) Connect(ctx context.Context, t *testing.T) {
	t.Helper()
	for _, port := range []struct {
		service string
		port    int
		dst     *string
	}{
		{Service, controlPlanePort, &p.BaseURL},
		{ingress.TraefikService, traefikHTTPPort, &p.HTTPAddr},
		{ingress.TraefikService, traefikHTTPSPort, &p.HTTPSAddr},
	} {
		addr, err := p.Project.Port(ctx, port.service, port.port)
		if err != nil {
			t.Fatal(err)
		}
		*port.dst = addr
	}
	p.BaseURL = "http://" + p.BaseURL
	p.waitReady(ctx, t)
}

// NewClient returns an unauthenticated client for the control plane. Its
// cookie jar keeps a session once the client logs in.
func (p *Platform) NewClient(t *testing.T, opts ...client.ClientOption) *client.ClientWithResponses {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	httpClient := &http.Client{Jar: jar, Timeout: 30 * time.Second}
	c, err := client.NewClientWithResponses(p.BaseURL, append([]client.ClientOption{client.WithHTTPClient(httpClient)}, opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// Login logs in with a new session and returns a client that uses it,
// including the session's CSRF token on unsafe requests.
func (p *Platform) Login(t *testing.T, email string, password redact.Secret) *client.ClientWithResponses {
	t.Helper()
	var csrf string
	c := p.NewClient(t, client.WithRequestEditorFn(func(_ context.Context, req *http.Request) error {
		switch req.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
		default:
			if csrf != "" {
				req.Header.Set("X-CSRF-Token", csrf)
			}
		}
		return nil
	}))
	// writeOnly makes oapi-codegen generate the password as optional.
	pw := password.Reveal()
	resp, err := c.LoginWithResponse(t.Context(), client.LoginJSONRequestBody{Email: client.Email(email), Password: &pw})
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if resp.JSON200 == nil || resp.JSON200.CsrfToken == nil {
		t.Fatalf("login: status %d: %s", resp.StatusCode(), resp.Body)
	}
	csrf = *resp.JSON200.CsrfToken
	return c
}

// IngressClient returns an HTTP client that reaches Traefik's published
// ports whatever host a URL names: port 80 of a URL goes to HTTPAddr and
// port 443 to HTTPSAddr. It sends the URL's host as the Host header and TLS
// server name, as a browser would once DNS points at the host. It doesn't
// follow redirects, and doesn't verify certificates: with ACMECAServer,
// Traefik serves its self-signed default certificate. It speaks HTTP/2
// when Traefik offers it.
func (p *Platform) IngressClient(t *testing.T) *http.Client {
	t.Helper()
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			_, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			switch port {
			case "80":
				return dialer.DialContext(ctx, network, p.HTTPAddr)
			case "443":
				return dialer.DialContext(ctx, network, p.HTTPSAddr)
			}
			return nil, fmt.Errorf("e2e: ingress client only dials ports 80 and 443, not %s", addr)
		},
		TLSClientConfig:   &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // G402: Traefik's self-signed default certificate, see above
		ForceAttemptHTTP2: true,
	}
	t.Cleanup(transport.CloseIdleConnections)
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Client{
		Transport:     transport,
		Jar:           jar,
		Timeout:       30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// BearerClient returns a client that authenticates with an API token.
func (p *Platform) BearerClient(t *testing.T, token redact.Secret) *client.ClientWithResponses {
	t.Helper()
	return p.NewClient(t, client.WithRequestEditorFn(func(_ context.Context, req *http.Request) error {
		req.Header.Set("Authorization", "Bearer "+token.Reveal())
		return nil
	}))
}

// waitReady polls GET /readyz until the control plane reports ready. The
// container healthcheck only proves liveness.
func (p *Platform) waitReady(ctx context.Context, t *testing.T) {
	t.Helper()
	c := p.NewClient(t)
	var last string
	for {
		resp, err := c.GetReadinessWithResponse(ctx)
		switch {
		case err != nil:
			last = err.Error()
		case resp.StatusCode() == http.StatusOK:
			return
		default:
			last = fmt.Sprintf("status %d: %s", resp.StatusCode(), resp.Body)
		}
		select {
		case <-ctx.Done():
			t.Fatalf("control plane not ready: %s", last)
		case <-time.After(time.Second):
		}
	}
}

// createAdmin creates the admin user the way an operator bootstraps one:
// with the CLI inside the control plane container, which owns the state
// database (§4.7). The password goes over stdin, never on the command line.
func (p *Platform) createAdmin(ctx context.Context, t *testing.T) {
	t.Helper()
	out, err := p.Project.ExecInput(ctx, strings.NewReader(p.AdminPassword.Reveal()+"\n"), Service,
		"pressfleet", "user", "create", "--email", p.AdminEmail, "--role", "admin", "--password-stdin")
	if err != nil {
		t.Fatalf("creating admin user: %v", err)
	}
	t.Logf("%s", strings.TrimSpace(string(out)))
}

// teardown removes everything Start created. It reports, rather than
// stops at, each failure so that as much as possible is cleaned up.
func (p *Platform) teardown(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
	defer cancel()
	if t.Failed() {
		if logs, err := p.Project.Logs(ctx); err != nil {
			t.Logf("collecting container logs: %v", err)
		} else {
			t.Logf("container logs of %s:\n%s", p.Project.Name, logs)
		}
	}
	// Hand anything the root container wrote back to the test user, so the
	// temporary directory can be removed. It fails harmlessly if the control
	// plane never started.
	owner := fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid())
	if _, err := p.Project.Exec(ctx, Service, "chown", "-R", owner, p.DataDir); err != nil {
		t.Logf("chown data dir: %v", err)
	}
	err := p.Project.Down(ctx, compose.DownOptions{Volumes: true})
	if err != nil {
		// Transient daemon errors ("removal already in progress") are common
		// enough on busy runners to deserve one retry.
		t.Logf("down: %v; retrying", err)
		err = p.Project.Down(ctx, compose.DownOptions{Volumes: true})
	}
	if err != nil {
		t.Errorf("down: %v", err)
	}
	res, err := p.Project.Resources(ctx)
	switch {
	case err != nil:
		t.Errorf("listing leftover resources: %v", err)
	case !res.Empty():
		t.Errorf("project %s left resources behind: %+v", p.Project.Name, res)
	}
}

// EnsureImages makes the images available to the local Docker daemon,
// pulling any that are missing. Site images come from `make images` or the
// registry; without them a site can't start, so the test fails with
// instructions rather than timing out in a provisioning job.
func EnsureImages(t *testing.T, images ...string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), pullTimeout)
	defer cancel()
	for _, image := range images {
		pulled, err := compose.EnsureImage(ctx, nil, image)
		if err != nil {
			t.Fatalf("site image %s is not available locally and couldn't be pulled; build the site images with `make images` (see images/README.md): %v", image, err)
		}
		if pulled {
			t.Logf("pulled %s", image)
		}
	}
}

// randomToken returns n random lowercase base32 characters, which are valid
// in Compose project names and passwords alike.
func randomToken(n int) string {
	return strings.ToLower(rand.Text())[:n]
}
