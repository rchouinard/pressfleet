package compose_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
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

// TestExecWPCLIOutputLimitDocker checks the Compose driver's cap on what
// ExecWPCLI keeps of a command's output: output beyond the limit is dropped
// and reported, and stderr keeps its end. The rest of ExecWPCLI's contract
// is in the conformance suite.
func TestExecWPCLIOutputLimitDocker(t *testing.T) {
	s := newDockerSite(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	siteID := s.cfg.ID
	if _, err := s.prov.CreateSite(ctx, s.cfg); err != nil {
		t.Fatal(err)
	}
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
	var ee *provisioner.ExecError
	if !errors.As(err, &ee) || !strings.HasSuffix(ee.Stderr, "the end") || len(ee.Stderr) > 70<<10 {
		t.Errorf("large stderr: err %v", err)
	}
}

// TestLogRotationDocker checks that both containers have the rotation
// limits that cap their disk use (§7.4), which is Docker's log driver
// configuration. Reading logs is in the conformance suite.
func TestLogRotationDocker(t *testing.T) {
	s := newDockerSite(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if _, err := s.prov.CreateSite(ctx, s.cfg); err != nil {
		t.Fatal(err)
	}
	for _, svc := range []string{"php", "nginx"} {
		out := s.docker(t, ctx, "container", "inspect", "--format", "{{json .HostConfig.LogConfig}}", s.containerID(t, ctx, svc))
		var lc struct {
			Type   string
			Config map[string]string
		}
		if err := json.Unmarshal([]byte(out), &lc); err != nil {
			t.Fatal(err)
		}
		if lc.Type != "json-file" || lc.Config["max-size"] == "" || lc.Config["max-file"] == "" {
			t.Errorf("%s log config = %+v, want json-file with rotation", svc, lc)
		}
	}
}
