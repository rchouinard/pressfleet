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
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/rchouinard/pressfleet/control-plane/internal/api/client"
	"github.com/rchouinard/pressfleet/control-plane/internal/config"
	"github.com/rchouinard/pressfleet/control-plane/internal/ingress"
	"github.com/rchouinard/pressfleet/control-plane/internal/platform"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner/compose"
	"github.com/rchouinard/pressfleet/control-plane/internal/redact"
	"github.com/rchouinard/pressfleet/control-plane/internal/secrets"
	"github.com/rchouinard/pressfleet/control-plane/internal/sftpgw"
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

// override is the compose.override.yaml the harness writes next to the
// rendered platform project. It publishes the control plane API on an
// ephemeral loopback port, so tests reach it the same way on Linux and
// Docker Desktop (where container IPs aren't routable from the host). It
// also moves Traefik from the host's ports 80 and 443 to ephemeral loopback
// ports (TCP only), so platforms can run side by side on one host, and next
// to anything else using those ports. Compose merges compose.override.yaml
// automatically, which keeps these test-only changes out of the rendered
// platform project; !override replaces the rendered port list instead of
// adding to it.
type override struct {
	// services are extra services, as YAML indented to be entries of
	// services: (see pebble.go).
	services string
	// traefikDependsOn are services Traefik starts after.
	traefikDependsOn []string
	// dns are the control plane's DNS servers (IP addresses).
	dns []string
	// env is extra environment of the control plane, on top of the
	// rendered one.
	env map[string]string
	// traefikMemory, if set, replaces Traefik's memory limit and sets
	// GOMEMLIMIT to the given value, for releases older than the limit
	// Traefik needs.
	traefikMemory, traefikGoMemLimit string
}

// render returns the override file's contents.
func (o override) render() string {
	var b strings.Builder
	b.WriteString(`# Written by the e2e test harness (control-plane/internal/e2e).
services:
  traefik:
    ports: !override
      - "127.0.0.1::80"
      - "127.0.0.1::443"
`)
	if o.traefikMemory != "" {
		b.WriteString("    environment:\n      GOMEMLIMIT: " + strconv.Quote(o.traefikGoMemLimit) + "\n")
		b.WriteString("    deploy:\n      resources:\n        limits:\n          memory: " + o.traefikMemory + "\n")
	}
	if len(o.traefikDependsOn) > 0 {
		b.WriteString("    depends_on:\n")
		for _, s := range o.traefikDependsOn {
			b.WriteString("      - " + s + "\n")
		}
	}
	// !override also replaces the SFTP gateway's host port.
	b.WriteString(`  control-plane:
    ports: !override
      - "127.0.0.1::8080"
      - "127.0.0.1::2222"
`)
	if len(o.dns) > 0 {
		b.WriteString("    dns:\n")
		for _, ip := range o.dns {
			b.WriteString("      - " + strconv.Quote(ip) + "\n")
		}
	}
	// Test platforms don't ask GitHub for releases (#140) unless a test
	// says otherwise.
	env := map[string]string{"PRESSFLEET_UPDATES_RELEASE_CHECK": "false"}
	maps.Copy(env, o.env)
	b.WriteString("    environment:\n")
	for _, k := range slices.Sorted(maps.Keys(env)) {
		b.WriteString("      " + k + ": " + strconv.Quote(env[k]) + "\n")
	}
	b.WriteString(o.services)
	return b.String()
}

// write writes the override file into a platform project directory.
func (o override) write(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "compose.override.yaml"), []byte(o.render()), 0o600); err != nil {
		t.Fatal(err)
	}
}

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
	// SFTPAddr is the host address of the SFTP gateway.
	SFTPAddr string
	// HostKey is the SFTP gateway's public host key; nil for adopted
	// projects.
	HostKey ssh.PublicKey
	// AdminEmail and AdminPassword are the credentials of the admin user
	// Start created; empty for adopted projects.
	AdminEmail    string
	AdminPassword redact.Secret
	// Client is logged in as the admin with a session cookie, and sends the
	// session's CSRF token with unsafe requests.
	Client *client.ClientWithResponses
	// Pebble is the platform's certificate authority; nil unless the
	// platform was started with StartWithPebble.
	Pebble *Pebble

	// Kube is the platform's Kubernetes side; nil for a platform on
	// Compose. Project is the zero value then.
	Kube *KubePlatform

	// apiTransport, if set, is how clients reach BaseURL: on Kubernetes,
	// through Traefik.
	apiTransport http.RoundTripper
	// override is the compose.override.yaml Start wrote, which Restart
	// keeps.
	override override
	// masterKey is the platform's master key, for SealSetting.
	masterKey secrets.MasterKey
}

