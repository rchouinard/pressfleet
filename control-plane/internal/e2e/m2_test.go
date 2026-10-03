package e2e_test

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/api/client"
	"github.com/rchouinard/pressfleet/control-plane/internal/catalog"
	"github.com/rchouinard/pressfleet/control-plane/internal/e2e"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner/compose"
)

// certWait bounds how long a domain may take to answer over HTTPS with a
// certificate from Pebble, counted from the end of the job that routed it
// (M2's "within seconds of provisioning"). Traefik requests the
// certificate as soon as it sees the route, often before the job ends, and
// Pebble issues it in about a second; the rest is headroom for slow CI
// runners.
const certWait = 30 * time.Second

// Domains the M2 acceptance test takes its site live on. The platform's
// DNS server (challtestsrv) resolves them to Traefik.
const (
	m2Primary = "www.m2.e2e.test"
	m2Alias   = "m2.e2e.test"
)

// TestM2Acceptance automates the M2 "done when" (design doc §8 Phase 2,
// #48): a new site is reachable over HTTPS on its preview domain within
// seconds of provisioning, go-live switches it to a custom domain with a
// valid certificate, and one site can't read or flush another site's
// object cache. On one platform whose certificates come from Pebble
// (e2e.StartWithPebble), with two sites, it checks:
//
//  1. each site's preview domain answers 200 over HTTPS with a certificate
//     for the domain that chains to Pebble's root, within certWait of its
//     provisioning job's end;
//  2. the page cache: anonymous pages are HITs through Traefik, and
//     logged-in visitors, query strings, the login page, and the REST API
//     bypass it;
//  3. from each site's php container, with its own object cache
//     credentials, the other site's keys can't be read, its database can't
//     be selected, moved to, swapped, or flushed, and SCAN lists no foreign
//     key; flushing a site's cache, from inside or through the purge API,
//     leaves the other site's entries;
//  4. the security set's defaults (#108, #109) through Traefik: user
//     enumeration is blocked, security headers are sent, and WordPress
//     rejects weak passwords;
//  5. preview password protection (#107): 401 without credentials, 200
//     with them;
//  6. go-live passes the DNS check for real (without skip_dns_check) and
//     the primary domain and the alias each get a valid certificate within
//     certWait;
//  7. wp-login.php is rate limited (last: it uses up the host's allowance).
//
// Other end-to-end tests go deeper into each feature on a platform without
// a certificate authority: TestSitePreview, TestSiteGoLive,
// TestSecuritySetReachesExistingSites, and TestSiteCreateDelete (page
// cache purging, network isolation).
func TestM2Acceptance(t *testing.T) {
	// See TestSiteCreateDelete for why site projects use an empty directory
	// and are cleaned up here.
	projectDir := t.TempDir()
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
	p := e2e.StartWithPebble(t)
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

	// Both sites provision at the same time.
	createdA := createSite(t, p, createGroup(t, p, "M2 Agency A"), "M2 Site A", php)
	createdB := createSite(t, p, createGroup(t, p, "M2 Agency B"), "M2 Site B", php)
	a, b := createdA.Site, createdB.Site
	projectA := compose.Project{Name: compose.SiteProjectName(a.ID), Dir: projectDir}
	projectB := compose.Project{Name: compose.SiteProjectName(b.ID), Dir: projectDir}
	siteProjects = append(siteProjects, projectA, projectB)
	t.Logf("sites %s (%s) and %s (%s)", a.ID, a.PreviewDomain, b.ID, b.PreviewDomain)
	provisioned := map[string]time.Time{
		a.ID: finishedJob(t, p, createdA.Job.ID),
		b.ID: finishedJob(t, p, createdB.Job.ID),
	}

	hc := p.VerifyingIngressClient(t)
	previewA := "https://" + a.PreviewDomain

	t.Run("new sites answer over HTTPS with a valid certificate within seconds", func(t *testing.T) {
		for _, site := range []client.Site{a, b} {
			t.Run(site.Slug, func(t *testing.T) {
				// In parallel, so one site's wait doesn't count against the
				// other's.
				t.Parallel()
				_, home := waitCertified(t, hc, "https://"+site.PreviewDomain+"/", nil, http.StatusOK, provisioned[site.ID])
				if !strings.Contains(home, "<title>"+site.Name) {
					t.Errorf("front page:\n%.500s", home)
				}
			})
		}
	})

	t.Run("the page cache serves anonymous visitors and bypasses the rest", func(t *testing.T) {
		xCache := func(t *testing.T, path string, header http.Header) string {
			t.Helper()
			resp, _ := ingressGet(t, hc, previewA+path, header)
			if resp.StatusCode != http.StatusOK {
				t.Errorf("GET %s: status %d", path, resp.StatusCode)
			}
			return resp.Header.Get("X-Cache")
		}
		xCache(t, "/", nil)
		if got := xCache(t, "/", nil); got != "HIT" {
			t.Errorf("anonymous home page: X-Cache %q, want HIT", got)
		}
		loggedIn := http.Header{"Cookie": {"wordpress_logged_in_0123456789abcdef0123456789abcdef=admin%7C1%7Cx"}}
		for _, tc := range []struct {
			name, path string
			header     http.Header
		}{
			{"logged-in visitor", "/", loggedIn},
			{"query string", "/?s=hello", nil},
			{"login page", "/wp-login.php", nil},
			{"REST API path", "/wp-json/", nil},
		} {
			if got := xCache(t, tc.path, tc.header); got != "BYPASS" {
				t.Errorf("%s (%s): X-Cache %q, want BYPASS", tc.name, tc.path, got)
			}
		}
	})

	t.Run("a site can't read or flush another site's object cache", func(t *testing.T) {
		for _, dir := range []struct {
			from, to     compose.Project
			fromID, toID string
		}{
			{projectA, projectB, a.ID, b.ID},
			{projectB, projectA, b.ID, a.ID},
		} {
			t.Run(dir.from.Name+" to "+dir.to.Name, func(t *testing.T) {
				checkCacheIsolation(t, dir.from, dir.to, dir.fromID, dir.toID)
			})
		}
		// The platform's cache purge flushes one site's cache only.
		wpB := func(args ...string) (string, error) {
			out, err := projectB.ExecService(ctx, "php", append([]string{"wp"}, args...)...)
			return strings.TrimSpace(string(out)), err
		}
		if _, err := wpB("cache", "set", "m2-purge-probe", "kept"); err != nil {
			t.Fatal(err)
		}
		resp, err := p.Client.PurgeSiteCacheWithResponse(ctx, a.ID)
		if err != nil {
			t.Fatal(err)
		}
		if resp.JSON202 == nil {
			t.Fatalf("purge: status %d: %s", resp.StatusCode(), resp.Body)
		}
		waitJob(t, p, resp.JSON202.Job.ID)
		if got, err := wpB("cache", "get", "m2-purge-probe"); err != nil || got != "kept" {
			t.Errorf("site B's entry after purging site A's cache: %q, %v", got, err)
		}
	})

	t.Run("the security set's defaults apply", func(t *testing.T) {
		// Sites use plain permalinks, so the REST API and the sitemaps are
		// at their query string URLs; /wp-json/... and /wp-sitemap-*.xml
		// don't route to them.
		t.Run("user enumeration is blocked", func(t *testing.T) {
			for path, want := range map[string]int{
				"/?author=1":                  http.StatusNotFound,
				"/?rest_route=/wp/v2/users":   http.StatusUnauthorized,
				"/?rest_route=/wp/v2/users/1": http.StatusUnauthorized,
				"/?sitemap=users":             http.StatusNotFound,
			} {
				if resp, body := ingressGet(t, hc, previewA+path, nil); resp.StatusCode != want {
					t.Errorf("%s: status %d, want %d:\n%.300s", path, resp.StatusCode, want, body)
				}
			}
			// The REST API itself works for anonymous visitors.
			if resp, body := ingressGet(t, hc, previewA+"/?rest_route=/wp/v2/posts", nil); resp.StatusCode != http.StatusOK || !strings.HasPrefix(body, "[") {
				t.Errorf("anonymous posts list: status %d:\n%.300s", resp.StatusCode, body)
			}
		})
		t.Run("security headers are sent", func(t *testing.T) {
			for _, path := range []string{"/", "/wp-login.php", "/?author=1"} {
				resp, _ := ingressGet(t, hc, previewA+path, nil)
				for name, want := range map[string]string{
					"Referrer-Policy":        "strict-origin-when-cross-origin",
					"X-Frame-Options":        "SAMEORIGIN",
					"X-Content-Type-Options": "nosniff",
					"Permissions-Policy":     "camera=(), microphone=(), usb=()",
				} {
					if got := resp.Header.Values(name); len(got) != 1 || !strings.EqualFold(got[0], want) {
						t.Errorf("%s: %s = %q, want %q once", path, name, got, want)
					}
				}
			}
		})
		t.Run("weak passwords are rejected", func(t *testing.T) {
			for password, why := range map[string]string{
				"Sh0rt-pw!":    "at least 12 characters",
				"000000000000": "most commonly used",
			} {
				out, err := projectA.ExecService(ctx, "php", "wp", "user", "create", "weakling", "weakling@e2e.test", "--user_pass="+password)
				if err == nil || !strings.Contains(err.Error()+string(out), why) {
					t.Errorf("creating a user with password %q: %v %s; want refused with %q", password, err, out, why)
				}
			}
			if _, err := projectA.ExecService(ctx, "php", "wp", "user", "get", "weakling"); err == nil {
				t.Error("the user with a weak password exists")
			}
		})
	})

	t.Run("preview password protection", func(t *testing.T) {
		previewB := "https://" + b.PreviewDomain + "/"
		protect := func(t *testing.T, on bool) *client.PreviewCredentials {
			t.Helper()
			resp, err := p.Client.UpdateSiteWithResponse(ctx, b.ID, client.UpdateSiteJSONRequestBody{
				PreviewProtection: &client.PreviewProtectionUpdate{Enabled: &on},
			})
			if err != nil {
				t.Fatal(err)
			}
			if resp.JSON202 == nil {
				t.Fatalf("update site: status %d: %s", resp.StatusCode(), resp.Body)
			}
			waitJob(t, p, resp.JSON202.Job.ID)
			return resp.JSON202.PreviewCredentials
		}
		creds := protect(t, true)
		if creds == nil {
			t.Fatal("enabling protection returned no credentials")
		}
		basic := func(password string) http.Header {
			return http.Header{"Authorization": {"Basic " + base64.StdEncoding.EncodeToString([]byte(creds.Username+":"+password))}}
		}
		waitIngress(t, hc, previewB, nil, http.StatusUnauthorized)
		if resp, _ := ingressGet(t, hc, previewB, basic("wrong")); resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("wrong password: status %d, want 401", resp.StatusCode)
		}
		if resp, body := ingressGet(t, hc, previewB, basic(creds.Password)); resp.StatusCode != http.StatusOK || !strings.Contains(body, "<title>"+b.Name) {
			t.Errorf("with the credentials: status %d:\n%.300s", resp.StatusCode, body)
		}
		protect(t, false)
		waitIngress(t, hc, previewB, nil, http.StatusOK)
	})

	t.Run("go-live gets valid certificates for the custom domains", func(t *testing.T) {
		// No skip_dns_check: the domains resolve to Traefik, and the check
		// knows it's this host (public_ip).
		resp, err := p.Client.GoLiveSiteWithResponse(ctx, a.ID, client.GoLiveSiteJSONRequestBody{
			PrimaryDomain: m2Primary,
			Aliases:       &[]string{m2Alias},
		})
		if err != nil {
			t.Fatal(err)
		}
		if resp.JSON202 == nil {
			t.Fatalf("go-live: status %d: %s", resp.StatusCode(), resp.Body)
		}
		live := finishedJob(t, p, resp.JSON202.Job.ID)

		res, home := waitCertified(t, hc, "https://"+m2Primary+"/", nil, http.StatusOK, live)
		if !strings.Contains(home, "https://"+m2Primary+"/") || !strings.Contains(home, "<title>"+a.Name) {
			t.Errorf("the primary domain's home page:\n%.1000s", home)
		}
		if got := res.Header.Get("Strict-Transport-Security"); got == "" {
			t.Error("the primary domain sends no Strict-Transport-Security")
		}
		res, _ = waitCertified(t, hc, "https://"+m2Alias+"/x?y=z", nil, http.StatusMovedPermanently, live)
		if loc := res.Header.Get("Location"); loc != "https://"+m2Primary+"/x?y=z" {
			t.Errorf("the alias redirects to %q", loc)
		}
		// Each alias has a certificate of its own.
		if names := res.TLS.PeerCertificates[0].DNSNames; len(names) != 1 || names[0] != m2Alias {
			t.Errorf("the alias's certificate is for %v", names)
		}
		res, _ = ingressGet(t, hc, previewA+"/", nil)
		if res.StatusCode != http.StatusMovedPermanently || res.Header.Get("Location") != "https://"+m2Primary+"/" {
			t.Errorf("the preview domain: status %d, Location %q", res.StatusCode, res.Header.Get("Location"))
		}
		if got := getSite(t, p, a.ID); got.URL == nil || *got.URL != "https://"+m2Primary {
			t.Errorf("site after go-live: %+v", got)
		}
	})

	// Last: nginx keys the limit on the client's address, and every request
	// the test sends comes from the same one.
	t.Run("wp-login.php is rate limited", func(t *testing.T) {
		login := "https://" + m2Primary + "/wp-login.php"
		var codes []int
		for range 20 {
			resp, _ := ingressGet(t, hc, login, nil)
			codes = append(codes, resp.StatusCode)
		}
		t.Logf("wp-login.php: %v", codes)
		if codes[0] != http.StatusOK || codes[len(codes)-1] != http.StatusTooManyRequests {
			t.Errorf("20 requests in a row got %v; want 200 first and 429 once the burst is used up", codes)
		}
		if resp, _ := ingressGet(t, hc, "https://"+m2Primary+"/", nil); resp.StatusCode != http.StatusOK {
			t.Errorf("the home page after the login limit: status %d", resp.StatusCode)
		}
	})

	var deletions []string
	for _, id := range []string{a.ID, b.ID} {
		deleted, err := p.Client.DeleteSiteWithResponse(ctx, id, nil)
		if err != nil {
			t.Fatal(err)
		}
		if deleted.JSON202 == nil {
			t.Fatalf("delete site %s: status %d: %s", id, deleted.StatusCode(), deleted.Body)
		}
		deletions = append(deletions, deleted.JSON202.Job.ID)
	}
	for _, id := range deletions {
		waitJob(t, p, id)
	}
	for _, sp := range siteProjects {
		if res, err := sp.Resources(ctx); err != nil || !res.Empty() {
			t.Errorf("site project %s left %+v (%v)", sp.Name, res, err)
		}
	}
}

