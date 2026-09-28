package api_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/api"
	"github.com/rchouinard/pressfleet/control-plane/internal/api/gen"
	"github.com/rchouinard/pressfleet/control-plane/internal/auth"
	"github.com/rchouinard/pressfleet/control-plane/internal/authz"
	"github.com/rchouinard/pressfleet/control-plane/internal/backup"
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
			b.FinishedAt == nil || b.Message != "" || b.ExpiredAt != nil {
			t.Errorf("backup = %s", w.Body)
		}

		// Once retention removed its snapshot, the backup says so.
		if _, err := store.NewBackupRecords(db).Expire(ctx, []string{b.ID}); err != nil {
			t.Fatal(err)
		}
		w = do(t, h, request{authed: true, method: http.MethodGet, target: backups})
		if list := decodeJSON[gen.BackupList](t, w.Body.Bytes()); len(list.Items) != 1 || list.Items[0].ExpiredAt == nil {
			t.Errorf("expired backup = %s", w.Body)
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

func TestSiteBackupScheduleEndpoints(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newSiteEnv(t, db, testPreviewBase)
		h := newHandler(t, api.Options{DB: db, Authenticator: &fakeAuth{}, Sites: e.svc, Backups: e.backups})
		group := &store.ClientGroup{Name: "Acme"}
		if err := store.NewClientGroups(db).Create(ctx, group); err != nil {
			t.Fatal(err)
		}
		site, _, err := e.svc.Create(ctx, sites.NewSite{ClientGroupID: group.ID, Name: "Acme", AdminEmail: "o@example.com", PHPVersion: "8.4"})
		if err != nil {
			t.Fatal(err)
		}
		target := "/api/v1/sites/" + site.ID + "/backup-schedule"

		// New sites are backed up daily in the default window.
		w := do(t, h, request{authed: true, method: http.MethodGet, target: target})
		got := decodeJSON[gen.BackupSchedule](t, w.Body.Bytes())
		if w.Code != http.StatusOK || !got.Enabled || got.IntervalHours != 24 || got.Hour != nil || got.NextRunAt == nil {
			t.Fatalf("default schedule: %d %s", w.Code, w.Body)
		}
		if h := got.NextRunAt.UTC().Hour(); h < 2 || h >= 6 {
			t.Errorf("next run at %s, want in the 02:00-06:00 UTC window", got.NextRunAt)
		}

		put := func(body string) *httptest.ResponseRecorder {
			return do(t, h, request{authed: true, method: http.MethodPut, target: target, body: body, contentType: "application/json"})
		}
		w = put(`{"enabled": true, "interval_hours": 12, "hour": 3}`)
		got = decodeJSON[gen.BackupSchedule](t, w.Body.Bytes())
		if w.Code != http.StatusOK || got.IntervalHours != 12 || got.Hour == nil || *got.Hour != 3 || got.NextRunAt == nil ||
			got.NextRunAt.UTC().Hour()%12 != 3 {
			t.Errorf("12-hourly at 3: %d %s", w.Code, w.Body)
		}
		w = put(`{"enabled": false}`)
		got = decodeJSON[gen.BackupSchedule](t, w.Body.Bytes())
		if w.Code != http.StatusOK || got.Enabled || got.IntervalHours != 24 || got.Hour != nil || got.NextRunAt != nil {
			t.Errorf("disabled: %d %s", w.Code, w.Body)
		}
		if w := do(t, h, request{authed: true, method: http.MethodGet, target: target}); !strings.Contains(w.Body.String(), `"next_run_at":null`) {
			t.Errorf("disabled schedule: %s", w.Body)
		}

		for _, body := range []string{
			`{"enabled": true, "interval_hours": 5}`,
			`{"enabled": true, "interval_hours": 200}`,
			`{"enabled": true, "hour": 24}`,
			`{"interval_hours": 24}`,
			`{"enabled": true, "minute": 5}`,
		} {
			if w := put(body); w.Code != http.StatusUnprocessableEntity && w.Code != http.StatusBadRequest {
				t.Errorf("PUT %s: %d %s", body, w.Code, w.Body)
			}
		}
		if w := put(`{"enabled": true, "interval_hours": 5}`); w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), "/interval_hours") {
			t.Errorf("bad interval: %d %s", w.Code, w.Body)
		}

		missing := "/api/v1/sites/" + id.New() + "/backup-schedule"
		if w := do(t, h, request{authed: true, method: http.MethodGet, target: missing}); w.Code != http.StatusNotFound {
			t.Errorf("GET of a missing site: %d %s", w.Code, w.Body)
		}
		if w := do(t, h, request{authed: true, method: http.MethodPut, target: missing, body: `{"enabled": true}`, contentType: "application/json"}); w.Code != http.StatusNotFound {
			t.Errorf("PUT of a missing site: %d %s", w.Code, w.Body)
		}

		// Changes are audited as site updates; viewers may only look.
		events, err := store.NewAuditEvents(db).List(ctx, store.AuditFilter{TargetType: "site", TargetID: site.ID})
		if err != nil {
			t.Fatal(err)
		}
		if !slices.ContainsFunc(events.Events, func(ev store.AuditEvent) bool {
			return ev.Action == "site.update" && ev.Result == store.AuditSuccess
		}) {
			t.Error("the schedule change isn't in the audit log")
		}
		viewer := newHandler(t, api.Options{DB: db, Authenticator: &fakeAuth{principal: &auth.Principal{
			UserID: "01j9zq3v6t8k4m2n5p7r9s1x3y", Role: store.RoleViewer, Method: auth.MethodSession, SessionID: "v",
		}}, Sites: e.svc, Backups: e.backups})
		if w := do(t, viewer, request{authed: true, method: http.MethodGet, target: target}); w.Code != http.StatusOK {
			t.Errorf("viewer GET: %d %s", w.Code, w.Body)
		}
		if w := do(t, viewer, request{authed: true, method: http.MethodPut, target: target, body: `{"enabled": true}`, contentType: "application/json"}); w.Code != http.StatusForbidden {
			t.Errorf("viewer PUT: %d %s", w.Code, w.Body)
		}
	})
}

