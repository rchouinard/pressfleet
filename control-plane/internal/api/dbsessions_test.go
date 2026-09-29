package api_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/api"
	"github.com/rchouinard/pressfleet/control-plane/internal/api/gen"
	"github.com/rchouinard/pressfleet/control-plane/internal/auth"
	"github.com/rchouinard/pressfleet/control-plane/internal/dbsession"
	"github.com/rchouinard/pressfleet/control-plane/internal/id"
	"github.com/rchouinard/pressfleet/control-plane/internal/sitedb"
	"github.com/rchouinard/pressfleet/control-plane/internal/sites"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
)

// newDatabaseSessions returns a database session service on the site
// environment's fake databases.
func newDatabaseSessions(t *testing.T, db *store.DB, e *siteEnv, now func() time.Time) *dbsession.Service {
	t.Helper()
	svc, err := dbsession.New(dbsession.Options{
		DB: db, Users: e.dbs, Now: now, Logger: storetest.Logger(),
		Tunnel: dbsession.Tunnel{SSHHost: "fleet.example", SSHPort: 2222, DatabaseAddr: "mariadb:3306"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return svc
}

func TestDatabaseSessionEndpoints(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newSiteEnv(t, db, testPreviewBase)
		clock := &testClock{t: time.Now()}
		opts := api.Options{DB: db, Authenticator: &fakeAuth{}, Sites: e.svc, DatabaseSessions: newDatabaseSessions(t, db, e, clock.now)}
		h := newHandler(t, opts)
		const adminID = "01j9zq3v6t8k4m2n5p7r9s1x3y"
		group := &store.ClientGroup{Name: "Acme"}
		if err := store.NewClientGroups(db).Create(ctx, group); err != nil {
			t.Fatal(err)
		}
		site, _, err := e.svc.Create(ctx, sites.NewSite{ClientGroupID: group.ID, Name: "Acme", AdminEmail: "o@example.com", PHPVersion: "8.4"})
		if err != nil {
			t.Fatal(err)
		}
		sessionsPath := "/api/v1/sites/" + site.ID + "/database/sessions"
		if w := do(t, h, request{authed: true, method: http.MethodPost, target: sessionsPath}); w.Code != http.StatusConflict {
			t.Errorf("open while provisioning: %d %s", w.Code, w.Body)
		}
		e.runJobs(t)

		// Open a read-only session by default.
		w := do(t, h, request{authed: true, method: http.MethodPost, target: sessionsPath})
		if w.Code != http.StatusCreated {
			t.Fatalf("open: %d %s", w.Code, w.Body)
		}
		opened := decodeJSON[gen.DatabaseSessionOpened](t, w.Body.Bytes())
		s := opened.Session
		if s.Access != gen.DatabaseSessionAccessReadOnly || s.Status != gen.DatabaseSessionStatusActive || s.SiteID != site.ID || s.UserID != adminID ||
			s.Username != "pft_"+s.ID || s.Database != "pf_"+site.ID || s.ExpiresAt.Sub(s.CreatedAt) != time.Hour {
			t.Errorf("session = %+v", s)
		}
		password := opened.Credentials.Password
		if password == "" || opened.Credentials.Username != s.Username || opened.Credentials.Database != s.Database {
			t.Errorf("credentials = %+v", opened.Credentials)
		}
		if want := "ssh -N -L 3307:mariadb:3306 -p 2222 " + site.Slug + "@fleet.example"; opened.Tunnel.SSHCommand != want ||
			opened.Tunnel.LocalPort != 3307 || opened.Tunnel.LocalHost != "127.0.0.1" {
			t.Errorf("tunnel = %+v", opened.Tunnel)
		}
		if u := e.dbs.TemporaryUsers()[s.ID]; u.Access != sitedb.AccessReadOnly || u.Password.Reveal() != password {
			t.Errorf("temporary user = %+v", u)
		}

		// A read-write session for two hours.
		w = do(t, h, request{authed: true, method: http.MethodPost, target: sessionsPath, body: `{"access":"read_write","ttl_seconds":7200}`})
		if w.Code != http.StatusCreated {
			t.Fatalf("open read-write: %d %s", w.Code, w.Body)
		}
		rw := decodeJSON[gen.DatabaseSessionOpened](t, w.Body.Bytes()).Session
		if rw.Access != gen.DatabaseSessionAccessReadWrite || rw.ExpiresAt.Sub(rw.CreatedAt) != 2*time.Hour {
			t.Errorf("read-write session = %+v", rw)
		}
		for _, body := range []string{`{"ttl_seconds":30}`, `{"ttl_seconds":28801}`, `{"access":"root"}`} {
			if w := do(t, h, request{authed: true, method: http.MethodPost, target: sessionsPath, body: body}); w.Code != http.StatusUnprocessableEntity {
				t.Errorf("open with %s: %d %s", body, w.Code, w.Body)
			}
		}

		// Listing shows both, without passwords.
		w = do(t, h, request{authed: true, method: http.MethodGet, target: sessionsPath})
		if w.Code != http.StatusOK || strings.Contains(w.Body.String(), password) {
			t.Fatalf("list: %d %s", w.Code, w.Body)
		}
		if list := decodeJSON[gen.DatabaseSessionList](t, w.Body.Bytes()); len(list.Items) != 2 {
			t.Errorf("list = %+v", list)
		}

		// Operators open read-only sessions, not read-write ones; Viewers
		// and read-only tokens neither.
		as := func(p auth.Principal) http.Handler {
			o := opts
			o.Authenticator = &fakeAuth{principal: &p}
			return newHandler(t, o)
		}
		operator := as(auth.Principal{UserID: adminID, Role: store.RoleOperator, Method: auth.MethodSession, SessionID: "o"})
		if w := do(t, operator, request{authed: true, method: http.MethodPost, target: sessionsPath}); w.Code != http.StatusCreated {
			t.Errorf("operator read-only: %d %s", w.Code, w.Body)
		}
		if w := do(t, operator, request{authed: true, method: http.MethodPost, target: sessionsPath, body: `{"access":"read_write"}`}); w.Code != http.StatusForbidden {
			t.Errorf("operator read-write: %d %s", w.Code, w.Body)
		}
		for _, p := range []auth.Principal{
			{UserID: adminID, Role: store.RoleViewer, Method: auth.MethodSession, SessionID: "v"},
			{UserID: adminID, Role: store.RoleAdmin, Method: auth.MethodToken, Scope: store.ScopeReadOnly, TokenID: "t"},
		} {
			hh := as(p)
			for _, req := range []request{
				{method: http.MethodPost, target: sessionsPath},
				{method: http.MethodDelete, target: sessionsPath + "/" + s.ID},
			} {
				req.authed = true
				if w := do(t, hh, req); w.Code != http.StatusForbidden {
					t.Errorf("%s %s as %s/%s: %d %s", req.method, req.target, p.Role, p.Scope, w.Code, w.Body)
				}
			}
		}

		// End: 204, then 404.
		end := sessionsPath + "/" + s.ID
		if w := do(t, h, request{authed: true, method: http.MethodDelete, target: end}); w.Code != http.StatusNoContent {
			t.Fatalf("end: %d %s", w.Code, w.Body)
		}
		if _, ok := e.dbs.TemporaryUsers()[s.ID]; ok {
			t.Error("the ended session's user remains")
		}
		for _, target := range []string{end, sessionsPath + "/" + id.New(), "/api/v1/sites/" + id.New() + "/database/sessions/" + rw.ID} {
			if w := do(t, h, request{authed: true, method: http.MethodDelete, target: target}); w.Code != http.StatusNotFound {
				t.Errorf("DELETE %s: %d %s", target, w.Code, w.Body)
			}
		}
		if w := do(t, h, request{authed: true, method: http.MethodGet, target: "/api/v1/sites/" + id.New() + "/database/sessions"}); w.Code != http.StatusNotFound {
			t.Errorf("list of a missing site: %d %s", w.Code, w.Body)
		}

		// Expired sessions drop out of the list.
		clock.add(2 * time.Hour)
		w = do(t, h, request{authed: true, method: http.MethodGet, target: sessionsPath})
		if list := decodeJSON[gen.DatabaseSessionList](t, w.Body.Bytes()); len(list.Items) != 0 {
			t.Errorf("list after expiry = %+v", list)
		}

		// The audit log has the session's ID, never its password.
		events, err := store.NewAuditEvents(db).List(ctx, store.AuditFilter{TargetType: "site", TargetID: site.ID})
		if err != nil {
			t.Fatal(err)
		}
		count := map[string]int{}
		for _, ev := range events.Events {
			count[ev.Action+":"+string(ev.Result)]++
			for k, v := range ev.Details {
				if strings.Contains(v, password) {
					t.Errorf("the password is in the %s event's %s", ev.Action, k)
				}
			}
			if ev.Action == "site.database.session.create" && ev.Result == store.AuditSuccess && ev.Details["session_id"] == s.ID &&
				(ev.Details["access"] != "read_only" || ev.Details["username"] != s.Username || ev.Details["expires_at"] == "") {
				t.Errorf("open event details = %v", ev.Details)
			}
			if ev.Action == "site.database.session.end" && ev.Result == store.AuditSuccess && ev.Details["session_id"] != s.ID {
				t.Errorf("end event details = %v", ev.Details)
			}
		}
		want := map[string]int{
			"site.database.session.create:success":            2,
			"site.database.session.create:failure":            1, // while provisioning; invalid bodies have no target
			"site.database.session.create:denied":             2,
			"site.database.session.create_read_write:success": 1,
			"site.database.session.create_read_write:denied":  1,
			"site.database.session.end:success":               1,
			"site.database.session.end:failure":               2,
			"site.database.session.end:denied":                2,
		}
		for k, n := range want {
			if count[k] != n {
				t.Errorf("%s audit events = %d, want %d (all: %v)", k, count[k], n, count)
			}
		}
	})
}
