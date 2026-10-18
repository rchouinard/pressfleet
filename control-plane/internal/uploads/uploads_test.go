package uploads_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/id"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
	"github.com/rchouinard/pressfleet/control-plane/internal/uploads"
)

// env is a service on a fresh database with a client group, a site in it,
// and two users.
type env struct {
	t        *testing.T
	db       *store.DB
	svc      *uploads.Service
	dir      string
	clock    *clock
	groupID  string
	siteID   string
	userID   string
	otherID  string
	maxBytes int64
}

type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func newEnv(t *testing.T, db *store.DB) *env {
	t.Helper()
	ctx := context.Background()
	e := &env{t: t, db: db, dir: filepath.Join(t.TempDir(), "uploads"), clock: &clock{now: time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)}, maxBytes: 1 << 20}
	svc, err := uploads.New(uploads.Options{DB: db, Dir: e.dir, MaxSize: e.maxBytes, Expiry: 24 * time.Hour, Now: e.clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	e.svc = svc
	group := &store.ClientGroup{Name: "Acme"}
	if err := store.NewClientGroups(db).Create(ctx, group); err != nil {
		t.Fatal(err)
	}
	e.groupID = group.ID
	e.siteID = id.New()
	now := time.Now()
	if _, err := db.ExecContext(ctx, `INSERT INTO sites
		(id, client_group_id, name, slug, status, preview_domain, php_version, wp_core_version, uid, created_at, updated_at)
		VALUES (?, ?, 'Site', 'site', 'running', 'site.preview.example', '8.4', '6.9.1', 20000, ?, ?)`,
		e.siteID, group.ID, now, now); err != nil {
		t.Fatal(err)
	}
	for _, dst := range []*string{&e.userID, &e.otherID} {
		u := store.User{Email: id.New() + "@example.com", PasswordHash: "x", Role: store.RoleOperator}
		if err := store.NewUsers(db).Create(ctx, &u); err != nil {
			t.Fatal(err)
		}
		*dst = u.ID
	}
	return e
}

func (e *env) importUpload(content []byte, sum bool) store.Upload {
	e.t.Helper()
	in := uploads.NewUpload{Purpose: store.UploadSiteImport, ClientGroupID: e.groupID, Filename: "wp-content.tar.gz",
		SizeBytes: int64(len(content)), CreatedByUserID: e.userID}
	if sum {
		in.SHA256 = checksum(content)
	}
	u, err := e.svc.Create(context.Background(), in)
	if err != nil {
		e.t.Fatal(err)
	}
	return u
}

// send uploads content in chunks of size n and completes the upload.
func (e *env) send(u store.Upload, content []byte, n int) store.Upload {
	e.t.Helper()
	ctx := context.Background()
	for off := 0; off < len(content); off += n {
		end := min(off+n, len(content))
		if _, err := e.svc.Write(ctx, u.ID, int64(off), bytes.NewReader(content[off:end])); err != nil {
			e.t.Fatalf("Write at %d: %v", off, err)
		}
	}
	done, err := e.svc.Complete(ctx, u.ID, checksum(content))
	if err != nil {
		e.t.Fatalf("Complete: %v", err)
	}
	return done
}

func (e *env) enqueue(state store.JobState) store.Job {
	e.t.Helper()
	ctx := context.Background()
	j := store.Job{Type: "site.import", TargetType: store.JobTargetClientGroup, TargetID: e.groupID}
	if err := store.NewJobs(e.db).Enqueue(ctx, &j); err != nil {
		e.t.Fatal(err)
	}
	e.setState(j, state)
	return j
}

func (e *env) setState(j store.Job, state store.JobState) {
	e.t.Helper()
	if _, err := e.db.ExecContext(context.Background(), "UPDATE jobs SET state = ? WHERE id = ?", string(state), j.ID); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) fileExists(uploadID string) bool {
	_, err := os.Stat(filepath.Join(e.dir, uploadID))
	return err == nil
}

func checksum(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func content(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*7 + i/251)
	}
	return b
}

// failingReader returns some bytes, then fails, like a dropped connection.
type failingReader struct {
	data []byte
}

func (r *failingReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, io.ErrUnexpectedEOF
	}
	n := copy(p, r.data)
	r.data = r.data[n:]
	return n, nil
}

