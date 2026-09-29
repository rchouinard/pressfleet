package api_test

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/api"
	"github.com/rchouinard/pressfleet/control-plane/internal/api/gen"
	"github.com/rchouinard/pressfleet/control-plane/internal/id"
	"github.com/rchouinard/pressfleet/control-plane/internal/sites"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
	"github.com/rchouinard/pressfleet/control-plane/internal/uploads"
)

// importArchive is a minimal wp-content archive.
func importArchive(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	body := []byte("/* Theme Name: X */")
	if err := tw.WriteHeader(&tar.Header{Name: "wp-content/themes/x/style.css", Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

const importDump = "CREATE TABLE `wp_options` (x int);\nCREATE TABLE `wp_posts` (x int);\n" +
	"CREATE TABLE `wp_users` (x int);\nCREATE TABLE `wp_usermeta` (x int);\n" +
	"INSERT INTO `wp_options` VALUES (1,'siteurl','https://old.example','yes'),(2,'home','https://old.example','yes')," +
	"(3,'blogname','Old Site','yes'),(4,'db_version','60000','yes');\n"

func TestImportSiteEndpoint(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newSiteEnv(t, db, testPreviewBase)
		h := newHandler(t, api.Options{DB: db, Authenticator: &fakeAuth{}, Sites: e.svc, Uploads: e.uploads})
		const admin = "01j9zq3v6t8k4m2n5p7r9s1x3y" // fakeAuth's user
		now := time.Now()
		if _, err := db.ExecContext(ctx, "INSERT INTO users (id, email, password_hash, role, created_at, updated_at) VALUES (?, ?, 'x', 'admin', ?, ?)",
			admin, "admin@example.com", now, now); err != nil {
			t.Fatal(err)
		}
		group := &store.ClientGroup{Name: "Acme"}
		if err := store.NewClientGroups(db).Create(ctx, group); err != nil {
			t.Fatal(err)
		}
		upload := func(data []byte) string {
			t.Helper()
			u, err := e.uploads.Create(ctx, uploads.NewUpload{
				Purpose: store.UploadSiteImport, ClientGroupID: group.ID, SizeBytes: int64(len(data)), CreatedByUserID: admin,
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := e.uploads.Write(ctx, u.ID, 0, bytes.NewReader(data)); err != nil {
				t.Fatal(err)
			}
			if _, err := e.uploads.Complete(ctx, u.ID, sha256Hex(data)); err != nil {
				t.Fatal(err)
			}
			return u.ID
		}
		archive, dump := upload(importArchive(t)), upload([]byte(importDump))
		body := func(archive, dump string) string {
			return fmt.Sprintf(`{"client_group_id":%q,"archive_upload_id":%q,"dump_upload_id":%q,"php_version":"8.4"}`, group.ID, archive, dump)
		}

		// Unknown uploads and groups are 422, and take nothing.
		expectStatus(t, do(t, h, request{authed: true, method: http.MethodPost, target: "/api/v1/sites/import", body: body(id.New(), dump)}),
			http.StatusUnprocessableEntity, "unknown archive")
		expectStatus(t, do(t, h, request{authed: true, method: http.MethodPost, target: "/api/v1/sites/import",
			body: fmt.Sprintf(`{"client_group_id":%q,"archive_upload_id":%q,"dump_upload_id":%q}`, id.New(), archive, dump)}),
			http.StatusUnprocessableEntity, "unknown group")

		w := do(t, h, request{authed: true, method: http.MethodPost, target: "/api/v1/sites/import", body: body(archive, dump)})
		if w.Code != http.StatusAccepted {
			t.Fatalf("import: %d %s", w.Code, w.Body)
		}
		accepted := decodeJSON[gen.SiteImportAccepted](t, w.Body.Bytes())
		if !id.Valid(accepted.SiteID) || accepted.Job.Type != sites.ImportJobType || accepted.Job.Target.ID != accepted.SiteID ||
			accepted.Job.Target.Type != gen.JobTargetTypeSite || accepted.Job.State != gen.JobStateQueued {
			t.Errorf("accepted = %s", w.Body)
		}
		if loc := w.Header().Get("Location"); loc != "/api/v1/jobs/"+accepted.Job.ID {
			t.Errorf("Location = %q", loc)
		}
		// The uploads are taken, and the site doesn't exist yet.
		expectStatus(t, do(t, h, request{authed: true, method: http.MethodPost, target: "/api/v1/sites/import", body: body(archive, dump)}),
			http.StatusConflict, "uploads in use")
		expectStatus(t, do(t, h, request{authed: true, method: http.MethodGet, target: "/api/v1/sites/" + accepted.SiteID}),
			http.StatusNotFound, "site before the job")

		e.runJobs(t)
		w = do(t, h, request{authed: true, method: http.MethodGet, target: "/api/v1/sites/" + accepted.SiteID})
		got := decodeJSON[gen.Site](t, w.Body.Bytes())
		if w.Code != http.StatusOK || got.Status != gen.SiteStatusRunning || got.Name != "Old Site" || got.ClientGroupID != group.ID ||
			got.AdminCredentialsAvailable {
			t.Errorf("imported site: %d %s", w.Code, w.Body)
		}
	})
}
