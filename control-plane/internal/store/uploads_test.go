package store_test

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/id"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
)

// uploadFixture is a client group, a site in it, and a user, for uploads to
// refer to.
type uploadFixture struct {
	groupID, siteID, userID string
}

func newUploadFixture(t *testing.T, db *store.DB) uploadFixture {
	t.Helper()
	ctx := context.Background()
	group := &store.ClientGroup{Name: "Acme " + id.New()}
	if err := store.NewClientGroups(db).Create(ctx, group); err != nil {
		t.Fatal(err)
	}
	site := id.New()
	now := time.Now()
	if _, err := db.ExecContext(ctx, `INSERT INTO sites
		(id, client_group_id, name, slug, status, preview_domain, php_version, wp_core_version, uid, created_at, updated_at)
		VALUES (?, ?, 'Site', ?, 'running', ?, '8.4', '6.9.1', ?, ?, ?)`,
		site, group.ID, "s"+site[:8], "s"+site[:8]+".preview.example", 20000+int(now.UnixNano()%10000), now, now); err != nil {
		t.Fatal(err)
	}
	u := store.User{Email: id.New() + "@example.com", PasswordHash: "x", Role: store.RoleOperator}
	if err := store.NewUsers(db).Create(ctx, &u); err != nil {
		t.Fatal(err)
	}
	return uploadFixture{groupID: group.ID, siteID: site, userID: u.ID}
}

