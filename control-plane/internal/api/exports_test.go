package api_test

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/api"
	"github.com/rchouinard/pressfleet/control-plane/internal/api/gen"
	"github.com/rchouinard/pressfleet/control-plane/internal/auth"
	"github.com/rchouinard/pressfleet/control-plane/internal/backup"
	"github.com/rchouinard/pressfleet/control-plane/internal/export"
	"github.com/rchouinard/pressfleet/control-plane/internal/id"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
	"github.com/rchouinard/pressfleet/control-plane/internal/sites"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
)

// fakeSnapshots is an export.Snapshots whose every snapshot holds the same
// wp-content and database.
type fakeSnapshots struct {
	mu sync.Mutex
	// known maps snapshot IDs to their sites.
	known map[backup.SnapshotID]string
}

func (f *fakeSnapshots) add(snap backup.SnapshotID, siteID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.known == nil {
		f.known = map[backup.SnapshotID]string{}
	}
	f.known[snap] = siteID
}

func (f *fakeSnapshots) check(siteID string, snap backup.SnapshotID) (backup.Snapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.known[snap] != siteID {
		return backup.Snapshot{}, backup.ErrSnapshotNotFound
	}
	return backup.Snapshot{ID: snap, SiteID: siteID, Time: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)}, nil
}

func (f *fakeSnapshots) DumpFiles(_ context.Context, siteID string, snap backup.SnapshotID, w io.Writer) (backup.Snapshot, error) {
	s, err := f.check(siteID, snap)
	if err != nil {
		return s, err
	}
	_, err = w.Write(wpContentTar(map[string]string{"index.php": "<?php // from the snapshot"}))
	return s, err
}

func (f *fakeSnapshots) DumpDatabase(_ context.Context, siteID string, snap backup.SnapshotID, w io.Writer) (backup.Snapshot, error) {
	s, err := f.check(siteID, snap)
	if err != nil {
		return s, err
	}
	_, err = io.WriteString(w, "-- the snapshot's dump\n")
	return s, err
}

// wpContentTar is a tar of files with paths relative to wp-content.
func wpContentTar(files map[string]string) []byte {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for name, content := range files {
		_ = tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(content))})
		_, _ = io.WriteString(tw, content)
	}
	_ = tw.Close()
	return buf.Bytes()
}

// exportJobResult is what the tests read of an export job's result.
type exportJobResult struct {
	ExportID  string `json:"export_id"`
	ExportURL string `json:"export_url"`
	Files     []struct {
		Name        string `json:"name"`
		DownloadURL string `json:"download_url"`
	} `json:"files"`
}

