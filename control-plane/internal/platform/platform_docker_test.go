package platform_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/config"
	"github.com/rchouinard/pressfleet/control-plane/internal/platform"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner/compose"
	"github.com/rchouinard/pressfleet/control-plane/internal/secrets"
)

// TestPlatformUp renders the platform project into a temporary data
// directory, brings it up against the local Docker daemon, checks it, and
// tears everything down including volumes. Run it with `make test-platform`,
// which builds the control plane image first.
func TestPlatformUp(t *testing.T) {
	if os.Getenv("PRESSFLEET_DOCKER_TESTS") != "1" {
		t.Skip("set PRESSFLEET_DOCKER_TESTS=1 to run tests against Docker")
	}
	image := os.Getenv("PRESSFLEET_TEST_CONTROL_PLANE_IMAGE")
	if image == "" {
		t.Skip("set PRESSFLEET_TEST_CONTROL_PLANE_IMAGE to a locally built control plane image (see make test-platform)")
	}

	// Pre-create the layout so the control plane (root in its container)
	// doesn't create root-owned directories the test can't remove.
	layout := config.Layout{Root: t.TempDir()}
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	images, err := platform.DefaultImages("test")
	if err != nil {
		t.Fatal(err)
	}
	images.ControlPlane = image
	const password = "test-root-pw"
	// A throwaway master key, outside the data directory as serve requires.
	keyFile := filepath.Join(t.TempDir(), "master.key")
	if err := os.WriteFile(keyFile, []byte(secrets.GenerateMasterKey().Encode().Reveal()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := platform.Config{
		ProjectName:         fmt.Sprintf("pftest-%d", time.Now().UnixNano()),
		DataDir:             layout.Root,
		Images:              images,
		MariaDBRootPassword: password,
		MasterKeyFile:       keyFile,
		// Development machines and CI runners often lack Docker address pools.
		SkipPreflight: true,
	}
	dir := layout.PlatformDir()
	if err := platform.Render(dir, cfg); err != nil {
		t.Fatal(err)
	}
	p := compose.Project{Name: cfg.ProjectName, Dir: dir}

	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		// Hand anything the root container wrote back to the test user.
		owner := fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid())
		if _, err := p.Exec(ctx, "control-plane", "chown", "-R", owner, layout.Root); err != nil {
			t.Logf("chown data dir: %v", err)
		}
		if err := p.Down(ctx, compose.DownOptions{Volumes: true}); err != nil {
			t.Errorf("down: %v", err)
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if err := p.Up(ctx, compose.UpOptions{}); err != nil {
		t.Fatal(err)
	}

	// Wait for MariaDB to be healthy and the control plane to be running.
	// The control plane's own healthcheck needs GET /healthz (#12); until
	// then it reports "starting" and later "unhealthy", which is logged
	// rather than asserted.
	var cpHealth string
	deadline := time.Now().Add(3 * time.Minute)
	for {
		containers, err := p.Ps(ctx)
		if err != nil {
			t.Fatal(err)
		}
		state := map[string]compose.Container{}
		for _, c := range containers {
			state[c.Service] = c
		}
		db, cp := state["mariadb"], state["control-plane"]
		for _, c := range containers {
			if c.State == "exited" || c.State == "restarting" || c.State == "dead" {
				t.Fatalf("service %s is %s: %+v", c.Service, c.State, containers)
			}
		}
		if db.Name != "" && db.Name != platform.MariaDBContainer(cfg.ProjectName) {
			t.Fatalf("MariaDB container is %s, want %s", db.Name, platform.MariaDBContainer(cfg.ProjectName))
		}
		if db.Health == "healthy" && cp.State == "running" {
			cpHealth = cp.Health
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("services not ready: %+v", containers)
		}
		time.Sleep(2 * time.Second)
	}
	t.Logf("control plane container health: %q", cpHealth)

	exec := func(service string, command ...string) string {
		t.Helper()
		out, err := p.Exec(ctx, service, command...)
		if err != nil {
			t.Fatalf("exec %s %v: %v", service, command, err)
		}
		return strings.TrimSpace(string(out))
	}

	// Memory limits match the §4.1 budget.
	for service, mb := range map[string]int{
		"mariadb":       platform.DefaultMariaDBMemoryMB,
		"control-plane": platform.DefaultControlPlaneMemoryMB,
	} {
		got := exec(service, "cat", "/sys/fs/cgroup/memory.max")
		if want := strconv.Itoa(mb * 1024 * 1024); got != want {
			t.Errorf("%s memory.max = %s, want %s", service, got, want)
		}
	}

	// MariaDB loaded the rendered tuning and accepts the root password from
	// the secret file over TCP (root@'%', as the control plane will connect).
	if got := exec("mariadb", "mariadb", "--defaults-extra-file=/var/lib/mysql/.my-healthcheck.cnf",
		"-Nse", "SELECT @@innodb_buffer_pool_size, @@max_connections"); got != "402653184\t150" {
		t.Errorf("buffer pool, max_connections = %q", got)
	}
	if got := exec("mariadb", "mariadb", "-h127.0.0.1", "-uroot", "--password="+password, "-Nse", "SELECT 1"); got != "1" {
		t.Errorf("root login returned %q", got)
	}

	// The control plane sees the data directory at the host's path, and can
	// drive the host's Docker daemon through the socket.
	exec("control-plane", "test", "-f", filepath.Join(dir, platform.ComposeFile))
	exec("control-plane", "test", "-f", "/run/secrets/mariadb_root_password")
	exec("control-plane", "test", "-f", platform.MasterKeySecretPath)
	if got := exec("control-plane", "docker", "compose", "--project-name", cfg.ProjectName, "ps", "--quiet"); len(strings.Fields(got)) != 2 {
		t.Errorf("docker compose ps inside the control plane listed %q", got)
	}

	if out, err := p.Exec(ctx, "control-plane", "pressfleet", "healthcheck"); err != nil {
		t.Logf("pressfleet healthcheck fails until GET /healthz exists (#12): %v %s", err, out)
	} else {
		t.Log("pressfleet healthcheck passed")
	}
}
