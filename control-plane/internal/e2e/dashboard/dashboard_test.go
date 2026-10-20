// Package dashboard_test runs the dashboard's Playwright suite (ui/e2e)
// against a real platform: the control plane image under test, booted by
// the e2e harness, with the embedded dashboard served through Traefik on
// the dashboard domain. See docs/testing.md, "Dashboard end-to-end tests".
//
// It lives in its own package so `make test-e2e` (the API suite) doesn't
// need Node.js and a browser, and `make test-ui-e2e` runs only this.
package dashboard_test

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/api/client"
	"github.com/rchouinard/pressfleet/control-plane/internal/catalog"
	"github.com/rchouinard/pressfleet/control-plane/internal/e2e"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner/compose"
)

// Environment variables of this test. The ones the specs read are in
// ui/e2e/env.ts.
const (
	// envArgs are extra arguments for `playwright test`, split on spaces,
	// e.g. "--headed" or "-g provision".
	envArgs = "PRESSFLEET_UI_E2E_ARGS"
	// envHold boots the platform and keeps it running, without running
	// the suite, until the test is interrupted, so specs can be run
	// against it again and again while they are written: "1" with the
	// default configuration, holdTwoFactorRequired with the second run's.
	envHold = "PRESSFLEET_UI_E2E_HOLD"
)

// holdTwoFactorRequired is the envHold value that holds the platform in
// the second run's configuration (twoFactorRequired).
const holdTwoFactorRequired = "two-factor-required"

// The suite runs twice on one platform. Most specs need the default
// two-factor policy: the platform's admin, whom every spec shares, has no
// second factor, and two_factor.required would restrict its sessions to
// enrolling one. The specs of forced enrollment and of the reauthentication
// dialog need the policy on, and a stale session, which the default
// 10-minute reauthentication max age would make them wait for. So after the
// first run the control plane restarts with twoFactorRequired, the way an
// operator changes its configuration, and the second run runs only those
// specs (playwright.config.ts picks them by PRESSFLEET_E2E_TWO_FACTOR_REQUIRED).
// They create their users with the CLI (the harness's POST /users), since the
// admin's sessions are restricted then.
var twoFactorRequired = map[string]string{
	"PRESSFLEET_TWO_FACTOR_REQUIRED":                 "admins",
	"PRESSFLEET_TWO_FACTOR_REAUTHENTICATION_MAX_AGE": reauthMaxAgeRequired.String(),
}

// The reauthentication max age of each run: the default, and the shortest
// two_factor.reauthentication_max_age allows, for the second run.
const (
	reauthMaxAgeDefault  = 10 * time.Minute
	reauthMaxAgeRequired = time.Minute
)

// platformFile is where hold mode writes the platform's details, relative
// to ui/; playwright.config.ts reads it when the environment doesn't name
// a platform. It is git-ignored and removed when the test ends.
const platformFile = "e2e/.platform.json"

// resultsDir is Playwright's output directory (traces, screenshots),
// relative to ui/. The control plane logs of a failed run go there too,
// so CI uploads one directory.
const resultsDir = "test-results"

// controlPlaneMemoryMB is the control plane's memory limit for the suite,
// in MiB (see TestDashboard).
const controlPlaneMemoryMB = 512

// suiteTimeout and requiredSuiteTimeout bound the suite's first and second
// runs (see twoFactorRequired). Together with booting the platform and
// restarting it in between they stay below make test-ui-e2e's go test
// -timeout, so teardown still runs.
const (
	suiteTimeout         = 25 * time.Minute
	requiredSuiteTimeout = 8 * time.Minute
)

