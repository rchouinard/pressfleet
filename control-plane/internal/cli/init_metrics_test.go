package cli_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"

	"github.com/rchouinard/pressfleet/control-plane/internal/auth"
	"github.com/rchouinard/pressfleet/control-plane/internal/config"
	"github.com/rchouinard/pressfleet/control-plane/internal/platform"
)

var (
	printedMetricsPassword = regexp.MustCompile(`(?m)^  Metrics password: (\S+)$`)
	rotatedMetricsPassword = regexp.MustCompile(`(?m)^Password: (\S+)$`)
)

// TestInitMetricsAccess follows the credential of the public metrics route
// from install, through a reinstall, to a rotation.
func TestInitMetricsAccess(t *testing.T) {
	e := newInitEnv(t)
	e.env["PRESSFLEET_METRICS_PUBLIC_DOMAIN"] = "metrics.example.com"
	dir := filepath.Join(e.dataDir, "platform")

	r := e.run("", e.nonInteractiveArgs()...)
	e.assertInstalled(r)
	m := printedMetricsPassword.FindStringSubmatch(r.stdout)
	if m == nil || !strings.Contains(r.stdout, "https://metrics.example.com/") {
		t.Fatalf("stdout:\n%s", r.stdout)
	}
	password := m[1]
	hash, err := platform.ReadMetricsAccessHash(dir)
	if err != nil || bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) != nil {
		t.Fatalf("the shown password doesn't match the stored hash: %v", err)
	}
	route, err := os.ReadFile(filepath.Join(dir, platform.TraefikMetricsConfigFile))
	if err != nil || !strings.Contains(string(route), "metrics.example.com") || strings.Contains(string(route), password) {
		t.Errorf("route: %v\n%s", err, route)
	}
	// Reinstalling must not need the config's environment variable: the
	// setting is kept in the config file.
	cfg, err := config.Load(e.configPath, func(string) (string, bool) { return "", false })
	if err != nil || cfg.Metrics.PublicDomain != "metrics.example.com" {
		t.Fatalf("public_domain = %q, %v", cfg.Metrics.PublicDomain, err)
	}

	// Reinstalling keeps the credential and doesn't show it again.
	e.platform = &fakePlatform{execErr: errors.New("exit status 1: creating user: " + auth.ErrEmailTaken.Error())}
	r = e.run("", e.nonInteractiveArgs("--force")...)
	if r.code != 0 {
		t.Fatalf("exit %d, stderr: %s", r.code, r.stderr)
	}
	if again, _ := platform.ReadMetricsAccessHash(dir); again != hash {
		t.Error("the credential changed on reinstall")
	}
	if strings.Contains(r.stdout, "Metrics password:") || !strings.Contains(r.stdout, "Metrics API:") {
		t.Errorf("stdout:\n%s", r.stdout)
	}

	// Rotation prints a new password once, and the old one stops working.
	env := map[string]string{config.EnvConfigPath: e.configPath, "PRESSFLEET_DATA_DIR": e.dataDir}
	rot := run(t, context.Background(), []string{"metrics-access", "rotate"}, env)
	if rot.code != 0 {
		t.Fatalf("exit %d, stderr: %s", rot.code, rot.stderr)
	}
	m = rotatedMetricsPassword.FindStringSubmatch(rot.stdout)
	if m == nil || m[1] == password {
		t.Fatalf("stdout:\n%s", rot.stdout)
	}
	newHash, _ := platform.ReadMetricsAccessHash(dir)
	if bcrypt.CompareHashAndPassword([]byte(newHash), []byte(m[1])) != nil ||
		bcrypt.CompareHashAndPassword([]byte(newHash), []byte(password)) == nil {
		t.Error("rotation didn't replace the credential")
	}
}

func TestMetricsAccessRotateWithoutDomain(t *testing.T) {
	e := newInitEnv(t)
	e.assertInstalled(e.run("", e.nonInteractiveArgs()...))
	env := map[string]string{config.EnvConfigPath: e.configPath, "PRESSFLEET_DATA_DIR": e.dataDir}
	r := run(t, context.Background(), []string{"metrics-access", "rotate"}, env)
	if r.code == 0 || !strings.Contains(r.stderr, "metrics.public_domain") {
		t.Errorf("exit %d, stderr: %s", r.code, r.stderr)
	}
}
