package api_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/api"
	"github.com/rchouinard/pressfleet/control-plane/internal/api/gen"
	"github.com/rchouinard/pressfleet/control-plane/internal/auth"
	"github.com/rchouinard/pressfleet/control-plane/internal/catalog"
	"github.com/rchouinard/pressfleet/control-plane/internal/id"
	"github.com/rchouinard/pressfleet/control-plane/internal/sites"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
)

func TestCoreEndpoints(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newSiteEnv(t, db, testPreviewBase)
		h := newHandler(t, api.Options{DB: db, Authenticator: &fakeAuth{}, Sites: e.svc, Backups: e.backups})
		now := time.Now()
		if _, err := db.ExecContext(ctx, "INSERT INTO users (id, email, password_hash, role, created_at, updated_at) VALUES (?, ?, 'x', 'admin', ?, ?)",
			"01j9zq3v6t8k4m2n5p7r9s1x3y", "admin@example.com", now, now); err != nil {
			t.Fatal(err)
		}
		group := &store.ClientGroup{Name: "Acme"}
		if err := store.NewClientGroups(db).Create(ctx, group); err != nil {
			t.Fatal(err)
		}
		site, _, err := e.svc.Create(ctx, sites.NewSite{ClientGroupID: group.ID, Name: "Acme", AdminEmail: "o@example.com", PHPVersion: "8.4"})
		if err != nil {
			t.Fatal(err)
		}
		e.runJobs(t)
		cat, err := catalog.Load()
		if err != nil {
			t.Fatal(err)
		}
		latest := cat.LatestWordPress()
		core := "/api/v1/sites/" + site.ID + "/core"

		// The site runs the latest release: nothing to do.
		w := do(t, h, request{authed: true, method: http.MethodGet, target: core})
		if w.Code != http.StatusOK {
			t.Fatalf("get core: %d %s", w.Code, w.Body)
		}
		st := decodeJSON[gen.SiteCore](t, w.Body.Bytes())
		if st.Version != latest.Version || st.Branch != latest.Branch() || st.Target != nil || st.Held ||
			st.DecidedBy != gen.SiteCoreDecidedByDefault || st.Warnings == nil {
			t.Errorf("core = %s", w.Body)
		}
		if w := do(t, h, request{authed: true, method: http.MethodPost, target: core + "/update"}); w.Code != http.StatusConflict {
			t.Errorf("update without a target: %d %s, want 409", w.Code, w.Body)
		}

		// Holding and approving.
		w = do(t, h, request{authed: true, method: http.MethodPut, target: core + "/policy", body: `{"hold":true}`})
		if w.Code != http.StatusOK {
			t.Fatalf("hold: %d %s", w.Code, w.Body)
		}
		if st := decodeJSON[gen.SiteCore](t, w.Body.Bytes()); !st.Held || !st.Policy.Hold || st.Policy.HoldSince == nil ||
			st.DecidedBy != gen.SiteCoreDecidedBySite {
			t.Errorf("held core = %s", w.Body)
		}
		w = do(t, h, request{authed: true, method: http.MethodPut, target: core + "/policy", body: `{"approved_branch":"` + latest.Branch() + `"}`})
		if st := decodeJSON[gen.SiteCore](t, w.Body.Bytes()); w.Code != http.StatusOK || st.Held || st.ApprovedBranch == nil || *st.ApprovedBranch != latest.Branch() {
			t.Errorf("approved: %d %s", w.Code, w.Body)
		}
		for _, body := range []string{`{"hold":true,"approved_branch":"` + latest.Branch() + `"}`, `{"approved_branch":"1.0"}`, `{"approved_branch":"7.1.2"}`, `{"extra":1}`} {
			if w := do(t, h, request{authed: true, method: http.MethodPut, target: core + "/policy", body: body}); w.Code != http.StatusUnprocessableEntity {
				t.Errorf("policy %s: %d %s, want 422", body, w.Code, w.Body)
			}
		}
		if w := do(t, h, request{authed: true, method: http.MethodGet, target: "/api/v1/sites/" + id.New() + "/core"}); w.Code != http.StatusNotFound {
			t.Errorf("missing site: %d", w.Code)
		}

		// Client group policy.
		gp := "/api/v1/client-groups/" + group.ID + "/core-policy"
		w = do(t, h, request{authed: true, method: http.MethodPut, target: gp, body: `{"hold":true}`})
		if p := decodeJSON[gen.CorePolicy](t, w.Body.Bytes()); w.Code != http.StatusOK || !p.Hold {
			t.Errorf("group hold: %d %s", w.Code, w.Body)
		}
		w = do(t, h, request{authed: true, method: http.MethodGet, target: gp})
		if p := decodeJSON[gen.CorePolicy](t, w.Body.Bytes()); w.Code != http.StatusOK || !p.Hold {
			t.Errorf("group policy: %d %s", w.Code, w.Body)
		}
		if w := do(t, h, request{authed: true, method: http.MethodGet, target: "/api/v1/client-groups/" + id.New() + "/core-policy"}); w.Code != http.StatusNotFound {
			t.Errorf("missing group: %d", w.Code)
		}
		// The site's own policy cleared, it follows the group.
		w = do(t, h, request{authed: true, method: http.MethodPut, target: core + "/policy", body: `{}`})
		if st := decodeJSON[gen.SiteCore](t, w.Body.Bytes()); w.Code != http.StatusOK || !st.Held || st.DecidedBy != gen.SiteCoreDecidedByClientGroup {
			t.Errorf("following the group: %d %s", w.Code, w.Body)
		}

		// The rollout.
		w = do(t, h, request{authed: true, method: http.MethodGet, target: "/api/v1/core-rollout"})
		ro := decodeJSON[gen.CoreRollout](t, w.Body.Bytes())
		if w.Code != http.StatusOK || ro.Halted || ro.MaxFailures != 3 || len(ro.Releases) == 0 || ro.Releases[0].Version != latest.Version || ro.Window != nil {
			t.Errorf("rollout: %d %s", w.Code, w.Body)
		}
		if w := do(t, h, request{authed: true, method: http.MethodPost, target: "/api/v1/core-rollout/resume"}); w.Code != http.StatusOK {
			t.Errorf("resume: %d %s", w.Code, w.Body)
		}

		// Viewers may look but not decide.
		viewer := newHandler(t, api.Options{DB: db, Authenticator: &fakeAuth{principal: &auth.Principal{
			UserID: "01j9zq3v6t8k4m2n5p7r9s1x3y", Role: store.RoleViewer, Method: auth.MethodSession, SessionID: "v",
		}}, Sites: e.svc, Backups: e.backups})
		if w := do(t, viewer, request{authed: true, method: http.MethodGet, target: core}); w.Code != http.StatusOK {
			t.Errorf("viewer get: %d", w.Code)
		}
		for _, req := range []request{
			{authed: true, method: http.MethodPut, target: core + "/policy", body: `{"hold":true}`},
			{authed: true, method: http.MethodPost, target: core + "/update"},
			{authed: true, method: http.MethodPut, target: gp, body: `{}`},
			{authed: true, method: http.MethodPost, target: "/api/v1/core-rollout/resume"},
		} {
			if w := do(t, viewer, req); w.Code != http.StatusForbidden {
				t.Errorf("viewer %s %s: %d", req.method, req.target, w.Code)
			}
		}
	})
}