func TestCreate(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		sum := checksum([]byte("x"))

		u, err := e.svc.Create(ctx, uploads.NewUpload{Purpose: store.UploadSiteDeploy, SiteID: e.siteID, Filename: "theme.zip",
			SizeBytes: 100, SHA256: strings.ToUpper(sum), CreatedByUserID: e.userID})
		if err != nil {
			t.Fatal(err)
		}
		// A deployment belongs to its site's group; the checksum is
		// normalized.
		if u.ClientGroupID != e.groupID || u.SiteID != e.siteID || u.SHA256 != sum || u.Status != store.UploadReceiving ||
			!u.ExpiresAt.Equal(e.clock.Now().Add(24*time.Hour)) {
			t.Errorf("created = %+v", u)
		}
		if info, err := os.Stat(filepath.Join(e.dir, u.ID)); err != nil || info.Size() != 0 || info.Mode().Perm() != 0o600 {
			t.Errorf("file = %v, %v", info, err)
		}
		if info, err := os.Stat(e.dir); err != nil || info.Mode().Perm() != 0o700 {
			t.Errorf("directory = %v, %v", info, err)
		}

		tests := []struct {
			name  string
			in    uploads.NewUpload
			field string
		}{
			{"unknown purpose", uploads.NewUpload{Purpose: "backup", ClientGroupID: e.groupID, SizeBytes: 1}, "/purpose"},
			{"import without group", uploads.NewUpload{Purpose: store.UploadSiteImport, SizeBytes: 1}, "/client_group_id"},
			{"import with site", uploads.NewUpload{Purpose: store.UploadSiteImport, ClientGroupID: e.groupID, SiteID: e.siteID, SizeBytes: 1}, "/site_id"},
			{"deploy without site", uploads.NewUpload{Purpose: store.UploadSiteDeploy, SizeBytes: 1}, "/site_id"},
			{"deploy with group", uploads.NewUpload{Purpose: store.UploadSiteDeploy, SiteID: e.siteID, ClientGroupID: e.groupID, SizeBytes: 1}, "/client_group_id"},
			{"empty", uploads.NewUpload{Purpose: store.UploadSiteImport, ClientGroupID: e.groupID}, "/size_bytes"},
			{"too large", uploads.NewUpload{Purpose: store.UploadSiteImport, ClientGroupID: e.groupID, SizeBytes: e.maxBytes + 1}, "/size_bytes"},
			{"bad checksum", uploads.NewUpload{Purpose: store.UploadSiteImport, ClientGroupID: e.groupID, SizeBytes: 1, SHA256: "abc"}, "/sha256"},
			{"path in name", uploads.NewUpload{Purpose: store.UploadSiteImport, ClientGroupID: e.groupID, SizeBytes: 1, Filename: "../x"}, "/filename"},
			{"long name", uploads.NewUpload{Purpose: store.UploadSiteImport, ClientGroupID: e.groupID, SizeBytes: 1, Filename: strings.Repeat("x", 256)}, "/filename"},
			{"missing group", uploads.NewUpload{Purpose: store.UploadSiteImport, ClientGroupID: id.New(), SizeBytes: 1}, "/client_group_id"},
			{"missing site", uploads.NewUpload{Purpose: store.UploadSiteDeploy, SiteID: id.New(), SizeBytes: 1}, "/site_id"},
		}
		for _, tt := range tests {
			tt.in.CreatedByUserID = e.userID
			_, err := e.svc.Create(ctx, tt.in)
			var ve *uploads.ValidationError
			if !errors.As(err, &ve) || len(ve.Fields) != 1 || ve.Fields[0].Field != tt.field {
				t.Errorf("%s: err = %v, want a validation error on %s", tt.name, err, tt.field)
			}
		}
		if _, err := e.svc.Create(ctx, uploads.NewUpload{Purpose: store.UploadSiteImport, ClientGroupID: e.groupID, SizeBytes: e.maxBytes, CreatedByUserID: e.userID}); err != nil {
			t.Errorf("Create at the size limit: %v", err)
		}
	})
}

