package api_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/api"
	"github.com/rchouinard/pressfleet/control-plane/internal/api/gen"
	"github.com/rchouinard/pressfleet/control-plane/internal/auth"
	"github.com/rchouinard/pressfleet/control-plane/internal/id"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
	"github.com/rchouinard/pressfleet/control-plane/internal/uploads"
)

// testUploadMaxSize is the upload size limit in API tests: above one
// chunk, so a chunk over the chunk limit can be tried.
const testUploadMaxSize = 2*uploads.MaxChunkBytes + 1

func newUploadService(t *testing.T, db *store.DB) *uploads.Service {
	t.Helper()
	svc, err := uploads.New(uploads.Options{DB: db, Dir: filepath.Join(t.TempDir(), "uploads"), MaxSize: testUploadMaxSize,
		Expiry: 24 * time.Hour, Logger: storetest.Logger()})
	if err != nil {
		t.Fatal(err)
	}
	return svc
}

// uploadEnv serves the API as several users of one database.
type uploadEnv struct {
	t        *testing.T
	db       *store.DB
	svc      *uploads.Service
	handlers map[string]http.Handler
	users    map[string]string
	groupID  string
	siteID   string
}

func newUploadEnv(t *testing.T, db *store.DB) *uploadEnv {
	t.Helper()
	ctx := context.Background()
	e := &uploadEnv{t: t, db: db, svc: newUploadService(t, db), handlers: map[string]http.Handler{}, users: map[string]string{}}
	for name, p := range map[string]auth.Principal{
		"operator":  {Role: store.RoleOperator, Method: auth.MethodSession, SessionID: "o"},
		"admin":     {Role: store.RoleAdmin, Method: auth.MethodSession, SessionID: "a"},
		"viewer":    {Role: store.RoleViewer, Method: auth.MethodSession, SessionID: "v"},
		"read-only": {Role: store.RoleOperator, Method: auth.MethodToken, TokenID: id.New(), Scope: store.ScopeReadOnly},
	} {
		u := store.User{Email: name + "@example.com", PasswordHash: "x", Role: p.Role}
		if err := store.NewUsers(db).Create(ctx, &u); err != nil {
			t.Fatal(err)
		}
		p.UserID = u.ID
		e.users[name] = u.ID
		e.handlers[name] = newHandler(t, api.Options{DB: db, Authenticator: &fakeAuth{principal: &p}, Uploads: e.svc})
	}
	group := &store.ClientGroup{Name: "Acme"}
	if err := store.NewClientGroups(db).Create(ctx, group); err != nil {
		t.Fatal(err)
	}
	e.groupID, e.siteID = group.ID, id.New()
	now := time.Now()
	if _, err := db.ExecContext(ctx, `INSERT INTO sites
		(id, client_group_id, name, slug, status, preview_domain, php_version, wp_core_version, uid, created_at, updated_at)
		VALUES (?, ?, 'Site', 'site', 'running', 'site.preview.example', '8.4', '6.9.1', 20000, ?, ?)`,
		e.siteID, group.ID, now, now); err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *uploadEnv) do(as, method, target, body string) *httptest.ResponseRecorder {
	e.t.Helper()
	return do(e.t, e.handlers[as], request{authed: true, method: method, target: target, body: body})
}

// chunk sends content as a chunk at offset.
func (e *uploadEnv) chunk(as, uploadID string, offset int, content []byte) *httptest.ResponseRecorder {
	e.t.Helper()
	r := httptest.NewRequest(http.MethodPatch, fmt.Sprintf("/api/v1/uploads/%s?offset=%d", uploadID, offset), bytes.NewReader(content))
	r.Header.Set("Content-Type", "application/octet-stream")
	r.Header.Set("X-Test-Auth", "ok")
	w := httptest.NewRecorder()
	e.handlers[as].ServeHTTP(w, r)
	return w
}

