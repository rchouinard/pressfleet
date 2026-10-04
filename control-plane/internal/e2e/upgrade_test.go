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

	"github.com/rchouinard/pressfleet/control-plane/internal/cli"
	"github.com/rchouinard/pressfleet/control-plane/internal/e2e"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner/compose"
	"github.com/rchouinard/pressfleet/control-plane/internal/redact"
)

// TestUpgrade installs the platform with `pressfleet init`, then upgrades it
// with `pressfleet upgrade` to another control plane image, standing in for
// the next release's: only the services running the control plane image are
// recreated, MariaDB, the object cache, and Traefik keep running, and the
// admin can still log in. Upgrading from the previous published release is
// tested once releases exist (#95, #100), and recovering from a failed
// upgrade in #142.
func TestUpgrade(t *testing.T) {
	image := e2e.Image(t)
	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	configPath := filepath.Join(root, "etc", "pressfleet.yaml")
	project := compose.Project{
		Name: "pfupgrade-" + strings.ToLower(rand.Text())[:10],
		Dir:  filepath.Join(dataDir, "platform"),
	}
	p := e2e.Adopt(t, project, dataDir)
	e2e.WriteOverride(t, project.Dir)
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Minute)
	defer cancel()

	const email = "admin@upgrade.test"
	password := redact.Secret(strings.ToLower(rand.Text()))
	run := func(stdin string, args ...string) (int, string) {
		t.Helper()
		var stdout, stderr bytes.Buffer
		code := cli.Run(ctx, append(args[:1:1], append([]string{"--config", configPath}, args[1:]...)...), cli.Options{
			Version:             "e2e",
			Stdin:               strings.NewReader(stdin),
			Stdout:              &stdout,
			Stderr:              &stderr,
			Env:                 func(string) (string, bool) { return "", false },
			PlatformProjectName: project.Name,
		})
		out := strings.ReplaceAll(stdout.String(), keyLine(stdout.String()), "    pfmk1:<redacted>")
		t.Logf("%s output:\n%s%s", args[0], out, stderr.String())
		return code, stderr.String()
	}
	if code, stderr := run(password.Reveal()+"\n", "init",
		"--non-interactive", "--confirm-key-stored", "--skip-preflight",
		"--dashboard-domain", "fleet.upgrade.test", "--preview-base-domain", "preview.upgrade.test",
		"--data-dir", dataDir, "--admin-email", email, "--admin-password-stdin",
		"--acme-ca-server", e2e.ACMECAServer, "--control-plane-image", image,
	); code != 0 {
		t.Fatalf("init exited %d: %s", code, stderr)
	}

	// The "next release": the same image under another name, which changes
	// the control plane's definition and nothing else.
	next := "pressfleet-control-plane:" + project.Name
	if err := compose.TagImage(ctx, nil, image, next); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = compose.RemoveImage(context.Background(), nil, next) })

	ids := func() map[string]string {
		t.Helper()
		out := map[string]string{}
		for _, s := range []string{"control-plane", "mariadb", "redis", "traefik"} {
			c, err := project.ServiceContainer(ctx, s)
			if err != nil {
				t.Fatal(err)
			}
			out[s] = c.ID
		}
		return out
	}
	before := ids()

	if code, stderr := run("", "upgrade", "--skip-preflight", "--control-plane-image", next); code != 0 {
		t.Fatalf("upgrade exited %d: %s", code, stderr)
	}
	after := ids()
	for _, s := range []string{"mariadb", "redis", "traefik"} {
		if before[s] != after[s] {
			t.Errorf("the upgrade recreated %s", s)
		}
	}
	if before["control-plane"] == after["control-plane"] {
		t.Error("the upgrade didn't recreate the control plane")
	}
	if c, err := project.ServiceContainer(ctx, "control-plane"); err != nil || c.Image != next {
		t.Errorf("control plane runs %q (%v), want %s", c.Image, err, next)
	}
	saved, err := os.ReadFile(filepath.Join(dataDir, "upgrade", "previous-platform", "compose.yaml"))
	if err != nil || !strings.Contains(string(saved), image) {
		t.Errorf("the saved platform project doesn't run the previous image: %v", err)
	}

	p.Connect(ctx, t)
	p.Login(t, email, password)

	// Nothing changed since: running it again does nothing.
	if code, stderr := run("", "upgrade", "--skip-preflight", "--control-plane-image", next); code != 0 {
		t.Fatalf("second upgrade exited %d: %s", code, stderr)
	}
	if again := ids(); again["control-plane"] != after["control-plane"] {
		t.Error("the second upgrade recreated the control plane")
	}
}
