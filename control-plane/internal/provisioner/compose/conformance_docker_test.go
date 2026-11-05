package compose_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/catalog"
	"github.com/rchouinard/pressfleet/control-plane/internal/id"
	"github.com/rchouinard/pressfleet/control-plane/internal/objectcache"
	"github.com/rchouinard/pressfleet/control-plane/internal/platform"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner/compose"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner/conformance"
	"github.com/rchouinard/pressfleet/control-plane/internal/sitedb"
)

// TestConformance runs the driver conformance suite (package conformance,
// docs/testing.md) against the Compose driver and real Docker. MariaDB and
// Valkey stand in for the platform's, shared by all the suite's sites, which
// the driver attaches to each site's network as it does the platform's.
//
// The site images must exist locally (make images); override them with
// PRESSFLEET_TEST_SITE_PHP_IMAGE, PRESSFLEET_TEST_SITE_PHP83_IMAGE, and
// PRESSFLEET_TEST_SITE_NGINX_IMAGE. Use -run 'TestConformance/Tasks' to run
// one group.
func TestConformance(t *testing.T) {
	if os.Getenv("PRESSFLEET_DOCKER_TESTS") != "1" {
		t.Skip("set PRESSFLEET_DOCKER_TESTS=1 to run tests against Docker")
	}
	images := map[string]catalog.Images{
		"8.4": {
			PHP:   envOr("PRESSFLEET_TEST_SITE_PHP_IMAGE", "ghcr.io/rchouinard/pressfleet/site-php:php8.4-wp7.1.2"),
			Nginx: envOr("PRESSFLEET_TEST_SITE_NGINX_IMAGE", "ghcr.io/rchouinard/pressfleet/site-nginx:wp7.1.2"),
		},
		"8.3": {
			PHP:   envOr("PRESSFLEET_TEST_SITE_PHP83_IMAGE", "ghcr.io/rchouinard/pressfleet/site-php:php8.3-wp7.1.2"),
			Nginx: envOr("PRESSFLEET_TEST_SITE_NGINX_IMAGE", "ghcr.io/rchouinard/pressfleet/site-nginx:wp7.1.2"),
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	for _, img := range images {
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

	suffix := strings.ToLower(id.New())
	db := platformServices{
		database: sitedb.Credentials{Database: "wordpress", Username: "wordpress", Password: `pa'ss\word$1"?>`},
		cache:    objectcache.Credentials{Username: "default", Password: "pf-conformance-cache-pw", Database: 1},
		mariadb:  "pftest-conf-" + suffix + "-db",
		valkey:   "pftest-conf-" + suffix + "-cache",
	}
	docker := func(args ...string) string {
		t.Helper()
		out, err := compose.DockerExec(ctx, nil, args...)
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(string(out))
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		for _, name := range []string{db.mariadb, db.valkey} {
			if _, err := compose.DockerExec(ctx, nil, "rm", "--force", "--volumes", name); err != nil {
				t.Errorf("cleanup: removing %s: %v", name, err)
			}
		}
		// The suite's sites are gone by now, and their networks with them.
		if _, err := compose.DockerExec(ctx, nil, "network", "rm", db.mariadb); err != nil {
			t.Errorf("cleanup: removing the database network: %v", err)
		}
	})
	// The stand-in MariaDB lives on its own network, as the platform's does.
	docker("network", "create", "--label", "pressfleet.test=true", db.mariadb)
	docker("run", "--detach", "--name", db.mariadb, "--network", db.mariadb, "--label", "pressfleet.test=true",
		"--env", "MARIADB_RANDOM_ROOT_PASSWORD=1", "--env", "MARIADB_DATABASE="+db.database.Database,
		"--env", "MARIADB_USER="+db.database.Username, "--env", "MARIADB_PASSWORD="+db.database.Password.Reveal(),
		"--health-cmd", "healthcheck.sh --connect --innodb_initialized", "--health-interval", "2s",
		platformImages.MariaDB)
	docker("run", "--detach", "--name", db.valkey, "--label", "pressfleet.test=true",
		platformImages.Redis, "valkey-server", "--requirepass", db.cache.Password.Reveal())
	for deadline := time.Now().Add(2 * time.Minute); docker("inspect", "--format", "{{.State.Health.Status}}", db.mariadb) != "healthy"; time.Sleep(time.Second) {
		if time.Now().After(deadline) {
			t.Fatal("database not healthy")
		}
	}

	conformance.Run(t, conformance.Env{
		Images:        images,
		PHPVersion:    "8.4",
		AltPHPVersion: "8.3",
		WPCoreVersion: "7.1.2",
		Database:      db.database,
		ObjectCache:   &db.cache,
		New: func(t *testing.T, s conformance.Setup) conformance.Instance {
			return newConformanceInstance(t, s, db)
		},
	})
}

// platformServices are the stand-ins for the platform's database and object
// cache.
type platformServices struct {
	database        sitedb.Credentials
	cache           objectcache.Credentials
	mariadb, valkey string // container names
}

// newConformanceInstance builds a Compose provisioner for the suite and the
// ways of looking inside its sites through the docker CLI.
func newConformanceInstance(t *testing.T, s conformance.Setup, svc platformServices) conformance.Instance {
	t.Helper()
	routes := t.TempDir()
	prov, err := compose.NewProvisioner(compose.ProvisionerConfig{
		Layout:            s.Layout,
		Secrets:           s.Secrets,
		Images:            s.Images,
		MariaDBContainer:  svc.mariadb,
		RedisContainer:    svc.valkey,
		TraefikDynamicDir: routes,
		WaitTimeout:       2 * time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	project := func(siteID string) compose.Project {
		return compose.Project{Name: compose.SiteProjectName(siteID), Dir: filepath.Join(s.Layout.SitesDir(), siteID)}
	}
	docker := func(ctx context.Context, args ...string) (string, error) {
		out, err := compose.DockerExec(ctx, nil, args...)
		return strings.TrimSpace(string(out)), err
	}
	return conformance.Instance{
		Driver: prov,

		Exec: func(ctx context.Context, siteID string, c provisioner.Component, argv ...string) (string, error) {
			out, err := project(siteID).Exec(ctx, string(c), argv...)
			return string(out), err
		},

		// The request goes from the php container to nginx over the site
		// network, as the driver's own probe does, but reports the whole
		// response. HTTP/1.0 keeps nginx from chunking the body.
		Get: func(ctx context.Context, siteID, host, path string) (*conformance.HTTPResponse, error) {
			out, err := project(siteID).Exec(ctx, string(provisioner.ComponentPHP), "php", "-r", httpGetScript, host, path)
			if err != nil {
				return nil, err
			}
			var r struct {
				Status  int
				Headers []string
				Body    string
			}
			if err := json.Unmarshal(out, &r); err != nil {
				return nil, fmt.Errorf("response %q: %w", out, err)
			}
			resp := &conformance.HTTPResponse{StatusCode: r.Status, Header: http.Header{}, Body: r.Body}
			for _, h := range r.Headers {
				if k, v, ok := strings.Cut(h, ":"); ok {
					resp.Header.Add(strings.TrimSpace(k), strings.TrimSpace(v))
				}
			}
			return resp, nil
		},

		Address: func(ctx context.Context, siteID string) (string, error) {
			ip, err := docker(ctx, "inspect", "--format", "{{range .NetworkSettings.Networks}}{{.IPAddress}} {{end}}",
				compose.SiteProjectName(siteID)+"-nginx-1")
			if err != nil {
				return "", err
			}
			addr, _, _ := strings.Cut(ip, " ")
			return addr + ":8080", nil
		},

		Leftovers: func(ctx context.Context, owner string, deleted bool) ([]string, error) {
			var left []string
			for _, label := range []string{"pressfleet.site-id", "pressfleet.repository"} {
				// A site's containers carry the site label too, and
				// are left alone: only tasks and sessions are looked at.
				for _, kind := range []string{"pressfleet.task=true", "pressfleet.session=true"} {
					out, err := docker(ctx, "container", "ls", "--all", "--format", "{{.Names}}",
						"--filter", "label="+kind, "--filter", "label="+label+"="+owner)
					if err != nil {
						return nil, err
					}
					left = append(left, strings.Fields(out)...)
				}
			}
			entries, err := os.ReadDir(s.Layout.TmpDir())
			if err != nil {
				return nil, err
			}
			for _, e := range entries {
				if strings.HasPrefix(e.Name(), "task-secrets-") {
					left = append(left, "secrets directory "+e.Name())
				}
			}
			if !deleted {
				return left, nil
			}
			p := project(owner)
			res, err := p.Resources(ctx)
			if err != nil {
				return nil, err
			}
			for _, kind := range [][]string{res.Containers, res.Networks, res.Volumes} {
				left = append(left, kind...)
			}
			if _, err := os.Stat(p.Dir); err == nil {
				left = append(left, "site directory "+p.Dir)
			}
			for _, shared := range []string{svc.mariadb, svc.valkey} {
				nets, err := docker(ctx, "inspect", "--format", "{{json .NetworkSettings.Networks}}", shared)
				if err != nil {
					return nil, err
				}
				if strings.Contains(nets, p.Name+`"`) {
					left = append(left, shared+" is still on the site network")
				}
			}
			return left, nil
		},

		InMaintenance: func(_ context.Context, siteID string) (bool, error) {
			_, err := os.Stat(filepath.Join(routes, compose.MaintenanceRoutesFile(siteID)))
			if os.IsNotExist(err) {
				return false, nil
			}
			return err == nil, err
		},
	}
}

// httpGetScript sends a GET with the host and path in argv to nginx and
// prints the response as JSON.
const httpGetScript = `error_reporting(0);
[, $host, $path] = $argv;
$f = fsockopen('nginx', 8080, $errno, $errstr, 10);
if (!$f) { fwrite(STDERR, "connecting to nginx: $errstr\n"); exit(1); }
stream_set_timeout($f, 30);
fwrite($f, "GET $path HTTP/1.0\r\nHost: $host\r\nX-Forwarded-Proto: https\r\nConnection: close\r\n\r\n");
$raw = stream_get_contents($f);
[$head, $body] = array_pad(explode("\r\n\r\n", $raw, 2), 2, '');
$lines = explode("\r\n", $head);
if (!preg_match('#^HTTP/[0-9.]+ ([0-9]{3})#', $lines[0], $m)) { fwrite(STDERR, "no HTTP response\n"); exit(1); }
echo json_encode(array('status' => (int) $m[1], 'headers' => array_slice($lines, 1), 'body' => $body));`