func TestResumeAfterInterruptedChunk(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		data := content(300_000)
		u := e.importUpload(data, true)
		const chunk = 100_000

		if got, err := e.svc.Write(ctx, u.ID, 0, bytes.NewReader(data[:chunk])); err != nil || got.ReceivedBytes != chunk {
			t.Fatalf("first chunk: %+v, %v", got, err)
		}
		// The connection drops halfway through the second chunk: nothing of
		// it counts.
		if _, err := e.svc.Write(ctx, u.ID, chunk, &failingReader{data: data[chunk : chunk+chunk/2]}); !errors.Is(err, io.ErrUnexpectedEOF) || !errors.Is(err, uploads.ErrInterrupted) {
			t.Fatalf("interrupted chunk: %v", err)
		}
		got, err := e.svc.Get(ctx, u.ID)
		if err != nil || got.ReceivedBytes != chunk {
			t.Fatalf("after the interruption: %+v, %v; want %d bytes received", got, err, chunk)
		}
		// Chunks elsewhere than where the upload stands are refused.
		for _, off := range []int64{0, chunk + 1, 2 * chunk} {
			_, err := e.svc.Write(ctx, u.ID, off, bytes.NewReader(data[:10]))
			var oe *uploads.OffsetError
			if !errors.As(err, &oe) || !errors.Is(err, uploads.ErrOffsetMismatch) || oe.Expected != chunk {
				t.Errorf("chunk at %d: %v, want an offset error expecting %d", off, err, chunk)
			}
		}
		// Completing too early fails.
		if _, err := e.svc.Complete(ctx, u.ID, ""); !errors.Is(err, uploads.ErrIncomplete) {
			t.Errorf("Complete of a partial upload: %v, want ErrIncomplete", err)
		}
		// Resume from the last chunk, in other sizes.
		for _, r := range [][2]int{{chunk, 250_000}, {250_000, 300_000}} {
			if _, err := e.svc.Write(ctx, u.ID, int64(r[0]), bytes.NewReader(data[r[0]:r[1]])); err != nil {
				t.Fatalf("chunk at %d: %v", r[0], err)
			}
		}
		if _, err := e.svc.Write(ctx, u.ID, int64(len(data)), bytes.NewReader([]byte{1})); !errors.Is(err, uploads.ErrPastEnd) {
			t.Errorf("chunk past the end: %v, want ErrPastEnd", err)
		}
		done, err := e.svc.Complete(ctx, u.ID, "")
		if err != nil {
			t.Fatal(err)
		}
		if done.Status != store.UploadComplete || done.ReceivedBytes != int64(len(data)) || done.SHA256 != checksum(data) {
			t.Errorf("completed = %+v", done)
		}
		if got, err := os.ReadFile(filepath.Join(e.dir, u.ID)); err != nil || !bytes.Equal(got, data) {
			t.Errorf("file holds %d bytes (%v), want the %d uploaded", len(got), err, len(data))
		}
		// Completing again is fine; writing isn't.
		if again, err := e.svc.Complete(ctx, u.ID, checksum(data)); err != nil || again.Status != store.UploadComplete {
			t.Errorf("Complete again: %+v, %v", again, err)
		}
		if _, err := e.svc.Write(ctx, u.ID, int64(len(data)), bytes.NewReader([]byte{1})); !errors.Is(err, uploads.ErrNotReceiving) {
			t.Errorf("Write after completion: %v, want ErrNotReceiving", err)
		}
	})
}

