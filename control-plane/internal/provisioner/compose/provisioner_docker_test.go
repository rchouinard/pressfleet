package compose_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/catalog"
	"github.com/rchouinard/pressfleet/control-plane/internal/config"
	"github.com/rchouinard/pressfleet/control-plane/internal/id"
	"github.com/rchouinard/pressfleet/control-plane/internal/platform"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner/compose"
	"github.com/rchouinard/pressfleet/control-plane/internal/sitedb"
)

// dockerSite is a Compose provisioner against the real Docker daemon, with a
// throwaway MariaDB container standing in for the platform's, and one site
// config ready to create.
type dockerSite struct {
	prov    *compose.Provisioner
	cfg     *provisioner.SiteConfig
	secrets memSecrets
	layout  config.Layout
	project compose.Project
	dbName  string
}

// newDockerSite skips unless PRESSFLEET_DOCKER_TESTS=1 and the site images
// for PHP 8.4 and 8.3 exist locally (make images). Everything it and the
// test create is removed afterwards, and the cleanup fails the test if
// anything of the site's project is left behind.
func newDockerSite(t *testing.T) *dockerSite {
	t.Helper()
	if os.Getenv("PRESSFLEET_DOCKER_TESTS") != "1" {
		t.Skip("set PRESSFLEET_DOCKER_TESTS=1 to run tests against Docker")
	}
	images := imageMap{
		"8.4": {
			PHP:   envOr("PRESSFLEET_TEST_SITE_PHP_IMAGE", "ghcr.io/rchouinard/pressfleet/site-php:php8.4-wp7.1.2"),
			Nginx: envOr("PRESSFLEET_TEST_SITE_NGINX_IMAGE", "ghcr.io/rchouinard/pressfleet/site-nginx:wp7.1.2"),
		},
		"8.3": {
			PHP:   envOr("PRESSFLEET_TEST_SITE_PHP83_IMAGE", "ghcr.io/rchouinard/pressfleet/site-php:php8.3-wp7.1.2"),
			Nginx: envOr("PRESSFLEET_TEST_SITE_NGINX_IMAGE", "ghcr.io/rchouinard/pressfleet/site-nginx:wp7.1.2"),
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	for _, img := range []catalog.Images{images["8.4"], images["8.3"]} {
		for _, ref := range []string{img.PHP, img.Nginx} {
			if _, err := compose.DockerExec(ctx, nil, "image", "inspect", ref); err != nil {
				t.Skipf("site image %s not available locally (make images): %v", ref, err)
			}
		}
	}
	platformImages, err := platform.DefaultImages("test")
	if err != nil {
		t.Fatal(err)
	}

	siteID := id.New()
	layout := config.Layout{Root: t.TempDir()}
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	const dbPassword = `pa'ss\word$1"?>`
	sec := memSecrets{}
	dbRef, saltsRef := siteSecrets(t, sec, siteID, sitedb.Credentials{Database: "wordpress", Username: "wordpress", Password: dbPassword})

	dir, err := layout.SiteDir(siteID)
	if err != nil {
		t.Fatal(err)
	}
	// The stand-in MariaDB lives on its own network, as the platform's does.
	dbName := compose.SiteProjectName(siteID) + "-testdb"
	s := &dockerSite{
		cfg: &provisioner.SiteConfig{
			ID:               siteID,
			ClientGroupID:    id.New(),
			PreviewDomain:    "acme.preview.example.com",
			PHPVersion:       "8.4",
			WPCoreVersion:    "7.1.2",
			Resources:        provisioner.DefaultResourceLimits(),
			DBSecretRef:      dbRef,
			WPSaltsSecretRef: saltsRef,
			UID:              provisioner.MinSiteUID + 4343,
		},
		secrets: sec,
		layout:  layout,
		project: compose.Project{Name: compose.SiteProjectName(siteID), Dir: dir},
		dbName:  dbName,
	}
	s.prov, err = compose.NewProvisioner(compose.ProvisionerConfig{
		Layout:           layout,
		Secrets:          sec,
		Images:           images,
		MariaDBContainer: dbName,
		WaitTimeout:      2 * time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if err := s.prov.DeleteSite(ctx, siteID); err != nil {
			t.Errorf("cleanup: delete site: %v", err)
		}
		if _, err := compose.DockerExec(ctx, nil, "rm", "--force", "--volumes", dbName); err != nil {
			t.Errorf("cleanup: removing database container: %v", err)
		}
		if _, err := compose.DockerExec(ctx, nil, "network", "rm", dbName); err != nil {
			t.Errorf("cleanup: removing database network: %v", err)
		}
		if res, err := s.project.Resources(ctx); err != nil {
			t.Errorf("cleanup: %v", err)
		} else if !res.Empty() {
			t.Errorf("cleanup: site resources left behind: %+v", res)
		}
	})
	s.docker(t, ctx, "network", "create", "--label", "pressfleet.test=true", dbName)
	s.docker(t, ctx, "run", "--detach", "--name", dbName, "--network", dbName, "--label", "pressfleet.test=true",
		"--env", "MARIADB_RANDOM_ROOT_PASSWORD=1", "--env", "MARIADB_DATABASE=wordpress",
		"--env", "MARIADB_USER=wordpress", "--env", "MARIADB_PASSWORD="+dbPassword,
		"--health-cmd", "healthcheck.sh --connect --innodb_initialized", "--health-interval", "2s",
		platformImages.MariaDB)
	return s
}

func (s *dockerSite) docker(t *testing.T, ctx context.Context, args ...string) string {
	t.Helper()
	out, err := compose.DockerExec(ctx, nil, args...)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}

// exec runs a command in a site container, bypassing the provisioner.
func (s *dockerSite) exec(t *testing.T, ctx context.Context, service string, command ...string) string {
	t.Helper()
	out, err := s.project.Exec(ctx, service, command...)
	if err != nil {
		t.Fatalf("exec %s %q: %v", service, command, err)
	}
	return strings.TrimSpace(string(out))
}

// containerID identifies a container instance, to tell whether it was
// recreated.
func (s *dockerSite) containerID(t *testing.T, ctx context.Context, service string) string {
	t.Helper()
	return s.docker(t, ctx, "inspect", "--format", "{{.Id}}", s.project.Name+"-"+service+"-1")
}

func (s *dockerSite) waitDBHealthy(t *testing.T, ctx context.Context) {
	t.Helper()
	for deadline := time.Now().Add(2 * time.Minute); ; {
		if s.docker(t, ctx, "inspect", "--format", "{{.State.Health.Status}}", s.dbName) == "healthy" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("database not healthy")
		}
		time.Sleep(time.Second)
	}
}

func (s *dockerSite) wantPhase(t *testing.T, ctx context.Context, want provisioner.SitePhase) {
	t.Helper()
	st, err := s.prov.GetSite(ctx, s.cfg.ID)
	if err != nil {
		t.Fatal(err)
	}
	if st.Phase != want {
		t.Fatalf("phase = %s (%s), want %s", st.Phase, st.Message, want)
	}
}

// TestProvisionerLifecycle drives one site through every lifecycle method
// against real Docker: create, converge, stop, start, updates that do and
// don't recreate containers (while running and while stopped), and delete.
func TestProvisionerLifecycle(t *testing.T) {
	s := newDockerSite(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	siteID := s.cfg.ID

	// Create: running, with MariaDB reachable as pf-mariadb.
	st, err := s.prov.CreateSite(ctx, s.cfg)
	if err != nil {
		t.Fatal(err)
	}
	if st.Phase != provisioner.SiteRunning || st.ID != siteID {
		t.Fatalf("CreateSite status = %+v", st)
	}
	s.waitDBHealthy(t, ctx)
	s.exec(t, ctx, "php", "wp", "core", "install", "--url=http://127.0.0.1:8080", "--title=Lifecycle",
		"--admin_user=admin", "--admin_password=test-admin-pw", "--admin_email=admin@example.com", "--skip-email")
	if got := s.exec(t, ctx, "php", "wp", "option", "get", "blogname"); got != "Lifecycle" {
		t.Errorf("blogname = %q", got)
	}

	// Create again converges without recreating anything.
	phpID, nginxID := s.containerID(t, ctx, "php"), s.containerID(t, ctx, "nginx")
	if _, err := s.prov.CreateSite(ctx, s.cfg); err != nil {
		t.Fatal(err)
	}
	if s.containerID(t, ctx, "php") != phpID || s.containerID(t, ctx, "nginx") != nginxID {
		t.Error("converging an unchanged site recreated containers")
	}

	// Stop and start.
	if err := s.prov.StopSite(ctx, siteID); err != nil {
		t.Fatal(err)
	}
	s.wantPhase(t, ctx, provisioner.SiteStopped)
	if err := s.prov.StartSite(ctx, siteID); err != nil {
		t.Fatal(err)
	}
	s.wantPhase(t, ctx, provisioner.SiteRunning)
	if got := s.exec(t, ctx, "php", "wp", "option", "get", "blogname"); got != "Lifecycle" {
		t.Errorf("after restart, blogname = %q", got)
	}

	// A resource change recreates both containers with the new limits.
	s.cfg.Resources.MemoryLimitMB = 640
	if st, err := s.prov.UpdateSite(ctx, s.cfg); err != nil || st.Phase != provisioner.SiteRunning {
		t.Fatalf("UpdateSite = %+v, %v", st, err)
	}
	if got := s.exec(t, ctx, "php", "cat", "/sys/fs/cgroup/memory.max"); got != strconv.Itoa(560*1024*1024) {
		t.Errorf("php memory.max = %s after raising the site limit to 640 MiB", got)
	}

	// A PHP version change swaps the php image.
	s.cfg.PHPVersion = "8.3"
	if _, err := s.prov.UpdateSite(ctx, s.cfg); err != nil {
		t.Fatal(err)
	}
	if got := s.exec(t, ctx, "php", "php", "-r", "echo PHP_MAJOR_VERSION, '.', PHP_MINOR_VERSION;"); got != "8.3" {
		t.Errorf("PHP version = %s after switching to 8.3", got)
	}
	s.wantPhase(t, ctx, provisioner.SiteRunning)

	// New salts change only wp-config.php: php is recreated to see the new
	// file, and nginx keeps running.
	phpID, nginxID = s.containerID(t, ctx, "php"), s.containerID(t, ctx, "nginx")
	oldSalt := s.exec(t, ctx, "php", "wp", "eval", "echo AUTH_KEY;")
	_, s.cfg.WPSaltsSecretRef = siteSecrets(t, s.secrets, siteID+"-v2",
		sitedb.Credentials{Database: "x", Username: "x", Password: "x"})
	if _, err := s.prov.UpdateSite(ctx, s.cfg); err != nil {
		t.Fatal(err)
	}
	if s.containerID(t, ctx, "php") == phpID {
		t.Error("php was not recreated after wp-config.php changed")
	}
	if s.containerID(t, ctx, "nginx") != nginxID {
		t.Error("nginx was recreated although only wp-config.php changed")
	}
	if got := s.exec(t, ctx, "php", "wp", "eval", "echo AUTH_KEY;"); got == oldSalt {
		t.Error("php still sees the old salts")
	}

	// Updating a stopped site keeps it stopped; the change applies on start.
	if err := s.prov.StopSite(ctx, siteID); err != nil {
		t.Fatal(err)
	}
	s.cfg.Resources.MemoryLimitMB = 512
	if st, err := s.prov.UpdateSite(ctx, s.cfg); err != nil || st.Phase != provisioner.SiteStopped {
		t.Fatalf("UpdateSite on a stopped site = %+v, %v", st, err)
	}
	if err := s.prov.StartSite(ctx, siteID); err != nil {
		t.Fatal(err)
	}
	if got := s.exec(t, ctx, "php", "cat", "/sys/fs/cgroup/memory.max"); got != strconv.Itoa(448*1024*1024) {
		t.Errorf("php memory.max = %s after lowering the site limit to 512 MiB while stopped", got)
	}
	// WP-CLI through the provisioner reaches the installed site.
	if out, err := s.prov.ExecWPCLI(ctx, siteID, []string{"option", "get", "blogname"}); err != nil || out != "Lifecycle\n" {
		t.Errorf("ExecWPCLI option get blogname = %q, %v", out, err)
	}

	// Delete removes everything, and deleting again succeeds.
	if err := s.prov.DeleteSite(ctx, siteID); err != nil {
		t.Fatal(err)
	}
	if res, err := s.project.Resources(ctx); err != nil || !res.Empty() {
		t.Errorf("after delete: resources %+v, %v", res, err)
	}
	dir, err := s.layout.SiteDir(siteID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("site directory remains after delete: %v", err)
	}
	if nets := s.docker(t, ctx, "inspect", "--format", "{{json .NetworkSettings.Networks}}", s.dbName); strings.Contains(nets, s.project.Name+`"`) {
		t.Errorf("MariaDB still on the site network: %s", nets)
	}
	if err := s.prov.DeleteSite(ctx, siteID); err != nil {
		t.Errorf("second delete: %v", err)
	}
	if _, err := s.prov.GetSite(ctx, siteID); !errors.Is(err, provisioner.ErrSiteNotFound) {
		t.Errorf("GetSite after delete = %v, want ErrSiteNotFound", err)
	}
	if err := s.prov.StartSite(ctx, siteID); !errors.Is(err, provisioner.ErrSiteNotFound) {
		t.Errorf("StartSite after delete = %v, want ErrSiteNotFound", err)
	}
}

// TestExecWPCLIDocker runs WP-CLI through the provisioner against a real
// site: as the site UID, with hostile arguments reaching it verbatim, with
// structured errors, bounded output, and a timeout that also ends the
// process inside the container.
func TestExecWPCLIDocker(t *testing.T) {
	s := newDockerSite(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	siteID := s.cfg.ID
	if _, err := s.prov.CreateSite(ctx, s.cfg); err != nil {
		t.Fatal(err)
	}
	wp := func(args ...string) string {
		t.Helper()
		out, err := s.prov.ExecWPCLI(ctx, siteID, args)
		if err != nil {
			t.Fatalf("wp %q: %v", args, err)
		}
		return out
	}

	// It runs as the site UID.
	if got := wp("eval", "--skip-wordpress", "echo posix_geteuid(), ':', posix_getegid();"); got != fmt.Sprintf("%d:%d", s.cfg.UID, s.cfg.UID) {
		t.Errorf("wp runs as %q, want the site UID %d", got, s.cfg.UID)
	}

	// Arguments arrive as WP-CLI's argv, unchanged. The PHP file prints
	// the process's argv after the file name, as JSON.
	s.exec(t, ctx, "php", "sh", "-c", `printf '%s' "$1" > /tmp/argv.php`, "sh",
		`<?php echo json_encode( array_slice( $_SERVER['argv'], array_search( '/tmp/argv.php', $_SERVER['argv'], true ) + 1 ) );`)
	raw := wp(append([]string{"eval-file", "--skip-wordpress", "/tmp/argv.php"}, hostileArgs...)...)
	var got []string
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("argv output %q: %v", raw, err)
	}
	if !slices.Equal(got, hostileArgs) {
		t.Errorf("wp saw argv\n %q\nwant\n %q", got, hostileArgs)
	}
	if _, err := s.project.Exec(ctx, "php", "test", "-e", "/tmp/pwned"); err == nil {
		t.Error("an argument was executed by a shell")
	}

	// A non-zero exit is an *ExecError with the exit code and stderr.
	_, err := s.prov.ExecWPCLI(ctx, siteID, []string{"eval", "--skip-wordpress", "echo 'out'; fwrite( STDERR, 'boom' ); exit( 3 );"})
	var ee *provisioner.ExecError
	if !errors.As(err, &ee) {
		t.Fatalf("err = %v, want *provisioner.ExecError", err)
	}
	if ee.ExitCode != 3 || ee.Stdout != "out" || !strings.Contains(ee.Stderr, "boom") {
		t.Errorf("ExecError = %+v", ee)
	}

	// Output beyond the limit is dropped and reported; stderr keeps its end.
	small, err := compose.NewProvisioner(compose.ProvisionerConfig{
		Layout: s.layout, Secrets: s.secrets, Images: imageMap{}, MariaDBContainer: s.dbName, WPCLIMaxOutput: 1000,
	})
	if err != nil {
		t.Fatal(err)
	}
	out, err := small.ExecWPCLI(ctx, siteID, []string{"eval", "--skip-wordpress", "echo str_repeat( 'x', 1 << 20 );"})
	if !errors.Is(err, compose.ErrOutputTruncated) || out != strings.Repeat("x", 1000) {
		t.Errorf("ExecWPCLI = %d bytes, %v; want 1000 bytes and ErrOutputTruncated", len(out), err)
	}
	_, err = small.ExecWPCLI(ctx, siteID, []string{"eval", "--skip-wordpress",
		"fwrite( STDERR, str_repeat( 'n', 1 << 20 ) . 'the end' ); exit( 1 );"})
	if !errors.As(err, &ee) || !strings.HasSuffix(ee.Stderr, "the end") || len(ee.Stderr) > 70<<10 {
		t.Errorf("large stderr: %d bytes, ending %q, err %v", len(ee.Stderr), ee.Stderr[max(0, len(ee.Stderr)-20):], err)
	}

	// The deadline ends the call, and timeout(1) ends WP-CLI in the
	// container shortly after.
	short, cancelShort := context.WithTimeout(ctx, 2*time.Second)
	defer cancelShort()
	start := time.Now()
	_, err = s.prov.ExecWPCLI(short, siteID, []string{"eval", "--skip-wordpress", "sleep( 120 ); // pf-timeout-test"})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want context.DeadlineExceeded", err)
	}
	if elapsed := time.Since(start); elapsed > 15*time.Second {
		t.Errorf("ExecWPCLI returned %s after its deadline", elapsed)
	}
	for deadline := time.Now().Add(20 * time.Second); ; {
		procs := s.exec(t, ctx, "php", "sh", "-c", `cat /proc/[0-9]*/cmdline | tr '\0' ' '`)
		if !strings.Contains(procs, "pf-timeout-test") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("WP-CLI still running in the container after the timeout: %s", procs)
		}
		time.Sleep(time.Second)
	}

	// A stopped site can't run WP-CLI.
	if err := s.prov.StopSite(ctx, siteID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.prov.ExecWPCLI(ctx, siteID, []string{"--info"}); !errors.Is(err, compose.ErrSiteNotRunning) {
		t.Errorf("on a stopped site: %v, want ErrSiteNotRunning", err)
	}
}
