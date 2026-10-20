package dashboard_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/e2e"
	"github.com/rchouinard/pressfleet/control-plane/internal/ingress"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner/compose"
)

// The suite's Chromium runs in a container of its own, not on the host
// (#179). Docker adds a bridge network to the host when a site's first
// container starts and removes it when its last one stops; Chromium on the
// host sees each change and aborts the requests it has in flight
// (net::ERR_NETWORK_CHANGED), so a page loses a script and stays blank, or a
// download is cancelled. In a container, the browser has a network
// namespace of its own that those changes never touch.
//
// Only the browser moves. The container runs Playwright's browser server
// (`playwright run-server`) and the test runner stays on the host, where
// Playwright's connectOptions point its workers at the server: the specs'
// own requests (the API, the harness server) still go out from the host,
// and traces, screenshots, and the report are still written to ui/. The
// server comes from ui/node_modules, mounted read-only at the same path,
// so it is exactly the Playwright the runner uses, and the image is the
// one Playwright publishes for that version, with the matching browsers.
//
// The container is on the platform network, where Traefik is: the browser
// sends the dashboard domain and the sites' test domains to Traefik's
// address there (playwright.config.ts).
const (
	browserService = "browser"
	// browserPort is the browser server's port in its container.
	browserPort = 3000
	// browserImageRepo is Playwright's image repository; the tag is
	// v<version>-noble.
	browserImageRepo = "mcr.microsoft.com/playwright"
	// browserShmMB is the container's /dev/shm, in MiB. Docker's default
	// of 64 MiB is too small for several Chromium instances, which crash
	// when it runs out.
	browserShmMB = 1024
	// browserWait bounds starting the browser server.
	browserWait = 2 * time.Minute
)

// browser is the suite's browser server.
type browser struct {
	// Endpoint is the server's WebSocket URL on the host, for
	// connectOptions.wsEndpoint.
	Endpoint string
	// IngressAddr is Traefik's HTTPS address as the browser reaches it,
	// on the platform network.
	IngressAddr string
	project     compose.Project
}

// startBrowser runs the browser server for the platform until the test
// ends. It is removed before the platform, whose network it is on.
func startBrowser(t *testing.T, p *e2e.Platform, ui string) *browser {
	t.Helper()
	nodeModules := filepath.Join(ui, "node_modules")
	version := playwrightVersion(t, nodeModules)
	image := browserImageRepo + ":v" + version + "-noble"

	ctx, cancel := context.WithTimeout(t.Context(), browserWait)
	defer cancel()
	traefik, err := p.Project.ServiceContainer(ctx, ingress.TraefikService)
	if err != nil {
		t.Fatal(err)
	}
	network := p.Project.Name + "_platform"
	traefikIP := traefik.Networks[network]
	if traefikIP == "" {
		t.Fatalf("traefik isn't on the platform network: %v", traefik.Networks)
	}

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte(browserCompose(image, nodeModules, network)), 0o600); err != nil {
		t.Fatal(err)
	}
	b := &browser{
		IngressAddr: traefikIP + ":443",
		project:     compose.Project{Name: p.Project.Name + "-browser", Dir: dir},
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if err := b.project.Down(ctx, compose.DownOptions{Volumes: true}); err != nil {
			t.Errorf("removing the browser: %v", err)
		}
	})
	// Pulled here rather than by Up, so a slow pull gets EnsureImages'
	// time and message instead of the start's.
	e2e.EnsureImages(t, image)
	if err := b.project.Up(ctx, compose.UpOptions{Wait: true, WaitTimeout: browserWait}); err != nil {
		t.Fatalf("starting the browser server: %v", err)
	}
	addr, err := b.project.Port(ctx, browserService, browserPort)
	if err != nil {
		t.Fatal(err)
	}
	b.Endpoint = "ws://" + addr + "/"
	t.Logf("Chromium runs in %s (%s); it reaches Traefik at %s", image, b.Endpoint, b.IngressAddr)
	return b
}

// browserCompose is the browser server's Compose project. The server runs
// with --unsafe, which lets clients choose Chromium's command-line
// arguments: the suite needs --host-resolver-rules. Its port is published
// on loopback only.
func browserCompose(image, nodeModules, network string) string {
	cli := filepath.Join(nodeModules, "@playwright", "test", "cli.js")
	port := strconv.Itoa(browserPort)
	// Any client may open a connection once the port answers.
	probe := fmt.Sprintf(`require('net').connect(%s, '127.0.0.1').on('connect', () => process.exit(0)).on('error', () => process.exit(1))`, port)
	return fmt.Sprintf(`services:
  %[1]s:
    image: %[2]q
    command: ["node", %[3]q, "run-server", "--host", "0.0.0.0", "--port", %[4]q, "--unsafe"]
    # Reaps the browsers' child processes.
    init: true
    shm_size: %[5]dm
    volumes:
      - type: bind
        source: %[6]q
        target: %[6]q
        read_only: true
    ports:
      - "127.0.0.1::%[4]s"
    networks:
      - platform
    healthcheck:
      test: ["CMD", "node", "-e", %[7]q]
      interval: 10s
      timeout: 5s
      start_period: 1m
      start_interval: 1s
networks:
  platform:
    name: %[8]q
    external: true
`, browserService, image, cli, port, browserShmMB, nodeModules, probe, network)
}

// playwrightVersion returns the version of the Playwright installed in
// ui/node_modules, which the browser image must match.
func playwrightVersion(t *testing.T, nodeModules string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(nodeModules, "@playwright", "test", "package.json"))
	if err != nil {
		t.Fatalf("reading Playwright's version: %v (run `pnpm -C ui install` first)", err)
	}
	var pkg struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(data, &pkg); err != nil || pkg.Version == "" {
		t.Fatalf("reading Playwright's version from %s: %v", nodeModules, err)
	}
	return pkg.Version
}
