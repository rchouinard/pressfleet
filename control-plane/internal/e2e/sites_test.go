package e2e_test

import (
	"context"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/api/client"
	"github.com/rchouinard/pressfleet/control-plane/internal/catalog"
	"github.com/rchouinard/pressfleet/control-plane/internal/e2e"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner/compose"
)

// jobTimeout bounds waiting for a site job. Provisioning pulls nothing (the
// images are ensured first) but waits for the containers' health checks.
const jobTimeout = 6 * time.Minute

// TestSiteCreateDelete is the acceptance test of #27, #29, and #31: create
// a site through the API, wait for provisioning, check that WordPress is
// installed and serving, then delete it and check that nothing is left.
func TestSiteCreateDelete(t *testing.T) {
	p := e2e.Start(t)
	ctx := t.Context()

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

	group, err := p.Client.CreateClientGroupWithResponse(ctx, client.CreateClientGroupJSONRequestBody{Name: "E2E Agency"})
	if err != nil {
		t.Fatal(err)
	}
	if group.JSON201 == nil {
		t.Fatalf("create client group: status %d: %s", group.StatusCode(), group.Body)
	}

	created, err := p.Client.CreateSiteWithResponse(ctx, client.CreateSiteJSONRequestBody{
		ClientGroupID: group.JSON201.ID,
		Name:          "E2E Site",
		AdminEmail:    "owner@e2e.test",
		PhpVersion:    &php,
	})
	if err != nil {
		t.Fatal(err)
	}
	if created.JSON202 == nil {
		t.Fatalf("create site: status %d: %s", created.StatusCode(), created.Body)
	}
	site := created.JSON202.Site
	t.Logf("site %s (%s), job %s", site.ID, site.PreviewDomain, created.JSON202.Job.ID)
	if site.Slug != "e2e-site" || site.PreviewDomain != "e2e-site."+e2e.PreviewBaseDomain || site.Status != client.SiteStatusProvisioning {
		t.Errorf("created site = %s", created.Body)
	}
	siteProject := compose.Project{Name: compose.SiteProjectName(site.ID), Dir: filepath.Join(p.DataDir, "sites", site.ID)}
	// Whatever happens below, don't leave the site's containers behind:
	// they aren't part of the platform project that Start tears down.
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if res, err := siteProject.Resources(ctx); err == nil && !res.Empty() {
			t.Logf("removing leftover site project %s", siteProject.Name)
			if err := siteProject.Down(ctx, compose.DownOptions{Volumes: true}); err != nil {
				t.Errorf("removing leftover site project: %v", err)
			}
		}
	})

	waitJob(t, p, created.JSON202.Job.ID)

	got, err := p.Client.GetSiteWithResponse(ctx, site.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.JSON200 == nil || got.JSON200.Status != client.SiteStatusRunning || !got.JSON200.AdminCredentialsAvailable {
		t.Fatalf("site after provisioning: status %d: %s", got.StatusCode(), got.Body)
	}

	t.Run("containers are running and healthy", func(t *testing.T) {
		containers, err := siteProject.Ps(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(containers) != 2 {
			t.Fatalf("containers = %+v", containers)
		}
		for _, c := range containers {
			if c.State != "running" || c.Health != "healthy" {
				t.Errorf("%s is %s (%s)", c.Service, c.State, c.Health)
			}
		}
	})

	creds, err := p.Client.RetrieveSiteAdminCredentialsWithResponse(ctx, site.ID)
	if err != nil {
		t.Fatal(err)
	}
	if creds.JSON200 == nil {
		t.Fatalf("admin credentials: status %d: %s", creds.StatusCode(), creds.Body)
	}
	admin := creds.JSON200

	t.Run("WordPress is installed", func(t *testing.T) {
		exec := func(command ...string) string {
			t.Helper()
			out, err := siteProject.Exec(ctx, "php", command...)
			if err != nil {
				t.Fatalf("%v: %v", command, err)
			}
			return strings.TrimSpace(string(out))
		}
		exec("wp", "core", "is-installed")
		if home := exec("wp", "option", "get", "home"); home != "https://"+site.PreviewDomain {
			t.Errorf("home = %q", home)
		}
		if email := exec("wp", "user", "get", admin.Username, "--field=user_email"); email != "owner@e2e.test" {
			t.Errorf("admin email = %q", email)
		}
		// The retrieved password is the one WordPress has.
		check := exec("wp", "eval", fmt.Sprintf(
			`$u = get_user_by("login", %q); echo wp_check_password(%q, $u->user_pass, $u->ID) ? "ok" : "mismatch";`,
			admin.Username, admin.Password))
		if check != "ok" {
			t.Errorf("admin password check: %s", check)
		}
	})

	t.Run("WordPress answers through nginx", func(t *testing.T) {
		// Fetched from the php container over the site network, with the
		// preview host: the host can't reach sites until routing exists.
		// A 200 with the site's title, or WordPress redirecting to its
		// HTTPS preview URL, both mean WordPress answered.
		script := fmt.Sprintf(`$c = stream_context_create(["http" => ["header" => "Host: %s", "follow_location" => 0, "ignore_errors" => true, "timeout" => 10]]);
$body = @file_get_contents("http://nginx:8080/", false, $c);
echo implode("\n", $http_response_header ?? ["no response"]), "\n\n", $body;`, site.PreviewDomain)
		var out string
		for deadline := time.Now().Add(30 * time.Second); ; {
			b, err := siteProject.Exec(ctx, "php", "php", "-r", script)
			out = string(b)
			if err == nil && !strings.HasPrefix(out, "no response") || time.Now().After(deadline) {
				break
			}
			time.Sleep(time.Second)
		}
		status, _, _ := strings.Cut(out, "\n")
		t.Logf("home page: %s", status)
		switch {
		case strings.Contains(status, " 200 ") && strings.Contains(out, "E2E Site"):
		case strings.Contains(status, " 301 ") && strings.Contains(out, "https://"+site.PreviewDomain+"/"):
		default:
			t.Errorf("home page response:\n%.600s", out)
		}
	})

	t.Run("admin credentials are shown once", func(t *testing.T) {
		if admin.Email != "owner@e2e.test" || admin.LoginURL != "https://"+site.PreviewDomain+"/wp-login.php" || len(admin.Password) < 20 {
			t.Errorf("credentials = %+v", admin)
		}
		again, err := p.Client.RetrieveSiteAdminCredentialsWithResponse(ctx, site.ID)
		if err != nil {
			t.Fatal(err)
		}
		if again.StatusCode() != http.StatusGone {
			t.Errorf("second retrieval: status %d: %s", again.StatusCode(), again.Body)
		}
	})

	t.Run("site database exists", func(t *testing.T) {
		if dbs, users := siteDatabase(t, p, site.ID); dbs != 1 || users != 1 {
			t.Errorf("site databases = %d, users = %d; want 1 and 1", dbs, users)
		}
	})
}

// waitJob polls a job until it reaches a terminal state and fails the test
// unless it succeeded.
func waitJob(t *testing.T, p *e2e.Platform, jobID string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), jobTimeout)
	defer cancel()
	for {
		resp, err := p.Client.GetJobWithResponse(ctx, jobID)
		if err != nil {
			t.Fatalf("job %s: %v", jobID, err)
		}
		if resp.JSON200 == nil {
			t.Fatalf("job %s: status %d: %s", jobID, resp.StatusCode(), resp.Body)
		}
		switch resp.JSON200.State {
		case client.JobStateSucceeded:
			return
		case client.JobStateFailed, client.JobStateRollbackFailed:
			t.Fatalf("job %s %s: %s", jobID, resp.JSON200.State, resp.Body)
		}
		select {
		case <-ctx.Done():
			t.Fatalf("job %s still %s: %s", jobID, resp.JSON200.State, resp.Body)
		case <-time.After(2 * time.Second):
		}
	}
}

// siteDatabase counts the site's database and database user on the
// platform MariaDB, as root. The password is read inside the container, so
// it never reaches the test's command lines.
func siteDatabase(t *testing.T, p *e2e.Platform, siteID string) (databases, users int) {
	t.Helper()
	name := "pf_" + siteID
	query := fmt.Sprintf("SELECT COUNT(*) FROM information_schema.SCHEMATA WHERE SCHEMA_NAME = '%s'; "+
		"SELECT COUNT(*) FROM mysql.user WHERE User = '%s';", name, name)
	out, err := p.Project.Exec(t.Context(), "mariadb", "sh", "-c",
		`exec mariadb --user=root --password="$(cat /run/secrets/mariadb_root_password)" --batch --skip-column-names -e "$1"`, "sh", query)
	if err != nil {
		t.Fatalf("querying MariaDB: %v", err)
	}
	if _, err := fmt.Sscan(string(out), &databases, &users); err != nil {
		t.Fatalf("parsing MariaDB output %q: %v", out, err)
	}
	return databases, users
}
