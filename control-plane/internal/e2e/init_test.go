package e2e_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/api/client"
	"github.com/rchouinard/pressfleet/control-plane/internal/cli"
	"github.com/rchouinard/pressfleet/control-plane/internal/e2e"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner/compose"
	"github.com/rchouinard/pressfleet/control-plane/internal/redact"
)

// TestInit installs the platform with `pressfleet init` into a temporary
// root, as an operator would with --non-interactive, and checks that the
// control plane is healthy and the new admin can log in.
func TestInit(t *testing.T) {
	image := e2e.Image(t)
	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	configPath := filepath.Join(root, "etc", "pressfleet.yaml")
	project := compose.Project{
		Name: "pfinit-" + strings.ToLower(rand.Text())[:10],
		Dir:  filepath.Join(dataDir, "platform"),
	}
	p := e2e.Adopt(t, project, dataDir)
	// Rendering leaves the override in place; init only checks for
	// compose.yaml when looking for an existing installation.
	e2e.WriteOverride(t, project.Dir)

	const email = "admin@init.test"
	password := redact.Secret(strings.ToLower(rand.Text()))
	var stdout, stderr bytes.Buffer
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Minute)
	defer cancel()
	code := cli.Run(ctx, []string{
		"init", "--config", configPath,
		"--non-interactive", "--confirm-key-stored",
		// Development machines and CI runners often lack Docker address
		// pools, and the example domains don't resolve.
		"--skip-preflight",
		"--dashboard-domain", "fleet.init.test",
		"--preview-base-domain", "preview.init.test",
		"--data-dir", dataDir,
		"--admin-email", email, "--admin-password-stdin",
		"--acme-ca-server", e2e.ACMECAServer,
		"--control-plane-image", image,
	}, cli.Options{
		Version:             "e2e",
		Stdin:               strings.NewReader(password.Reveal() + "\n"),
		Stdout:              &stdout,
		Stderr:              &stderr,
		Env:                 func(string) (string, bool) { return "", false },
		PlatformProjectName: project.Name,
	})
	t.Logf("init output:\n%s", strings.ReplaceAll(stdout.String(), keyLine(stdout.String()), "    pfmk1:<redacted>"))
	if code != 0 {
		t.Fatalf("init exited %d: %s", code, stderr.String())
	}

	info, err := os.Stat(filepath.Join(root, "etc", "master.key"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("master key file: %v, %v", info, err)
	}

	containers, err := project.Ps(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range containers {
		if c.Health != "healthy" {
			t.Errorf("service %s is %s (%s)", c.Service, c.State, c.Health)
		}
	}

	// POST /api/v1/auth/login with the admin init created.
	p.Connect(ctx, t)
	c := p.Login(t, email, password)
	resp, err := c.GetCurrentUserWithResponse(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if resp.JSON200 == nil || string(resp.JSON200.Email) != email || resp.JSON200.Role != client.Admin {
		t.Errorf("current user: status %d: %s", resp.StatusCode(), resp.Body)
	}
}

// keyLine returns the line of init's output holding the master key, so it
// stays out of the test log.
func keyLine(out string) string {
	for line := range strings.SplitSeq(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "pfmk1:") {
			return line
		}
	}
	return "\x00"
}