// finishedJob waits for a job to succeed and returns when it finished, by
// the control plane's clock (the host's).
func finishedJob(t *testing.T, p *e2e.Platform, jobID string) time.Time {
	t.Helper()
	waitJob(t, p, jobID)
	resp, err := p.Client.GetJobWithResponse(t.Context(), jobID)
	if err != nil {
		t.Fatal(err)
	}
	if resp.JSON200 == nil || resp.JSON200.FinishedAt == nil {
		t.Fatalf("job %s: status %d: %s", jobID, resp.StatusCode(), resp.Body)
	}
	return *resp.JSON200.FinishedAt
}

// waitCertified repeats a GET through Traefik with a client that verifies
// certificates (e2e.Platform.VerifyingIngressClient) until it answers with
// the wanted status, and fails the test unless that happens within
// certWait of since. Until Traefik has the domain's certificate from
// Pebble, it serves its default one, which fails verification.
func waitCertified(t *testing.T, hc *http.Client, url string, header http.Header, want int, since time.Time) (*http.Response, string) {
	t.Helper()
	var last string
	for {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
		if err != nil {
			t.Fatal(err)
		}
		for k, v := range header {
			req.Header[k] = v
		}
		resp, err := hc.Do(req)
		if err == nil {
			body, readErr := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			switch {
			case readErr != nil:
				last = readErr.Error()
			case resp.StatusCode == want:
				leaf := resp.TLS.PeerCertificates[0]
				t.Logf("GET %s: %s %s after the job ended, with a certificate for %v issued by %q",
					url, resp.Status, time.Since(since).Round(100*time.Millisecond), leaf.DNSNames, leaf.Issuer.CommonName)
				if len(resp.TLS.VerifiedChains) == 0 {
					t.Errorf("GET %s: the certificate wasn't verified", url)
				}
				return resp, string(body)
			default:
				last = fmt.Sprintf("status %d", resp.StatusCode)
			}
		} else {
			last = err.Error()
		}
		if time.Since(since) > certWait {
			t.Fatalf("GET %s: no %d with a valid certificate within %s of the job's end; last: %s", url, want, certWait, last)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// cacheConnect connects to the object cache the way the Redis Object Cache
// plugin does, with the site's own credentials from wp-config.php.
const cacheConnect = `$r = new Redis();
$r->connect(WP_REDIS_HOST, WP_REDIS_PORT, 2);
if (!$r->auth(WP_REDIS_PASSWORD) || !$r->select(WP_REDIS_DATABASE)) { fwrite(STDERR, "can't connect\n"); exit(1); }
`

// cacheKeys prints the site's object cache database, then the names of its
// keys that match the pattern, one per line.
const cacheKeys = cacheConnect + `echo WP_REDIS_DATABASE, "\n";
$c = "0";
do {
	[$c, $keys] = $r->rawCommand("SCAN", $c, "MATCH", "*%s*", "COUNT", "1000");
	foreach ($keys as $k) { echo $k, "\n"; }
} while ($c !== "0");
`

// cacheAttack tries, with the site's own credentials, to reach another
// site's key (%[2]s) and database (%[1]d). It prints one line per command:
// "<name> ok <JSON result>" or "<name> err <error>".
const cacheAttack = cacheConnect + `$other_db = %[1]d; $other_key = '%[2]s';
$try = function ($name, ...$cmd) use ($r) {
	$r->clearLastError();
	try {
		$res = $r->rawCommand(...$cmd);
	} catch (RedisException $e) {
		printf("%%s err %%s\n", $name, $e->getMessage());
		return;
	}
	if ($res === false && $r->getLastError()) {
		printf("%%s err %%s\n", $name, $r->getLastError());
		return;
	}
	printf("%%s ok %%s\n", $name, json_encode($res));
};
$mine = WP_REDIS_PREFIX . "m2-attack";
$r->set($mine, "x");
$try("get", "GET", $other_key);
$try("del", "DEL", $other_key);
$try("select", "SELECT", $other_db);
$try("move", "MOVE", $mine, $other_db);
$try("copy", "COPY", $mine, $mine, "DB", $other_db);
$try("swapdb", "SWAPDB", WP_REDIS_DATABASE, $other_db);
$try("flushall", "FLUSHALL");
$try("keys", "KEYS", "*");
$try("scan", "SCAN", "0", "COUNT", "10000");
$try("flushdb", "FLUSHDB");
`

// checkCacheIsolation checks, from one site's php container, that the site
// can't reach the other site's object cache entry, which the other site
// sets and reads back through WordPress.
func checkCacheIsolation(t *testing.T, from, to compose.Project, fromID, toID string) {
	t.Helper()
	ctx := t.Context()
	value := "secret of " + toID
	if out, err := to.ExecService(ctx, "php", "wp", "cache", "set", "m2-secret", value); err != nil {
		t.Fatalf("wp cache set: %v: %s", err, out)
	}
	out, err := to.ExecService(ctx, "php", "wp", "eval", fmt.Sprintf(cacheKeys, "m2-secret"))
	if err != nil {
		t.Fatalf("listing %s's keys: %v: %s", to.Name, err, out)
	}
	lines := strings.Fields(string(out))
	if len(lines) != 2 || !strings.HasPrefix(lines[1], toID+":") {
		t.Fatalf("%s's database and keys: %q; want its database and one key under %s:", to.Name, lines, toID)
	}
	otherDB, otherKey := lines[0], lines[1]
	var db int
	if _, err := fmt.Sscan(otherDB, &db); err != nil {
		t.Fatal(err)
	}
	out, err = from.ExecService(ctx, "php", "wp", "eval", fmt.Sprintf(cacheAttack, db, otherKey))
	if err != nil {
		t.Fatalf("probing from %s: %v: %s", from.Name, err, out)
	}
	results := map[string]string{}
	for line := range strings.Lines(strings.TrimSpace(string(out))) {
		name, result, _ := strings.Cut(strings.TrimSpace(line), " ")
		results[name] = result
		t.Logf("%s: %s", name, result)
	}
	for _, name := range []string{"get", "del", "select", "move", "copy", "swapdb", "flushall", "keys"} {
		if r := results[name]; !strings.HasPrefix(r, "err NOPERM") {
			t.Errorf("%s from %s: %q, want refused (NOPERM)", name, from.Name, r)
		}
	}
	if r := results["scan"]; !strings.HasPrefix(r, "ok ") || strings.Contains(r, toID) || !strings.Contains(r, fromID+":") {
		t.Errorf("SCAN from %s: %q; want its own keys only", from.Name, r)
	}
	if r := results["flushdb"]; r != `ok true` {
		t.Errorf("FLUSHDB of its own database from %s: %q", from.Name, r)
	}
	// Flushing through WordPress, as the plugin does.
	if out, err := from.ExecService(ctx, "php", "wp", "cache", "flush"); err != nil {
		t.Errorf("wp cache flush in %s: %v: %s", from.Name, err, out)
	}
	if got, err := to.ExecService(ctx, "php", "wp", "cache", "get", "m2-secret"); err != nil || strings.TrimSpace(string(got)) != value {
		t.Errorf("%s's entry after %s's attempts: %q, %v; want %q", to.Name, from.Name, got, err, value)
	}
}