func sha256Hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func TestUploadEndpoints(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newUploadEnv(t, db)
		// Larger than the server's usual body limit in tests (4096 bytes).
		data := bytes.Repeat([]byte("pressfleet-upload "), 1000)
		sum := sha256Hex(data)

		w := e.do("operator", http.MethodPost, "/api/v1/uploads",
			fmt.Sprintf(`{"purpose":"site_import","client_group_id":%q,"filename":"wp-content.tar.gz","size_bytes":%d,"sha256":%q}`,
				e.groupID, len(data), strings.ToUpper(sum)))
		expectStatus(t, w, http.StatusCreated, "create")
		u := decodeJSON[gen.Upload](t, w.Body.Bytes())
		if w.Header().Get("Location") != "/api/v1/uploads/"+u.ID || u.Purpose != "site_import" || u.ClientGroupID != e.groupID ||
			u.SiteID != nil || u.SizeBytes != int64(len(data)) || u.ReceivedBytes != 0 || u.Sha256 == nil || *u.Sha256 != sum ||
			u.Status != "uploading" || u.JobID != nil || u.MaxChunkBytes != uploads.MaxChunkBytes || u.ExpiresAt == nil ||
			u.Filename != "wp-content.tar.gz" {
			t.Errorf("created: %s (Location %q)", w.Body, w.Header().Get("Location"))
		}
		path := "/api/v1/uploads/" + u.ID

		// Only its creator sees it; viewers and read-only tokens can't
		// upload at all.
		expectStatus(t, e.do("admin", http.MethodGet, path, ""), http.StatusNotFound, "another user's upload")
		expectStatus(t, e.do("viewer", http.MethodGet, path, ""), http.StatusForbidden, "viewer")
		expectStatus(t, e.do("read-only", http.MethodGet, path, ""), http.StatusForbidden, "read-only token")
		expectStatus(t, e.do("viewer", http.MethodPost, "/api/v1/uploads",
			fmt.Sprintf(`{"purpose":"site_import","client_group_id":%q,"size_bytes":1}`, e.groupID)), http.StatusForbidden, "viewer create")
		expectStatus(t, e.chunk("admin", u.ID, 0, data), http.StatusNotFound, "another user's chunk")

		// Chunks in order, resuming where the upload stands.
		w = e.chunk("operator", u.ID, 0, data[:6000])
		expectStatus(t, w, http.StatusOK, "first chunk")
		if got := decodeJSON[gen.Upload](t, w.Body.Bytes()); got.ReceivedBytes != 6000 {
			t.Errorf("after the first chunk: %s", w.Body)
		}
		w = e.chunk("operator", u.ID, 5000, data[5000:])
		expectStatus(t, w, http.StatusConflict, "chunk at the wrong offset")
		if p := decodeProblem(t, w); !strings.Contains(p.Detail, "6000") {
			t.Errorf("offset conflict doesn't say where to resume: %q", p.Detail)
		}
		w = e.do("operator", http.MethodGet, path, "")
		if got := decodeJSON[gen.Upload](t, w.Body.Bytes()); w.Code != http.StatusOK || got.ReceivedBytes != 6000 {
			t.Errorf("get: %d %s", w.Code, w.Body)
		}
		w = do(t, e.handlers["operator"], request{authed: true, method: http.MethodPatch, target: path + "?offset=6000", body: "x", contentType: "text/plain"})
		expectStatus(t, w, http.StatusUnsupportedMediaType, "chunk as text")
		w = do(t, e.handlers["operator"], request{authed: true, method: http.MethodPatch, target: path, body: "x", contentType: "application/octet-stream"})
		expectStatus(t, w, http.StatusBadRequest, "chunk without offset")
		expectStatus(t, e.chunk("operator", u.ID, 6000, nil), http.StatusBadRequest, "empty chunk")
		expectStatus(t, e.do("operator", http.MethodPost, path+"/complete", `{}`), http.StatusConflict, "complete too early")
		expectStatus(t, e.chunk("operator", u.ID, 6000, append(bytes.Clone(data[6000:]), 'x')), http.StatusRequestEntityTooLarge, "past the end")
		expectStatus(t, e.chunk("operator", u.ID, 6000, data[6000:]), http.StatusOK, "last chunk")

		w = e.do("operator", http.MethodPost, path+"/complete", `{}`)
		expectStatus(t, w, http.StatusOK, "complete")
		if got := decodeJSON[gen.Upload](t, w.Body.Bytes()); got.Status != "complete" || got.ReceivedBytes != int64(len(data)) {
			t.Errorf("completed: %s", w.Body)
		}
		expectStatus(t, e.chunk("operator", u.ID, len(data), []byte("x")), http.StatusConflict, "chunk after completion")
		expectStatus(t, e.do("operator", http.MethodDelete, path, ""), http.StatusNoContent, "delete")
		expectStatus(t, e.do("operator", http.MethodGet, path, ""), http.StatusNotFound, "deleted upload")

		// A checksum given on completion; content that doesn't match it is
		// discarded.
		w = e.do("operator", http.MethodPost, "/api/v1/uploads", fmt.Sprintf(`{"purpose":"site_import","client_group_id":%q,"size_bytes":3}`, e.groupID))
		expectStatus(t, w, http.StatusCreated, "create without checksum")
		u = decodeJSON[gen.Upload](t, w.Body.Bytes())
		if u.Sha256 != nil {
			t.Errorf("checksum = %q, want null", *u.Sha256)
		}
		expectStatus(t, e.chunk("operator", u.ID, 0, []byte("abc")), http.StatusOK, "chunk")
		expectStatus(t, e.do("operator", http.MethodPost, "/api/v1/uploads/"+u.ID+"/complete", `{}`), http.StatusUnprocessableEntity, "no checksum")
		w = e.do("operator", http.MethodPost, "/api/v1/uploads/"+u.ID+"/complete", fmt.Sprintf(`{"sha256":%q}`, sha256Hex([]byte("abd"))))
		expectStatus(t, w, http.StatusUnprocessableEntity, "wrong checksum")
		if p := decodeProblem(t, w); len(p.Errors) != 1 || p.Errors[0].Field != "/sha256" {
			t.Errorf("wrong checksum: %s", w.Body)
		}
		expectStatus(t, e.do("operator", http.MethodGet, "/api/v1/uploads/"+u.ID, ""), http.StatusNotFound, "discarded upload")

		// Invalid uploads.
		for name, body := range map[string]string{
			"unknown group":     fmt.Sprintf(`{"purpose":"site_import","client_group_id":%q,"size_bytes":1}`, id.New()),
			"import no group":   `{"purpose":"site_import","size_bytes":1}`,
			"deploy no site":    `{"purpose":"site_deploy","size_bytes":1}`,
			"unknown site":      fmt.Sprintf(`{"purpose":"site_deploy","site_id":%q,"size_bytes":1}`, id.New()),
			"too large":         fmt.Sprintf(`{"purpose":"site_import","client_group_id":%q,"size_bytes":%d}`, e.groupID, testUploadMaxSize+1),
			"unknown purpose":   fmt.Sprintf(`{"purpose":"backup","client_group_id":%q,"size_bytes":1}`, e.groupID),
			"bad checksum":      fmt.Sprintf(`{"purpose":"site_import","client_group_id":%q,"size_bytes":1,"sha256":"abc"}`, e.groupID),
			"empty":             fmt.Sprintf(`{"purpose":"site_import","client_group_id":%q,"size_bytes":0}`, e.groupID),
			"directory in name": fmt.Sprintf(`{"purpose":"site_import","client_group_id":%q,"size_bytes":1,"filename":"a/b"}`, e.groupID),
		} {
			expectStatus(t, e.do("operator", http.MethodPost, "/api/v1/uploads", body), http.StatusUnprocessableEntity, name)
		}

		// A deployment belongs to its site's group, and is audited as one.
		w = e.do("operator", http.MethodPost, "/api/v1/uploads", fmt.Sprintf(`{"purpose":"site_deploy","site_id":%q,"size_bytes":%d}`, e.siteID, uploads.MaxChunkBytes+1))
		expectStatus(t, w, http.StatusCreated, "create deployment")
		dep := decodeJSON[gen.Upload](t, w.Body.Bytes())
		if dep.SiteID == nil || *dep.SiteID != e.siteID || dep.ClientGroupID != e.groupID {
			t.Errorf("deployment: %s", w.Body)
		}
		// A chunk may not exceed the chunk limit.
		expectStatus(t, e.chunk("operator", dep.ID, 0, make([]byte, uploads.MaxChunkBytes+1)), http.StatusRequestEntityTooLarge, "chunk over the limit")
		if got, err := e.svc.Get(context.Background(), dep.ID); err != nil || got.ReceivedBytes != 0 {
			t.Errorf("after the oversized chunk: %+v, %v", got, err)
		}
		expectStatus(t, e.chunk("operator", dep.ID, 0, make([]byte, uploads.MaxChunkBytes)), http.StatusOK, "chunk at the limit")
		expectStatus(t, e.do("viewer", http.MethodDelete, "/api/v1/uploads/"+dep.ID, ""), http.StatusForbidden, "viewer delete")

		events := allEvents(t, db)
		count := func(action string, result store.AuditResult, targetID string) int {
			return len(slices.DeleteFunc(slices.Clone(events), func(ev store.AuditEvent) bool {
				return ev.Action != action || ev.Result != result || (targetID != "" && ev.TargetID != targetID)
			}))
		}
		if n := count("upload.site_deploy", store.AuditSuccess, dep.ID); n != 2 {
			t.Errorf("successful upload.site_deploy events for the deployment = %d, want 2 (create, chunk)", n)
		}
		if n := count("upload.site_deploy", store.AuditDenied, dep.ID); n != 1 {
			t.Errorf("denied upload.site_deploy events = %d, want 1", n)
		}
		if n := count("upload.site_import", store.AuditDenied, ""); n != 3 {
			t.Errorf("denied upload.site_import events = %d, want 3 (viewer get and create, read-only get)", n)
		}
		if n := count("upload.site_import", store.AuditSuccess, ""); n < 6 {
			t.Errorf("successful upload.site_import events = %d, want at least 6", n)
		}
	})
}
