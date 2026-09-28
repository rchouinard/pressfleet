package api_test

import (
	"context"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/api"
	"github.com/rchouinard/pressfleet/control-plane/internal/api/gen"
	"github.com/rchouinard/pressfleet/control-plane/internal/auth"
	"github.com/rchouinard/pressfleet/control-plane/internal/backup/sitebackup"
	"github.com/rchouinard/pressfleet/control-plane/internal/id"
	"github.com/rchouinard/pressfleet/control-plane/internal/sites"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
)

func TestSiteBackupsEndpoints(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newSiteEnv(t, db, testPreviewBase)
		h := newHandler(t, api.Options{DB: db, Authenticator: &fakeAuth{}, Sites: e.svc, Backups: e.backups})
		// Jobs record the user who started them: fakeAuth's admin.
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
		// Still provisioning: nothing to back up yet.
		backups := "/api/v1/sites/" + site.ID + "/backups"
		if w := do(t, h, request{authed: true, method: http.MethodPost, target: backups}); w.Code != http.StatusConflict {
			t.Errorf("backup while provisioning: %d %s", w.Code, w.Body)
		}
		e.runJobs(t)

		w := do(t, h, request{authed: true, method: http.MethodGet, target: backups})
		if list := decodeJSON[gen.BackupList](t, w.Body.Bytes()); w.Code != http.StatusOK || list.Items == nil || len(list.Items) != 0 || list.NextCursor != nil {
			t.Errorf("empty list: %d %s", w.Code, w.Body)
		}

		// Back up: 202 with the job.
		w = do(t, h, request{authed: true, method: http.MethodPost, target: backups})
		if w.Code != http.StatusAccepted {
			t.Fatalf("backup: %d %s", w.Code, w.Body)
		}
		job := decodeJSON[gen.JobAccepted](t, w.Body.Bytes()).Job
		if job.Type != sitebackup.JobType || job.Target.ID != site.ID || job.Target.Type != gen.JobTargetTypeSite ||
			job.State != gen.JobStateQueued || job.CreatedBy == nil {
			t.Errorf("job = %s", w.Body)
		}
		if loc := w.Header().Get("Location"); loc != "/api/v1/jobs/"+job.ID {
			t.Errorf("Location = %q", loc)
		}
		e.runJobs(t)

		w = do(t, h, request{authed: true, method: http.MethodGet, target: backups})
		list := decodeJSON[gen.BackupList](t, w.Body.Bytes())
		if w.Code != http.StatusOK || len(list.Items) != 1 {
			t.Fatalf("list: %d %s", w.Code, w.Body)
		}
		b := list.Items[0]
		if !id.Valid(b.ID) || b.SiteID != site.ID || b.ClientGroupID != group.ID || b.Kind != gen.Manual ||
			b.Status != gen.BackupStatusSucceeded || b.SnapshotID == nil || *b.SnapshotID == "" || b.WpCoreVersion != site.WPCoreVersion ||
			b.SizeBytes == nil || *b.SizeBytes != 1024 || b.AddedBytes == nil || b.FileCount == nil || b.JobID == nil || *b.JobID != job.ID ||
			b.FinishedAt == nil || b.Message != "" {
			t.Errorf("backup = %s", w.Body)
		}

		// A failed backup has no snapshot or sizes; filters and paging.
		e.engine.Err = func(op, _ string) error {
			if op == "Backup" {
				return sitebackup.ErrNotBackupable // any error; retried until it gives up
			}
			return nil
		}
		if w := do(t, h, request{authed: true, method: http.MethodPost, target: backups}); w.Code != http.StatusAccepted {
			t.Fatalf("second backup: %d %s", w.Code, w.Body)
		}
		e.runJobs(t)
		w = do(t, h, request{authed: true, method: http.MethodGet, target: backups + "?status=failed"})
		list = decodeJSON[gen.BackupList](t, w.Body.Bytes())
		if len(list.Items) != 1 || list.Items[0].Status != gen.BackupStatusFailed || list.Items[0].SnapshotID != nil ||
			list.Items[0].SizeBytes != nil || list.Items[0].Message == "" {
			t.Errorf("failed backups: %s", w.Body)
		}
		w = do(t, h, request{authed: true, method: http.MethodGet, target: backups + "?limit=1"})
		list = decodeJSON[gen.BackupList](t, w.Body.Bytes())
		if len(list.Items) != 1 || list.NextCursor == nil || list.Items[0].Status != gen.BackupStatusFailed {
			t.Fatalf("first page: %s", w.Body)
		}
		w = do(t, h, request{authed: true, method: http.MethodGet, target: backups + "?limit=1&cursor=" + *list.NextCursor})
		if list := decodeJSON[gen.BackupList](t, w.Body.Bytes()); len(list.Items) != 1 || list.Items[0].ID != b.ID || list.NextCursor != nil {
			t.Errorf("second page: %s", w.Body)
		}
		if w := do(t, h, request{authed: true, method: http.MethodGet, target: backups + "?cursor=nope"}); w.Code != http.StatusBadRequest {
			t.Errorf("bad cursor: %d %s", w.Code, w.Body)
		}
		if w := do(t, h, request{authed: true, method: http.MethodGet, target: backups + "?status=paused"}); w.Code != http.StatusBadRequest {
			t.Errorf("bad status: %d %s", w.Code, w.Body)
		}

		// Missing sites.
		missing := "/api/v1/sites/" + id.New() + "/backups"
		for _, method := range []string{http.MethodGet, http.MethodPost} {
			if w := do(t, h, request{authed: true, method: method, target: missing}); w.Code != http.StatusNotFound {
				t.Errorf("%s of a missing site: %d %s", method, w.Code, w.Body)
			}
		}

		// The audit log records the backups taken.
		events, err := store.NewAuditEvents(db).List(ctx, store.AuditFilter{TargetType: "site", TargetID: site.ID})
		if err != nil {
			t.Fatal(err)
		}
		var actions []string
		for _, ev := range events.Events {
			if ev.Action == "backup.create" && ev.Result == store.AuditSuccess {
				actions = append(actions, ev.Action)
			}
		}
		if len(actions) != 2 {
			t.Errorf("successful backup.create audit events = %d, want 2", len(actions))
		}

		// Viewers may list backups but not take them; operators may.
		viewer := newHandler(t, api.Options{DB: db, Authenticator: &fakeAuth{principal: &auth.Principal{
			UserID: "01j9zq3v6t8k4m2n5p7r9s1x3y", Role: store.RoleViewer, Method: auth.MethodSession, SessionID: "v",
		}}, Sites: e.svc, Backups: e.backups})
		if w := do(t, viewer, request{authed: true, method: http.MethodGet, target: backups}); w.Code != http.StatusOK {
			t.Errorf("viewer list: %d %s", w.Code, w.Body)
		}
		if w := do(t, viewer, request{authed: true, method: http.MethodPost, target: backups}); w.Code != http.StatusForbidden {
			t.Errorf("viewer backup: %d %s", w.Code, w.Body)
		}
		operator := newHandler(t, api.Options{DB: db, Authenticator: &fakeAuth{principal: &auth.Principal{
			UserID: "01j9zq3v6t8k4m2n5p7r9s1x3y", Role: store.RoleOperator, Method: auth.MethodSession, SessionID: "o",
		}}, Sites: e.svc, Backups: e.backups})
		if w := do(t, operator, request{authed: true, method: http.MethodPost, target: backups}); w.Code != http.StatusAccepted {
			t.Errorf("operator backup: %d %s", w.Code, w.Body)
		}
		events, err = store.NewAuditEvents(db).List(ctx, store.AuditFilter{TargetType: "site", TargetID: site.ID})
		if err != nil {
			t.Fatal(err)
		}
		if !slices.ContainsFunc(events.Events, func(ev store.AuditEvent) bool {
			return ev.Action == "backup.create" && ev.Result == store.AuditDenied
		}) {
			t.Error("the viewer's denied backup isn't in the audit log")
		}
	})
}