// TestDashboard boots a platform and runs the Playwright suite against it.
func TestDashboard(t *testing.T) {
	e2e.Image(t) // skips unless Docker tests are enabled, before anything slow
	ui := uiDir(t)
	// Sites the specs create aren't part of the platform project, which
	// Start tears down. Registered first, this runs last, after MariaDB
	// (which keeps the site networks in use) is gone; siteProjects is
	// filled by the cleanup registered after Start, which runs first.
	var siteProjects []compose.Project
	sitesListed := false
	projectDir := t.TempDir()
	t.Cleanup(func() { removeSites(t, siteProjects) })

	// The specs run in parallel: several sites' jobs, logins, and restic
	// at once. Their docker, docker compose, and restic processes count
	// against the control plane's memory limit, and the default 192 MiB
	// doesn't fit them: the control plane is killed, and comes back on
	// another port.
	p := e2e.StartWithControlPlaneMemory(t, controlPlaneMemoryMB)
	t.Cleanup(func() {
		if !sitesListed {
			siteProjects = listSiteProjects(t, p, projectDir)
		}
		if t.Failed() {
			saveLogs(t, p.Project, filepath.Join(ui, resultsDir, "platform-logs.txt"))
		}
	})
	// listSites lists the sites before the second run's restart: the
	// admin's sessions are restricted to enrolling after it. That run
	// creates no sites.
	listSites := func() {
		siteProjects, sitesListed = listSiteProjects(t, p, projectDir), true
	}

	// The monitoring specs wait for collected metrics and for alerts to
	// fire and resolve: collect and evaluate every few seconds instead of
	// every 30, as TestM4Acceptance does. Before the harness and the
	// environment, because the control plane's published port changes.
	defaults := map[string]string{
		"PRESSFLEET_METRICS_INTERVAL_SECONDS":           "5",
		"PRESSFLEET_ALERTS_EVALUATION_INTERVAL_SECONDS": "5",
	}
	p.Restart(t, defaults)

	php, otherPHP := siteImages(t)
	harness := startHarness(t, p, projectDir)
	b := startBrowser(t, p, ui)
	t.Cleanup(func() {
		if t.Failed() {
			saveLogs(t, b.project, filepath.Join(ui, resultsDir, "browser-logs.txt"))
		}
	})
	// env is the platform's details for the specs. The control plane's
	// published port changes when it restarts, so it is read again then.
	env := func(policy string, reauthMaxAge time.Duration) map[string]string {
		return map[string]string{
			"PRESSFLEET_E2E_BASE_URL":                         "https://" + e2e.DashboardDomain,
			"PRESSFLEET_E2E_INGRESS_ADDR":                     p.HTTPSAddr,
			"PRESSFLEET_E2E_BROWSER_ENDPOINT":                 b.Endpoint,
			"PRESSFLEET_E2E_BROWSER_INGRESS_ADDR":             b.IngressAddr,
			"PRESSFLEET_E2E_API_URL":                          p.BaseURL,
			"PRESSFLEET_E2E_HARNESS_URL":                      harness,
			"PRESSFLEET_E2E_ADMIN_EMAIL":                      p.AdminEmail,
			"PRESSFLEET_E2E_ADMIN_PASSWORD":                   p.AdminPassword.Reveal(),
			"PRESSFLEET_E2E_PHP_VERSION":                      php,
			"PRESSFLEET_E2E_OTHER_PHP_VERSION":                otherPHP,
			"PRESSFLEET_E2E_TWO_FACTOR_REQUIRED":              policy,
			"PRESSFLEET_E2E_REAUTHENTICATION_MAX_AGE_SECONDS": strconv.Itoa(int(reauthMaxAge.Seconds())),
		}
	}
	required := func() map[string]string {
		listSites()
		// Restart doesn't keep the variables of an earlier restart.
		cfg := maps.Clone(defaults)
		maps.Copy(cfg, twoFactorRequired)
		p.Restart(t, cfg)
		return env(twoFactorRequired["PRESSFLEET_TWO_FACTOR_REQUIRED"], reauthMaxAgeRequired)
	}

	switch os.Getenv(envHold) {
	case "":
	case holdTwoFactorRequired:
		hold(t, filepath.Join(ui, platformFile), required())
		return
	default:
		hold(t, filepath.Join(ui, platformFile), env("off", reauthMaxAgeDefault))
		return
	}

	// Both runs go ahead even if the first fails, so one test run reports
	// every failing spec.
	runSuite(t, ui, env("off", reauthMaxAgeDefault), "", suiteTimeout)
	runSuite(t, ui, required(), holdTwoFactorRequired, requiredSuiteTimeout)
}

// runSuite runs the Playwright suite once against the platform described
// by env. name tells the second run's output apart from the first's
// (playwright.config.ts writes it under test-results/<name>); "" is the
// first. A failure fails the test without stopping it.
func runSuite(t *testing.T, ui string, env map[string]string, name string, timeout time.Duration) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), timeout)
	defer cancel()
	args := append([]string{"exec", "playwright", "test"}, strings.Fields(os.Getenv(envArgs))...)
	cmd := exec.CommandContext(ctx, "pnpm", args...)
	cmd.Dir = ui
	cmd.Env = os.Environ()
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	// Let Playwright close its browsers and write its traces.
	cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
	cmd.WaitDelay = 30 * time.Second
	t.Logf("running pnpm %s in %s (%s)", strings.Join(args, " "), ui, cmp.Or(name, "default configuration"))
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			t.Errorf("the Playwright suite failed (%s, exit status %d); traces and screenshots are in %s",
				cmp.Or(name, "default configuration"), exit.ExitCode(), filepath.Join(ui, resultsDir, name))
			return
		}
		t.Fatalf("running the Playwright suite: %v (run `pnpm -C ui install` first)", err)
	}
}

