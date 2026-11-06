package api_test

import (
	"context"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/api"
	"github.com/rchouinard/pressfleet/control-plane/internal/api/gen"
	"github.com/rchouinard/pressfleet/control-plane/internal/auth"
	"github.com/rchouinard/pressfleet/control-plane/internal/sites"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
)

// The three rollout endpoints report a hold with its reason, and none
// without one.
func TestRolloutEndpointsReportHold(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		var held atomic.Bool
		e := newSiteEnv(t, db, testPreviewBase, func(o *sites.Options) {
			o.RolloutHold = func() (string, bool) {
				if held.Load() {
					return "the upgrade to 1.2.0 failed", true
				}
				return "", false
			}
		})
		h := newHandler(t, api.Options{DB: db, Authenticator: &fakeAuth{}, Sites: e.svc, Backups: e.backups})
		get := func(target string) (isHeld bool, reason string) {
			t.Helper()
			w := do(t, h, request{authed: true, method: http.MethodGet, target: target})
			if w.Code != http.StatusOK {
				t.Fatalf("GET %s: %d %s", target, w.Code, w.Body)
			}
			body := decodeJSON[struct {
				Held       bool    `json:"held"`
				HoldReason *string `json:"hold_reason"`
			}](t, w.Body.Bytes())
			if body.HoldReason != nil {
				reason = *body.HoldReason
			}
			return body.Held, reason
		}
		for _, target := range []string{"/api/v1/core-rollout", "/api/v1/image-rollout", "/api/v1/template-rollout"} {
			if isHeld, reason := get(target); isHeld || reason != "" {
				t.Errorf("%s without a hold: held %v, %q", target, isHeld, reason)
			}
		}
		held.Store(true)
		for _, target := range []string{"/api/v1/core-rollout", "/api/v1/image-rollout", "/api/v1/template-rollout"} {
			if isHeld, reason := get(target); !isHeld || reason != "the upgrade to 1.2.0 failed" {
				t.Errorf("%s with a hold: held %v, %q", target, isHeld, reason)
			}
		}
		held.Store(false)
		if isHeld, _ := get("/api/v1/core-rollout"); isHeld {
			t.Error("still held after the hold lifted")
		}
	})
}

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