func TestUploads(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		f := newUploadFixture(t, db)
		uploads := store.NewUploads(db)
		at := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
		expires := at.Add(24 * time.Hour)

		// Create starts an upload receiving, whatever else is set.
		u := &store.Upload{
			Purpose: store.UploadSiteDeploy, ClientGroupID: f.groupID, SiteID: f.siteID, CreatedByUserID: f.userID,
			Filename: "theme.zip", SizeBytes: 10, SHA256: "abc", ExpiresAt: expires,
			// Ignored.
			ReceivedBytes: 5, HashState: []byte("x"), Status: store.UploadInUse, JobID: id.New(),
		}
		if err := uploads.Create(ctx, u, at); err != nil {
			t.Fatal(err)
		}
		if u.ID == "" || u.Status != store.UploadReceiving || u.ReceivedBytes != 0 || u.HashState != nil || u.JobID != "" ||
			!u.CreatedAt.Equal(at) || !u.UpdatedAt.Equal(at) || !u.ExpiresAt.Equal(expires) {
			t.Errorf("created = %+v", u)
		}
		got, err := uploads.Get(ctx, u.ID)
		if err != nil {
			t.Fatal(err)
		}
		if !uploadsEqual(got, *u) {
			t.Errorf("Get = %+v, want %+v", got, *u)
		}
		if _, err := uploads.Get(ctx, id.New()); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("Get of a missing upload: %v, want ErrNotFound", err)
		}

		// Invalid uploads and missing references.
		for name, bad := range map[string]store.Upload{
			"purpose":    {Purpose: "backup", ClientGroupID: f.groupID, CreatedByUserID: f.userID, SizeBytes: 1, ExpiresAt: expires},
			"size":       {Purpose: store.UploadSiteImport, ClientGroupID: f.groupID, CreatedByUserID: f.userID, ExpiresAt: expires},
			"expiry":     {Purpose: store.UploadSiteImport, ClientGroupID: f.groupID, CreatedByUserID: f.userID, SizeBytes: 1},
			"no group":   {Purpose: store.UploadSiteImport, CreatedByUserID: f.userID, SizeBytes: 1, ExpiresAt: expires},
			"no user":    {Purpose: store.UploadSiteImport, ClientGroupID: f.groupID, SizeBytes: 1, ExpiresAt: expires},
			"bad group":  {Purpose: store.UploadSiteImport, ClientGroupID: id.New(), CreatedByUserID: f.userID, SizeBytes: 1, ExpiresAt: expires},
			"bad site":   {Purpose: store.UploadSiteDeploy, ClientGroupID: f.groupID, SiteID: id.New(), CreatedByUserID: f.userID, SizeBytes: 1, ExpiresAt: expires},
			"bad user":   {Purpose: store.UploadSiteImport, ClientGroupID: f.groupID, CreatedByUserID: id.New(), SizeBytes: 1, ExpiresAt: expires},
			"duplicated": {ID: u.ID, Purpose: store.UploadSiteImport, ClientGroupID: f.groupID, CreatedByUserID: f.userID, SizeBytes: 1, ExpiresAt: expires},
		} {
			if err := uploads.Create(ctx, &bad, at); err == nil {
				t.Errorf("%s: Create succeeded", name)
			} else if name == "bad group" || name == "bad site" || name == "bad user" {
				if !errors.Is(err, store.ErrReference) {
					t.Errorf("%s: %v, want ErrReference", name, err)
				}
			}
		}

		// Advance only from the current offset.
		later := at.Add(time.Hour)
		if ok, err := uploads.Advance(ctx, u.ID, 0, 6, []byte{1, 2, 3}, later.Add(24*time.Hour), later); err != nil || !ok {
			t.Fatalf("Advance = %v, %v", ok, err)
		}
		if ok, err := uploads.Advance(ctx, u.ID, 0, 4, []byte{9}, later, later); err != nil || ok {
			t.Errorf("Advance from a stale offset = %v, %v; want no change", ok, err)
		}
		// Not all bytes yet: can't complete.
		if ok, err := uploads.Complete(ctx, u.ID, "abc", later, later); err != nil || ok {
			t.Errorf("Complete of a partial upload = %v, %v; want no change", ok, err)
		}
		if ok, err := uploads.Advance(ctx, u.ID, 6, 10, []byte{4, 5}, later.Add(24*time.Hour), later); err != nil || !ok {
			t.Fatalf("Advance = %v, %v", ok, err)
		}
		got, _ = uploads.Get(ctx, u.ID)
		if got.ReceivedBytes != 10 || !bytes.Equal(got.HashState, []byte{4, 5}) || !got.ExpiresAt.Equal(later.Add(24*time.Hour)) || !got.UpdatedAt.Equal(later) {
			t.Errorf("after Advance = %+v", got)
		}

		// A claim needs a complete upload.
		job := store.Job{Type: "site.deploy", TargetType: store.JobTargetSite, TargetID: f.siteID}
		if err := store.NewJobs(db).Enqueue(ctx, &job); err != nil {
			t.Fatal(err)
		}
		if ok, err := uploads.Claim(ctx, u.ID, job.ID, later); err != nil || ok {
			t.Errorf("Claim of an incomplete upload = %v, %v; want no change", ok, err)
		}
		if ok, err := uploads.Complete(ctx, u.ID, "def", later.Add(48*time.Hour), later); err != nil || !ok {
			t.Fatalf("Complete = %v, %v", ok, err)
		}
		if ok, err := uploads.Complete(ctx, u.ID, "def", later, later); err != nil || ok {
			t.Errorf("Complete again = %v, %v; want no change", ok, err)
		}
		if ok, err := uploads.Advance(ctx, u.ID, 10, 11, nil, later, later); err != nil || ok {
			t.Errorf("Advance of a complete upload = %v, %v; want no change", ok, err)
		}
		got, _ = uploads.Get(ctx, u.ID)
		if got.Status != store.UploadComplete || got.SHA256 != "def" || !got.ExpiresAt.Equal(later.Add(48*time.Hour)) {
			t.Errorf("after Complete = %+v", got)
		}
		if _, err := uploads.Claim(ctx, u.ID, id.New(), later); !errors.Is(err, store.ErrReference) {
			t.Errorf("Claim for a missing job: %v, want ErrReference", err)
		}
		if ok, err := uploads.Claim(ctx, u.ID, job.ID, later); err != nil || !ok {
			t.Fatalf("Claim = %v, %v", ok, err)
		}
		if ok, err := uploads.Claim(ctx, u.ID, job.ID, later); err != nil || ok {
			t.Errorf("Claim again = %v, %v; want no change", ok, err)
		}
		got, _ = uploads.Get(ctx, u.ID)
		if got.Status != store.UploadInUse || got.JobID != job.ID || !got.ExpiresAt.IsZero() {
			t.Errorf("after Claim = %+v", got)
		}

		if ok, err := uploads.Exists(ctx, u.ID); err != nil || !ok {
			t.Errorf("Exists = %v, %v", ok, err)
		}
		if ok, err := uploads.Delete(ctx, u.ID); err != nil || !ok {
			t.Errorf("Delete = %v, %v", ok, err)
		}
		if ok, err := uploads.Delete(ctx, u.ID); err != nil || ok {
			t.Errorf("Delete again = %v, %v", ok, err)
		}
		if ok, err := uploads.Exists(ctx, u.ID); err != nil || ok {
			t.Errorf("Exists after Delete = %v, %v", ok, err)
		}
	})
}

