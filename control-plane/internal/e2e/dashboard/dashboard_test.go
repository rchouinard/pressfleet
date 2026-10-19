// Package dashboard_test runs the dashboard's Playwright suite (ui/e2e)
// against a real platform: the control plane image under test, booted by
// the e2e harness, with the embedded dashboard served through Traefik on
// the dashboard domain. See docs/testing.md, "Dashboard end-to-end tests".
//
// It lives in its own package so `make test-e2e` (the API suite) doesn't
// need Node.js and a browser, and `make test-ui-e2e` runs only this.
package dashboard_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
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
	// against it again and again while they are written.
	envHold = "PRESSFLEET_UI_E2E_HOLD"
)

// platformFile is where hold mode writes the platform's details, relative
// to ui/; playwright.config.ts reads it when the environment doesn't name
// a platform. It is git-ignored and removed when the test ends.
const platformFile = "e2e/.platform.json"

// resultsDir is Playwright's output directory (traces, screenshots),
// relative to ui/. The control plane logs of a failed run go there too,
// so CI uploads one directory.
const resultsDir = "test-results"

// suiteTimeout bounds one run of the suite, below make test-ui-e2e's
// go test -timeout so teardown still runs.
const suiteTimeout = 25 * time.Minute

// TestDashboard boots a platform and runs the Playwright suite against it.
func TestDashboard(t *testing.T) {
	e2e.Image(t) // skips unless Docker tests are enabled, before anything slow
	ui := uiDir(t)
	// Sites the specs create aren't part of the platform project, which
	// Start tears down. Registered first, this runs last, after MariaDB
	// (which keeps the site networks in use) is gone; siteProjects is
	// filled by the cleanup registered after Start, which runs first.
	var siteProjects []compose.Project
	projectDir := t.TempDir()
	t.Cleanup(func() { removeSites(t, siteProjects) })

	p := e2e.Start(t)
	t.Cleanup(func() {
		siteProjects = listSiteProjects(t, p, projectDir)
		if t.Failed() {
			savePlatformLogs(t, p, filepath.Join(ui, resultsDir, "platform-logs.txt"))
		}
	})

	// The monitoring specs wait for collected metrics and for alerts to
	// fire and resolve: collect and evaluate every few seconds instead of
	// every 30, as TestM4Acceptance does. Before the harness and the
	// environment, because the control plane's published port changes.
	p.Restart(t, map[string]string{
		"PRESSFLEET_METRICS_INTERVAL_SECONDS":           "5",
		"PRESSFLEET_ALERTS_EVALUATION_INTERVAL_SECONDS": "5",
	})

	php := siteImages(t)
	harness := startHarness(t, p, projectDir)
	env := map[string]string{
		"PRESSFLEET_E2E_BASE_URL":       "https://" + e2e.DashboardDomain,
		"PRESSFLEET_E2E_INGRESS_ADDR":   p.HTTPSAddr,
		"PRESSFLEET_E2E_API_URL":        p.BaseURL,
		"PRESSFLEET_E2E_HARNESS_URL":    harness,
		"PRESSFLEET_E2E_ADMIN_EMAIL":    p.AdminEmail,
		"PRESSFLEET_E2E_ADMIN_PASSWORD": p.AdminPassword.Reveal(),
		"PRESSFLEET_E2E_PHP_VERSION":    php,
	}

	if os.Getenv(envHold) == "1" {
		hold(t, filepath.Join(ui, platformFile), env)
		return
	}

	ctx, cancel := context.WithTimeout(t.Context(), suiteTimeout)
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
	t.Logf("running pnpm %s in %s", strings.Join(args, " "), ui)
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			t.Fatalf("the Playwright suite failed (exit status %d); traces and screenshots are in %s",
				exit.ExitCode(), filepath.Join(ui, resultsDir))
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
// specs provision sites on.
func siteImages(t *testing.T) string {
	t.Helper()
	cat, err := catalog.Load()
	if err != nil {
		t.Fatal(err)
	}
	php, ok := cat.DefaultPHPVersion(time.Now())
	if !ok {
		t.Fatal("no default PHP version")
	}
	images, err := cat.Images(php, cat.LatestWordPress().Version)
	if err != nil {
		t.Fatal(err)
	}
	e2e.EnsureImages(t, images.PHP, images.Nginx)
	return php
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
		"Run the specs against it with `pnpm -C ui test:e2e` (add --headed, --ui, -g ...).\n"+
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

// savePlatformLogs writes the platform's container logs where CI uploads
// them from.
func savePlatformLogs(t *testing.T, p *e2e.Platform, file string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	logs, err := p.Project.Logs(ctx)
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
