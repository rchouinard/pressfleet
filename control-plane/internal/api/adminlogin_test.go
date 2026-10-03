package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/rchouinard/pressfleet/control-plane/internal/adminlogin"
	"github.com/rchouinard/pressfleet/control-plane/internal/api"
	"github.com/rchouinard/pressfleet/control-plane/internal/api/gen"
	"github.com/rchouinard/pressfleet/control-plane/internal/auth"
	"github.com/rchouinard/pressfleet/control-plane/internal/id"
	"github.com/rchouinard/pressfleet/control-plane/internal/secrets"
	"github.com/rchouinard/pressfleet/control-plane/internal/secrets/secretstest"
	"github.com/rchouinard/pressfleet/control-plane/internal/sites"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
)

// newAdminLoginSigner returns the platform login signer on db.
func newAdminLoginSigner(t *testing.T, db *store.DB) *adminlogin.Signer {
	t.Helper()
	signer, err := adminlogin.Ensure(context.Background(), db, secrets.NewStore(db, secretstest.Keyring(t, 1)))
	if err != nil {
		t.Fatal(err)
	}
	return signer
}

// newAdminLogin returns an admin login service on the site environment's
// fake provisioner.
func newAdminLogin(t *testing.T, db *store.DB, e *siteEnv) *adminlogin.Service {
	t.Helper()
	return adminlogin.NewService(db, e.prov, newAdminLoginSigner(t, db))
}