func TestUploadsCleanupQueries(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		f := newUploadFixture(t, db)
		uploads := store.NewUploads(db)
		at := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
		create := func(expires time.Time) store.Upload {
			t.Helper()
			u := store.Upload{Purpose: store.UploadSiteImport, ClientGroupID: f.groupID, CreatedByUserID: f.userID, SizeBytes: 1, ExpiresAt: expires}
			if err := uploads.Create(ctx, &u, at); err != nil {
				t.Fatal(err)
			}
			return u
		}
		claim := func(u store.Upload, state store.JobState) (store.Upload, store.Job) {
			t.Helper()
			if ok, err := uploads.Advance(ctx, u.ID, 0, 1, nil, u.ExpiresAt, at); err != nil || !ok {
				t.Fatalf("Advance = %v, %v", ok, err)
			}
			if ok, err := uploads.Complete(ctx, u.ID, "x", u.ExpiresAt, at); err != nil || !ok {
				t.Fatalf("Complete = %v, %v", ok, err)
			}
			job := store.Job{Type: "site.import", TargetType: store.JobTargetClientGroup, TargetID: f.groupID}
			if err := store.NewJobs(db).Enqueue(ctx, &job); err != nil {
				t.Fatal(err)
			}
			if _, err := db.ExecContext(ctx, "UPDATE jobs SET state = ? WHERE id = ?", string(state), job.ID); err != nil {
				t.Fatal(err)
			}
			if ok, err := uploads.Claim(ctx, u.ID, job.ID, at); err != nil || !ok {
				t.Fatalf("Claim = %v, %v", ok, err)
			}
			return u, job
		}

		expired := create(at.Add(-time.Minute))
		fresh := create(at.Add(time.Hour))
		// In use by jobs: expiry doesn't matter, the job's state does.
		running, _ := claim(create(at.Add(-time.Hour)), store.JobRunning)
		claim(create(at.Add(-time.Hour)), store.JobRollingBack)
		succeeded, succeededJob := claim(create(at.Add(time.Hour)), store.JobSucceeded)
		failed, _ := claim(create(at.Add(time.Hour)), store.JobFailed)
		rollbackFailed, _ := claim(create(at.Add(time.Hour)), store.JobRollbackFailed)
		orphaned, orphanJob := claim(create(at.Add(time.Hour)), store.JobQueued)
		if _, err := db.ExecContext(ctx, "DELETE FROM jobs WHERE id = ?", orphanJob.ID); err != nil {
			t.Fatal(err)
		}

		ids := func(us []store.Upload) []string {
			var out []string
			for _, u := range us {
				out = append(out, u.ID)
			}
			slices.Sort(out)
			return out
		}
		sorted := func(s ...string) []string { slices.Sort(s); return s }

		got, err := uploads.Expired(ctx, at)
		if err != nil {
			t.Fatal(err)
		}
		if want := sorted(expired.ID); !slices.Equal(ids(got), want) {
			t.Errorf("Expired = %v, want %v", ids(got), want)
		}
		got, err = uploads.Finished(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if want := sorted(succeeded.ID, failed.ID, rollbackFailed.ID, orphaned.ID); !slices.Equal(ids(got), want) {
			t.Errorf("Finished = %v, want %v", ids(got), want)
		}

		// Conditional deletes.
		if ok, err := uploads.DeleteUnclaimed(ctx, succeeded.ID); err != nil || ok {
			t.Errorf("DeleteUnclaimed of a claimed upload = %v, %v", ok, err)
		}
		if ok, err := uploads.DeleteExpired(ctx, running.ID, at); err != nil || ok {
			t.Errorf("DeleteExpired of a claimed upload = %v, %v", ok, err)
		}
		if ok, err := uploads.DeleteExpired(ctx, fresh.ID, at); err != nil || ok {
			t.Errorf("DeleteExpired of an unexpired upload = %v, %v", ok, err)
		}
		if ok, err := uploads.DeleteExpired(ctx, expired.ID, at); err != nil || !ok {
			t.Errorf("DeleteExpired = %v, %v", ok, err)
		}
		if ok, err := uploads.DeleteClaimed(ctx, succeeded.ID, id.New()); err != nil || ok {
			t.Errorf("DeleteClaimed by another job = %v, %v", ok, err)
		}
		if ok, err := uploads.DeleteClaimed(ctx, succeeded.ID, succeededJob.ID); err != nil || !ok {
			t.Errorf("DeleteClaimed = %v, %v", ok, err)
		}
		if ok, err := uploads.DeleteClaimed(ctx, fresh.ID, ""); err != nil || ok {
			t.Errorf("DeleteClaimed of an unclaimed upload = %v, %v", ok, err)
		}

		// Uploads go with their site, group, and user.
		deploy := store.Upload{Purpose: store.UploadSiteDeploy, ClientGroupID: f.groupID, SiteID: f.siteID, CreatedByUserID: f.userID, SizeBytes: 1, ExpiresAt: at}
		if err := uploads.Create(ctx, &deploy, at); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, "DELETE FROM sites WHERE id = ?", f.siteID); err != nil {
			t.Fatal(err)
		}
		if _, err := uploads.Get(ctx, deploy.ID); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("upload of a deleted site: %v, want ErrNotFound", err)
		}
		if _, err := db.ExecContext(ctx, "DELETE FROM users WHERE id = ?", f.userID); err != nil {
			t.Fatal(err)
		}
		if ok, err := uploads.Exists(ctx, fresh.ID); err != nil || ok {
			t.Errorf("upload of a deleted user exists = %v, %v", ok, err)
		}
	})
}

