package e2e_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/api"
	"github.com/rchouinard/pressfleet/control-plane/internal/api/client"
	"github.com/rchouinard/pressfleet/control-plane/internal/catalog"
	"github.com/rchouinard/pressfleet/control-plane/internal/e2e"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner/compose"
	"github.com/rchouinard/pressfleet/control-plane/internal/redact"
)

// loginRefusal is the one error the site's login module shows for every
// failure (images/shared/mu-plugins/pressfleet-login/admin-login.php).
const loginRefusal = "The login link is not valid"

// tokenShape matches a v1 login token, which no audit event may hold.
var tokenShape = regexp.MustCompile(`v1\.[A-Za-z0-9_-]{20,}`)

// TestAdminLogin checks the one-click WordPress admin login (#127 to #129,
// design doc §5.9) against real site containers, through Traefik, the way a
// browser does it: the token from POST /api/v1/sites/{id}/admin-login is
// submitted as a form field to the site's login URL, with a cookie jar. On
// one platform and three sites (two provisioned, one imported), it checks:
//
//  1. a token logs in as the site's default administrator and reaches
//     /wp-admin/;
//  2. replaying a token, and using one after it expired, are refused;
//  3. a token for one site is refused by another;
//  4. a Viewer and a read-only token are denied, and every issued token is
//     an audit event that names the WordPress user and the jti, never the
//     token;
//  5. an imported site has no default administrator, so the call returns
//     the "choose a user" 409, and logging in as an administrator from the
//     list works;
//  6. after a control plane restart that also requires two-factor, the
//     reconcile job puts the wp-config.php constants back into a site that
//     lacks them (as sites provisioned before #127 do), and a dashboard
//     login reaches wp-admin although the administrator has to enroll a
//     second factor for a password login.
func TestAdminLogin(t *testing.T) {
	projectDir := t.TempDir()
	var projects []compose.Project
	// Registered before Start, so it runs after the platform teardown: see
	// TestSiteCreateDelete.
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		for _, proj := range projects {
			if res, err := proj.Resources(ctx); err == nil && !res.Empty() {
				t.Logf("removing leftover site project %s", proj.Name)
				if err := proj.Down(ctx, compose.DownOptions{Volumes: true}); err != nil {
					t.Errorf("removing leftover site project %s: %v", proj.Name, err)
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
	e2e.EnsureImages(t, images.PHP, images.Nginx)
	groupID := createGroup(t, p, "E2E Login Agency")

	project := func(siteID string) compose.Project {
		proj := compose.Project{Name: compose.SiteProjectName(siteID), Dir: projectDir}
		projects = append(projects, proj)
		return proj
	}
	wp := func(t *testing.T, proj compose.Project, args ...string) string {
		t.Helper()
		out, err := proj.ExecService(ctx, "php", append([]string{"wp"}, args...)...)
		if err != nil {
			t.Fatalf("wp %s: %v: %s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}
	provision := func(name string) (client.Site, compose.Project) {
		created := createSite(t, p, groupID, name, php)
		proj := project(created.Site.ID)
		waitJob(t, p, created.Job.ID)
		return getSite(t, p, created.Site.ID), proj
	}
	siteA, projA := provision("E2E Login A")
	siteB, _ := provision("E2E Login B")
	defaultAdmin := wp(t, projA, "user", "list", "--role=administrator", "--field=user_login")

	hc := p.IngressClient(t)
	issue := func(t *testing.T, c *client.ClientWithResponses, siteID, user string) client.AdminLogin {
		t.Helper()
		body := client.CreateSiteAdminLoginJSONRequestBody{}
		if user != "" {
			body.User = &user
		}
		resp, err := c.CreateSiteAdminLoginWithResponse(ctx, siteID, body)
		if err != nil {
			t.Fatal(err)
		}
		if resp.JSON200 == nil {
			t.Fatalf("admin login for %s: status %d: %s", siteID, resp.StatusCode(), resp.Body)
		}
		if got := resp.HTTPResponse.Header.Get("Cache-Control"); got != "no-store" {
			t.Errorf("Cache-Control = %q, want no-store", got)
		}
		return *resp.JSON200
	}
	// A fresh browser: its own cookie jar.
	browser := func(t *testing.T) *http.Client {
		t.Helper()
		jar, err := cookiejar.New(nil)
		if err != nil {
			t.Fatal(err)
		}
		b := p.IngressClient(t)
		b.Jar = jar
		return b
	}
	// submit POSTs the token as the form field the dashboard sends.
	submit := func(t *testing.T, b *http.Client, loginURL, token string) (*http.Response, string) {
		t.Helper()
		resp, err := b.PostForm(loginURL, url.Values{"token": {token}})
		if err != nil {
			t.Fatalf("POST %s: %v", loginURL, err)
		}
		defer func() { _ = resp.Body.Close() }()
		var buf bytes.Buffer
		if _, err := buf.ReadFrom(resp.Body); err != nil {
			t.Fatal(err)
		}
		return resp, buf.String()
	}
	hasAuthCookie := func(b *http.Client, site client.Site) bool {
		u, _ := url.Parse("https://" + site.PreviewDomain + "/wp-admin/")
		for _, c := range b.Jar.Cookies(u) {
			if strings.HasPrefix(c.Name, "wordpress_logged_in_") {
				return true
			}
		}
		return false
	}
	// loggedInAs reads the account's profile page, which only an
	// administrator's session reaches, and returns the user it shows.
	userLogin := regexp.MustCompile(`id="user_login" value="([^"]*)"`)
	loggedInAs := func(t *testing.T, b *http.Client, site client.Site) string {
		t.Helper()
		resp, body := ingressGet(t, b, "https://"+site.PreviewDomain+"/wp-admin/profile.php", nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET wp-admin/profile.php: %s, location %q:\n%.500s", resp.Status, resp.Header.Get("Location"), body)
		}
		m := userLogin.FindStringSubmatch(body)
		if m == nil {
			t.Fatalf("wp-admin/profile.php shows no user_login:\n%.1000s", body)
		}
		return m[1]
	}
	// login submits a token and expects to land in wp-admin as want.
	login := func(t *testing.T, b *http.Client, site client.Site, l client.AdminLogin, want string) {
		t.Helper()
		wantURL := "https://" + site.PreviewDomain + "/wp-login.php?action=pressfleet-login"
		if l.URL != wantURL {
			t.Errorf("login url = %q, want %q", l.URL, wantURL)
		}
		resp, body := submit(t, b, l.URL, l.Token)
		if resp.StatusCode != http.StatusFound || !strings.HasSuffix(resp.Header.Get("Location"), "/wp-admin/") {
			t.Fatalf("login: %s, location %q:\n%.500s", resp.Status, resp.Header.Get("Location"), body)
		}
		if got := loggedInAs(t, b, site); got != want {
			t.Errorf("logged in as %q, want %q", got, want)
		}
		if resp, body := ingressGet(t, b, "https://"+site.PreviewDomain+"/wp-admin/", nil); resp.StatusCode != http.StatusOK || !strings.Contains(body, "Dashboard") {
			t.Errorf("GET wp-admin/: %s (location %q)", resp.Status, resp.Header.Get("Location"))
		}
	}
	// refused expects the one generic error and no session.
	refused := func(t *testing.T, site client.Site, loginURL, token string) {
		t.Helper()
		b := browser(t)
		resp, body := submit(t, b, loginURL, token)
		if resp.StatusCode != http.StatusForbidden || !strings.Contains(body, loginRefusal) {
			t.Errorf("refused login: %s:\n%.500s", resp.Status, body)
		}
		if strings.Contains(body, token) {
			t.Error("the error page repeats the token")
		}
		if hasAuthCookie(b, site) {
			t.Error("a refused login set a WordPress session")
		}
		if resp, _ := ingressGet(t, b, "https://"+site.PreviewDomain+"/wp-admin/", nil); resp.StatusCode != http.StatusFound ||
			!strings.Contains(resp.Header.Get("Location"), "wp-login.php") {
			t.Errorf("wp-admin/ after a refused login: %s, location %q", resp.Status, resp.Header.Get("Location"))
		}
	}
	jtiOf := func(t *testing.T, token string) string {
		t.Helper()
		parts := strings.Split(token, ".")
		if len(parts) != 3 || parts[0] != "v1" {
			t.Fatalf("token has an unexpected shape: %d parts", len(parts))
		}
		raw, err := base64.RawURLEncoding.DecodeString(parts[1])
		if err != nil {
			t.Fatal(err)
		}
		var claims struct {
			Site string `json:"site"`
			User string `json:"user"`
			JTI  string `json:"jti"`
		}
		if err := json.Unmarshal(raw, &claims); err != nil {
			t.Fatal(err)
		}
		return claims.JTI
	}
	waitIngress(t, hc, "https://"+siteA.PreviewDomain+"/wp-login.php", nil, http.StatusOK)
	waitIngress(t, hc, "https://"+siteB.PreviewDomain+"/wp-login.php", nil, http.StatusOK)

	// Issued now, used after the other checks, once it has expired.
	expiring := issue(t, p.Client, siteA.ID, "")

	var replayJTI string
	t.Run("default administrator", func(t *testing.T) {
		l := issue(t, p.Client, siteA.ID, "")
		if l.User != defaultAdmin {
			t.Errorf("token user = %q, want the default administrator %q", l.User, defaultAdmin)
		}
		if ttl := time.Until(l.ExpiresAt); ttl <= 0 || ttl > time.Minute {
			t.Errorf("token expires in %s, want within a minute", ttl)
		}
		if resp, err := p.Client.ListSiteAdministratorsWithResponse(ctx, siteA.ID); err != nil || resp.JSON200 == nil {
			t.Errorf("list administrators: %v", err)
		} else if len(resp.JSON200.Items) != 1 || resp.JSON200.Items[0].Login != defaultAdmin || !resp.JSON200.Items[0].Default {
			t.Errorf("administrators = %+v, want only the default %q", resp.JSON200.Items, defaultAdmin)
		}
		b := browser(t)
		login(t, b, siteA, l, defaultAdmin)
		replayJTI = jtiOf(t, l.Token)

		// The same token again, from another browser, is refused, and the
		// first session is unaffected.
		refused(t, siteA, l.URL, l.Token)
		if got := loggedInAs(t, b, siteA); got != defaultAdmin {
			t.Errorf("first session after the replay: %q", got)
		}
		// An unknown or mangled token is refused the same way.
		refused(t, siteA, l.URL, "v1.not-a-token.not-a-signature")
		fresh := issue(t, p.Client, siteA.ID, "")
		refused(t, siteA, l.URL, fresh.Token[:len(fresh.Token)-4]+"AAAA")
		// The mangled attempt didn't use up the real token.
		login(t, browser(t), siteA, fresh, defaultAdmin)
	})

	t.Run("other site", func(t *testing.T) {
		forA := issue(t, p.Client, siteA.ID, "")
		forB := issue(t, p.Client, siteB.ID, "")
		refused(t, siteB, forB.URL, forA.Token)
		refused(t, siteA, forA.URL, forB.Token)
		// Neither refusal used the tokens up: each works on its own site.
		login(t, browser(t), siteA, forA, defaultAdmin)
		login(t, browser(t), siteB, forB, wp(t, project(siteB.ID), "user", "list", "--role=administrator", "--field=user_login"))
	})

	t.Run("access and audit", func(t *testing.T) {
		viewer := newUser(t, p, "login-viewer@e2e.test", client.Viewer)
		me, err := viewer.GetCurrentUserWithResponse(ctx)
		if err != nil || me.JSON200 == nil {
			t.Fatalf("viewer: %v", err)
		}
		readOnly, err := p.Client.CreateAPITokenWithResponse(ctx, client.CreateAPITokenJSONRequestBody{
			Name: "login-read-only", Scope: client.TokenScopeReadOnly,
		})
		if err != nil || readOnly.JSON201 == nil {
			t.Fatalf("read-only token: %v", err)
		}
		for name, c := range map[string]*client.ClientWithResponses{
			"viewer":          viewer,
			"read-only token": p.BearerClient(t, redact.Secret(readOnly.JSON201.Token)),
		} {
			resp, err := c.CreateSiteAdminLoginWithResponse(ctx, siteA.ID, client.CreateSiteAdminLoginJSONRequestBody{})
			if err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode() != http.StatusForbidden || resp.JSON200 != nil {
				t.Errorf("%s: admin login status %d, want 403: %s", name, resp.StatusCode(), resp.Body)
			}
			list, err := c.ListSiteAdministratorsWithResponse(ctx, siteA.ID)
			if err != nil {
				t.Fatal(err)
			}
			if list.StatusCode() != http.StatusForbidden || list.JSON200 != nil {
				t.Errorf("%s: administrators status %d, want 403: %s", name, list.StatusCode(), list.Body)
			}
		}

		events := auditEvents(t, p, "site.admin_login")
		var issued, denied int
		var sawReplay bool
		for _, ev := range events {
			if ev.TargetID == nil || ev.TargetType == nil || *ev.TargetType != "site" {
				t.Errorf("audit event without a site target: %+v", ev)
				continue
			}
			switch ev.Result {
			case client.Success:
				issued++
				if ev.Details == nil || (*ev.Details)["wp_user"] == "" || len((*ev.Details)["jti"]) != 32 {
					t.Errorf("issued token without the WordPress user and jti: %+v", ev)
				}
				if ev.Details != nil && (*ev.Details)["jti"] == replayJTI {
					sawReplay = true
				}
				if ev.ActorUserID == nil || *ev.ActorUserID == me.JSON200.ID {
					t.Errorf("issued token attributed to %v", ev.ActorUserID)
				}
			case client.Denied:
				if ev.ActorUserID != nil && *ev.ActorUserID == me.JSON200.ID {
					denied++
				}
			}
			raw, err := json.Marshal(ev)
			if err != nil {
				t.Fatal(err)
			}
			if tokenShape.Match(raw) {
				t.Errorf("an audit event holds something shaped like a token: %s", raw)
			}
		}
		if !sawReplay {
			t.Errorf("no audit event for the token with jti %s among %d", replayJTI, len(events))
		}
		// Tokens for A (the expiring one, the default, the mangled attempt's
		// fresh one, and the one of the other-site check) and for B.
		if issued < 5 {
			t.Errorf("%d issued-token audit events, want at least 5", issued)
		}
		if denied < 2 {
			t.Errorf("%d denial audit events for the viewer, want at least 2 (login and administrators)", denied)
		}
	})

	t.Run("imported site", func(t *testing.T) {
		// A's content and database make a source the platform didn't
		// create, so the imported site has no default administrator.
		const content = "/var/www/wordpress"
		archive, err := projA.ExecService(ctx, "php", "tar", "-czf", "-", "-C", content, "wp-content")
		if err != nil {
			t.Fatalf("archiving wp-content: %v", err)
		}
		dump, err := p.Project.Exec(ctx, "mariadb", "sh", "-c",
			`exec mariadb-dump --user=root --password="$(cat /run/secrets/mariadb_root_password)" --single-transaction --hex-blob --databases "$1"`,
			"sh", "pf_"+siteA.ID)
		if err != nil {
			t.Fatalf("dumping the source database: %v", err)
		}
		resp, err := p.Client.ImportSiteWithResponse(ctx, client.ImportSiteJSONRequestBody{
			ClientGroupID:   groupID,
			ArchiveUploadID: uploadSiteImport(t, p, groupID, archive),
			DumpUploadID:    uploadSiteImport(t, p, groupID, dump),
			PhpVersion:      &php,
		})
		if err != nil || resp.JSON202 == nil {
			t.Fatalf("import: %v: %s", err, resp.Body)
		}
		project(resp.JSON202.SiteID)
		waitJob(t, p, resp.JSON202.Job.ID)
		site := getSite(t, p, resp.JSON202.SiteID)
		waitIngress(t, hc, "https://"+site.PreviewDomain+"/wp-login.php", nil, http.StatusOK)

		got, err := p.Client.CreateSiteAdminLoginWithResponse(ctx, site.ID, client.CreateSiteAdminLoginJSONRequestBody{})
		if err != nil {
			t.Fatal(err)
		}
		if got.StatusCode() != http.StatusConflict || got.ApplicationProblemJSON409 == nil || got.ApplicationProblemJSON409.Type != api.ChooseUserProblemType {
			t.Fatalf("login without a user: status %d, want the choose-user 409: %s", got.StatusCode(), got.Body)
		}
		if !strings.HasSuffix(got.ApplicationProblemJSON409.Type, "#59-dashboard-login-to-wordpress") {
			t.Errorf("problem type = %q", got.ApplicationProblemJSON409.Type)
		}

		list, err := p.Client.ListSiteAdministratorsWithResponse(ctx, site.ID)
		if err != nil || list.JSON200 == nil {
			t.Fatalf("list administrators: %v: %s", err, list.Body)
		}
		var logins []string
		for _, a := range list.JSON200.Items {
			logins = append(logins, a.Login)
			if a.Default {
				t.Errorf("administrator %q is the default of an imported site", a.Login)
			}
		}
		if !slices.Contains(logins, defaultAdmin) {
			t.Fatalf("administrators = %v, want %q among them", logins, defaultAdmin)
		}

		// A login that isn't an administrator is a validation error.
		wp(t, project(site.ID), "user", "create", "login-editor", "login-editor@e2e.test", "--role=editor")
		notAdmin := "login-editor"
		r, err := p.Client.CreateSiteAdminLoginWithResponse(ctx, site.ID, client.CreateSiteAdminLoginJSONRequestBody{User: &notAdmin})
		if err != nil {
			t.Fatal(err)
		}
		if r.StatusCode() != http.StatusUnprocessableEntity {
			t.Errorf("login as an editor: status %d, want 422: %s", r.StatusCode(), r.Body)
		}

		l := issue(t, p.Client, site.ID, defaultAdmin)
		if l.User != defaultAdmin {
			t.Errorf("token user = %q, want %q", l.User, defaultAdmin)
		}
		login(t, browser(t), site, l, defaultAdmin)
	})

	t.Run("expired token", func(t *testing.T) {
		if wait := time.Until(expiring.ExpiresAt) + 2*time.Second; wait > 0 {
			t.Logf("waiting %s for the token to expire", wait.Round(time.Second))
			time.Sleep(wait)
		}
		refused(t, siteA, expiring.URL, expiring.Token)
	})

	t.Run("restart and reconcile", func(t *testing.T) {
		// Sites provisioned before the login existed have no constants:
		// take them out of A's wp-config.php, as the control plane
		// (which owns the file) would find them.
		config := p.DataDir + "/sites/" + siteA.ID + "/wp-config.php"
		constants := func() string {
			t.Helper()
			out, err := p.Project.Exec(ctx, e2e.Service, "sh", "-c", `grep -c "PRESSFLEET_LOGIN_PUBLIC_KEY\|PRESSFLEET_SITE_ID" "$1" || true`, "sh", config)
			if err != nil {
				t.Fatalf("reading %s: %v", config, err)
			}
			return strings.TrimSpace(string(out))
		}
		if got := constants(); got != "2" {
			t.Fatalf("%s login constants in the new site's wp-config.php, want 2", got)
		}
		if out, err := p.Project.Exec(ctx, e2e.Service, "sh", "-c", `sed -i '/PRESSFLEET_LOGIN_PUBLIC_KEY\|PRESSFLEET_SITE_ID/d' "$1"`, "sh", config); err != nil {
			t.Fatalf("removing the constants: %v: %s", err, out)
		}
		if got := constants(); got != "0" {
			t.Fatalf("%s login constants left after removing them", got)
		}
		phpBefore, err := projA.ServiceContainer(ctx, "php")
		if err != nil {
			t.Fatal(err)
		}

		// The same restart requires two-factor, so the reconcile also
		// installs the Two Factor plugin the next check needs.
		p.Restart(t, map[string]string{"PRESSFLEET_WORDPRESS_SECURITY_TWO_FACTOR": "required_for_admins"})
		waitJob(t, p, reconcileJob(t, p, siteA.ID))
		waitJob(t, p, reconcileJob(t, p, siteB.ID))

		if got := constants(); got != "2" {
			t.Errorf("%s login constants in wp-config.php after the reconcile, want 2", got)
		}
		phpAfter, err := projA.ServiceContainer(ctx, "php")
		if err != nil {
			t.Fatal(err)
		}
		if phpAfter.ID == phpBefore.ID {
			t.Error("php wasn't recreated, so it can't see the restored constants")
		}
		defined := wp(t, projA, "eval", `echo defined("PRESSFLEET_SITE_ID") && PRESSFLEET_SITE_ID === "`+siteA.ID+`" && defined("PRESSFLEET_LOGIN_PUBLIC_KEY") ? "yes" : "no";`)
		if defined != "yes" {
			t.Errorf("site A's PHP sees the constants = %q, want yes", defined)
		}
		waitIngress(t, hc, "https://"+siteA.PreviewDomain+"/wp-login.php", nil, http.StatusOK)

		// The login works on the reconciled site, which now requires
		// two-factor, and its session isn't sent to the enrollment screen.
		if got := wp(t, projA, "eval", `echo pressfleet_two_factor_setup_required( get_user_by( "login", "`+defaultAdmin+`" ) ) ? "yes" : "no";`); got != "yes" {
			t.Fatalf("administrator %s must set up two-factor = %q, want yes (a password login would be sent to enrollment)", defaultAdmin, got)
		}
		b := browser(t)
		login(t, b, siteA, issue(t, p.Client, siteA.ID, ""), defaultAdmin)
		for _, path := range []string{"/wp-admin/", "/wp-admin/edit.php", "/wp-admin/options-general.php"} {
			resp, body := ingressGet(t, b, "https://"+siteA.PreviewDomain+path, nil)
			if resp.StatusCode != http.StatusOK {
				t.Errorf("GET %s with the dashboard session: %s, location %q", path, resp.Status, resp.Header.Get("Location"))
			}
			if strings.Contains(body, "two-factor-options") || strings.Contains(resp.Header.Get("Location"), "two-factor") {
				t.Errorf("GET %s shows the two-factor enrollment", path)
			}
		}
		// The session carries the platform login marker.
		sessions := wp(t, projA, "eval", `$u = get_user_by( "login", "`+defaultAdmin+`" ); $m = array(); foreach ( WP_Session_Tokens::get_instance( $u->ID )->get_all() as $s ) { $m[] = isset( $s["pressfleet_login"] ) ? "login" : "password"; } echo implode( ",", $m );`)
		if !strings.Contains(sessions, "login") {
			t.Errorf("no session marked as a platform login among %q", sessions)
		}
		// The other site, reconciled too, still takes logins.
		adminB := wp(t, project(siteB.ID), "user", "list", "--role=administrator", "--field=user_login")
		login(t, browser(t), siteB, issue(t, p.Client, siteB.ID, ""), adminB)
	})
}

// uploadSiteImport sends data through the upload API as a site import
// upload and returns the completed upload's ID.
func uploadSiteImport(t *testing.T, p *e2e.Platform, groupID string, data []byte) string {
	t.Helper()
	ctx := t.Context()
	sum := sha256.Sum256(data)
	sumHex := hex.EncodeToString(sum[:])
	resp, err := p.Client.CreateUploadWithResponse(ctx, client.CreateUploadJSONRequestBody{
		Purpose: client.UploadPurposeSiteImport, ClientGroupID: &groupID, SizeBytes: int64(len(data)), Sha256: &sumHex,
	})
	if err != nil || resp.JSON201 == nil {
		t.Fatalf("create upload: %v %s", err, resp.Body)
	}
	u := resp.JSON201
	for off := 0; off < len(data); off += int(u.MaxChunkBytes) {
		end := min(off+int(u.MaxChunkBytes), len(data))
		c, err := p.Client.AppendUploadChunkWithBodyWithResponse(ctx, u.ID, &client.AppendUploadChunkParams{Offset: int64(off)},
			"application/octet-stream", bytes.NewReader(data[off:end]))
		if err != nil || c.JSON200 == nil {
			t.Fatalf("chunk at %d: %v %s", off, err, c.Body)
		}
	}
	done, err := p.Client.CompleteUploadWithResponse(ctx, u.ID, client.CompleteUploadJSONRequestBody{})
	if err != nil || done.JSON200 == nil {
		t.Fatalf("complete upload: %v %s", err, done.Body)
	}
	return u.ID
}
