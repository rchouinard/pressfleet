package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/api"
	"github.com/rchouinard/pressfleet/control-plane/internal/api/gen"
	"github.com/rchouinard/pressfleet/control-plane/internal/auth"
	"github.com/rchouinard/pressfleet/control-plane/internal/sites"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
)

func TestDeleteSiteWithBackups(t *testing.T) {
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
		newSite := func(name string) store.Site {
			t.Helper()
			site, _, err := e.svc.Create(ctx, sites.NewSite{ClientGroupID: group.ID, Name: name, AdminEmail: "o@example.com", PHPVersion: "8.4"})
			if err != nil {
				t.Fatal(err)
			}
			e.runJobs(t)
			for range 2 {
				if w := do(t, h, request{authed: true, method: http.MethodPost, target: "/api/v1/sites/" + site.ID + "/backups"}); w.Code != http.StatusAccepted {
					t.Fatalf("backup: %d %s", w.Code, w.Body)
				}
				e.runJobs(t)
			}
			return site
		}
		doomed, neighbor := newSite("Doomed"), newSite("Neighbor")
		target := "/api/v1/sites/" + doomed.ID + "?delete_backups=true"

		// Deleting backups needs an Admin, and the denial is audited as
		// the action asked for.
		operator := newHandler(t, api.Options{DB: db, Sites: e.svc, Backups: e.backups, Authenticator: &fakeAuth{principal: &auth.Principal{
			UserID: "01j9zq3v6t8k4m2n5p7r9s1x3y", Role: store.RoleOperator, Method: auth.MethodSession, SessionID: "test",
		}}})
		if w := do(t, operator, request{authed: true, method: http.MethodDelete, target: target}); w.Code != http.StatusForbidden {
			t.Errorf("operator deleting with backups: %d %s", w.Code, w.Body)
		}
		if w := do(t, h, request{authed: true, method: http.MethodDelete, target: "/api/v1/sites/" + doomed.ID + "?delete_backups=maybe"}); w.Code != http.StatusBadRequest {
			t.Errorf("bad delete_backups: %d %s", w.Code, w.Body)
		}

		w := do(t, h, request{authed: true, method: http.MethodDelete, target: target})
		if w.Code != http.StatusAccepted {
			t.Fatalf("delete: %d %s", w.Code, w.Body)
		}
		del := decodeJSON[gen.JobAccepted](t, w.Body.Bytes()).Job
		if del.Type != sites.DeleteJobType {
			t.Errorf("job = %s", w.Body)
		}
		e.runJobs(t)
		job, err := store.NewJobs(db).Get(ctx, del.ID)
		if err != nil {
			t.Fatal(err)
		}
		var res map[string]int
		if err := json.Unmarshal(job.Result, &res); err != nil {
			t.Fatal(err)
		}
		if job.State != store.JobSucceeded || res["forgotten_snapshots"] != 2 || res["deleted_backup_records"] != 2 {
			t.Errorf("job %s: %s, result %s", job.State, job.Error, job.Result)
		}

		// No snapshots or records of the site remain, so it is unknown to
		// the backups endpoints too; the neighbor keeps its own.
		if snaps, _ := e.engine.ListSnapshots(ctx, doomed.ID); len(snaps) != 0 {
			t.Errorf("%d snapshots of the deleted site left", len(snaps))
		}
		if w := do(t, h, request{authed: true, method: http.MethodGet, target: "/api/v1/sites/" + doomed.ID + "/backups"}); w.Code != http.StatusNotFound {
			t.Errorf("backups of the deleted site: %d %s", w.Code, w.Body)
		}
		if snaps, _ := e.engine.ListSnapshots(ctx, neighbor.ID); len(snaps) != 2 {
			t.Errorf("neighbor has %d snapshots, want 2", len(snaps))
		}
		w = do(t, h, request{authed: true, method: http.MethodGet, target: "/api/v1/sites/" + neighbor.ID + "/backups"})
		if list := decodeJSON[gen.BackupList](t, w.Body.Bytes()); len(list.Items) != 2 {
			t.Errorf("neighbor's backups: %s", w.Body)
		}

		// Without the flag, the backups stay, and the audit log tells the
		// two deletions apart.
		if w := do(t, h, request{authed: true, method: http.MethodDelete, target: "/api/v1/sites/" + neighbor.ID + "?delete_backups=false"}); w.Code != http.StatusAccepted {
			t.Fatalf("delete neighbor: %d %s", w.Code, w.Body)
		}
		e.runJobs(t)
		if snaps, _ := e.engine.ListSnapshots(ctx, neighbor.ID); len(snaps) != 2 {
			t.Errorf("deleted neighbor has %d snapshots, want 2", len(snaps))
		}

		events, err := store.NewAuditEvents(db).List(ctx, store.AuditFilter{TargetType: "site"})
		if err != nil {
			t.Fatal(err)
		}
		var withBackups, denied, plain int
		for _, ev := range events.Events {
			switch {
			case ev.Action == "site.delete_with_backups" && ev.Result == store.AuditSuccess:
				withBackups++
				if ev.TargetID != doomed.ID || ev.Details["delete_backups"] != "true" || ev.Details["job_id"] != del.ID {
					t.Errorf("event = %+v", ev)
				}
			case ev.Action == "site.delete_with_backups" && ev.Result == store.AuditDenied:
				denied++
			case ev.Action == "site.delete" && ev.Result == store.AuditSuccess:
				plain++
				if ev.TargetID != neighbor.ID || ev.Details["delete_backups"] != "false" {
					t.Errorf("event = %+v", ev)
				}
			}
		}
		if withBackups != 1 || denied != 1 || plain != 1 {
			t.Errorf("audit events: %d with backups, %d denied, %d plain; want 1 each", withBackups, denied, plain)
		}
	})
}