func TestWriteChecks(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		data := content(1000)
		u := e.importUpload(data, true)

		if _, err := e.svc.Write(ctx, u.ID, 0, bytes.NewReader(nil)); !errors.Is(err, uploads.ErrEmptyChunk) {
			t.Errorf("empty chunk: %v, want ErrEmptyChunk", err)
		}
		if _, err := e.svc.Write(ctx, u.ID, 0, bytes.NewReader(content(1001))); !errors.Is(err, uploads.ErrPastEnd) {
			t.Errorf("chunk past the end: %v, want ErrPastEnd", err)
		}
		for _, missing := range []string{id.New(), "../../etc/passwd"} {
			if _, err := e.svc.Write(ctx, missing, 0, bytes.NewReader(data)); !errors.Is(err, uploads.ErrNotFound) {
				t.Errorf("Write to %s: %v, want ErrNotFound", missing, err)
			}
		}

		// One writer at a time.
		pr, pw := io.Pipe()
		done := make(chan error, 1)
		go func() {
			_, err := e.svc.Write(ctx, u.ID, 0, pr)
			done <- err
		}()
		if _, err := pw.Write(data[:10]); err != nil {
			t.Fatal(err)
		}
		// The first chunk has taken the lock once it has read bytes.
		if _, err := e.svc.Write(ctx, u.ID, 0, bytes.NewReader(data)); !errors.Is(err, uploads.ErrBusy) {
			t.Errorf("concurrent Write: %v, want ErrBusy", err)
		}
		if _, err := e.svc.Complete(ctx, u.ID, ""); !errors.Is(err, uploads.ErrBusy) {
			t.Errorf("Complete during a Write: %v, want ErrBusy", err)
		}
		if err := e.svc.Delete(ctx, u.ID); !errors.Is(err, uploads.ErrBusy) {
			t.Errorf("Delete during a Write: %v, want ErrBusy", err)
		}
		if _, err := pw.Write(data[10:]); err != nil {
			t.Fatal(err)
		}
		_ = pw.Close()
		if err := <-done; err != nil {
			t.Fatalf("first Write: %v", err)
		}
		if got, _ := e.svc.Get(ctx, u.ID); got.ReceivedBytes != 1000 {
			t.Errorf("received = %d, want 1000", got.ReceivedBytes)
		}
	})
}

func TestComplete(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		data := content(5000)

		// Without a checksum at creation, one is needed on completion.
		u := e.importUpload(data, false)
		if _, err := e.svc.Write(ctx, u.ID, 0, bytes.NewReader(data)); err != nil {
			t.Fatal(err)
		}
		var ve *uploads.ValidationError
		if _, err := e.svc.Complete(ctx, u.ID, ""); !errors.As(err, &ve) || ve.Fields[0].Field != "/sha256" {
			t.Errorf("Complete without a checksum: %v", err)
		}
		if _, err := e.svc.Complete(ctx, u.ID, "xyz"); !errors.As(err, &ve) {
			t.Errorf("Complete with a malformed checksum: %v", err)
		}
		if got, err := e.svc.Complete(ctx, u.ID, strings.ToUpper(checksum(data))); err != nil || got.SHA256 != checksum(data) {
			t.Errorf("Complete = %+v, %v", got, err)
		}
		if _, err := e.svc.Complete(ctx, u.ID, checksum([]byte("other"))); !errors.Is(err, uploads.ErrNotReceiving) {
			t.Errorf("Complete again with another checksum: %v, want ErrNotReceiving", err)
		}

		// A checksum at completion must agree with the one at creation.
		u = e.importUpload(data, true)
		if _, err := e.svc.Write(ctx, u.ID, 0, bytes.NewReader(data)); err != nil {
			t.Fatal(err)
		}
		if _, err := e.svc.Complete(ctx, u.ID, checksum([]byte("other"))); !errors.As(err, &ve) {
			t.Errorf("Complete with a different checksum: %v", err)
		}

		// Content that doesn't match the checksum is discarded.
		corrupt := bytes.Clone(data)
		corrupt[42] ^= 1
		u = e.importUpload(data, true)
		if _, err := e.svc.Write(ctx, u.ID, 0, bytes.NewReader(corrupt)); err != nil {
			t.Fatal(err)
		}
		if _, err := e.svc.Complete(ctx, u.ID, ""); !errors.Is(err, uploads.ErrChecksumMismatch) {
			t.Fatalf("Complete of corrupt content: %v, want ErrChecksumMismatch", err)
		}
		if _, err := e.svc.Get(ctx, u.ID); !errors.Is(err, uploads.ErrNotFound) {
			t.Errorf("corrupt upload still exists: %v", err)
		}
		if e.fileExists(u.ID) {
			t.Error("corrupt upload's file still exists")
		}
	})
}