// uiDir returns the absolute path of the repository's ui/ directory and
// checks that its dependencies, Playwright included, are installed.
func uiDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.Abs(filepath.Join("..", "..", "..", "..", "ui"))
	if err != nil {
		t.Fatal(err)
	}
	// Not a skip: with Docker tests enabled, a suite that can't run fails.
	if _, err := os.Stat(filepath.Join(dir, "node_modules", ".bin", "playwright")); err != nil {
		t.Fatalf("Playwright isn't installed in %s: run `pnpm -C ui install` (make test-ui-e2e does)", dir)
	}
	return dir
}

// siteImages makes the site images for the default PHP version and the
// latest WordPress available, and returns that PHP version: the one the
// specs provision sites on. It also makes another PHP version's php image
// available, and returns that version, for the spec that changes a site's
// PHP version (as TestSiteCreateDelete does).
func siteImages(t *testing.T) (php, otherPHP string) {
	t.Helper()
	cat, err := catalog.Load()
	if err != nil {
		t.Fatal(err)
	}
	php, ok := cat.DefaultPHPVersion(time.Now())
	if !ok {
		t.Fatal("no default PHP version")
	}
	for _, v := range cat.AvailablePHPVersions(time.Now()) {
		if v.Version != php {
			otherPHP = v.Version
			break
		}
	}
	if otherPHP == "" {
		t.Fatal("the catalog offers only one PHP version")
	}
	wp := cat.LatestWordPress().Version
	images, err := cat.Images(php, wp)
	if err != nil {
		t.Fatal(err)
	}
	other, err := cat.Images(otherPHP, wp)
	if err != nil {
		t.Fatal(err)
	}
	e2e.EnsureImages(t, images.PHP, images.Nginx, other.PHP)
	return php, otherPHP
}

// hold writes the platform's details for playwright.config.ts and waits
// until the test is interrupted (Ctrl-C) or runs out of time. Returning,
// rather than being killed, lets the cleanups tear the platform down.
func hold(t *testing.T, file string, env map[string]string) {
	t.Helper()
	data, err := json.MarshalIndent(env, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, data, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Remove(file); err != nil && !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("removing %s: %v", file, err)
		}
	})
	ctx, stop := signal.NotifyContext(t.Context(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if deadline, ok := t.Deadline(); ok {
		var cancel context.CancelFunc
		// Leave time for the teardown.
		ctx, cancel = context.WithDeadline(ctx, deadline.Add(-5*time.Minute))
		defer cancel()
	}
	fmt.Printf("\nThe platform is up; its details are in %s.\n"+
		"Run the specs against it with `pnpm -C ui test:e2e` (add --ui, -g ...);\n"+
		"for --headed or --debug, set PRESSFLEET_E2E_HOST_BROWSER=1 to use a browser on the host.\n"+
		"Press Ctrl-C to tear it down.\n\n", file)
	<-ctx.Done()
}

// listSiteProjects returns the Compose projects of the sites that still
// exist, so they can be removed with the platform.
func listSiteProjects(t *testing.T, p *e2e.Platform, dir string) []compose.Project {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	var projects []compose.Project
	var cursor *client.Cursor
	for {
		resp, err := p.Client.ListSitesWithResponse(ctx, &client.ListSitesParams{Cursor: cursor})
		if err != nil {
			t.Logf("listing sites to remove: %v", err)
			return projects
		}
		if resp.JSON200 == nil {
			t.Logf("listing sites to remove: status %d: %s", resp.StatusCode(), resp.Body)
			return projects
		}
		for _, s := range resp.JSON200.Items {
			projects = append(projects, compose.Project{Name: compose.SiteProjectName(s.ID), Dir: dir})
		}
		if resp.JSON200.NextCursor == nil {
			return projects
		}
		cursor = resp.JSON200.NextCursor
	}
}

// removeSites removes what is left of site projects.
func removeSites(t *testing.T, projects []compose.Project) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	for _, sp := range projects {
		if res, err := sp.Resources(ctx); err == nil && !res.Empty() {
			t.Logf("removing leftover site project %s", sp.Name)
			if err := sp.Down(ctx, compose.DownOptions{Volumes: true}); err != nil {
				t.Errorf("removing leftover site project %s: %v", sp.Name, err)
			}
		}
	}
}

// saveLogs writes a project's container logs where CI uploads them from.
func saveLogs(t *testing.T, project compose.Project, file string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	logs, err := project.Logs(ctx)
	if err != nil {
		t.Logf("collecting container logs: %v", err)
		return
	}
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Logf("saving container logs: %v", err)
		return
	}
	if err := os.WriteFile(file, logs, 0o644); err != nil {
		t.Logf("saving container logs: %v", err)
	}
}
