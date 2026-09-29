package platform_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/config"
	"github.com/rchouinard/pressfleet/control-plane/internal/e2e"
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
	const (
		password      = "test-root-pw"
		redisPassword = "test-redis-admin-pw"
	)
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
		RedisAdminPassword:  redisPassword,
		SSHHostKey:          e2e.HostKey(t),
		MasterKeyFile:       keyFile,
		// Development machines and CI runners often lack Docker address pools.
		SkipPreflight: true,
		// Never ask Let's Encrypt for certificates from a test.
		ACME: platform.ACMEConfig{CAServer: e2e.ACMECAServer},
	}
	dir := layout.PlatformDir()
	if err := platform.Render(dir, cfg); err != nil {
		t.Fatal(err)
	}
	// Publishes Traefik on ephemeral loopback ports instead of the host's
	// 80 and 443.
	e2e.WriteOverride(t, dir)
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

	// Wait for MariaDB and Redis to be healthy and the control plane to be
	// running.
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
		db, cp, redis, tr := state["mariadb"], state["control-plane"], state["redis"], state["traefik"]
		for _, c := range containers {
			if c.State == "exited" || c.State == "restarting" || c.State == "dead" {
				t.Fatalf("service %s is %s: %+v", c.Service, c.State, containers)
			}
		}
		if db.Name != "" && db.Name != platform.MariaDBContainer(cfg.ProjectName) {
			t.Fatalf("MariaDB container is %s, want %s", db.Name, platform.MariaDBContainer(cfg.ProjectName))
		}
		if redis.Name != "" && redis.Name != platform.RedisContainer(cfg.ProjectName) {
			t.Fatalf("Redis container is %s, want %s", redis.Name, platform.RedisContainer(cfg.ProjectName))
		}
		if db.Health == "healthy" && redis.Health == "healthy" && cp.State == "running" &&
			tr.Health == "healthy" && state["socket-proxy"].Health == "healthy" && state["maintenance"].Health == "healthy" {
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
		"redis":         platform.DefaultRedisMaxMemoryMB + platform.RedisOverheadMB,
		"traefik":       platform.DefaultTraefikMemoryMB,
		"maintenance":   platform.MaintenanceMemoryMB,
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

	// Redis: the default user is disabled, the admin logs in with the
	// password from the control plane's secret, and the memory cap and
	// eviction policy from valkey.conf are in force.
	if _, err := p.Exec(ctx, "redis", "valkey-cli", "-e", "ping"); err == nil || !strings.Contains(err.Error(), "NOAUTH") {
		t.Errorf("unauthenticated PING: err = %v, want NOAUTH", err)
	}
	if _, err := p.Exec(ctx, "redis", "valkey-cli", "--no-auth-warning", "--user", "default", "--pass", "x", "-e", "ping"); err == nil {
		t.Error("default user PING succeeded")
	}
	adminArgs := []string{"valkey-cli", "--no-auth-warning", "--user", platform.RedisAdminUser, "--pass", redisPassword, "-e"}
	admin := func(args ...string) string {
		t.Helper()
		return exec("redis", append(adminArgs, args...)...)
	}
	if got := admin("CONFIG", "GET", "maxmemory"); got != "maxmemory\n"+strconv.Itoa(platform.DefaultRedisMaxMemoryMB*1024*1024) {
		t.Errorf("maxmemory = %q", got)
	}
	if got := admin("CONFIG", "GET", "maxmemory-policy"); got != "maxmemory-policy\nallkeys-lru" {
		t.Errorf("maxmemory-policy = %q", got)
	}
	if got := admin("CONFIG", "GET", "databases"); got != "databases\n"+strconv.Itoa(platform.RedisDatabases) {
		t.Errorf("databases = %q", got)
	}
	// The memory cap is enforced. SETRANGE at offset 512 KiB - 1 writes a
	// 512 KiB value from a short command line. With maxmemory lowered and
	// eviction off, writes are refused once the cap is reached...
	fill := func(i int) error {
		_, err := p.Exec(ctx, "redis", append(adminArgs, "SETRANGE", "big"+strconv.Itoa(i), strconv.Itoa(512<<10-1), "x")...)
		return err
	}
	admin("CONFIG", "SET", "maxmemory-policy", "noeviction")
	admin("CONFIG", "SET", "maxmemory", "2mb")
	var oom error
	for i := 0; i < 8 && oom == nil; i++ {
		oom = fill(i)
	}
	if oom == nil || !strings.Contains(oom.Error(), "OOM") {
		t.Errorf("writes over maxmemory: err = %v, want OOM", oom)
	}
	// ...and with allkeys-lru, the same writes evict older keys instead.
	admin("FLUSHALL")
	admin("CONFIG", "SET", "maxmemory-policy", "allkeys-lru")
	for i := range 8 {
		if err := fill(i); err != nil {
			t.Fatalf("write under allkeys-lru: %v", err)
		}
	}
	if got := admin("INFO", "stats"); !regexp.MustCompile(`(?m)^evicted_keys:[1-9]`).MatchString(got) {
		t.Errorf("no evictions under allkeys-lru:\n%s", got)
	}

	// The control plane sees the data directory at the host's path, and can
	// drive the host's Docker daemon through the socket.
	exec("control-plane", "test", "-f", filepath.Join(dir, platform.ComposeFile))
	exec("control-plane", "test", "-f", "/run/secrets/mariadb_root_password")
	if got := exec("control-plane", "cat", "/run/secrets/redis_admin_password"); got != redisPassword {
		t.Errorf("control plane's Redis admin password = %q", got)
	}
	exec("control-plane", "test", "-f", platform.MasterKeySecretPath)
	if got := exec("control-plane", "docker", "compose", "--project-name", cfg.ProjectName, "ps", "--quiet"); len(strings.Fields(got)) != 6 {
		t.Errorf("docker compose ps inside the control plane listed %q", got)
	}

	if out, err := p.Exec(ctx, "control-plane", "pressfleet", "healthcheck"); err != nil {
		t.Logf("pressfleet healthcheck fails until GET /healthz exists (#12): %v %s", err, out)
	} else {
		t.Log("pressfleet healthcheck passed")
	}
}
