package api_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/api"
	"github.com/rchouinard/pressfleet/control-plane/internal/api/gen"
	"github.com/rchouinard/pressfleet/control-plane/internal/id"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
)

func TestClientGroupsEndpoints(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		h := newHandler(t, api.Options{DB: db, Authenticator: &fakeAuth{}})
		call := func(method, target, body string) (int, []byte) {
			t.Helper()
			w := do(t, h, request{authed: true, method: method, target: target, body: body})
			return w.Code, w.Body.Bytes()
		}

		// Create.
		w := do(t, h, request{authed: true, method: http.MethodPost, target: "/api/v1/client-groups", body: `{"name":"Acme","notes":"Big client"}`})
		if w.Code != http.StatusCreated {
			t.Fatalf("create: %d %s", w.Code, w.Body)
		}
		acme := decodeJSON[gen.ClientGroup](t, w.Body.Bytes())
		if !id.Valid(acme.ID) || acme.Name != "Acme" || acme.Notes != "Big client" || acme.SiteCount != 0 ||
			acme.BackupRepository != nil || acme.CreatedAt.IsZero() {
			t.Errorf("created = %s", w.Body)
		}
		if loc := w.Header().Get("Location"); loc != "/api/v1/client-groups/"+acme.ID {
			t.Errorf("Location = %q", loc)
		}
		if code, body := call(http.MethodPost, "/api/v1/client-groups", `{"name":"Acme"}`); code != http.StatusConflict {
			t.Errorf("create duplicate: %d %s", code, body)
		}
		if code, body := call(http.MethodPost, "/api/v1/client-groups", `{"name":""}`); code != http.StatusUnprocessableEntity {
			t.Errorf("create without a name: %d %s", code, body)
		}
		for _, name := range []string{"Beta", "Charlie"} {
			if code, body := call(http.MethodPost, "/api/v1/client-groups", `{"name":"`+name+`"}`); code != http.StatusCreated {
				t.Fatalf("create %s: %d %s", name, code, body)
			}
		}

		// Get.
		code, body := call(http.MethodGet, "/api/v1/client-groups/"+acme.ID, "")
		if code != http.StatusOK {
			t.Fatalf("get: %d %s", code, body)
		}
		if got := decodeJSON[gen.ClientGroup](t, body); got.ID != acme.ID || got.Notes != "Big client" {
			t.Errorf("get = %s", body)
		}
		if code, body := call(http.MethodGet, "/api/v1/client-groups/"+id.New(), ""); code != http.StatusNotFound {
			t.Errorf("get missing: %d %s", code, body)
		}

		// Update: only the fields present change.
		code, body = call(http.MethodPatch, "/api/v1/client-groups/"+acme.ID, `{"name":"Acme Corp"}`)
		if code != http.StatusOK {
			t.Fatalf("update: %d %s", code, body)
		}
		if got := decodeJSON[gen.ClientGroup](t, body); got.Name != "Acme Corp" || got.Notes != "Big client" || !got.UpdatedAt.After(acme.UpdatedAt.Add(-time.Nanosecond)) {
			t.Errorf("update = %s", body)
		}
		code, body = call(http.MethodPatch, "/api/v1/client-groups/"+acme.ID, `{"notes":""}`)
		if got := decodeJSON[gen.ClientGroup](t, body); code != http.StatusOK || got.Name != "Acme Corp" || got.Notes != "" {
			t.Errorf("clear notes: %d %s", code, body)
		}
		if code, body := call(http.MethodPatch, "/api/v1/client-groups/"+acme.ID, `{"name":"Beta"}`); code != http.StatusConflict {
			t.Errorf("rename to a taken name: %d %s", code, body)
		}
		if code, body := call(http.MethodPatch, "/api/v1/client-groups/"+id.New(), `{"name":"x"}`); code != http.StatusNotFound {
			t.Errorf("update missing: %d %s", code, body)
		}
		if code, body := call(http.MethodPatch, "/api/v1/client-groups/"+acme.ID, `{}`); code != http.StatusUnprocessableEntity {
			t.Errorf("empty update: %d %s", code, body)
		}

		// List: by name, paged.
		var names []string
		target := "/api/v1/client-groups?limit=2"
		for range 3 {
			code, body := call(http.MethodGet, target, "")
			if code != http.StatusOK {
				t.Fatalf("list: %d %s", code, body)
			}
			l := decodeJSON[gen.ClientGroupList](t, body)
			for _, g := range l.Items {
				names = append(names, g.Name)
			}
			if l.NextCursor == nil {
				break
			}
			target = "/api/v1/client-groups?limit=2&cursor=" + *l.NextCursor
		}
		if len(names) != 3 || names[0] != "Acme Corp" || names[1] != "Beta" || names[2] != "Charlie" {
			t.Errorf("list = %v", names)
		}
		for _, c := range []string{"bogus!", "Ym9ndXM"} {
			if code, body := call(http.MethodGet, "/api/v1/client-groups?cursor="+c, ""); code != http.StatusBadRequest {
				t.Errorf("list with cursor %q: %d %s", c, code, body)
			}
		}

		// Delete: refused while the group has sites.
		now := time.Now()
		siteID := id.New()
		if _, err := db.ExecContext(ctx, `INSERT INTO sites
			(id, client_group_id, name, slug, status, preview_domain, php_version, wp_core_version, uid, created_at, updated_at)
			VALUES (?, ?, 'Site', 'site', 'running', 'site.preview.example', '8.4', '6.9.1', 20000, ?, ?)`,
			siteID, acme.ID, now, now); err != nil {
			t.Fatal(err)
		}
		code, body = call(http.MethodGet, "/api/v1/client-groups/"+acme.ID, "")
		if got := decodeJSON[gen.ClientGroup](t, body); code != http.StatusOK || got.SiteCount != 1 {
			t.Errorf("site count: %d %s", code, body)
		}
		w = do(t, h, request{authed: true, method: http.MethodDelete, target: "/api/v1/client-groups/" + acme.ID})
		if w.Code != http.StatusConflict {
			t.Fatalf("delete group with sites: %d %s", w.Code, w.Body)
		}
		if p := decodeProblem(t, w); p.Detail == "" {
			t.Errorf("conflict without detail: %+v", p)
		}
		if _, err := db.ExecContext(ctx, "DELETE FROM sites WHERE id = ?", siteID); err != nil {
			t.Fatal(err)
		}
		// Refused while the group's backup repository exists.
		groups := store.NewClientGroups(db)
		if err := groups.SetBackupRepository(ctx, acme.ID, "/data/backups/repositories/"+acme.ID); err != nil {
			t.Fatal(err)
		}
		code, body = call(http.MethodGet, "/api/v1/client-groups/"+acme.ID, "")
		if got := decodeJSON[gen.ClientGroup](t, body); code != http.StatusOK || got.BackupRepository == nil {
			t.Errorf("backup repository: %d %s", code, body)
		}
		w = do(t, h, request{authed: true, method: http.MethodDelete, target: "/api/v1/client-groups/" + acme.ID})
		if p := decodeProblem(t, w); w.Code != http.StatusConflict || !strings.Contains(p.Detail, "backup repository") {
			t.Fatalf("delete group with a backup repository: %d %+v", w.Code, p)
		}
		if err := groups.SetBackupRepository(ctx, acme.ID, ""); err != nil {
			t.Fatal(err)
		}
		if code, body := call(http.MethodDelete, "/api/v1/client-groups/"+acme.ID, ""); code != http.StatusNoContent {
			t.Fatalf("delete: %d %s", code, body)
		}
		if code, body := call(http.MethodGet, "/api/v1/client-groups/"+acme.ID, ""); code != http.StatusNotFound {
			t.Errorf("get after delete: %d %s", code, body)
		}
		if code, body := call(http.MethodDelete, "/api/v1/client-groups/"+acme.ID, ""); code != http.StatusNotFound {
			t.Errorf("delete again: %d %s", code, body)
		}

		// State-changing requests are audited with the group as the target.
		events, err := store.NewAuditEvents(db).List(ctx, store.AuditFilter{TargetType: "client_group", TargetID: acme.ID})
		if err != nil {
			t.Fatal(err)
		}
		if n := len(events.Events); n < 3 || events.Events[n-1].Action != "client_group.create" {
			t.Errorf("audit events = %+v", events.Events)
		}
	})
}
