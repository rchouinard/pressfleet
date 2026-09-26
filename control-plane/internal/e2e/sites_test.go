package e2e_test

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/api/client"
	"github.com/rchouinard/pressfleet/control-plane/internal/catalog"
	"github.com/rchouinard/pressfleet/control-plane/internal/e2e"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner/compose"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
)

// jobTimeout bounds waiting for a site job. Provisioning pulls nothing (the
// images are ensured first) but waits for the containers' health checks.
const jobTimeout = 6 * time.Minute

// TestSiteCreateDelete is the acceptance test of #27, #29, and #31: create
// a site through the API, wait for provisioning, check that WordPress is
// installed and serving, then delete it and check that nothing is left.
func TestSiteCreateDelete(t *testing.T) {
	// Whatever happens below, don't leave the site's containers, network,
	// or volumes behind: they aren't part of the platform project that
	// Start tears down. Registered first, this runs last, after the
	// platform teardown has removed MariaDB, which would otherwise keep the
	// site network in use.
	var siteProject compose.Project
	t.Cleanup(func() {
		if siteProject.Name == "" {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if res, err := siteProject.Resources(ctx); err == nil && !res.Empty() {
			t.Logf("removing leftover site project %s", siteProject.Name)
			if err := siteProject.Down(ctx, compose.DownOptions{Volumes: true}); err != nil {
				t.Errorf("removing leftover site project: %v", err)
			}
		}
	})
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
	siteProject = compose.Project{Name: compose.SiteProjectName(site.ID), Dir: filepath.Join(p.DataDir, "sites", site.ID)}

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

	deleted, err := p.Client.DeleteSiteWithResponse(ctx, site.ID)
	if err != nil {
		t.Fatal(err)
	}
	if deleted.JSON202 == nil {
		t.Fatalf("delete site: status %d: %s", deleted.StatusCode(), deleted.Body)
	}
	waitJob(t, p, deleted.JSON202.Job.ID)

	t.Run("site is gone from the API", func(t *testing.T) {
		got, err := p.Client.GetSiteWithResponse(ctx, site.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.StatusCode() != http.StatusNotFound {
			t.Errorf("get deleted site: status %d: %s", got.StatusCode(), got.Body)
		}
	})

	t.Run("no containers, networks, volumes, or files are left", func(t *testing.T) {
		res, err := siteProject.Resources(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if !res.Empty() {
			t.Errorf("site project left resources behind: %+v", res)
		}
		if _, err := os.Stat(siteProject.Dir); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("site directory: %v, want it gone", err)
		}
	})

	t.Run("no database or user is left", func(t *testing.T) {
		if dbs, users := siteDatabase(t, p, site.ID); dbs != 0 || users != 0 {
			t.Errorf("site databases = %d, users = %d; want none", dbs, users)
		}
	})

	// Last, because it stops the platform: the state database belongs to
	// the control plane, so it is read once the control plane has stopped.
	t.Run("no secrets are left", func(t *testing.T) {
		// The harness's teardown can't hand the files back once the
		// control plane is stopped, so do it now.
		stateDir := filepath.Join(p.DataDir, "state")
		owner := fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid())
		if _, err := p.Project.Exec(ctx, e2e.Service, "chown", "-R", owner, p.DataDir); err != nil {
			t.Fatal(err)
		}
		if err := p.Project.Stop(ctx); err != nil {
			t.Fatal(err)
		}
		db := storetest.OpenUnmigrated(t, store.Config{Driver: store.SQLite, SQLitePath: filepath.Join(stateDir, "pressfleet.db")})
		for _, table := range []string{"secrets", "site_databases", "sites", "schedules WHERE target_type = 'site'"} {
			var n int
			if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table).Scan(&n); err != nil {
				t.Fatal(err)
			}
			if n != 0 {
				t.Errorf("%d rows left in %s", n, table)
			}
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
