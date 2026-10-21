package e2e_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/api/client"
	"github.com/rchouinard/pressfleet/control-plane/internal/catalog"
	"github.com/rchouinard/pressfleet/control-plane/internal/e2e"
	"github.com/rchouinard/pressfleet/control-plane/internal/ingress"
	"github.com/rchouinard/pressfleet/control-plane/internal/platform"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner/compose"
	"github.com/rchouinard/pressfleet/control-plane/internal/sites"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
)

// jobTimeout bounds waiting for a site job. Provisioning pulls nothing (the
// images are ensured first) but waits for the containers' health checks.
const jobTimeout = 6 * time.Minute

// TestSiteCreateDelete automates the M1 "done when" (design doc §8 Phase 1,
// #32) and is the acceptance test of #27, #28, #29, and #31. On one
// platform, it:
//
//  1. creates two sites, each in its own client group, through the API and
//     waits for them to be provisioned;
//  2. checks that WordPress is installed and answers over HTTPS on its
//     preview domain, from the host through Traefik (#36), which reaches
//     the site's nginx over the site network;
//  3. checks that the two sites can't reach each other's containers by
//     name or IP, while each reaches the platform MariaDB and object cache;
//  4. changes a site's PHP version and stops and starts it (#28);
//  5. deletes both sites and checks that no containers, networks, volumes,
//     rendered files, databases, database users or grants, secrets, or
//     state rows are left.
func TestSiteCreateDelete(t *testing.T) {
	// A site's project directory belongs to the control plane, which runs
	// as root and creates it with mode 0700, so the test (an unprivileged
	// user in CI) can't read it, and `docker compose` fails when it can't
	// stat the directory's .env. The test addresses site projects by name
	// with this empty directory instead: ps and down work from the Compose
	// labels alone, and commands in containers go through ExecService
	// (`docker exec`). Created before the cleanup below, it is removed
	// after it.
	projectDir := t.TempDir()
	// Whatever happens below, don't leave the sites' containers, networks,
	// or volumes behind: they aren't part of the platform project that
	// Start tears down. Registered first, this runs last, after the
	// platform teardown has removed MariaDB, which would otherwise keep the
	// site networks in use.
	var siteProjects []compose.Project
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		for _, sp := range siteProjects {
			if res, err := sp.Resources(ctx); err == nil && !res.Empty() {
				t.Logf("removing leftover site project %s", sp.Name)
				if err := sp.Down(ctx, compose.DownOptions{Volumes: true}); err != nil {
					t.Errorf("removing leftover site project %s: %v", sp.Name, err)
				}
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
	// The first site moves to another PHP version later on.
	otherPHP := ""
	for _, v := range cat.AvailablePHPVersions(time.Now()) {
		if v.Version != php {
			otherPHP = v.Version
			break
		}
	}
	if otherPHP == "" {
		t.Fatal("the catalog offers only one PHP version")
	}
	otherImages, err := cat.Images(otherPHP, cat.LatestWordPress().Version)
	if err != nil {
		t.Fatal(err)
	}
	e2e.EnsureImages(t, images.PHP, images.Nginx, otherImages.PHP)

	// Both sites provision at the same time: their jobs have different
	// lock keys.
	groupID := createGroup(t, p, "E2E Agency")
	created := createSite(t, p, groupID, "E2E Site", php)
	other := createSite(t, p, createGroup(t, p, "E2E Other Agency"), "E2E Other Site", php)
	site := created.Site
	t.Logf("sites %s (%s) and %s (%s)", site.ID, site.PreviewDomain, other.Site.ID, other.Site.PreviewDomain)
	if site.Slug != "e2e-site" || site.PreviewDomain != "e2e-site."+e2e.PreviewBaseDomain || site.Status != client.SiteStatusProvisioning ||
		site.ClientGroupID != groupID {
		t.Errorf("created site = %+v", site)
	}
	siteProject := compose.Project{Name: compose.SiteProjectName(site.ID), Dir: projectDir}
	otherProject := compose.Project{Name: compose.SiteProjectName(other.Site.ID), Dir: projectDir}
	siteProjects = append(siteProjects, siteProject, otherProject)
	waitJob(t, p, created.Job.ID)
	waitJob(t, p, other.Job.ID)

	for _, id := range []string{site.ID, other.Site.ID} {
		if got := getSite(t, p, id); got.Status != client.SiteStatusRunning || !got.AdminCredentialsAvailable {
			t.Fatalf("site %s after provisioning: %+v", id, got)
		}
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
			out, err := siteProject.ExecService(ctx, "php", command...)
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

	hc := p.IngressClient(t)
	previewURL := "https://" + site.PreviewDomain

	// answers checks that WordPress answers on the site's preview domain
	// through Traefik: see step 2 in the test's documentation.
	answers := func(t *testing.T) {
		t.Helper()
		resp, home := waitIngress(t, hc, previewURL+"/", nil, http.StatusOK)
		if !strings.Contains(home, "<title>E2E Site") || !strings.Contains(home, "/wp-content/") {
			t.Errorf("front page:\n%.800s", home)
		}
		if resp.ProtoMajor != 2 {
			t.Errorf("protocol = %s, want HTTP/2", resp.Proto)
		}
		// Preview domains stay out of search engines (§5.1).
		if got := resp.Header.Get("X-Robots-Tag"); got != "noindex" {
			t.Errorf("X-Robots-Tag = %q, want noindex", got)
		}
		// WordPress only serves the login page over HTTPS. It serves it
		// here, so it believes Traefik (a trusted proxy) that the request
		// came over HTTPS.
		if _, login := waitIngress(t, hc, previewURL+"/wp-login.php", nil, http.StatusOK); !strings.Contains(login, `name="log"`) {
			t.Errorf("wp-login.php:\n%.800s", login)
		}
		resp, _ = ingressGet(t, hc, "http://"+site.PreviewDomain+"/about/?x=1", nil)
		if loc := resp.Header.Get("Location"); resp.StatusCode/100 != 3 || loc != previewURL+"/about/?x=1" {
			t.Errorf("HTTP: status %d, Location %q; want a redirect to HTTPS", resp.StatusCode, loc)
		}
	}
	t.Run("WordPress is reached over HTTPS on its preview domain", answers)

	// Traefik reaches nginx over the site network (#36), and nginx takes
	// the client's address from Traefik's X-Forwarded-For, which Traefik
	// sets itself rather than passing on the client's. So nginx's access
	// log, and its login rate limit (§6.1), see the client rather than
	// Traefik, and a client can't pick an address.
	t.Run("nginx sees the client's address", func(t *testing.T) {
		traefik, err := p.Project.ServiceContainer(ctx, ingress.TraefikService)
		if err != nil {
			t.Fatal(err)
		}
		traefikIP := traefik.Networks[siteProject.Name]
		if traefikIP == "" {
			t.Fatalf("Traefik isn't on %s: %v", siteProject.Name, traefik.Networks)
		}
		marker := "pf-client-" + strings.ToLower(rand.Text()[:8])
		forged := http.Header{"X-Forwarded-For": {"203.0.113.9"}}
		waitIngress(t, hc, previewURL+"/?"+marker, forged, http.StatusOK)
		logs, err := siteProject.Logs(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var remote string
		for line := range strings.Lines(string(logs)) {
			i := strings.Index(line, "{")
			if !strings.Contains(line, marker) || i < 0 {
				continue
			}
			var entry struct {
				RemoteAddr string `json:"remote_addr"`
			}
			if err := json.Unmarshal([]byte(line[i:]), &entry); err != nil {
				t.Fatalf("access log line %q: %v", line, err)
			}
			remote = entry.RemoteAddr
		}
		t.Logf("nginx logged the client as %s; Traefik is %s", remote, traefikIP)
		if remote == "" || remote == traefikIP || remote == "203.0.113.9" {
			t.Errorf("nginx logged remote_addr %q; want the client, not Traefik (%s) or the forged address", remote, traefikIP)
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

	t.Run("site databases exist", func(t *testing.T) {
		for _, id := range []string{site.ID, other.Site.ID} {
			if n := siteDatabase(t, p, id); n != (dbObjects{databases: 1, users: 1, grants: 1}) {
				t.Errorf("site %s: %+v; want a database, a user, and its grant", id, n)
			}
		}
	})

	// Each site has its own object cache user (#38). What those users can
	// reach is tested against the real server in objectcache/redis.
	t.Run("object cache users exist", func(t *testing.T) {
		users := cacheUsers(t, p)
		for _, id := range []string{site.ID, other.Site.ID} {
			if !slices.Contains(users, "pf_"+id) {
				t.Errorf("site %s has no object cache user: %v", id, users)
			}
		}
	})

	// Provisioning installs the Redis Object Cache plugin from the site
	// image, configured with the site's own user and database, and enables
	// its drop-in (#39).
	t.Run("the object cache is connected", func(t *testing.T) {
		for _, sp := range []compose.Project{siteProject, otherProject} {
			out, err := sp.ExecService(ctx, "php", "wp", "redis", "status")
			if err != nil {
				t.Fatalf("%s: wp redis status: %v", sp.Name, err)
			}
			status := string(out)
			for _, want := range []string{"Status: Connected", "Drop-in: Valid", "Client: PhpRedis", `WP_REDIS_PASSWORD: ["pf_`} {
				if !strings.Contains(status, want) {
					t.Errorf("%s: wp redis status lacks %q:\n%s", sp.Name, want, status)
				}
			}
			if strings.Contains(status, "WP_REDIS_DATABASE: 0") {
				t.Errorf("%s uses database 0:\n%s", sp.Name, status)
			}
		}
	})

	// The default plugin stack (#43): installed from the site image,
	// active, and updating itself. Two-factor is off by default, so no Two
	// Factor plugin.
	t.Run("the default plugins are active", func(t *testing.T) {
		out, err := siteProject.ExecService(ctx, "php", "wp", "plugin", "list", "--format=json",
			"--fields=name,status,auto_update", "--skip-update-check")
		if err != nil {
			t.Fatal(err)
		}
		var list []struct {
			Name       string `json:"name"`
			Status     string `json:"status"`
			AutoUpdate string `json:"auto_update"`
		}
		if err := json.Unmarshal(out, &list); err != nil {
			t.Fatalf("plugin list %s: %v", out, err)
		}
		got := map[string]string{}
		for _, pl := range list {
			got[pl.Name] = pl.Status + "/" + pl.AutoUpdate
		}
		for _, slug := range []string{sites.PluginNginxHelper, sites.PluginRedisCache} {
			if got[slug] != "active/on" {
				t.Errorf("%s is %q, want active with auto-updates on", slug, got[slug])
			}
		}
		if s, ok := got[sites.PluginTwoFactor]; ok {
			t.Errorf("%s is installed (%s) although two-factor is off", sites.PluginTwoFactor, s)
		}
	})

	// Page cache purging (#41). Requests come from a client container on
	// the site network, so nginx trusts their X-Forwarded-Proto and caches
	// them under the site's https URL, as behind Traefik.
	xCache := func(t *testing.T) string {
		t.Helper()
		resp := fetch(t, images.PHP, site, "/", "X-Forwarded-Proto: https")
		for line := range strings.Lines(resp) {
			if name, value, ok := strings.Cut(strings.TrimSpace(line), ":"); ok && strings.EqualFold(name, "X-Cache") {
				return strings.TrimSpace(value)
			}
		}
		return ""
	}
	cached := func(t *testing.T) {
		t.Helper()
		xCache(t)
		if got := xCache(t); got != "HIT" {
			t.Fatalf("home page X-Cache = %q, want HIT", got)
		}
	}
	siteExec := func(t *testing.T, command ...string) (string, error) {
		t.Helper()
		out, err := siteProject.ExecService(ctx, "php", command...)
		return strings.TrimSpace(string(out)), err
	}

	t.Run("publishing a post purges the pages it affects", func(t *testing.T) {
		settings, err := siteExec(t, "wp", "option", "get", "rt_wp_nginx_helper_options", "--format=json")
		if err != nil || !strings.Contains(settings, `"purge_method":"unlink_files"`) || !strings.Contains(settings, `"enable_purge":1`) {
			t.Errorf("Nginx Helper settings = %s (%v)", settings, err)
		}
		cached(t)
		if _, err := siteExec(t, "wp", "post", "create", "--post_title=E2E news", "--post_status=publish"); err != nil {
			t.Fatal(err)
		}
		if got := xCache(t); got != "MISS" {
			t.Errorf("home page X-Cache after publishing = %q, want MISS", got)
		}
	})

	t.Run("the cache purge API clears both caches", func(t *testing.T) {
		if _, err := siteExec(t, "wp", "cache", "set", "e2e-probe", "cached"); err != nil {
			t.Fatal(err)
		}
		if got, err := siteExec(t, "wp", "cache", "get", "e2e-probe"); err != nil || got != "cached" {
			t.Fatalf("object cache before the purge: %q, %v", got, err)
		}
		cached(t)
		resp, err := p.Client.PurgeSiteCacheWithResponse(ctx, site.ID)
		if err != nil {
			t.Fatal(err)
		}
		if resp.JSON202 == nil {
			t.Fatalf("purge: status %d: %s", resp.StatusCode(), resp.Body)
		}
		waitJob(t, p, resp.JSON202.Job.ID)
		if got, err := siteExec(t, "wp", "cache", "get", "e2e-probe"); err == nil {
			t.Errorf("object cache entry survived the purge: %q", got)
		}
		if got := xCache(t); got != "MISS" {
			t.Errorf("home page X-Cache after the purge = %q, want MISS", got)
		}
	})

	// Two sites can't reach each other over the network (§6): a site's
	// containers are only on its own network, and from one site's php
	// container the other site's containers neither resolve by name nor
	// answer on their IPs, while its own nginx and the platform MariaDB and
	// object cache (connected to every site network under their aliases)
	// do. Both directions are checked.
	// Traefik is on every site network so that it can reach the site's
	// nginx, but its internal entry point (/metrics, /ping) names every
	// site and its traffic, so it listens on loopback only (security review
	// F5). A site's php container can't connect to it, by name or by
	// Traefik's address on the site's network, while the same probe still
	// reaches the shared services next to it. The control plane's collector
	// keeps working: see TestIngress.
	t.Run("sites can't reach Traefik's internal port", func(t *testing.T) {
		traefik, err := p.Project.ServiceContainer(t.Context(), ingress.TraefikService)
		if err != nil {
			t.Fatal(err)
		}
		traefikIP := traefik.Networks[siteProject.Name]
		if traefikIP == "" {
			t.Fatalf("Traefik isn't on %s: %v", siteProject.Name, traefik.Networks)
		}
		internal := ":" + strconv.Itoa(ingress.InternalPort)
		results := probe(t, siteProject, []string{
			compose.MariaDBAlias + ":3306", // a positive control
			compose.TraefikAlias + internal,
			traefikIP + internal,
		})
		for i, r := range results {
			t.Logf("%s open=%v ip=%q %s", r.target, r.open, r.ip, r.detail)
			if want := i == 0; r.open != want {
				t.Errorf("%s open = %v, want %v (%s)", r.target, r.open, want, r.detail)
			}
		}
	})

	t.Run("sites can't reach each other", func(t *testing.T) {
		a := siteNetwork(t, siteProject)
		b := siteNetwork(t, otherProject)
		for _, dir := range []struct{ from, to siteNet }{{a, b}, {b, a}} {
			results := probe(t, dir.from.project, []string{
				// Positive controls: the probe works.
				"nginx:8080",
				dir.from.ips["nginx"] + ":8080",
				compose.MariaDBAlias + ":3306",
				compose.RedisAlias + ":6379",
				// The other site, by container name and by IP.
				dir.to.names["nginx"] + ":8080",
				dir.to.names["php"] + ":9000",
				dir.to.ips["nginx"] + ":8080",
				dir.to.ips["php"] + ":9000",
			})
			from := dir.from.project.Name
			for i, r := range results {
				t.Logf("from %s: %s open=%v ip=%q %s", from, r.target, r.open, r.ip, r.detail)
				if want := i < 4; r.open != want {
					t.Errorf("from %s: %s open = %v, want %v (%s)", from, r.target, r.open, want, r.detail)
				}
			}
			// The service name resolves to the site's own nginx.
			if results[0].ip != dir.from.ips["nginx"] {
				t.Errorf("from %s: nginx resolves to %q, want its own nginx %s", from, results[0].ip, dir.from.ips["nginx"])
			}
			for _, r := range results[4:6] {
				if r.ip != "" {
					t.Errorf("from %s: %s resolves to %s", from, r.target, r.ip)
				}
			}
		}
	})

	t.Run("the site is listed and has a live status", func(t *testing.T) {
		list, err := p.Client.ListSitesWithResponse(ctx, &client.ListSitesParams{ClientGroupID: &groupID})
		if err != nil {
			t.Fatal(err)
		}
		if list.JSON200 == nil || len(list.JSON200.Items) != 1 || list.JSON200.Items[0].ID != site.ID {
			t.Fatalf("list: status %d: %s", list.StatusCode(), list.Body)
		}
		if got := getSite(t, p, site.ID); got.Runtime == nil || got.Runtime.Phase != client.SiteRuntimePhaseRunning {
			t.Errorf("runtime = %+v", got.Runtime)
		}
	})

	// #28's acceptance criterion: changing the PHP version recreates the
	// php container on the new image; nginx keeps running.
	t.Run("changing the PHP version recreates php on the new image", func(t *testing.T) {
		phpBefore, err := siteProject.ServiceContainer(ctx, "php")
		if err != nil {
			t.Fatal(err)
		}
		nginxBefore, err := siteProject.ServiceContainer(ctx, "nginx")
		if err != nil {
			t.Fatal(err)
		}
		if !e2e.RunsImage(t, phpBefore, images.PHP) {
			t.Fatalf("php runs %s, want %s", phpBefore.Image, images.PHP)
		}
		resp, err := p.Client.UpdateSiteWithResponse(ctx, site.ID, client.UpdateSiteJSONRequestBody{PhpVersion: &otherPHP})
		if err != nil {
			t.Fatal(err)
		}
		if resp.JSON202 == nil {
			t.Fatalf("update site: status %d: %s", resp.StatusCode(), resp.Body)
		}
		if s := resp.JSON202.Site; s.Status != client.SiteStatusUpdating || s.PhpVersion != otherPHP {
			t.Errorf("accepted site = %s", resp.Body)
		}
		waitJob(t, p, resp.JSON202.Job.ID)

		phpAfter, err := siteProject.ServiceContainer(ctx, "php")
		if err != nil {
			t.Fatal(err)
		}
		if phpAfter.ID == phpBefore.ID || !e2e.RunsImage(t, phpAfter, otherImages.PHP) || !phpAfter.Running {
			t.Errorf("php after the update = %+v; want a new, running container of %s", phpAfter, otherImages.PHP)
		}
		if nginxAfter, err := siteProject.ServiceContainer(ctx, "nginx"); err != nil || nginxAfter.ID != nginxBefore.ID {
			t.Errorf("nginx after the update = %+v, %v; want it untouched", nginxAfter, err)
		}
		out, err := siteProject.ExecService(ctx, "php", "php", "-r", `echo PHP_MAJOR_VERSION, ".", PHP_MINOR_VERSION;`)
		if err != nil || string(out) != otherPHP {
			t.Errorf("PHP version in the container = %q, %v; want %s", out, err, otherPHP)
		}
		if got := getSite(t, p, site.ID); got.Status != client.SiteStatusRunning || got.PhpVersion != otherPHP {
			t.Errorf("site after the update: status %s, PHP %s", got.Status, got.PhpVersion)
		}
		answers(t)
	})

	t.Run("stop and start", func(t *testing.T) {
		stop, err := p.Client.StopSiteWithResponse(ctx, site.ID)
		if err != nil {
			t.Fatal(err)
		}
		if stop.JSON202 == nil {
			t.Fatalf("stop: status %d: %s", stop.StatusCode(), stop.Body)
		}
		waitJob(t, p, stop.JSON202.Job.ID)
		got := getSite(t, p, site.ID)
		if got.Status != client.SiteStatusStopped || got.Runtime == nil || got.Runtime.Phase != client.SiteRuntimePhaseStopped {
			t.Errorf("after stop: status %s, runtime %+v", got.Status, got.Runtime)
		}
		if c, err := siteProject.ServiceContainer(ctx, "php"); err != nil || c.Running {
			t.Errorf("php after stop = %+v, %v", c, err)
		}

		start, err := p.Client.StartSiteWithResponse(ctx, site.ID)
		if err != nil {
			t.Fatal(err)
		}
		if start.JSON202 == nil {
			t.Fatalf("start: status %d: %s", start.StatusCode(), start.Body)
		}
		waitJob(t, p, start.JSON202.Job.ID)
		got = getSite(t, p, site.ID)
		if got.Status != client.SiteStatusRunning || got.Runtime == nil || got.Runtime.Phase != client.SiteRuntimePhaseRunning {
			t.Errorf("after start: status %s, runtime %+v", got.Status, got.Runtime)
		}
		answers(t)
	})

	// Delete both sites; their deletions run at the same time.
	var deletions []string
	for _, id := range []string{site.ID, other.Site.ID} {
		deleted, err := p.Client.DeleteSiteWithResponse(ctx, id, nil)
		if err != nil {
			t.Fatal(err)
		}
		if deleted.JSON202 == nil {
			t.Fatalf("delete site %s: status %d: %s", id, deleted.StatusCode(), deleted.Body)
		}
		deletions = append(deletions, deleted.JSON202.Job.ID)
	}
	for _, jobID := range deletions {
		waitJob(t, p, jobID)
	}

	t.Run("sites are gone from the API", func(t *testing.T) {
		for _, id := range []string{site.ID, other.Site.ID} {
			got, err := p.Client.GetSiteWithResponse(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if got.StatusCode() != http.StatusNotFound {
				t.Errorf("get deleted site %s: status %d: %s", id, got.StatusCode(), got.Body)
			}
		}
	})

	t.Run("no containers, networks, volumes, or files are left", func(t *testing.T) {
		for _, sp := range siteProjects {
			res, err := sp.Resources(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if !res.Empty() {
				t.Errorf("site project %s left resources behind: %+v", sp.Name, res)
			}
		}
		// The test created the sites directory, so it can see into it.
		entries, err := os.ReadDir(filepath.Join(p.DataDir, "sites"))
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			t.Fatal(err)
		}
		for _, e := range entries {
			t.Errorf("left in the sites directory: %s", e.Name())
		}
		// The platform services are on no site network any more.
		for _, service := range []string{"mariadb", "redis", ingress.TraefikService} {
			c, err := p.Project.ServiceContainer(ctx, service)
			if err != nil {
				t.Fatal(err)
			}
			for _, sp := range siteProjects {
				if _, ok := c.Networks[sp.Name]; ok {
					t.Errorf("%s is still on %s", service, sp.Name)
				}
			}
		}
	})

	t.Run("no database, user, or grant is left", func(t *testing.T) {
		for _, id := range []string{site.ID, other.Site.ID} {
			if n := siteDatabase(t, p, id); n != (dbObjects{}) {
				t.Errorf("site %s left %+v on MariaDB", id, n)
			}
		}
	})

	t.Run("no object cache user is left", func(t *testing.T) {
		for _, u := range cacheUsers(t, p) {
			if strings.HasPrefix(u, "pf_") {
				t.Errorf("object cache user %s is left", u)
			}
		}
	})

	// Last, because it stops the platform: the state database belongs to
	// the control plane, so it is read once the control plane has stopped.
	t.Run("no secrets or state are left", func(t *testing.T) {
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
		// The platform's own keys (platform_keys) outlive every site.
		for _, table := range []string{"secrets WHERE id NOT IN (SELECT secret_ref FROM platform_keys)", "site_databases", "site_object_caches", "sites", "site_aliases", "schedules WHERE target_type = 'site'"} {
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

// createGroup creates a client group through the API and returns its ID.
func createGroup(t *testing.T, p *e2e.Platform, name string) string {
	t.Helper()
	resp, err := p.Client.CreateClientGroupWithResponse(t.Context(), client.CreateClientGroupJSONRequestBody{Name: name})
	if err != nil {
		t.Fatal(err)
	}
	if resp.JSON201 == nil {
		t.Fatalf("create client group: status %d: %s", resp.StatusCode(), resp.Body)
	}
	return resp.JSON201.ID
}

// createSite starts provisioning a site through the API.
func createSite(t *testing.T, p *e2e.Platform, groupID, name, php string) client.SiteAccepted {
	t.Helper()
	resp, err := p.Client.CreateSiteWithResponse(t.Context(), client.CreateSiteJSONRequestBody{
		ClientGroupID: groupID,
		Name:          name,
		AdminEmail:    "owner@e2e.test",
		PhpVersion:    &php,
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.JSON202 == nil {
		t.Fatalf("create site %q: status %d: %s", name, resp.StatusCode(), resp.Body)
	}
	return *resp.JSON202
}

// fetch gets a page of the site from a throwaway client container on the
// site's network, the way Traefik reaches it, and returns the response
// headers and body. The client is curl in the site's own php image, which
// is already on the host. headers are extra request headers. Transient
// failures (nginx re-resolving a recreated php) are retried for a while.
func fetch(t *testing.T, image string, site client.Site, path string, headers ...string) string {
	t.Helper()
	var (
		out []byte
		err error
	)
	command := []string{"--silent", "--show-error", "--max-time", "10", "--include", "--header", "Host: " + site.PreviewDomain}
	for _, h := range headers {
		command = append(command, "--header", h)
	}
	command = append(command, "http://nginx:8080"+path)
	for deadline := time.Now().Add(30 * time.Second); ; {
		out, err = compose.RunContainer(t.Context(), nil, compose.RunOptions{
			Image:      image,
			Network:    compose.SiteProjectName(site.ID),
			Entrypoint: "curl",
			Command:    command,
		})
		if err == nil && !strings.HasPrefix(string(out), "HTTP/1.1 5") || time.Now().After(deadline) {
			break
		}
		time.Sleep(time.Second)
	}
	if err != nil {
		t.Fatalf("fetching %s from %s: %v", path, site.PreviewDomain, err)
	}
	status, _, _ := strings.Cut(string(out), "\n")
	t.Logf("GET %s: %s", path, strings.TrimSpace(status))
	return string(out)
}

// siteNet is what the isolation check knows about a site's containers.
type siteNet struct {
	project compose.Project
	names   map[string]string // service -> container name
	ips     map[string]string // service -> IP on the site network
}

// siteNetwork inspects a site's containers and checks that each is on the
// site's network and no other.
func siteNetwork(t *testing.T, project compose.Project) siteNet {
	t.Helper()
	n := siteNet{project: project, names: map[string]string{}, ips: map[string]string{}}
	for _, service := range []string{"nginx", "php"} {
		c, err := project.ServiceContainer(t.Context(), service)
		if err != nil {
			t.Fatal(err)
		}
		if networks := slices.Collect(maps.Keys(c.Networks)); len(networks) != 1 || c.Networks[project.Name] == "" {
			t.Errorf("%s is on networks %v, want only %s", c.Name, networks, project.Name)
		}
		n.names[service], n.ips[service] = c.Name, c.Networks[project.Name]
	}
	return n
}

// probeResult is the outcome of one TCP connection attempt.
type probeResult struct {
	target string // host:port
	ip     string // what the host resolved to; empty if it didn't
	open   bool
	detail string
}

// probeScript tries a TCP connection to each host:port argument and prints
// one line per target: "<target> <open|closed> <ip or -> <detail>". A host
// that doesn't resolve is closed without trying; a blocked connection
// times out after 2 seconds.
const probeScript = `foreach (array_slice($argv, 1) as $target) {
	$start = microtime(true);
	$i = strrpos($target, ":");
	$ip = gethostbyname(substr($target, 0, $i));
	if (!filter_var($ip, FILTER_VALIDATE_IP)) { printf("%s closed - unresolved (%.1fs)\n", $target, microtime(true) - $start); continue; }
	$fp = @fsockopen($ip, (int) substr($target, $i + 1), $errno, $errstr, 2);
	if ($fp) { fclose($fp); printf("%s open %s\n", $target, $ip); continue; }
	printf("%s closed %s %s (%.1fs)\n", $target, $ip, $errstr, microtime(true) - $start);
}`

// probe runs probeScript in a site's php container.
func probe(t *testing.T, project compose.Project, targets []string) []probeResult {
	t.Helper()
	out, err := project.ExecService(t.Context(), "php", append([]string{"php", "-r", probeScript, "--"}, targets...)...)
	if err != nil {
		t.Fatalf("probing from %s: %v", project.Name, err)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) != len(targets) {
		t.Fatalf("probe output from %s:\n%s", project.Name, out)
	}
	results := make([]probeResult, len(lines))
	for i, line := range lines {
		f := strings.SplitN(line, " ", 4)
		if len(f) < 3 || f[0] != targets[i] {
			t.Fatalf("probe output line %q for %s", line, targets[i])
		}
		results[i] = probeResult{target: f[0], open: f[1] == "open"}
		if f[2] != "-" {
			results[i].ip = f[2]
		}
		if len(f) == 4 {
			results[i].detail = f[3]
		}
	}
	return results
}

// getSite gets a site through the API, failing the test unless it exists.
func getSite(t *testing.T, p *e2e.Platform, siteID string) client.Site {
	t.Helper()
	resp, err := p.Client.GetSiteWithResponse(t.Context(), siteID)
	if err != nil {
		t.Fatal(err)
	}
	if resp.JSON200 == nil {
		t.Fatalf("get site %s: status %d: %s", siteID, resp.StatusCode(), resp.Body)
	}
	return *resp.JSON200
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

// cacheUsers lists the object cache's ACL users, as the admin. The
// password, which the harness rendered into the data directory, goes to
// valkey-cli over stdin, so it never reaches a command line.
func cacheUsers(t *testing.T, p *e2e.Platform) []string {
	t.Helper()
	pw, err := os.ReadFile(filepath.Join(p.DataDir, "platform", platform.RedisAdminPasswordSecret))
	if err != nil {
		t.Fatal(err)
	}
	out, err := p.Project.ExecInput(t.Context(), bytes.NewReader(append(pw, '\n')), "redis",
		"valkey-cli", "--no-auth-warning", "--user", platform.RedisAdminUser, "--askpass", "-e", "ACL", "USERS")
	if err != nil {
		t.Fatalf("listing object cache users: %v", err)
	}
	users := strings.Fields(string(out))
	if !slices.Contains(users, platform.RedisAdminUser) {
		t.Fatalf("unexpected ACL USERS output %q", out)
	}
	return users
}

// dbObjects counts a site's objects on the platform MariaDB.
type dbObjects struct {
	databases, users, grants int
}

// siteDatabase counts the site's database, database user, and database
// grants on the platform MariaDB, as root. The password is read inside the
// container, so it never reaches the test's command lines.
func siteDatabase(t *testing.T, p *e2e.Platform, siteID string) dbObjects {
	t.Helper()
	name := "pf_" + siteID
	query := fmt.Sprintf("SELECT COUNT(*) FROM information_schema.SCHEMATA WHERE SCHEMA_NAME = '%[1]s'; "+
		"SELECT COUNT(*) FROM mysql.user WHERE User = '%[1]s'; "+
		"SELECT COUNT(*) FROM mysql.db WHERE Db = '%[1]s' OR User = '%[1]s';", name)
	out, err := p.Project.Exec(t.Context(), "mariadb", "sh", "-c",
		`exec mariadb --user=root --password="$(cat /run/secrets/mariadb_root_password)" --batch --skip-column-names -e "$1"`, "sh", query)
	if err != nil {
		t.Fatalf("querying MariaDB: %v", err)
	}
	var n dbObjects
	if _, err := fmt.Sscan(string(out), &n.databases, &n.users, &n.grants); err != nil {
		t.Fatalf("parsing MariaDB output %q: %v", out, err)
	}
	return n
}
