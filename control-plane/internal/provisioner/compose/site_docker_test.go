package compose_test

import (
	"context"
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/config"
	"github.com/rchouinard/pressfleet/control-plane/internal/id"
	"github.com/rchouinard/pressfleet/control-plane/internal/platform"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner/compose"
	"github.com/rchouinard/pressfleet/control-plane/internal/redact"
	"github.com/rchouinard/pressfleet/control-plane/internal/sitedb"
	"github.com/rchouinard/pressfleet/control-plane/internal/wpconfig"
)

// TestSiteUp renders a site, brings it up with the real site images next to a
// throwaway MariaDB attached the way the driver will attach the platform's
// (network alias pf-mariadb), installs WordPress, and checks the result.
// Everything it creates is removed afterwards.
//
// The site images must exist locally (make images); override them with
// PRESSFLEET_TEST_SITE_PHP_IMAGE and PRESSFLEET_TEST_SITE_NGINX_IMAGE.
func TestSiteUp(t *testing.T) {
	if os.Getenv("PRESSFLEET_DOCKER_TESTS") != "1" {
		t.Skip("set PRESSFLEET_DOCKER_TESTS=1 to run tests against Docker")
	}
	images := compose.SiteImages{
		PHP:   envOr("PRESSFLEET_TEST_SITE_PHP_IMAGE", "ghcr.io/rchouinard/pressfleet/site-php:php8.4-wp7.1.2"),
		Nginx: envOr("PRESSFLEET_TEST_SITE_NGINX_IMAGE", "ghcr.io/rchouinard/pressfleet/site-nginx:wp7.1.2"),
	}
	platformImages, err := platform.DefaultImages("test")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	docker := func(args ...string) string {
		t.Helper()
		out, err := compose.DockerExec(ctx, nil, args...)
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(string(out))
	}
	for _, image := range []string{images.PHP, images.Nginx} {
		if _, err := compose.DockerExec(ctx, nil, "image", "inspect", image); err != nil {
			t.Skipf("site image %s not available locally (make images): %v", image, err)
		}
	}

	siteID := id.New()
	project := compose.SiteProjectName(siteID)
	layout := config.Layout{Root: t.TempDir()}
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	dir, err := layout.SiteDir(siteID)
	if err != nil {
		t.Fatal(err)
	}

	// Values that need escaping, to prove they reach PHP intact.
	const (
		dbPassword  = `pa'ss\word$1"?>`
		trickyValue = `it's a \'test\' '); echo 'injected'; //`
	)
	uid := provisioner.MinSiteUID + 4242
	in := compose.SiteRender{
		Site: &provisioner.SiteConfig{
			ID:               siteID,
			ClientGroupID:    id.New(),
			PreviewDomain:    "acme.preview.example.com",
			PHPVersion:       "8.4",
			WPCoreVersion:    "7.1.2",
			Resources:        provisioner.DefaultResourceLimits(),
			DBSecretRef:      id.New(),
			WPSaltsSecretRef: id.New(),
			UID:              uid,
		},
		Images: images,
		WPConfig: wpconfig.Params{
			DB:    sitedb.Credentials{Database: "wordpress", Username: "wordpress", Password: dbPassword},
			Salts: wpconfig.GenerateSalts(),
			Constants: []wpconfig.Constant{
				{Name: "PF_TEST_STRING", Value: trickyValue},
				{Name: "PF_TEST_SECRET", Value: redact.Secret(`\`)},
				{Name: "PF_TEST_INT", Value: -7},
			},
		},
	}
	if _, err := compose.RenderSite(dir, in); err != nil {
		t.Fatal(err)
	}
	p := compose.Project{Name: project, Dir: dir}

	// MariaDB starts on its own network and joins the site network under
	// the platform alias, as the driver will do with the platform's MariaDB.
	dbNet, dbName := project+"-testdb", project+"-testdb"
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		// The database must leave the site network before Compose can
		// remove it.
		if _, err := compose.DockerExec(ctx, nil, "rm", "--force", "--volumes", dbName); err != nil {
			t.Errorf("removing database container: %v", err)
		}
		if err := p.Down(ctx, compose.DownOptions{Volumes: true}); err != nil {
			t.Errorf("down: %v", err)
		}
		if _, err := compose.DockerExec(ctx, nil, "network", "rm", dbNet); err != nil {
			t.Errorf("removing database network: %v", err)
		}
	})
	docker("network", "create", "--label", "pressfleet.test=true", dbNet)
	docker("run", "--detach", "--name", dbName, "--network", dbNet, "--label", "pressfleet.test=true",
		"--env", "MARIADB_RANDOM_ROOT_PASSWORD=1", "--env", "MARIADB_DATABASE=wordpress",
		"--env", "MARIADB_USER=wordpress", "--env", "MARIADB_PASSWORD="+dbPassword,
		"--health-cmd", "healthcheck.sh --connect --innodb_initialized", "--health-interval", "2s",
		platformImages.MariaDB)

	if err := p.Up(ctx, compose.UpOptions{Wait: true, WaitTimeout: 2 * time.Minute}); err != nil {
		t.Fatal(err)
	}
	docker("network", "connect", "--alias", compose.MariaDBAlias, project, dbName)
	for deadline := time.Now().Add(2 * time.Minute); ; {
		if docker("inspect", "--format", "{{.State.Health.Status}}", dbName) == "healthy" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("database not healthy")
		}
		time.Sleep(time.Second)
	}

	exec := func(service string, command ...string) string {
		t.Helper()
		out, err := p.Exec(ctx, service, command...)
		if err != nil {
			t.Fatalf("exec %s %v: %v", service, command, err)
		}
		return strings.TrimSpace(string(out))
	}

	// The generated file is valid PHP, and WordPress installs with it.
	if out := exec("php", "php", "-l", "/var/www/wordpress/wp-config.php"); !strings.Contains(out, "No syntax errors") {
		t.Errorf("php -l: %s", out)
	}
	// The URL nginx is fetched at below, so WordPress doesn't redirect to a
	// host the test can't reach.
	exec("php", "wp", "core", "install", "--url=http://127.0.0.1:8080", "--title=PressFleet site test",
		"--admin_user=admin", "--admin_password=test-admin-pw", "--admin_email=admin@example.com", "--skip-email")
	if out := exec("php", "wp", "core", "is-installed"); out != "" {
		t.Errorf("wp core is-installed printed %q", out)
	}

	// Escaped values arrive unchanged.
	raw := exec("php", "wp", "eval",
		"echo json_encode( array( DB_PASSWORD, PF_TEST_STRING, PF_TEST_SECRET, PF_TEST_INT, DB_HOST, DISABLE_WP_CRON, WP_AUTO_UPDATE_CORE ) );")
	var got []any
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("wp eval output %q: %v", raw, err)
	}
	want := []any{dbPassword, trickyValue, `\`, float64(-7), compose.DefaultDBHost, true, false}
	if len(got) != len(want) {
		t.Fatalf("constants = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("constant %d = %#v, want %#v", i, got[i], want[i])
		}
	}
	if out := exec("php", "wp", "eval", `require_once ABSPATH . "wp-admin/includes/file.php"; echo get_filesystem_method();`); out != "direct" {
		t.Errorf("filesystem method = %q", out)
	}
	if out := exec("php", "wp", "plugin", "list", "--status=must-use", "--field=name"); !strings.Contains(out, "pressfleet") {
		t.Errorf("platform mu-plugin not loaded: %q", out)
	}

	// Both containers run as the site UID with the rendered limits;
	// wp-config.php is read-only and absent from nginx.
	for _, svc := range []string{"php", "nginx"} {
		if got := exec(svc, "id", "-u"); got != strconv.Itoa(uid) {
			t.Errorf("%s runs as %s, want %d", svc, got, uid)
		}
	}
	if got := exec("php", "cat", "/sys/fs/cgroup/memory.max"); got != strconv.Itoa(448*1024*1024) {
		t.Errorf("php memory.max = %s", got)
	}
	if got := exec("nginx", "cat", "/sys/fs/cgroup/memory.max"); got != strconv.Itoa(64*1024*1024) {
		t.Errorf("nginx memory.max = %s", got)
	}
	if _, err := p.Exec(ctx, "php", "sh", "-c", "echo x >> /var/www/wordpress/wp-config.php"); err == nil {
		t.Error("wp-config.php is writable in php")
	}
	if _, err := p.Exec(ctx, "nginx", "test", "-e", "/var/www/wordpress/wp-config.php"); err == nil {
		t.Error("wp-config.php is present in nginx")
	}
	if _, err := p.Exec(ctx, "nginx", "touch", "/var/www/wordpress/wp-content/x"); err == nil {
		t.Error("wp-content is writable in nginx")
	}

	// nginx serves the installed site through php.
	// nginx re-resolves php every 10 s, so allow for a stale lookup.
	var body string
	for deadline := time.Now().Add(30 * time.Second); ; {
		out, err := p.Exec(ctx, "nginx", "wget", "-qO-", "http://127.0.0.1:8080/")
		body = string(out)
		if err == nil || time.Now().After(deadline) {
			break
		}
		time.Sleep(time.Second)
	}
	if !strings.Contains(body, "PressFleet site test") {
		t.Errorf("home page does not look like the installed site: %.300s", body)
	}

	// After a re-render changes wp-config.php, php sees the new file once it
	// is recreated (the driver's job, per RenderResult).
	in.WPConfig.Constants[2].Value = 8
	res, err := compose.RenderSite(dir, in)
	if err != nil {
		t.Fatal(err)
	}
	if !res.WPConfigChanged || res.ComposeChanged {
		t.Fatalf("re-render result = %+v", res)
	}
	nginxStarted := func() string {
		return docker("inspect", "--format", "{{.Id}} {{.State.StartedAt}}", project+"-nginx-1")
	}
	before := nginxStarted()
	docker("compose", "--project-name", project, "--project-directory", dir, "up", "--detach", "--wait", "--force-recreate", "php")
	if after := nginxStarted(); after != before {
		t.Errorf("recreating php restarted nginx: %s -> %s", before, after)
	}
	if got := exec("php", "wp", "eval", "echo PF_TEST_INT;"); got != "8" {
		t.Errorf("after recreating php, PF_TEST_INT = %q, want 8", got)
	}

}

func envOr(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}