func TestAdminLoginEndpoints(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newSiteEnv(t, db, testPreviewBase)
		signer := newAdminLoginSigner(t, db)
		var err error
		opts := api.Options{DB: db, Authenticator: &fakeAuth{}, Sites: e.svc, AdminLogin: adminlogin.NewService(db, e.prov, signer)}
		h := newHandler(t, opts)
		const adminID = "01j9zq3v6t8k4m2n5p7r9s1x3y"
		group := &store.ClientGroup{Name: "Acme"}
		if err := store.NewClientGroups(db).Create(ctx, group); err != nil {
			t.Fatal(err)
		}
		create := func(name string) store.Site {
			t.Helper()
			site, _, err := e.svc.Create(ctx, sites.NewSite{ClientGroupID: group.ID, Name: name, AdminEmail: "o@example.com", PHPVersion: "8.4"})
			if err != nil {
				t.Fatal(err)
			}
			return site
		}
		site, other := create("Acme"), create("Other")
		e.runJobs(t)
		site, _, err = e.svc.Get(ctx, site.ID)
		if err != nil {
			t.Fatal(err)
		}
		other, _, err = e.svc.Get(ctx, other.ID)
		if err != nil {
			t.Fatal(err)
		}

		// WP-CLI answers `wp user list` from this list.
		var wpListed []string
		var userListArgs []string
		e.prov.WPCLI = func(_ string, args []string) (string, error) {
			if len(args) < 2 || args[0] != "user" || args[1] != "list" {
				return "", nil
			}
			userListArgs = args
			var users []map[string]string
			for _, login := range wpListed {
				users = append(users, map[string]string{"user_login": login, "display_name": strings.ToUpper(login), "user_email": login + "@example.com"})
			}
			out, _ := json.Marshal(users)
			return string(out), nil
		}
		loginPath := func(s store.Site) string { return "/api/v1/sites/" + s.ID + "/admin-login" }
		adminsPath := func(s store.Site) string { return "/api/v1/sites/" + s.ID + "/administrators" }
		login := func(h http.Handler, s store.Site, body string) (int, []byte) {
			w := do(t, h, request{authed: true, method: http.MethodPost, target: loginPath(s), body: body})
			if w.Code == http.StatusOK && w.Header().Get("Cache-Control") != "no-store" {
				t.Errorf("Cache-Control = %q", w.Header().Get("Cache-Control"))
			}
			return w.Code, w.Body.Bytes()
		}
		as := func(p auth.Principal) http.Handler {
			o := opts
			o.Authenticator = &fakeAuth{principal: &p}
			return newHandler(t, o)
		}

		// The default administrator, for an Operator.
		wpListed = []string{"someone", site.AdminUsername}
		operator := as(auth.Principal{UserID: adminID, Role: store.RoleOperator, Method: auth.MethodSession, SessionID: "o"})
		code, body := login(operator, site, "")
		if code != http.StatusOK {
			t.Fatalf("default admin: %d %s", code, body)
		}
		got := decodeJSON[gen.AdminLogin](t, body)
		if want := "https://" + site.PreviewDomain + "/wp-login.php?action=pressfleet-login"; got.URL != want || got.User != site.AdminUsername || got.Token == "" {
			t.Errorf("login = %+v, want url %s and user %s", got, want, site.AdminUsername)
		}
		claims, err := signer.Verifier().Verify(got.Token, site.ID)
		if err != nil || claims.User != site.AdminUsername || claims.Actor != adminID || got.ExpiresAt.Unix() != claims.ExpiresAt {
			t.Errorf("claims = %+v, %v", claims, err)
		}
		if want := []string{"user", "list", "--role=administrator"}; !slices.Equal(userListArgs[:3], want) {
			t.Errorf("WP-CLI args = %v", userListArgs)
		}

		// The primary domain once the site is live.
		if _, err := store.NewSites(db).SetDomains(ctx, site.ID, store.SiteDomains{Primary: "acme.example"}); err != nil {
			t.Fatal(err)
		}
		primary, _, _ := e.svc.Get(ctx, site.ID)
		if code, body := login(h, primary, ""); code != http.StatusOK ||
			decodeJSON[gen.AdminLogin](t, body).URL != "https://acme.example/wp-login.php?action=pressfleet-login" {
			t.Errorf("live site: %d %s", code, body)
		}

		// An explicit user, matched without regard to case; the token is for the
		// login WordPress reports.
		wpListed = []string{"someone", site.AdminUsername}
		if code, body := login(h, site, `{"user":"SomeOne"}`); code != http.StatusOK || decodeJSON[gen.AdminLogin](t, body).User != "someone" {
			t.Errorf("explicit user: %d %s", code, body)
		}

		// A user who isn't an administrator: 422.
		if code, body := login(h, site, `{"user":"editor"}`); code != http.StatusUnprocessableEntity {
			t.Errorf("non-administrator: %d %s", code, body)
		}

		// A deleted or demoted default administrator, and an imported site:
		// the distinct 409.
		chooseUser := func(what string, s store.Site) {
			t.Helper()
			w := do(t, h, request{authed: true, method: http.MethodPost, target: loginPath(s)})
			if p := decodeProblem(t, w); w.Code != http.StatusConflict || p.Type != api.ChooseUserProblemType {
				t.Errorf("%s: %d %+v", what, w.Code, p)
			}
		}
		wpListed = []string{"someone"}
		chooseUser("deleted default admin", site)
		if _, err := db.ExecContext(ctx, "UPDATE sites SET admin_username = '' WHERE id = ?", other.ID); err != nil {
			t.Fatal(err)
		}
		wpListed = []string{"imported-admin"}
		chooseUser("imported site", other)
		// The administrators endpoint lists who to choose from.
		w := do(t, h, request{authed: true, method: http.MethodGet, target: adminsPath(other)})
		if w.Code != http.StatusOK || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("administrators: %d %s", w.Code, w.Body)
		}
		if list := decodeJSON[gen.SiteAdministratorList](t, w.Body.Bytes()); len(list.Items) != 1 ||
			list.Items[0] != (gen.SiteAdministrator{Login: "imported-admin", DisplayName: "IMPORTED-ADMIN", Email: "imported-admin@example.com"}) {
			t.Errorf("administrators = %+v", list)
		}
		// The platform's default is marked.
		wpListed = []string{"someone", site.AdminUsername}
		w = do(t, h, request{authed: true, method: http.MethodGet, target: adminsPath(site)})
		if list := decodeJSON[gen.SiteAdministratorList](t, w.Body.Bytes()); w.Code != http.StatusOK || len(list.Items) != 2 ||
			list.Items[0].Default || !list.Items[1].Default {
			t.Errorf("administrators with default: %d %+v", w.Code, list)
		}
		// An imported site can log in with an explicit user.
		wpListed = []string{"imported-admin"}
		if code, body := login(h, other, `{"user":"imported-admin"}`); code != http.StatusOK {
			t.Errorf("imported site, explicit user: %d %s", code, body)
		}

		// Not found, bad request.
		if w := do(t, h, request{authed: true, method: http.MethodPost, target: "/api/v1/sites/" + id.New() + "/admin-login"}); w.Code != http.StatusNotFound {
			t.Errorf("missing site: %d %s", w.Code, w.Body)
		}
		if code, body := login(h, site, `{"user":""}`); code != http.StatusUnprocessableEntity && code != http.StatusBadRequest {
			t.Errorf("empty user: %d %s", code, body)
		}

		// Maintenance mode and stopped sites: 409, and no token.
		wpListed = []string{site.AdminUsername}
		if _, err := store.NewSites(db).SetMaintenance(ctx, site.ID, true); err != nil {
			t.Fatal(err)
		}
		if code, body := login(h, site, ""); code != http.StatusConflict {
			t.Errorf("maintenance: %d %s", code, body)
		}
		if w := do(t, h, request{authed: true, method: http.MethodGet, target: adminsPath(site)}); w.Code != http.StatusOK {
			t.Errorf("administrators in maintenance: %d %s", w.Code, w.Body)
		}
		if _, err := store.NewSites(db).SetMaintenance(ctx, site.ID, false); err != nil {
			t.Fatal(err)
		}
		if ok, err := store.NewSites(db).SetStatus(ctx, site.ID, store.SiteStopped, ""); err != nil || !ok {
			t.Fatal(ok, err)
		}
		if code, body := login(h, site, ""); code != http.StatusConflict {
			t.Errorf("stopped: %d %s", code, body)
		} else if p := decodeJSONProblem(t, body); p.Type == api.ChooseUserProblemType {
			t.Errorf("stopped site got the choose-user problem: %+v", p)
		}
		if w := do(t, h, request{authed: true, method: http.MethodGet, target: adminsPath(site)}); w.Code != http.StatusConflict {
			t.Errorf("administrators of a stopped site: %d %s", w.Code, w.Body)
		}
		if ok, err := store.NewSites(db).SetStatus(ctx, site.ID, store.SiteRunning, ""); err != nil || !ok {
			t.Fatal(ok, err)
		}

		// Viewers and read-only tokens are denied, on both endpoints.
		for _, p := range []auth.Principal{
			{UserID: adminID, Role: store.RoleViewer, Method: auth.MethodSession, SessionID: "v"},
			{UserID: adminID, Role: store.RoleAdmin, Method: auth.MethodToken, Scope: store.ScopeReadOnly, TokenID: "t"},
		} {
			hh := as(p)
			for _, req := range []request{
				{method: http.MethodPost, target: loginPath(site)},
				{method: http.MethodGet, target: adminsPath(site)},
			} {
				req.authed = true
				if w := do(t, hh, req); w.Code != http.StatusForbidden {
					t.Errorf("%s %s as %s/%s: %d %s", req.method, req.target, p.Role, p.Scope, w.Code, w.Body)
				}
			}
		}
		// Admins and full tokens may.
		if code, body := login(as(auth.Principal{UserID: adminID, Role: store.RoleAdmin, Method: auth.MethodToken, Scope: store.ScopeFull, TokenID: "t"}), site, ""); code != http.StatusOK {
			t.Errorf("full token: %d %s", code, body)
		}

		// Every issued token is an audit event with the WordPress user and the
		// jti, never the token.
		events, err := store.NewAuditEvents(db).List(ctx, store.AuditFilter{TargetType: "site", TargetID: site.ID})
		if err != nil {
			t.Fatal(err)
		}
		var issued int
		for _, ev := range events.Events {
			if ev.Action != "site.admin_login" || ev.Result != store.AuditSuccess {
				continue
			}
			issued++
			if ev.Details["wp_user"] == "" || len(ev.Details["jti"]) != 32 || ev.ActorUserID != adminID {
				t.Errorf("event = %+v", ev)
			}
			for k, v := range ev.Details {
				if strings.HasPrefix(v, "v1.") {
					t.Errorf("the token is in the event's %s", k)
				}
			}
		}
		if issued != 4 { // default, live site, explicit, full token
			t.Errorf("%d issued-token events, want 4", issued)
		}
	})
}

// decodeJSONProblem decodes a problem body.
func decodeJSONProblem(t *testing.T, body []byte) problem {
	t.Helper()
	var p problem
	if err := json.Unmarshal(body, &p); err != nil {
		t.Fatalf("decoding problem: %v; body: %s", err, body)
	}
	return p
}
