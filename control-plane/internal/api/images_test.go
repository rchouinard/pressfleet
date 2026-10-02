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
	"github.com/rchouinard/pressfleet/control-plane/internal/sites"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
)

func TestImageRolloutEndpoints(t *testing.T) {
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
		if _, _, err := e.svc.Create(ctx, sites.NewSite{ClientGroupID: group.ID, Name: "Acme", AdminEmail: "o@example.com", PHPVersion: "8.4"}); err != nil {
			t.Fatal(err)
		}
		e.runJobs(t)

		w := do(t, h, request{authed: true, method: http.MethodGet, target: "/api/v1/image-rollout"})
		ro := decodeJSON[gen.ImageRollout](t, w.Body.Bytes())
		if w.Code != http.StatusOK || ro.Halted || ro.MaxFailures != 3 || ro.Window != nil || ro.Images == nil || ro.Failures == nil {
			t.Errorf("rollout: %d %s", w.Code, w.Body)
		}
		// Resuming queues a pass, which finds the site's builds.
		if w := do(t, h, request{authed: true, method: http.MethodPost, target: "/api/v1/image-rollout/resume"}); w.Code != http.StatusOK {
			t.Errorf("resume: %d %s", w.Code, w.Body)
		}
		e.runJobs(t)
		w = do(t, h, request{authed: true, method: http.MethodGet, target: "/api/v1/image-rollout"})
		ro = decodeJSON[gen.ImageRollout](t, w.Body.Bytes())
		if w.Code != http.StatusOK || len(ro.Images) != 2 || ro.Images[0].Build == "" || ro.Images[0].CheckedAt.IsZero() {
			t.Errorf("rollout after a pass: %d %s", w.Code, w.Body)
		}

		// Viewers may look but not resume.
		viewer := newHandler(t, api.Options{DB: db, Authenticator: &fakeAuth{principal: &auth.Principal{
			UserID: "01j9zq3v6t8k4m2n5p7r9s1x3y", Role: store.RoleViewer, Method: auth.MethodSession, SessionID: "v",
		}}, Sites: e.svc, Backups: e.backups})
		if w := do(t, viewer, request{authed: true, method: http.MethodGet, target: "/api/v1/image-rollout"}); w.Code != http.StatusOK {
			t.Errorf("viewer get: %d", w.Code)
		}
		if w := do(t, viewer, request{authed: true, method: http.MethodPost, target: "/api/v1/image-rollout/resume"}); w.Code != http.StatusForbidden {
			t.Errorf("viewer resume: %d", w.Code)
		}
	})
}

func TestListPHPVersions(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newSiteEnv(t, db, testPreviewBase)
		h := newHandler(t, api.Options{DB: db, Authenticator: &fakeAuth{}, Sites: e.svc, Backups: e.backups})
		cat, err := catalog.Load()
		if err != nil {
			t.Fatal(err)
		}
		const target = "/api/v1/catalog/php-versions"

		if w := do(t, h, request{method: http.MethodGet, target: target}); w.Code != http.StatusUnauthorized {
			t.Errorf("unauthenticated: %d, want 401", w.Code)
		}
		w := do(t, h, request{authed: true, method: http.MethodGet, target: target})
		if w.Code != http.StatusOK {
			t.Fatalf("list: %d %s", w.Code, w.Body)
		}
		got := decodeJSON[gen.PHPVersionCatalog](t, w.Body.Bytes())
		want := cat.PHPVersions()
		if len(got.Items) != len(want) {
			t.Fatalf("items = %s, want %d versions", w.Body, len(want))
		}
		for i, v := range want {
			it := got.Items[i]
			if it.Version != v.Version || string(it.Status) != string(v.Status(time.Now())) ||
				it.ActiveSupportEnds.Format(time.DateOnly) != v.ActiveSupportEnds.Format(time.DateOnly) ||
				it.SecuritySupportEnds.Format(time.DateOnly) != v.SecuritySupportEnds.Format(time.DateOnly) {
				t.Errorf("item %d = %+v, want %+v", i, it, v)
			}
		}
		if def, _ := cat.DefaultPHPVersion(time.Now()); got.Default == nil || *got.Default != def {
			t.Errorf("default = %v, want %s", got.Default, def)
		}
	})
}