func TestClaim(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		data := content(2000)
		imp := e.importUpload(data, true)
		importUse := uploads.Use{Purpose: store.UploadSiteImport, ClientGroupID: e.groupID, UserID: e.userID}
		job := e.enqueue(store.JobQueued)

		// Not before it is complete.
		if _, err := e.svc.Claim(ctx, db, imp.ID, importUse, job.ID); !errors.Is(err, uploads.ErrNotComplete) {
			t.Errorf("Claim of a partial upload: %v, want ErrNotComplete", err)
		}
		imp = e.send(imp, data, 700)

		otherGroup := &store.ClientGroup{Name: "Other"}
		if err := store.NewClientGroups(db).Create(ctx, otherGroup); err != nil {
			t.Fatal(err)
		}
		for name, tt := range map[string]struct {
			use  uploads.Use
			want error
		}{
			"another user":         {uploads.Use{Purpose: store.UploadSiteImport, ClientGroupID: e.groupID, UserID: e.otherID}, uploads.ErrNotFound},
			"no user":              {uploads.Use{Purpose: store.UploadSiteImport, ClientGroupID: e.groupID}, uploads.ErrNotFound},
			"another purpose":      {uploads.Use{Purpose: store.UploadSiteDeploy, SiteID: e.siteID, UserID: e.userID}, uploads.ErrWrongUpload},
			"another client group": {uploads.Use{Purpose: store.UploadSiteImport, ClientGroupID: otherGroup.ID, UserID: e.userID}, uploads.ErrWrongUpload},
		} {
			if _, err := e.svc.Claim(ctx, db, imp.ID, tt.use, job.ID); !errors.Is(err, tt.want) {
				t.Errorf("%s: %v, want %v", name, err, tt.want)
			}
		}
		if _, err := e.svc.Claim(ctx, db, id.New(), importUse, job.ID); !errors.Is(err, uploads.ErrNotFound) {
			t.Errorf("Claim of a missing upload: %v", err)
		}

		// A claim in a transaction that rolls back doesn't stick.
		errRollback := errors.New("rollback")
		err := db.InTx(ctx, func(q store.Querier) error {
			if _, err := e.svc.Claim(ctx, q, imp.ID, importUse, job.ID); err != nil {
				return err
			}
			return errRollback
		})
		if !errors.Is(err, errRollback) {
			t.Fatal(err)
		}
		if got, _ := e.svc.Get(ctx, imp.ID); got.Status != store.UploadComplete {
			t.Errorf("after a rolled back claim: %+v", got)
		}

		claimed, err := e.svc.Claim(ctx, db, imp.ID, importUse, job.ID)
		if err != nil {
			t.Fatal(err)
		}
		if claimed.Status != store.UploadInUse || claimed.JobID != job.ID || !claimed.ExpiresAt.IsZero() {
			t.Errorf("claimed = %+v", claimed)
		}
		if _, err := e.svc.Claim(ctx, db, imp.ID, importUse, e.enqueue(store.JobQueued).ID); !errors.Is(err, uploads.ErrInUse) {
			t.Errorf("second claim: %v, want ErrInUse", err)
		}
		if err := e.svc.Delete(ctx, imp.ID); !errors.Is(err, uploads.ErrInUse) {
			t.Errorf("Delete of a claimed upload: %v, want ErrInUse", err)
		}

		// Only the claiming job reads it.
		f, got, err := e.svc.Open(ctx, imp.ID, job.ID)
		if err != nil {
			t.Fatal(err)
		}
		read, err := io.ReadAll(f)
		_ = f.Close()
		if err != nil || !bytes.Equal(read, data) || got.ID != imp.ID {
			t.Errorf("Open read %d bytes (%v)", len(read), err)
		}
		if path, _, err := e.svc.Path(ctx, imp.ID, job.ID); err != nil || path != filepath.Join(e.dir, imp.ID) {
			t.Errorf("Path = %q, %v", path, err)
		}
		other := e.enqueue(store.JobQueued)
		if _, _, err := e.svc.Open(ctx, imp.ID, other.ID); !errors.Is(err, uploads.ErrNotClaimed) {
			t.Errorf("Open by another job: %v, want ErrNotClaimed", err)
		}
		if err := e.svc.Release(ctx, imp.ID, other.ID); !errors.Is(err, uploads.ErrNotClaimed) {
			t.Errorf("Release by another job: %v, want ErrNotClaimed", err)
		}

		// Deployments are claimed for their site.
		dep, err := e.svc.Create(ctx, uploads.NewUpload{Purpose: store.UploadSiteDeploy, SiteID: e.siteID, SizeBytes: 3, CreatedByUserID: e.userID})
		if err != nil {
			t.Fatal(err)
		}
		dep = e.send(dep, []byte("zip"), 3)
		if _, err := e.svc.Claim(ctx, db, dep.ID, uploads.Use{Purpose: store.UploadSiteDeploy, SiteID: id.New(), UserID: e.userID}, other.ID); !errors.Is(err, uploads.ErrWrongUpload) {
			t.Errorf("Claim for another site: %v, want ErrWrongUpload", err)
		}
		if _, err := e.svc.Claim(ctx, db, dep.ID, uploads.Use{Purpose: store.UploadSiteDeploy, SiteID: e.siteID, UserID: e.userID}, other.ID); err != nil {
			t.Errorf("Claim for the site: %v", err)
		}

		// Release removes it; releasing again is fine.
		for range 2 {
			if err := e.svc.Release(ctx, imp.ID, job.ID); err != nil {
				t.Errorf("Release: %v", err)
			}
		}
		if _, err := e.svc.Get(ctx, imp.ID); !errors.Is(err, uploads.ErrNotFound) || e.fileExists(imp.ID) {
			t.Errorf("released upload: %v, file exists %v", err, e.fileExists(imp.ID))
		}
		if _, _, err := e.svc.Open(ctx, imp.ID, job.ID); !errors.Is(err, uploads.ErrNotFound) {
			t.Errorf("Open of a released upload: %v, want ErrNotFound", err)
		}
	})
}

