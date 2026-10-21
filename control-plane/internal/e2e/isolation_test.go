package e2e_test

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/catalog"
	"github.com/rchouinard/pressfleet/control-plane/internal/e2e"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner/compose"
)

// TestTenantIsolation checks, on running containers, the parts of the
// design doc §6 tenant isolation that other end-to-end tests don't (#97):
//
//  1. UID separation: each site's nginx and php run as one non-root UID
//     from the site range, a different one per site, and wp-config.php,
//     which holds the site's credentials, is readable by that UID only and
//     isn't in nginx at all;
//  2. open_basedir: PHP served through nginx and PHP-FPM, as visitors
//     reach it, can't read outside its own files;
//  3. MariaDB grants: with its own credentials from wp-config.php, a site
//     sees and reaches only its own database, and has no global
//     privileges (mysql.user, CREATE DATABASE, FILE).
//
// Network isolation is TestSiteCreateDelete's, the object cache's
// TestM2Acceptance's and the objectcache/redis Docker test's, and the
// socket proxy's TestIngress's.
func TestTenantIsolation(t *testing.T) {
	// See TestSiteCreateDelete for why site projects use an empty directory
	// and are cleaned up here.
	projectDir := t.TempDir()
	var siteProjects []compose.Project
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		for _, sp := range siteProjects {
			if res, err := sp.Resources(ctx); err == nil && !res.Empty() {
				if err := sp.Down(ctx, compose.DownOptions{Volumes: true}); err != nil {
					t.Errorf("removing site project %s: %v", sp.Name, err)
				}
			}
		}
	})
	p := e2e.Start(t)

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

	a := createSite(t, p, createGroup(t, p, "Isolation A"), "Isolation A", php)
	b := createSite(t, p, createGroup(t, p, "Isolation B"), "Isolation B", php)
	projA := compose.Project{Name: compose.SiteProjectName(a.Site.ID), Dir: projectDir}
	projB := compose.Project{Name: compose.SiteProjectName(b.Site.ID), Dir: projectDir}
	siteProjects = append(siteProjects, projA, projB)
	waitJob(t, p, a.Job.ID)
	waitJob(t, p, b.Job.ID)

	t.Run("each site runs as its own UID", func(t *testing.T) {
		uids := map[string]int{}
		for _, proj := range []compose.Project{projA, projB} {
			uid := siteUID(t, proj, "php")
			if got := siteUID(t, proj, "nginx"); got != uid {
				t.Errorf("%s: nginx runs as UID %d, php as %d; want the same", proj.Name, got, uid)
			}
			if uid < provisioner.MinSiteUID || uid > provisioner.MaxSiteUID {
				t.Errorf("%s runs as UID %d, outside the site range %d-%d", proj.Name, uid, provisioner.MinSiteUID, provisioner.MaxSiteUID)
			}
			for other, otherUID := range uids {
				if otherUID == uid {
					t.Errorf("%s and %s both run as UID %d", proj.Name, other, uid)
				}
			}
			uids[proj.Name] = uid

			// Owner and mode, as the php container sees the bind mount.
			if got, want := siteSh(t, proj, "stat -c '%u %a' /var/www/wordpress/wp-config.php"), fmt.Sprintf("%d 400", uid); got != want {
				t.Errorf("%s: wp-config.php owner and mode %q, want %q", proj.Name, got, want)
			}
			if out, err := proj.ExecService(t.Context(), "nginx", "test", "-e", "/var/www/wordpress/wp-config.php"); err == nil {
				t.Errorf("%s: wp-config.php exists in nginx: %s", proj.Name, out)
			}
		}
	})

	t.Run("PHP served by FPM is confined by open_basedir", func(t *testing.T) {
		// A file in wp-content's root (not uploads, which refuses PHP), so
		// nginx passes it to FPM like any plugin file. Each line is
		// "<check> <result>".
		const script = `<?php
header('Content-Type: text/plain');
echo "open_basedir ", ini_get('open_basedir'), "\n";
echo "etc ", var_export(@file_get_contents('/etc/hostname') !== false, true), "\n";
echo "proc ", var_export(@file_get_contents('/proc/self/environ') !== false, true), "\n";
echo "root ", var_export(@scandir('/') !== false, true), "\n";
echo "own ", var_export(@file_get_contents(__DIR__ . '/../index.php') !== false, true), "\n";
`
		siteSh(t, projA, "cat > /var/www/wordpress/wp-content/pf-isolation.php <<'EOF'\n"+script+"EOF\n")
		resp := fetch(t, images.PHP, a.Site, "/wp-content/pf-isolation.php")
		_, body, _ := strings.Cut(resp, "\r\n\r\n")
		got := map[string]string{}
		for line := range strings.Lines(body) {
			k, v, _ := strings.Cut(strings.TrimSpace(line), " ")
			got[k] = v
		}
		want := map[string]string{
			"open_basedir": "/var/www/wordpress:/opt/pressfleet/mu-plugins:/var/cache/pressfleet/fastcgi:/tmp",
			"etc":          "false",
			"proc":         "false",
			"root":         "false",
			// The positive control: the probe reads its own site's files.
			"own": "true",
		}
		for k, v := range want {
			if got[k] != v {
				t.Errorf("%s = %q, want %q (response:\n%s)", k, got[k], v, resp)
			}
		}
	})

	t.Run("a site's database user reaches only its own database", func(t *testing.T) {
		results := map[string]string{}
		out, err := projA.ExecService(t.Context(), "php", "wp", "eval", fmt.Sprintf(dbAttack, "pf_"+b.Site.ID))
		if err != nil {
			t.Fatalf("probing MariaDB from %s: %v: %s", projA.Name, err, out)
		}
		for line := range strings.Lines(strings.TrimSpace(string(out))) {
			name, result, _ := strings.Cut(strings.TrimSpace(line), " ")
			results[name] = result
			t.Logf("%s: %s", name, result)
		}
		if got, want := results["databases"], "ok information_schema,pf_"+a.Site.ID; got != want {
			t.Errorf("SHOW DATABASES = %q, want %q", got, want)
		}
		for _, name := range []string{"other_site", "mysql_user", "create_database"} {
			if r := results[name]; !strings.HasPrefix(r, "err ") || !strings.Contains(r, "denied") {
				t.Errorf("%s: %q, want access denied", name, r)
			}
		}
		// LOAD_FILE needs the global FILE privilege; without it MariaDB
		// returns NULL rather than an error.
		if r := results["load_file"]; r != "ok NULL" {
			t.Errorf("LOAD_FILE: %q, want NULL", r)
		}
	})
}