// SealSetting encrypts a setting with the platform's master key, as
// pressfleet init writes secrets into the config file (e.g.
// config.BackupS3SecretAccessKeySetting), for Restart's environment.
func (p *Platform) SealSetting(t *testing.T, name string, plaintext redact.Secret) string {
	t.Helper()
	keys, err := secrets.NewKeyring(p.masterKey)
	if err != nil {
		t.Fatal(err)
	}
	return keys.SealSetting(name, plaintext)
}

// BackupNetwork is the platform's backup network, where a self-hosted S3
// service for backups is attached (platform.BackupNetwork).
func (p *Platform) BackupNetwork() string { return platform.BackupNetwork(p.Project.Name) }

// Start boots the platform under a unique Compose project name with a
// temporary data directory, waits until the control plane is ready, creates
// an admin user, and logs in. Everything is torn down, including volumes and
// networks, when the test ends, whether it passed or not; if it failed, the
// container logs are written to the test log first.
//
// Its ACME directory (ACMECAServer) never resolves, so Traefik serves its
// self-signed default certificate; StartWithPebble boots a platform that
// gets real certificates.
//
// Start skips the test unless the environment enables Docker tests.
func Start(t *testing.T) *Platform {
	t.Helper()
	return start(t, false)
}

// start is Start, and with withPebble, StartWithPebble.
func start(t *testing.T, withPebble bool) *Platform {
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
	masterKey := secrets.GenerateMasterKey()
	if err := os.WriteFile(keyFile, []byte(masterKey.Encode().Reveal()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := layout.PlatformDir()
	hostKey := HostKey(t)
	signer, err := ssh.ParsePrivateKey([]byte(hostKey.Reveal()))
	if err != nil {
		t.Fatal(err)
	}
	cfg := platform.Config{
		ProjectName:         "pfe2e-" + randomToken(10),
		DataDir:             layout.Root,
		Images:              images,
		MariaDBRootPassword: redact.Secret(randomToken(26)),
		RedisAdminPassword:  redact.Secret(randomToken(26)),
		SSHHostKey:          hostKey,
		MasterKeyFile:       keyFile,
		// Development machines and CI runners often lack Docker address pools.
		SkipPreflight:     true,
		PreviewBaseDomain: PreviewBaseDomain,
		DashboardDomain:   DashboardDomain,
		PublicIP:          PublicIP,
		ACME:              platform.ACMEConfig{CAServer: ACMECAServer},
	}
	var o override
	if withPebble {
		cfg.ACME, o = preparePebble(t, dir)
	}
	if err := platform.Render(dir, cfg); err != nil {
		t.Fatal(err)
	}
	o.write(t, dir)

	p := Adopt(t, compose.Project{Name: cfg.ProjectName, Dir: dir}, layout.Root)
	p.override = o
	p.masterKey = masterKey
	p.HostKey = signer.PublicKey()
	p.AdminEmail, p.AdminPassword = "admin@e2e.test", redact.Secret(randomToken(26))

	ctx, cancel := context.WithTimeout(t.Context(), bootTimeout)
	defer cancel()
	t.Logf("starting platform project %s in %s", p.Project.Name, p.DataDir)
	p.up(ctx, t)
	p.Connect(ctx, t)
	if withPebble {
		p.connectPebble(ctx, t)
	}
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
	override{}.write(t, dir)
}

// WriteReleaseOverride is WriteOverride for a release older than the Traefik
// memory limit raised in 6f4041a (96 MiB to 192 MiB). At 96 MiB, on a busy
// runner, Traefik serves but every healthcheck exceeds its 5s timeout, which
// fails the release's install; the tests that use this aren't about Traefik's
// memory, so they run it with the current limit.
func WriteReleaseOverride(t *testing.T, dir string) {
	t.Helper()
	override{traefikMemory: "192M", traefikGoMemLimit: "144MiB"}.write(t, dir)
}

// Restart recreates the control plane with extra PRESSFLEET_* environment
// variables, the way an operator changes its configuration and restarts it,
// then reconnects and logs in again. Only the control plane is recreated;
// MariaDB and the sites keep running.
func (p *Platform) Restart(t *testing.T, env map[string]string) {
	t.Helper()
	if p.Kube != nil {
		p.kubeRestart(t, env)
		return
	}
	o := p.override
	o.env = maps.Clone(p.override.env)
	if o.env == nil {
		o.env = map[string]string{}
	}
	maps.Copy(o.env, env)
	o.write(t, p.Project.Dir)
	ctx, cancel := context.WithTimeout(t.Context(), bootTimeout)
	defer cancel()
	t.Logf("restarting the control plane of %s", p.Project.Name)
	p.up(ctx, t)
	p.Connect(ctx, t)
	p.Client = p.Login(t, p.AdminEmail, p.AdminPassword)
}

// up brings the project up and waits until its services are running, and
// healthy where they have a healthcheck. --wait fails if any exits or turns
// unhealthy.
func (p *Platform) up(ctx context.Context, t *testing.T) {
	t.Helper()
	if err := p.Project.Up(ctx, compose.UpOptions{Wait: true, WaitTimeout: bootTimeout - 30*time.Second}); err != nil {
		t.Fatalf("starting platform %s: %v", p.Project.Name, err)
	}
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
// WriteOverride) and waits until the control plane reports ready. On
// Kubernetes, it finds Traefik's NodePorts and waits the same way.
func (p *Platform) Connect(ctx context.Context, t *testing.T) {
	t.Helper()
	if p.Kube != nil {
		p.kubeConnect(ctx, t)
		return
	}
	for _, port := range []struct {
		service string
		port    int
		dst     *string
	}{
		{Service, controlPlanePort, &p.BaseURL},
		{ingress.TraefikService, traefikHTTPPort, &p.HTTPAddr},
		{ingress.TraefikService, traefikHTTPSPort, &p.HTTPSAddr},
		{Service, platform.SFTPContainerPort, &p.SFTPAddr},
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
	return p.newClient(t, nil, opts...)
}

// newClient is NewClient with an HTTP transport; nil means the default.
func (p *Platform) newClient(t *testing.T, transport http.RoundTripper, opts ...client.ClientOption) *client.ClientWithResponses {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	if transport == nil {
		transport = p.apiTransport
	}
	httpClient := &http.Client{Jar: jar, Transport: transport, Timeout: 30 * time.Second}
	c, err := client.NewClientWithResponses(p.BaseURL, append([]client.ClientOption{client.WithHTTPClient(httpClient)}, opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// Login logs in with a new session and returns a client that uses it,
// including the session's CSRF token on unsafe requests. The client
// reauthenticates with the password when a sensitive action finds the
// session's last check too old (reauthTransport), so it lasts as long as a
// test does; the user must have no second factor.
func (p *Platform) Login(t *testing.T, email string, password redact.Secret) *client.ClientWithResponses {
	t.Helper()
	var csrf string
	rt := &reauthTransport{base: http.DefaultTransport}
	if p.apiTransport != nil {
		rt.base = p.apiTransport
	}
	c := p.newClient(t, rt, client.WithRequestEditorFn(func(_ context.Context, req *http.Request) error {
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
	rt.reauthenticate = reauthenticateWithPassword(c, password)
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
	return p.ingressClient(t, &tls.Config{InsecureSkipVerify: true}) //nolint:gosec // G402: Traefik's self-signed default certificate, see above
}

// VerifyingIngressClient is IngressClient, but verifies certificates like
// a browser that trusts Pebble's root does, for the URL's host: a request
// fails while Traefik still serves its default certificate for the host,
// before Pebble has issued one. It needs a platform started with
// StartWithPebble.
func (p *Platform) VerifyingIngressClient(t *testing.T) *http.Client {
	t.Helper()
	if p.Pebble == nil {
		t.Fatal("e2e: VerifyingIngressClient needs a platform started with StartWithPebble")
	}
	return p.ingressClient(t, &tls.Config{RootCAs: p.Pebble.Roots, MinVersion: tls.VersionTLS12})
}

func (p *Platform) ingressClient(t *testing.T, tlsConfig *tls.Config) *http.Client {
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
		TLSClientConfig:   tlsConfig,
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
		// Logs don't show why a healthcheck failed. Docker keeps that
		// with the container, so it has to be read before Down removes it.
		p.logHealth(ctx, t)
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

// logHealth logs the healthcheck state of the project's containers that
// have one, with the output of the most recent probes: the whole log for a
// container that isn't healthy, the last probe for one that is.
func (p *Platform) logHealth(ctx context.Context, t *testing.T) {
	health, err := p.Project.Healthchecks(ctx)
	if err != nil {
		t.Logf("collecting healthchecks: %v", err)
		return
	}
	var b strings.Builder
	for _, h := range health {
		fmt.Fprintf(&b, "%s: health %s (failing streak %d), state %s, exit code %d, OOM killed %t, restarts %d\n",
			h.Name, h.Health, h.FailingStreak, h.Status, h.ExitCode, h.OOMKilled, h.RestartCount)
		probes := h.Probes
		if h.Health == "healthy" && len(probes) > 1 {
			probes = probes[len(probes)-1:]
		}
		for _, probe := range probes {
			fmt.Fprintf(&b, "    probe %s to %s, %s, exit code %d: %s\n",
				probe.Start.Format(time.RFC3339), probe.End.Format(time.RFC3339), probe.End.Sub(probe.Start).Round(time.Millisecond),
				probe.ExitCode, strings.TrimSpace(probe.Output))
		}
	}
	if b.Len() == 0 {
		b.WriteString("no container of the project has a healthcheck\n")
	}
	t.Logf("container healthchecks of %s:\n%s", p.Project.Name, b.String())
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

// EnsureStagingImages makes site-php images ready for staging sites. Call it,
// after EnsureImages, in any test that creates a staging site, and before it
// creates the production site: a staging site inherits production's build, and
// is refused if that build lacks the outbound mail switch (#272).
//
// EnsureImages uses a local image as it is, and image tags are rebuilt without
// changing, so a host can hold a build from before the switch, which the control
// plane may have no credentials to replace. Such a tag is pulled here, once, and
// the refresh is logged. An image with the switch, such as one from `make images`,
// isn't touched. If the pull doesn't help, the test stops, naming the image.
func EnsureStagingImages(t *testing.T, images ...string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), pullTimeout)
	defer cancel()
	for _, image := range images {
		refreshed, err := compose.EnsureImageFile(ctx, nil, image, provisioner.MailSwitchPath)
		if err != nil {
			t.Fatalf("%s has no mail switch; pull it or run `make images`: %v", image, err)
		}
		if refreshed {
			t.Logf("refreshed %s: the local build had no mail switch", image)
		}
	}
}

// RunsImage reports whether a site container runs the build that the image
// tag names now. Site containers run pinned builds (repository@sha256:...,
// §5.5), not the tag, so the image IDs are compared.
func RunsImage(t *testing.T, c compose.ContainerDetails, image string) bool {
	t.Helper()
	id, err := compose.ImageID(t.Context(), nil, image)
	if err != nil {
		t.Fatal(err)
	}
	return c.ImageID == id
}

// HostKey returns a new SFTP gateway host key for a rendered platform
// (platform.Config.SSHHostKey).
func HostKey(t *testing.T) redact.Secret {
	t.Helper()
	pem, err := sftpgw.GenerateHostKey()
	if err != nil {
		t.Fatal(err)
	}
	return redact.Secret(pem)
}

// randomToken returns n random lowercase base32 characters, which are valid
// in Compose project names and passwords alike.
func randomToken(n int) string {
	return strings.ToLower(rand.Text())[:n]
}

// ProcessWatch samples the processes running in the control plane container
// (docker compose top), to show what the control plane runs while a test
// drives it. It follows the service across restarts. Sampling every 100 ms or
// so can miss a process that lives for less than that, which is why tests
// keep a positive control: something they know the watch should see.
type ProcessWatch struct {
	cancel context.CancelFunc
	done   chan struct{}

	mu      sync.Mutex
	seen    map[string]int // program name -> samples that showed it
	samples int
}

// WatchControlPlane starts sampling the control plane's processes. Call Stop
// to end the watch and get what it saw.
func (p *Platform) WatchControlPlane(t *testing.T) *ProcessWatch {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	w := &ProcessWatch{cancel: cancel, done: make(chan struct{}), seen: map[string]int{}}
	go func() {
		defer close(w.done)
		for ctx.Err() == nil {
			out, err := p.Project.Top(ctx, Service)
			if err == nil {
				w.record(out)
			}
			select {
			case <-ctx.Done():
			case <-time.After(50 * time.Millisecond):
			}
		}
	}()
	t.Cleanup(w.cancel)
	return w
}

// record notes the programs in one `compose top` listing.
func (w *ProcessWatch) record(out []byte) {
	progs := ParseTop(string(out))
	w.mu.Lock()
	defer w.mu.Unlock()
	w.samples++
	for _, prog := range progs {
		w.seen[prog]++
	}
}

// ParseTop returns the distinct program names (the base name of the command's
// first word) in the listing `docker compose top` prints: per container a
// name line and a table whose header has CMD as its last column.
func ParseTop(out string) []string {
	var progs []string
	cmdCol := -1
	for line := range strings.Lines(out) {
		line = strings.TrimRight(line, "\r\n")
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if i := slices.Index(fields, "CMD"); i >= 0 && slices.Contains(fields, "PID") {
			cmdCol = i
			continue
		}
		if cmdCol < 0 || len(fields) <= cmdCol {
			continue // a container's name line
		}
		prog := filepath.Base(fields[cmdCol])
		if !slices.Contains(progs, prog) {
			progs = append(progs, prog)
		}
	}
	return progs
}

// Stop ends the watch and returns how many samples showed each program, and
// how many samples there were.
func (w *ProcessWatch) Stop() (seen map[string]int, samples int) {
	w.cancel()
	<-w.done
	w.mu.Lock()
	defer w.mu.Unlock()
	return maps.Clone(w.seen), w.samples
}
