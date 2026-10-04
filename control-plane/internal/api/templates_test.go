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
	"github.com/rchouinard/pressfleet/control-plane/internal/sites"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
)

func TestTemplateRolloutEndpoints(t *testing.T) {
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
		// The re-rendered site passes its health check.
		wpcli := e.prov.WPCLI
		e.prov.WPCLI = func(siteID string, args []string) (string, error) {
			if strings.Join(args, " ") == "core version" {
				return site.WPCoreVersion + "\n", nil
			}
			return wpcli(siteID, args)
		}

		const target = "/api/v1/template-rollout"
		w := do(t, h, request{authed: true, method: http.MethodGet, target: target})
		ro := decodeJSON[gen.TemplateRollout](t, w.Body.Bytes())
		if w.Code != http.StatusOK || ro.Halted || ro.MaxFailures != 3 || ro.Templates != testTemplates || ro.PendingSites != 0 || ro.Failures == nil {
			t.Errorf("rollout: %d %s", w.Code, w.Body)
		}
		// An upgrade with other templates: the site is pending until a
		// pass re-renders it.
		e.tmpl.SetFingerprint("r2-test")
		w = do(t, h, request{authed: true, method: http.MethodGet, target: target})
		if ro = decodeJSON[gen.TemplateRollout](t, w.Body.Bytes()); ro.Templates != "r2-test" || ro.PendingSites != 1 {
			t.Errorf("rollout after the upgrade: %d %s", w.Code, w.Body)
		}
		if w := do(t, h, request{authed: true, method: http.MethodPost, target: target + "/resume"}); w.Code != http.StatusOK {
			t.Errorf("resume: %d %s", w.Code, w.Body)
		}
		e.runJobs(t)
		w = do(t, h, request{authed: true, method: http.MethodGet, target: target})
		if ro = decodeJSON[gen.TemplateRollout](t, w.Body.Bytes()); ro.PendingSites != 0 || len(ro.Failures) != 0 {
			t.Errorf("rollout after a pass: %d %s", w.Code, w.Body)
		}

		// Viewers may look but not resume.
		viewer := newHandler(t, api.Options{DB: db, Authenticator: &fakeAuth{principal: &auth.Principal{
			UserID: "01j9zq3v6t8k4m2n5p7r9s1x3y", Role: store.RoleViewer, Method: auth.MethodSession, SessionID: "v",
		}}, Sites: e.svc, Backups: e.backups})
		if w := do(t, viewer, request{authed: true, method: http.MethodGet, target: target}); w.Code != http.StatusOK {
			t.Errorf("viewer get: %d", w.Code)
		}
		if w := do(t, viewer, request{authed: true, method: http.MethodPost, target: target + "/resume"}); w.Code != http.StatusForbidden {
			t.Errorf("viewer resume: %d", w.Code)
		}
	})
}
