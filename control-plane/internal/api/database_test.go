package api_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/api"
	"github.com/rchouinard/pressfleet/control-plane/internal/api/gen"
	"github.com/rchouinard/pressfleet/control-plane/internal/auth"
	"github.com/rchouinard/pressfleet/control-plane/internal/export"
	"github.com/rchouinard/pressfleet/control-plane/internal/id"
	"github.com/rchouinard/pressfleet/control-plane/internal/sites"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
)

// newExports returns an export service on the site environment's fakes,
// with its job handlers registered on the environment's pool.
func newExports(t *testing.T, db *store.DB, e *siteEnv) *export.Service {
	t.Helper()
	svc, _ := newExportsAt(t, db, e, time.Now, nil)
	return svc
}

// newExportsAt is newExports with a clock and snapshots.
func newExportsAt(t *testing.T, db *store.DB, e *siteEnv, now func() time.Time, snaps export.Snapshots) (*export.Service, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "exports")
	if snaps == nil {
		snaps = &fakeSnapshots{}
	}
	svc, err := export.New(export.Options{
		DB: db, Databases: e.dbs, Tasks: e.prov, Snapshots: snaps, Dir: dir, Now: now, Logger: storetest.Logger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range svc.Handlers() {
		if err := e.pool.Register(h); err != nil {
			t.Fatal(err)
		}
	}
	return svc, dir
}

func TestDatabaseExportEndpoints(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newSiteEnv(t, db, testPreviewBase)
		clock := &testClock{t: time.Now()}
		exports, dir := newExportsAt(t, db, e, clock.now, nil)
		opts := api.Options{DB: db, Authenticator: &fakeAuth{}, Sites: e.svc, Exports: exports}
		h := newHandler(t, opts)
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
		exportPath := "/api/v1/sites/" + site.ID + "/database/export"
		// Still provisioning: no database yet.
		if w := do(t, h, request{authed: true, method: http.MethodPost, target: exportPath}); w.Code != http.StatusConflict {
			t.Errorf("export while provisioning: %d %s", w.Code, w.Body)
		}
		e.runJobs(t)
		data := []byte("CREATE TABLE `wp_options` (x int);\nINSERT INTO `wp_options` VALUES (42);\n")
		e.dbs.SetData(site.ID, data)

		// Export: 202 with the job, whose result links to the download.
		w := do(t, h, request{authed: true, method: http.MethodPost, target: exportPath})
		if w.Code != http.StatusAccepted {
			t.Fatalf("export: %d %s", w.Code, w.Body)
		}
		job := decodeJSON[gen.JobAccepted](t, w.Body.Bytes()).Job
		if job.Type != export.JobType || job.Target.ID != site.ID || job.Target.Type != gen.JobTargetTypeSite || job.CreatedBy == nil {
			t.Errorf("job = %s", w.Body)
		}
		if loc := w.Header().Get("Location"); loc != "/api/v1/jobs/"+job.ID {
			t.Errorf("Location = %q", loc)
		}
		e.runJobs(t)
		done, err := store.NewJobs(db).Get(ctx, job.ID)
		if err != nil || done.State != store.JobSucceeded {
			t.Fatalf("export job = %+v, %v", done, err)
		}
		var result struct {
			ExportID    string `json:"export_id"`
			DownloadURL string `json:"download_url"`
			SizeBytes   int64  `json:"size_bytes"`
			ExpiresAt   string `json:"expires_at"`
		}
		if err := json.Unmarshal(done.Result, &result); err != nil {
			t.Fatal(err)
		}
		download := "/api/v1/sites/" + site.ID + "/database/exports/" + result.ExportID + "/download"
		if !id.Valid(result.ExportID) || result.DownloadURL != download || result.SizeBytes == 0 || result.ExpiresAt == "" {
			t.Fatalf("result = %s", done.Result)
		}

		// Download: the gzipped dump, as an attachment.
		w = do(t, h, request{authed: true, method: http.MethodGet, target: download})
		if w.Code != http.StatusOK {
			t.Fatalf("download: %d %s", w.Code, w.Body)
		}
		if ct, cd := w.Header().Get("Content-Type"), w.Header().Get("Content-Disposition"); ct != "application/gzip" ||
			cd != `attachment; filename=`+site.Slug+"-"+result.ExportID+".sql.gz" {
			t.Errorf("Content-Type = %q, Content-Disposition = %q", ct, cd)
		}
		if int64(w.Body.Len()) != result.SizeBytes || w.Header().Get("Content-Length") != strconv.FormatInt(result.SizeBytes, 10) {
			t.Errorf("body is %d bytes, Content-Length %q, result says %d", w.Body.Len(), w.Header().Get("Content-Length"), result.SizeBytes)
		}
		zr, err := gzip.NewReader(bytes.NewReader(w.Body.Bytes()))
		if err != nil {
			t.Fatal(err)
		}
		if got, err := io.ReadAll(zr); err != nil || !bytes.Equal(got, data) {
			t.Errorf("dump = %q, %v; want %q", got, err, data)
		}

		// Read-only tokens and viewers can't download; operators can.
		for _, p := range []auth.Principal{
			{UserID: "01j9zq3v6t8k4m2n5p7r9s1x3y", Role: store.RoleViewer, Method: auth.MethodSession, SessionID: "v"},
			{UserID: "01j9zq3v6t8k4m2n5p7r9s1x3y", Role: store.RoleAdmin, Method: auth.MethodToken, Scope: store.ScopeReadOnly, TokenID: "t"},
		} {
			o := opts
			o.Authenticator = &fakeAuth{principal: &p}
			hh := newHandler(t, o)
			for _, req := range []request{{method: http.MethodPost, target: exportPath}, {method: http.MethodGet, target: download}} {
				req.authed = true
				if w := do(t, hh, req); w.Code != http.StatusForbidden {
					t.Errorf("%s %s as %s/%s: %d %s", req.method, req.target, p.Role, p.Scope, w.Code, w.Body)
				}
			}
		}
		o := opts
		o.Authenticator = &fakeAuth{principal: &auth.Principal{
			UserID: "01j9zq3v6t8k4m2n5p7r9s1x3y", Role: store.RoleOperator, Method: auth.MethodSession, SessionID: "o",
		}}
		if w := do(t, newHandler(t, o), request{authed: true, method: http.MethodGet, target: download}); w.Code != http.StatusOK {
			t.Errorf("operator download: %d %s", w.Code, w.Body)
		}

		// Unknown exports, another site's, and missing sites are 404.
		for _, target := range []string{
			"/api/v1/sites/" + site.ID + "/database/exports/" + id.New() + "/download",
			"/api/v1/sites/" + id.New() + "/database/exports/" + result.ExportID + "/download",
		} {
			if w := do(t, h, request{authed: true, method: http.MethodGet, target: target}); w.Code != http.StatusNotFound {
				t.Errorf("GET %s: %d %s", target, w.Code, w.Body)
			}
		}
		if w := do(t, h, request{authed: true, method: http.MethodPost, target: "/api/v1/sites/" + id.New() + "/database/export"}); w.Code != http.StatusNotFound {
			t.Errorf("export of a missing site: %d %s", w.Code, w.Body)
		}

		// Once expired, the download is 404 and the file is gone.
		clock.add(export.DefaultTTL)
		w = do(t, h, request{authed: true, method: http.MethodGet, target: download})
		if w.Code != http.StatusNotFound {
			t.Errorf("expired download: %d %s", w.Code, w.Body)
		}
		if p := decodeProblem(t, w); !strings.Contains(p.Detail, "expired") {
			t.Errorf("expired download detail = %q", p.Detail)
		}
		if left, err := os.ReadDir(dir); err != nil || len(left) != 0 {
			t.Errorf("exports directory after expiry = %v, %v", left, err)
		}
		if got, err := store.NewExports(db).Get(ctx, result.ExportID); err != nil || got.Status != store.ExportExpired {
			t.Errorf("expired export = %+v, %v", got, err)
		}

		// The audit log records the export and every download, including
		// denied and failed ones.
		events, err := store.NewAuditEvents(db).List(ctx, store.AuditFilter{TargetType: "site", TargetID: site.ID})
		if err != nil {
			t.Fatal(err)
		}
		count := map[string]int{}
		for _, ev := range events.Events {
			count[ev.Action+":"+string(ev.Result)]++
		}
		want := map[string]int{
			"site.database.export:success":          1,
			"site.database.export:failure":          1, // while provisioning
			"site.database.export:denied":           2,
			"site.database.export.download:success": 2,
			"site.database.export.download:failure": 2, // the unknown export and the expired one
			"site.database.export.download:denied":  2,
		}
		for k, n := range want {
			if count[k] != n {
				t.Errorf("%s audit events = %d, want %d (all: %v)", k, count[k], n, count)
			}
		}
	})
}