func TestUploadTargetClientGroup(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		f := newUploadFixture(t, db)
		u := store.Upload{Purpose: store.UploadSiteImport, ClientGroupID: f.groupID, CreatedByUserID: f.userID, SizeBytes: 1, ExpiresAt: time.Now().Add(time.Hour)}
		if err := store.NewUploads(db).Create(ctx, &u, time.Now()); err != nil {
			t.Fatal(err)
		}
		for _, tt := range []struct{ id, want string }{{u.ID, f.groupID}, {id.New(), ""}} {
			got, err := store.TargetClientGroup(ctx, db, "upload", tt.id)
			if err != nil || got != tt.want {
				t.Errorf("TargetClientGroup(upload, %s) = %q, %v; want %q", tt.id, got, err, tt.want)
			}
		}
	})
}

// uploadsEqual compares uploads, whose HashState makes them incomparable
// with ==.
func uploadsEqual(a, b store.Upload) bool {
	if !bytes.Equal(a.HashState, b.HashState) {
		return false
	}
	a.HashState, b.HashState = nil, nil
	return a.ID == b.ID && a.Purpose == b.Purpose && a.ClientGroupID == b.ClientGroupID && a.SiteID == b.SiteID &&
		a.CreatedByUserID == b.CreatedByUserID && a.Filename == b.Filename && a.SizeBytes == b.SizeBytes &&
		a.ReceivedBytes == b.ReceivedBytes && a.SHA256 == b.SHA256 && a.Status == b.Status && a.JobID == b.JobID &&
		a.CreatedAt.Equal(b.CreatedAt) && a.UpdatedAt.Equal(b.UpdatedAt) && a.ExpiresAt.Equal(b.ExpiresAt)
}