func TestSiteExportEndpoints(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newSiteEnv(t, db, testPreviewBase)
		snaps := &fakeSnapshots{}
		clock := &testClock{t: time.Now()}
		exports, _ := newExportsAt(t, db, e, clock.now, snaps)
		opts := api.Options{DB: db, Authenticator: &fakeAuth{}, Sites: e.svc, Backups: e.backups, Exports: exports}
		h := newHandler(t, opts)
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
		e.dbs.SetData(site.ID, []byte("-- the live dump\n"))
		e.prov.RunTaskFunc = func(_ string, task provisioner.Task) (*provisioner.TaskResult, error) {
			switch task.Command[0] {
			case "du":
				return &provisioner.TaskResult{Stdout: "8\t/site/wp-content\n"}, nil
			case "tar":
				_, err := task.Stdout.Write(wpContentTar(map[string]string{
					"./index.php": strings.Repeat("<?php // live\n", 200), "./object-cache.php": "<?php",
				}))
				return &provisioner.TaskResult{}, err
			}
			return &provisioner.TaskResult{}, nil
		}

		// Export the live site.
		w := do(t, h, request{authed: true, method: http.MethodPost, target: "/api/v1/sites/" + site.ID + "/export"})
		if w.Code != http.StatusAccepted {
			t.Fatalf("export: %d %s", w.Code, w.Body)
		}
		job := decodeJSON[gen.JobAccepted](t, w.Body.Bytes()).Job
		if job.Type != export.SiteJobType || job.Target.ID != site.ID || w.Header().Get("Location") != "/api/v1/jobs/"+job.ID {
			t.Errorf("job = %s", w.Body)
		}
		res := runExportJob(t, db, e, job.ID)
		if len(res.Files) != 2 || res.ExportURL != "/api/v1/exports/"+res.ExportID {
			t.Fatalf("result = %+v", res)
		}

		// The export and its files.
		w = do(t, h, request{authed: true, method: http.MethodGet, target: res.ExportURL})
		if w.Code != http.StatusOK {
			t.Fatalf("get export: %d %s", w.Code, w.Body)
		}
		got := decodeJSON[gen.Export](t, w.Body.Bytes())
		if got.Kind != gen.ExportKindSite || got.Status != gen.ExportStatusReady || got.SiteSlug != site.Slug || len(got.Files) != 2 ||
			got.Source.SiteURL != "https://"+site.PreviewDomain || got.Source.PhpVersion != "8.4" || got.BackupID != nil || got.SizeBytes == nil {
			t.Errorf("export = %s", w.Body)
		}
		w = do(t, h, request{authed: true, method: http.MethodGet, target: "/api/v1/exports?site_id=" + site.ID + "&kind=site,snapshot"})
		if list := decodeJSON[gen.ExportList](t, w.Body.Bytes()); w.Code != http.StatusOK || len(list.Items) != 1 || list.Items[0].ID != res.ExportID {
			t.Errorf("list: %d %s", w.Code, w.Body)
		}
		if w := do(t, h, request{authed: true, method: http.MethodGet, target: "/api/v1/exports"}); w.Code != http.StatusBadRequest {
			t.Errorf("list without a filter: %d %s", w.Code, w.Body)
		}

		// Download: the whole file, then the rest of it from an offset, as
		// a resumed download asks.
		archive := "/api/v1/exports/" + res.ExportID + "/files/wp-content.tar.gz"
		w = do(t, h, request{authed: true, method: http.MethodGet, target: archive})
		if w.Code != http.StatusOK {
			t.Fatalf("download: %d %s", w.Code, w.Body)
		}
		full, etag := w.Body.Bytes(), w.Header().Get("ETag")
		if ct, cd, ar := w.Header().Get("Content-Type"), w.Header().Get("Content-Disposition"), w.Header().Get("Accept-Ranges"); ct != "application/gzip" ||
			cd != "attachment; filename="+site.Slug+"-wp-content.tar.gz" || ar != "bytes" || etag == "" {
			t.Errorf("headers = %v", w.Header())
		}
		w = do(t, h, request{authed: true, method: http.MethodGet, target: archive, header: map[string]string{"Range": "bytes=10-", "If-Range": etag}})
		if w.Code != http.StatusPartialContent || !bytes.Equal(w.Body.Bytes(), full[10:]) ||
			w.Header().Get("Content-Range") != "bytes 10-"+itoa(len(full)-1)+"/"+itoa(len(full)) {
			t.Errorf("range: %d, %d bytes, Content-Range %q", w.Code, w.Body.Len(), w.Header().Get("Content-Range"))
		}
		// A range taken from another file gets the whole file.
		w = do(t, h, request{authed: true, method: http.MethodGet, target: archive, header: map[string]string{"Range": "bytes=10-", "If-Range": `"other"`}})
		if w.Code != http.StatusOK || !bytes.Equal(w.Body.Bytes(), full) {
			t.Errorf("range with another ETag: %d, %d bytes", w.Code, w.Body.Len())
		}
		w = do(t, h, request{authed: true, method: http.MethodGet, target: archive, header: map[string]string{"Range": "bytes=999999-"}})
		if w.Code != http.StatusRequestedRangeNotSatisfiable {
			t.Errorf("range past the end: %d", w.Code)
		}
		if w := do(t, h, request{authed: true, method: http.MethodGet, target: "/api/v1/exports/" + id.New() + "/files/database.sql.gz"}); w.Code != http.StatusNotFound {
			t.Errorf("download of a missing export: %d %s", w.Code, w.Body)
		}

		// Viewers see exports but can't start or download one, nor can
		// read-only tokens.
		for _, p := range []auth.Principal{
			{UserID: "01j9zq3v6t8k4m2n5p7r9s1x3y", Role: store.RoleViewer, Method: auth.MethodSession, SessionID: "v"},
			{UserID: "01j9zq3v6t8k4m2n5p7r9s1x3y", Role: store.RoleAdmin, Method: auth.MethodToken, Scope: store.ScopeReadOnly, TokenID: "t"},
		} {
			o := opts
			o.Authenticator = &fakeAuth{principal: &p}
			hh := newHandler(t, o)
			if w := do(t, hh, request{authed: true, method: http.MethodGet, target: res.ExportURL}); w.Code != http.StatusOK {
				t.Errorf("get export as %s/%s: %d", p.Role, p.Scope, w.Code)
			}
			for _, req := range []request{
				{method: http.MethodPost, target: "/api/v1/sites/" + site.ID + "/export"},
				{method: http.MethodGet, target: archive},
			} {
				req.authed = true
				if w := do(t, hh, req); w.Code != http.StatusForbidden {
					t.Errorf("%s %s as %s/%s: %d %s", req.method, req.target, p.Role, p.Scope, w.Code, w.Body)
				}
			}
		}

		// Export one of the site's backups.
		records := store.NewBackupRecords(db)
		backupOf := func(siteID, slug string) store.BackupRecord {
			t.Helper()
			rec := store.BackupRecord{SiteID: siteID, SiteName: "Old " + slug, SiteSlug: slug, ClientGroupID: group.ID, Kind: store.BackupManual, WPCoreVersion: "6.9.1"}
			if err := records.Create(ctx, &rec); err != nil {
				t.Fatal(err)
			}
			snap := strings.Repeat("ab", 32)[:56] + strings.ToLower(id.New())[18:]
			if _, err := records.Succeed(ctx, rec.ID, store.BackupResult{SnapshotID: snap, SizeBytes: 100}); err != nil {
				t.Fatal(err)
			}
			snaps.add(backup.SnapshotID(snap), siteID)
			rec, err := records.Get(ctx, rec.ID)
			if err != nil {
				t.Fatal(err)
			}
			return rec
		}
		rec := backupOf(site.ID, site.Slug)
		w = do(t, h, request{authed: true, method: http.MethodPost, target: "/api/v1/sites/" + site.ID + "/backups/" + rec.ID + "/export"})
		if w.Code != http.StatusAccepted {
			t.Fatalf("backup export: %d %s", w.Code, w.Body)
		}
		res = runExportJob(t, db, e, decodeJSON[gen.JobAccepted](t, w.Body.Bytes()).Job.ID)
		got = decodeJSON[gen.Export](t, do(t, h, request{authed: true, method: http.MethodGet, target: res.ExportURL}).Body.Bytes())
		if got.Kind != gen.ExportKindSnapshot || got.BackupID == nil || *got.BackupID != rec.ID || got.Source.TakenAt == nil {
			t.Errorf("backup export = %+v", got)
		}

		// A deleted site's backups are listed and exported under its group.
		gone := backupOf(id.New(), "gone")
		w = do(t, h, request{authed: true, method: http.MethodGet, target: "/api/v1/client-groups/" + group.ID + "/backups?deleted=true"})
		list := decodeJSON[gen.BackupList](t, w.Body.Bytes())
		if w.Code != http.StatusOK || len(list.Items) != 1 || list.Items[0].ID != gone.ID || list.Items[0].SiteSlug != "gone" || list.Items[0].SiteName != "Old gone" {
			t.Errorf("deleted sites' backups: %d %s", w.Code, w.Body)
		}
		w = do(t, h, request{authed: true, method: http.MethodGet, target: "/api/v1/client-groups/" + group.ID + "/backups"})
		if list := decodeJSON[gen.BackupList](t, w.Body.Bytes()); w.Code != http.StatusOK || len(list.Items) != 2 {
			t.Errorf("group's backups: %d %s", w.Code, w.Body)
		}
		w = do(t, h, request{authed: true, method: http.MethodPost, target: "/api/v1/client-groups/" + group.ID + "/backups/" + gone.ID + "/export"})
		if w.Code != http.StatusAccepted {
			t.Fatalf("deleted site's backup export: %d %s", w.Code, w.Body)
		}
		job = decodeJSON[gen.JobAccepted](t, w.Body.Bytes()).Job
		if job.Target.Type != gen.JobTargetTypeClientGroup || job.Target.ID != group.ID {
			t.Errorf("job = %+v", job)
		}
		res = runExportJob(t, db, e, job.ID)
		w = do(t, h, request{authed: true, method: http.MethodGet, target: "/api/v1/exports/" + res.ExportID + "/files/database.sql.gz"})
		if w.Code != http.StatusOK || w.Header().Get("Content-Disposition") != "attachment; filename=gone-database.sql.gz" {
			t.Errorf("deleted site's download: %d %v", w.Code, w.Header())
		}
		w = do(t, h, request{authed: true, method: http.MethodGet, target: "/api/v1/exports?client_group_id=" + group.ID})
		if list := decodeJSON[gen.ExportList](t, w.Body.Bytes()); w.Code != http.StatusOK || len(list.Items) != 3 {
			t.Errorf("group's exports: %d %s", w.Code, w.Body)
		}

		// Backups of another site or group are not found; a backup without
		// a snapshot can't be exported.
		running := store.BackupRecord{SiteID: site.ID, ClientGroupID: group.ID, Kind: store.BackupManual}
		if err := records.Create(ctx, &running); err != nil {
			t.Fatal(err)
		}
		for target, want := range map[string]int{
			"/api/v1/sites/" + site.ID + "/backups/" + gone.ID + "/export":         http.StatusNotFound,
			"/api/v1/sites/" + site.ID + "/backups/" + id.New() + "/export":        http.StatusNotFound,
			"/api/v1/client-groups/" + id.New() + "/backups/" + rec.ID + "/export": http.StatusNotFound,
			"/api/v1/sites/" + site.ID + "/backups/" + running.ID + "/export":      http.StatusConflict,
			"/api/v1/sites/" + id.New() + "/export":                                http.StatusNotFound,
		} {
			if w := do(t, h, request{authed: true, method: http.MethodPost, target: target}); w.Code != want {
				t.Errorf("POST %s: %d %s, want %d", target, w.Code, w.Body, want)
			}
		}
		if w := do(t, h, request{authed: true, method: http.MethodGet, target: "/api/v1/client-groups/" + id.New() + "/backups"}); w.Code != http.StatusNotFound {
			t.Errorf("backups of a missing group: %d %s", w.Code, w.Body)
		}

		// Expired exports say so, and their files are gone.
		clock.add(export.DefaultSiteTTL)
		got = decodeJSON[gen.Export](t, do(t, h, request{authed: true, method: http.MethodGet, target: res.ExportURL}).Body.Bytes())
		if got.Status != gen.ExportStatusExpired || len(got.Files) != 0 || got.ExpiredAt == nil {
			t.Errorf("expired export = %+v", got)
		}
		if w := do(t, h, request{authed: true, method: http.MethodGet, target: archive}); w.Code != http.StatusNotFound {
			t.Errorf("expired download: %d", w.Code)
		}

		// The audit log records every export and download, by export.
		events, err := store.NewAuditEvents(db).List(ctx, store.AuditFilter{Action: "site.export"})
		if err != nil {
			t.Fatal(err)
		}
		count := map[string]int{}
		for _, ev := range events.Events {
			count[ev.TargetType+":"+string(ev.Result)]++
		}
		want := map[string]int{
			"site:success": 2, "client_group:success": 1, "site:denied": 2, "site:failure": 4, "client_group:failure": 1,
			"export:success": 4, "export:denied": 2, "export:failure": 3,
		}
		for k, n := range want {
			if count[k] != n {
				t.Errorf("%s audit events = %d, want %d (all: %v)", k, count[k], n, count)
			}
		}
	})
}

// runExportJob runs the queued jobs and returns the export job's result.
func runExportJob(t *testing.T, db *store.DB, e *siteEnv, jobID string) exportJobResult {
	t.Helper()
	e.runJobs(t)
	done, err := store.NewJobs(db).Get(context.Background(), jobID)
	if err != nil {
		t.Fatal(err)
	}
	if done.State != store.JobSucceeded {
		t.Fatalf("export job = %s: %s", done.State, done.Error)
	}
	var res exportJobResult
	if err := json.Unmarshal(done.Result, &res); err != nil {
		t.Fatal(err)
	}
	return res
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}