func TestDelete(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		for _, complete := range []bool{false, true} {
			data := content(10)
			u := e.importUpload(data, true)
			if complete {
				e.send(u, data, 10)
			}
			if err := e.svc.Delete(ctx, u.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := e.svc.Get(ctx, u.ID); !errors.Is(err, uploads.ErrNotFound) || e.fileExists(u.ID) {
				t.Errorf("deleted upload: %v, file exists %v", err, e.fileExists(u.ID))
			}
			if err := e.svc.Delete(ctx, u.ID); !errors.Is(err, uploads.ErrNotFound) {
				t.Errorf("Delete again: %v, want ErrNotFound", err)
			}
		}
	})
}

// TestCleanup covers the acceptance criterion "abandoned and consumed
// uploads are cleaned up".
func TestCleanup(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		data := content(100)
		use := uploads.Use{Purpose: store.UploadSiteImport, ClientGroupID: e.groupID, UserID: e.userID}
		claimed := func(state store.JobState) (store.Upload, store.Job) {
			t.Helper()
			u := e.send(e.importUpload(data, true), data, 50)
			j := e.enqueue(store.JobQueued)
			if _, err := e.svc.Claim(ctx, db, u.ID, use, j.ID); err != nil {
				t.Fatal(err)
			}
			e.setState(j, state)
			return u, j
		}

		abandoned := e.importUpload(data, true)
		if _, err := e.svc.Write(ctx, abandoned.ID, 0, bytes.NewReader(data[:40])); err != nil {
			t.Fatal(err)
		}
		unclaimed := e.send(e.importUpload(data, true), data, 100)
		running, runningJob := claimed(store.JobRunning)
		retrying, _ := claimed(store.JobQueued)
		rollingBack, _ := claimed(store.JobRollingBack)
		succeeded, _ := claimed(store.JobSucceeded)
		failed, _ := claimed(store.JobFailed)
		rollbackFailed, _ := claimed(store.JobRollbackFailed)
		orphan := id.New()
		if err := os.WriteFile(filepath.Join(e.dir, orphan), data, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(e.dir, "README"), data, 0o600); err != nil {
			t.Fatal(err)
		}

		// Nothing has expired yet; the finished jobs' uploads and the orphan go.
		res, err := e.svc.Cleanup(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if want := (uploads.CleanupResult{Finished: 3, Orphaned: 1}); res != want {
			t.Errorf("Cleanup = %+v, want %+v", res, want)
		}
		for _, u := range []store.Upload{succeeded, failed, rollbackFailed} {
			if _, err := e.svc.Get(ctx, u.ID); !errors.Is(err, uploads.ErrNotFound) || e.fileExists(u.ID) {
				t.Errorf("upload of a finished job: %v, file exists %v", err, e.fileExists(u.ID))
			}
		}
		if e.fileExists(orphan) || !e.fileExists("README") {
			t.Errorf("orphan exists %v, README exists %v", e.fileExists(orphan), e.fileExists("README"))
		}

		// A day on, without a chunk, the unclaimed uploads are abandoned.
		// Claimed ones stay while their job runs, however long.
		e.clock.advance(24*time.Hour + time.Second)
		// A chunk just now keeps an upload.
		kept := e.importUpload(data, true)
		e.clock.advance(time.Hour)
		if _, err := e.svc.Write(ctx, kept.ID, 0, bytes.NewReader(data[:10])); err != nil {
			t.Fatal(err)
		}
		res, err = e.svc.Cleanup(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if want := (uploads.CleanupResult{Expired: 2}); res != want {
			t.Errorf("Cleanup = %+v, want %+v", res, want)
		}
		for _, u := range []store.Upload{abandoned, unclaimed} {
			if _, err := e.svc.Get(ctx, u.ID); !errors.Is(err, uploads.ErrNotFound) || e.fileExists(u.ID) {
				t.Errorf("abandoned upload: %v, file exists %v", err, e.fileExists(u.ID))
			}
		}
		for _, u := range []store.Upload{kept, running, retrying, rollingBack} {
			if _, err := e.svc.Get(ctx, u.ID); err != nil || !e.fileExists(u.ID) {
				t.Errorf("upload %s was removed: %v", u.ID, err)
			}
		}

		// The running job ends; its upload goes on the next run.
		e.setState(runningJob, store.JobSucceeded)
		if res, err := e.svc.Cleanup(ctx); err != nil || res != (uploads.CleanupResult{Finished: 1}) {
			t.Errorf("Cleanup = %+v, %v", res, err)
		}
		if res, err := e.svc.Cleanup(ctx); err != nil || res != (uploads.CleanupResult{}) {
			t.Errorf("Cleanup again = %+v, %v", res, err)
		}
	})
}

