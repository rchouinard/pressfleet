package api_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/api"
	"github.com/rchouinard/pressfleet/control-plane/internal/api/gen"
	"github.com/rchouinard/pressfleet/control-plane/internal/id"
	"github.com/rchouinard/pressfleet/control-plane/internal/pluginupdates"
	"github.com/rchouinard/pressfleet/control-plane/internal/sites"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
)

func TestUpdateEndpoints(t *testing.T) {
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
		updates := "/api/v1/sites/" + site.ID + "/updates"

		// Creating the site scheduled its checks.
		if _, err := store.NewSchedules(db).Get(ctx, pluginupdates.ScheduleName(site.ID)); err != nil {
			t.Errorf("update check schedule: %v", err)
		}

		// Not checked yet: no counts on the site, empty lists.
		w := do(t, h, request{authed: true, method: http.MethodGet, target: updates})
		if w.Code != http.StatusOK {
			t.Fatalf("get updates: %d %s", w.Code, w.Body)
		}
		if u := decodeJSON[gen.SiteUpdates](t, w.Body.Bytes()); u.CheckedAt != nil || u.Plugins == nil || u.Themes == nil || len(u.Plugins)+len(u.Themes) != 0 {
			t.Errorf("updates before a check = %s", w.Body)
		}
		w = do(t, h, request{authed: true, method: http.MethodGet, target: "/api/v1/sites/" + site.ID})
		if s := decodeJSON[gen.Site](t, w.Body.Bytes()); w.Code != http.StatusOK || s.PendingUpdates != nil {
			t.Errorf("site before a check: %d %s", w.Code, w.Body)
		}

		if err := store.NewUpdateReports(db).Put(ctx, store.UpdateReport{SiteID: site.ID, Updates: []store.PendingUpdate{
			{Kind: store.UpdateKindPlugin, Name: "akismet", Title: "Akismet", Version: "5.3", UpdateVersion: "5.4"},
			{Kind: store.UpdateKindPlugin, Name: "woocommerce", Title: "WooCommerce", Version: "9.1.0", UpdateVersion: "9.2.1"},
			{Kind: store.UpdateKindTheme, Name: "twentytwentyfive", Version: "1.0", UpdateVersion: "1.1"},
		}}); err != nil {
			t.Fatal(err)
		}
		w = do(t, h, request{authed: true, method: http.MethodGet, target: updates})
		u := decodeJSON[gen.SiteUpdates](t, w.Body.Bytes())
		if w.Code != http.StatusOK || u.CheckedAt == nil || len(u.Plugins) != 2 || len(u.Themes) != 1 ||
			u.Plugins[1] != (gen.PendingUpdate{Name: "woocommerce", Title: "WooCommerce", Version: "9.1.0", UpdateVersion: "9.2.1"}) ||
			u.Themes[0].Name != "twentytwentyfive" {
			t.Errorf("updates = %d %s", w.Code, w.Body)
		}
		w = do(t, h, request{authed: true, method: http.MethodGet, target: "/api/v1/sites/" + site.ID})
		if s := decodeJSON[gen.Site](t, w.Body.Bytes()); s.PendingUpdates == nil || s.PendingUpdates.Plugins != 2 || s.PendingUpdates.Themes != 1 {
			t.Errorf("site counts: %s", w.Body)
		}
		w = do(t, h, request{authed: true, method: http.MethodGet, target: "/api/v1/sites"})
		if l := decodeJSON[gen.SiteList](t, w.Body.Bytes()); len(l.Items) != 1 || l.Items[0].PendingUpdates == nil || l.Items[0].PendingUpdates.Plugins != 2 {
			t.Errorf("site list counts: %s", w.Body)
		}

		// Checking now queues one job, however often it is asked.
		w = do(t, h, request{authed: true, method: http.MethodPost, target: updates + "/check"})
		if w.Code != http.StatusAccepted || w.Header().Get("Location") == "" {
			t.Fatalf("check: %d %s", w.Code, w.Body)
		}
		first := decodeJSON[gen.JobAccepted](t, w.Body.Bytes()).Job
		if first.Type != pluginupdates.JobType || first.Target.ID != site.ID {
			t.Errorf("check job = %s", w.Body)
		}
		w = do(t, h, request{authed: true, method: http.MethodPost, target: updates + "/check"})
		if again := decodeJSON[gen.JobAccepted](t, w.Body.Bytes()).Job; w.Code != http.StatusAccepted || again.ID != first.ID {
			t.Errorf("second check: %d %s, want job %s", w.Code, w.Body, first.ID)
		}

		missing := "/api/v1/sites/" + id.New() + "/updates"
		if w := do(t, h, request{authed: true, method: http.MethodGet, target: missing}); w.Code != http.StatusNotFound {
			t.Errorf("get updates of a missing site: %d", w.Code)
		}
		if w := do(t, h, request{authed: true, method: http.MethodPost, target: missing + "/check"}); w.Code != http.StatusNotFound {
			t.Errorf("check a missing site: %d", w.Code)
		}
	})
}