func TestSiteRestoreEndpoint(t *testing.T) {
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
		snap, err := e.engine.Backup(ctx, site.ID, backup.Options{Kind: "manual"})
		if err != nil {
			t.Fatal(err)
		}
		restore := "/api/v1/sites/" + site.ID + "/restore"

		// Problems with the request: 422 for the body, the snapshot,
		// or the source, 404 for the site.
		for _, tt := range []struct {
			target, body string
			code         int
			field        string
		}{
			{restore, `{}`, http.StatusUnprocessableEntity, ""},
			{restore, `{"snapshot_id":"x","extra":1}`, http.StatusUnprocessableEntity, ""},
			{restore, `{"snapshot_id":"nope-nope-nope"}`, http.StatusUnprocessableEntity, "/snapshot_id"},
			{restore, `{"snapshot_id":"` + string(snap.ID) + `","source_site_id":"` + id.New() + `"}`, http.StatusUnprocessableEntity, "/source_site_id"},
			{"/api/v1/sites/" + id.New() + "/restore", `{"snapshot_id":"` + string(snap.ID) + `"}`, http.StatusNotFound, ""},
		} {
			w := do(t, h, request{authed: true, method: http.MethodPost, target: tt.target, body: tt.body})
			if w.Code != tt.code {
				t.Errorf("restore %s: %d %s, want %d", tt.body, w.Code, w.Body, tt.code)
				continue
			}
			if tt.field != "" {
				p := decodeJSON[gen.Problem](t, w.Body.Bytes())
				if p.Errors == nil || len(*p.Errors) != 1 || (*p.Errors)[0].Field != tt.field {
					t.Errorf("restore %s: problem %s, want an error on %s", tt.body, w.Body, tt.field)
				}
			}
		}

		// Viewers may not restore.
		viewer := newHandler(t, api.Options{DB: db, Authenticator: &fakeAuth{principal: &auth.Principal{
			UserID: "01j9zq3v6t8k4m2n5p7r9s1x3y", Role: store.RoleViewer, Method: auth.MethodSession, SessionID: "v",
		}}, Sites: e.svc, Backups: e.backups})
		body := `{"snapshot_id":"` + string(snap.ID) + `"}`
		if w := do(t, viewer, request{authed: true, method: http.MethodPost, target: restore, body: body}); w.Code != http.StatusForbidden {
			t.Errorf("viewer restore: %d %s", w.Code, w.Body)
		}

		// Operators may: 202 with the site, now updating, and the job.
		operator := newHandler(t, api.Options{DB: db, Authenticator: &fakeAuth{principal: &auth.Principal{
			UserID: "01j9zq3v6t8k4m2n5p7r9s1x3y", Role: store.RoleOperator, Method: auth.MethodSession, SessionID: "o",
		}}, Sites: e.svc, Backups: e.backups})
		w := do(t, operator, request{authed: true, method: http.MethodPost, target: restore, body: body})
		if w.Code != http.StatusAccepted {
			t.Fatalf("restore: %d %s", w.Code, w.Body)
		}
		accepted := decodeJSON[gen.SiteAccepted](t, w.Body.Bytes())
		if accepted.Site.ID != site.ID || accepted.Site.Status != gen.SiteStatusUpdating || accepted.Job.Type != sites.RestoreJobType ||
			accepted.Job.Target.ID != site.ID || accepted.Job.State != gen.JobStateQueued || accepted.Job.CreatedBy == nil {
			t.Errorf("restore response = %s", w.Body)
		}
		if loc := w.Header().Get("Location"); loc != "/api/v1/jobs/"+accepted.Job.ID {
			t.Errorf("Location = %q", loc)
		}
		// A second restore waits for the first: the site is busy.
		if w := do(t, h, request{authed: true, method: http.MethodPost, target: restore, body: body}); w.Code != http.StatusConflict {
			t.Errorf("restore while restoring: %d %s", w.Code, w.Body)
		}

		events, err := store.NewAuditEvents(db).List(ctx, store.AuditFilter{TargetType: "site", TargetID: site.ID})
		if err != nil {
			t.Fatal(err)
		}
		results := map[store.AuditResult]int{}
		for _, ev := range events.Events {
			if ev.Action == string(authz.RestoreBackup) {
				results[ev.Result]++
			}
		}
		if results[store.AuditSuccess] != 1 || results[store.AuditDenied] != 1 || results[store.AuditFailure] < 3 {
			t.Errorf("backup.restore audit events = %v", results)
		}
	})
}
