package e2e_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/api/client"
	"github.com/rchouinard/pressfleet/control-plane/internal/catalog"
	"github.com/rchouinard/pressfleet/control-plane/internal/cli"
	"github.com/rchouinard/pressfleet/control-plane/internal/e2e"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner/compose"
	"github.com/rchouinard/pressfleet/control-plane/internal/redact"
)

// TestDisasterRecovery runs the host-loss drill of docs/disaster-recovery.md
// (#96). It installs a platform with `pressfleet init`, gives it a site with
// content and a backup, and backs up the state database. It then loses the
// host: the platform and the site are torn down with their volumes, and the
// data directory, config file, and everything else are deleted, except the
// backup location (<data_dir>/backups) and the saved master key. A fresh
// `pressfleet init` with the same answers and `pressfleet recover` must
// bring the platform back: the old admin logs in, the site is recreated from
// its backup with its files and database, and the restored login key still
// opens wp-admin, because the site's public key didn't change.
//
// A staging site of that site takes part: it is created before the state
// backup and has no backup of its own, because its scheduled backups are off
// (#196). After recovery it comes back from its production site's newest
// snapshot, at its own preview host (#206).
func TestDisasterRecovery(t *testing.T) {
	image := e2e.Image(t)
	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	configPath := filepath.Join(root, "etc", "pressfleet.yaml")
	project := compose.Project{
		Name: "pfdr-" + strings.ToLower(rand.Text())[:10],
		Dir:  filepath.Join(dataDir, "platform"),
	}
	var siteProject, stagingProject compose.Project
	// Registered before Adopt, so it runs after the platform teardown: see
	// TestSiteCreateDelete.
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		for _, proj := range []compose.Project{stagingProject, siteProject} {
			if proj.Name == "" {
				continue
			}
			if res, err := proj.Resources(ctx); err == nil && !res.Empty() {
				t.Logf("removing leftover site project %s", proj.Name)
				if err := proj.Down(ctx, compose.DownOptions{Volumes: true}); err != nil {
					t.Errorf("removing leftover site project %s: %v", proj.Name, err)
				}
			}
		}
	})
	p := e2e.Adopt(t, project, dataDir)
	ctx, cancel := context.WithTimeout(t.Context(), 40*time.Minute)
	defer cancel()

	const email = "admin@recovery.test"
	password := redact.Secret(strings.ToLower(rand.Text()))
	pressfleet := func(stdin string, args ...string) (int, string) {
		t.Helper()
		var stdout, stderr bytes.Buffer
		code := cli.Run(ctx, args, cli.Options{
			Version:             "e2e",
			Stdin:               strings.NewReader(stdin),
			Stdout:              &stdout,
			Stderr:              &stderr,
			Env:                 func(string) (string, bool) { return "", false },
			PlatformProjectName: project.Name,
		})
		out := strings.ReplaceAll(stdout.String(), keyLine(stdout.String()), "    pfmk1:<redacted>")
		t.Logf("pressfleet %s:\n%s%s", args[0], out, stderr.String())
		return code, out + stderr.String()
	}
	install := func(adminPassword redact.Secret, extra ...string) {
		t.Helper()
		// Rendering leaves the override in place; init only checks for
		// compose.yaml when looking for an existing installation.
		e2e.WriteOverride(t, project.Dir)
		args := append([]string{
			"init", "--config", configPath,
			"--non-interactive", "--confirm-key-stored", "--skip-preflight",
			"--dashboard-domain", "fleet.recovery.test", "--preview-base-domain", "preview.recovery.test",
			"--data-dir", dataDir, "--admin-email", email, "--admin-password-stdin",
			"--acme-ca-server", e2e.ACMECAServer, "--control-plane-image", image,
		}, extra...)
		if code, out := pressfleet(adminPassword.Reveal()+"\n", args...); code != 0 {
			t.Fatalf("init exited %d: %s", code, out)
		}
		p.Connect(ctx, t)
	}

	// 1. The old host: a platform with a site, its content backed up, and
	// a state backup taken after that.
	install(password)
	p.Client = p.Login(t, email, password)
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
	e2e.EnsureStagingImages(t, images.PHP)
	groupID := createGroup(t, p, "E2E Recovery Agency")
	created := createSite(t, p, groupID, "E2E Recovery", php)
	siteProject = compose.Project{Name: compose.SiteProjectName(created.Site.ID), Dir: filepath.Join(dataDir, "sites", created.Site.ID)}
	waitJob(t, p, created.Job.ID)
	site := getSite(t, p, created.Site.ID)
	siteSh(t, siteProject, "mkdir -p "+wpContent+"/uploads/pf && echo "+restoreKeep+" > "+wpContent+"/uploads/pf/keep.txt")
	siteWP(t, siteProject, "option", "update", "pf_restore_marker", "before")
	snapshot := backupSite(t, p, site.ID)
	publicKey := siteWP(t, siteProject, "config", "get", "PRESSFLEET_LOGIN_PUBLIC_KEY")
	// The staging site is cloned from a fresh backup of the site, taken
	// after the one above; it has no backup of its own.
	stagingResp, err := p.Client.CreateStagingSiteWithResponse(ctx, site.ID, client.CreateStagingSiteJSONRequestBody{})
	if err != nil {
		t.Fatal(err)
	}
	if stagingResp.JSON202 == nil || stagingResp.JSON202.PreviewCredentials == nil {
		t.Fatalf("create staging site: status %d: %s", stagingResp.StatusCode(), stagingResp.Body)
	}
	staging, creds := stagingResp.JSON202.Site, *stagingResp.JSON202.PreviewCredentials
	stagingProject = compose.Project{Name: compose.SiteProjectName(staging.ID), Dir: filepath.Join(dataDir, "sites", staging.ID)}
	waitJob(t, p, stagingResp.JSON202.Job.ID)
	if n := backupCount(t, p, staging.ID); n != 0 {
		t.Fatalf("the staging site has %d backups of its own before the drill", n)
	}
	out, err := project.ExecService(ctx, e2e.Service, "pressfleet", "state", "backup")
	if err != nil {
		t.Fatalf("state backup: %v: %s", err, out)
	}
	t.Logf("%s", strings.TrimSpace(string(out)))

	// 2. Lose the host. Only the backup location and the master key
	// survive. The files the root control plane wrote are handed back to
	// the test user first, except the repositories, whose group the
	// backup tasks need.
	owner := fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid())
	if out, err := project.ExecService(ctx, e2e.Service, "sh", "-c",
		`find "$1" -mindepth 1 -maxdepth 1 ! -name backups -exec chown -R "$2" {} +`, "sh", dataDir, owner); err != nil {
		t.Fatalf("chown: %v: %s", err, out)
	}
	// The platform first: its services are attached to the site's network.
	if err := project.Down(ctx, compose.DownOptions{Volumes: true}); err != nil {
		t.Fatal(err)
	}
	if err := siteProject.Down(ctx, compose.DownOptions{Volumes: true}); err != nil {
		t.Fatal(err)
	}
	if err := stagingProject.Down(ctx, compose.DownOptions{Volumes: true}); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "backups" {
			if err := os.RemoveAll(filepath.Join(dataDir, e.Name())); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := os.Remove(configPath); err != nil {
		t.Fatal(err)
	}
	for _, proj := range []compose.Project{stagingProject, siteProject, project} {
		if res, err := proj.Resources(ctx); err != nil || !res.Empty() {
			t.Fatalf("project %s left resources behind: %+v, %v", proj.Name, res, err)
		}
	}

	// 3. The new host: init with the same answers, reusing the saved
	// master key (--force, since the key file is there), then recover.
	// The new admin password is replaced by the restored users.
	install(redact.Secret(strings.ToLower(rand.Text())), "--force")
	if code, out := pressfleet("", "recover", "--config", configPath); code != 0 ||
		!strings.Contains(out, "login key was restored and decrypts with the master key") ||
		!strings.Contains(out, "2 sites will be recovered") {
		t.Fatalf("recover exited %d: %s", code, out)
	}
	p.Connect(ctx, t)
	p.Client = p.Login(t, email, password)

	// 4. The site comes back from its backup.
	waitRecovered(ctx, t, p, site.ID)
	waitRecovered(ctx, t, p, staging.ID)
	if got := siteWP(t, siteProject, "option", "get", "pf_restore_marker"); got != "before" {
		t.Errorf("pf_restore_marker = %q", got)
	}
	if got := siteSh(t, siteProject, "cat "+wpContent+"/uploads/pf/keep.txt"); got != restoreKeep {
		t.Errorf("keep.txt = %q", got)
	}
	if got := siteWP(t, siteProject, "config", "get", "PRESSFLEET_LOGIN_PUBLIC_KEY"); got != publicKey {
		t.Errorf("the site's login public key changed: %q, was %q", got, publicKey)
	}
	list, err := p.Client.ListSiteBackupsWithResponse(ctx, site.ID, &client.ListSiteBackupsParams{})
	if err != nil || list.JSON200 == nil {
		t.Fatalf("the site's backups after recovery: %v %s", err, list.Body)
	}
	// Not necessarily the newest: creating the staging site took a sync
	// backup of the site after it.
	if !slices.ContainsFunc(list.JSON200.Items, func(b client.Backup) bool { return deref(b.SnapshotID) == snapshot }) {
		t.Errorf("the site's backups after recovery don't include snapshot %s: %s", snapshot, list.Body)
	}

	// The staging site has production's content at its own preview host,
	// and still no backup of its own.
	if got := siteWP(t, stagingProject, "option", "get", "pf_restore_marker"); got != "before" {
		t.Errorf("the staging site's pf_restore_marker = %q", got)
	}
	if got := siteSh(t, stagingProject, "cat "+wpContent+"/uploads/pf/keep.txt"); got != restoreKeep {
		t.Errorf("the staging site's keep.txt = %q", got)
	}
	stagingURL := "https://" + staging.PreviewDomain
	for _, option := range []string{"home", "siteurl"} {
		if got := siteWP(t, stagingProject, "option", "get", option); got != stagingURL {
			t.Errorf("the staging site's %s = %q, want %q", option, got, stagingURL)
		}
	}
	basic := http.Header{"Authorization": {"Basic " + base64.StdEncoding.EncodeToString([]byte(creds.Username+":"+creds.Password))}}
	if resp, body := waitIngress(t, p.IngressClient(t), stagingURL+"/", basic, http.StatusOK); strings.Contains(body, site.PreviewDomain) {
		t.Errorf("the staging site (%s) links to the production site's host %s", resp.Status, site.PreviewDomain)
	}
	if n := backupCount(t, p, staging.ID); n != 0 {
		t.Errorf("the staging site has %d backups of its own after recovery", n)
	}

	// 5. One-click login works with the restored key.
	resp, err := p.Client.CreateSiteAdminLoginWithResponse(ctx, site.ID, client.CreateSiteAdminLoginJSONRequestBody{})
	if err != nil || resp.JSON200 == nil {
		t.Fatalf("admin login: %v: %s", err, resp.Body)
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	b := p.IngressClient(t)
	b.Jar = jar
	login, err := b.PostForm(resp.JSON200.URL, url.Values{"token": {resp.JSON200.Token}})
	if err != nil {
		t.Fatal(err)
	}
	_ = login.Body.Close()
	if login.StatusCode != http.StatusFound || !strings.HasSuffix(login.Header.Get("Location"), "/wp-admin/") {
		t.Errorf("one-click login after recovery: %s, location %q", login.Status, login.Header.Get("Location"))
	}

	// 6. State backups go on, into the same repository.
	out, err = project.ExecService(ctx, e2e.Service, "pressfleet", "state", "backup")
	if err != nil {
		t.Fatalf("state backup after recovery: %v: %s", err, out)
	}
	out, err = project.ExecService(ctx, e2e.Service, "pressfleet", "state", "backups", "--json")
	if err != nil || strings.Count(string(out), `"id"`) < 2 {
		t.Errorf("state backups after recovery: %v: %s", err, out)
	}
}

// waitRecovered waits until the site's recover job finished and the site
// runs again.
func waitRecovered(ctx context.Context, t *testing.T, p *e2e.Platform, siteID string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(ctx, 2*jobTimeout)
	defer cancel()
	for {
		site := getSite(t, p, siteID)
		switch site.Status {
		case client.SiteStatusRunning:
			return
		case client.SiteStatusFailed:
			t.Fatalf("the site's recovery failed: %s", deref(site.StatusMessage))
		}
		select {
		case <-ctx.Done():
			t.Fatalf("the site is still %s: %s", site.Status, deref(site.StatusMessage))
		case <-time.After(3 * time.Second):
		}
	}
}

// backupCount returns how many backups the site has.
func backupCount(t *testing.T, p *e2e.Platform, siteID string) int {
	t.Helper()
	list, err := p.Client.ListSiteBackupsWithResponse(t.Context(), siteID, &client.ListSiteBackupsParams{})
	if err != nil || list.JSON200 == nil {
		t.Fatalf("list backups of %s: %v %s", siteID, err, list.Body)
	}
	return len(list.JSON200.Items)
}