// siteUID returns the UID a site container's processes run as.
func siteUID(t *testing.T, proj compose.Project, service string) int {
	t.Helper()
	out, err := proj.ExecService(t.Context(), service, "id", "-u")
	if err != nil {
		t.Fatalf("id -u in %s %s: %v: %s", proj.Name, service, err, out)
	}
	uid, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		t.Fatalf("id -u in %s %s: %q", proj.Name, service, out)
	}
	return uid
}

// dbAttack connects to MariaDB with the site's own credentials, as
// WordPress does (DB_HOST, DB_USER, DB_PASSWORD from wp-config.php), and
// tries to reach beyond its database, here %[1]s. It prints one line per
// query: "<name> ok <rows, comma-separated>" or "<name> err <error>".
const dbAttack = `mysqli_report(MYSQLI_REPORT_OFF);
[$host, $port] = array_pad(explode(':', DB_HOST, 2), 2, '3306');
$m = mysqli_init();
if (!$m->real_connect($host, DB_USER, DB_PASSWORD, null, (int) $port)) { fwrite(STDERR, "can't connect\n"); exit(1); }
$try = function ($name, $sql) use ($m) {
	$r = $m->query($sql);
	if ($r === false) { printf("%%s err %%s\n", $name, $m->error); return; }
	if ($r === true) { printf("%%s ok\n", $name); return; }
	$vals = [];
	while ($row = $r->fetch_row()) { $vals[] = $row[0] ?? 'NULL'; }
	printf("%%s ok %%s\n", $name, implode(',', $vals));
};
$try("databases", "SHOW DATABASES");
$try("other_site", "SELECT COUNT(*) FROM %[1]s.wp_options");
$try("mysql_user", "SELECT User FROM mysql.user");
$try("create_database", "CREATE DATABASE pf_isolation_probe");
$try("load_file", "SELECT LOAD_FILE('/etc/passwd')");
`