func TestCleanupSkipsBusyUploads(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		data := content(100)
		u := e.importUpload(data, true)
		e.clock.advance(25 * time.Hour)

		pr, pw := io.Pipe()
		done := make(chan error, 1)
		go func() {
			_, err := e.svc.Write(ctx, u.ID, 0, pr)
			done <- err
		}()
		if _, err := pw.Write(data[:10]); err != nil {
			t.Fatal(err)
		}
		if res, err := e.svc.Cleanup(ctx); err != nil || res != (uploads.CleanupResult{}) {
			t.Errorf("Cleanup during a Write = %+v, %v", res, err)
		}
		if _, err := pw.Write(data[10:]); err != nil {
			t.Fatal(err)
		}
		_ = pw.Close()
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		// The chunk moved the expiry.
		if res, err := e.svc.Cleanup(ctx); err != nil || res != (uploads.CleanupResult{}) {
			t.Errorf("Cleanup after the Write = %+v, %v", res, err)
		}
	})
}

func TestEnsureCleanupSchedule(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		for range 2 {
			if err := uploads.EnsureCleanupSchedule(ctx, db); err != nil {
				t.Fatal(err)
			}
		}
		s, err := store.NewSchedules(db).Get(ctx, uploads.CleanupScheduleName)
		if err != nil {
			t.Fatal(err)
		}
		if s.JobType != uploads.CleanupJobType || s.Interval != uploads.CleanupInterval {
			t.Errorf("schedule = %+v", s)
		}
	})
}

func TestNewValidatesOptions(t *testing.T) {
	db := storetest.Open(t, store.SQLite)
	dir := t.TempDir()
	for name, opts := range map[string]uploads.Options{
		"no DB":        {Dir: dir, MaxSize: 1, Expiry: time.Hour},
		"relative dir": {DB: db, Dir: "uploads", MaxSize: 1, Expiry: time.Hour},
		"no size":      {DB: db, Dir: dir, Expiry: time.Hour},
		"no expiry":    {DB: db, Dir: dir, MaxSize: 1},
	} {
		if _, err := uploads.New(opts); err == nil {
			t.Errorf("%s: New succeeded", name)
		}
	}
}
