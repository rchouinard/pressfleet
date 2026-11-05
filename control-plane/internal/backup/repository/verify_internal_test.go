package repository

import (
	"context"
	"errors"
	"log/slog"
	"maps"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/rchouinard/pressfleet/control-plane/internal/config"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner/provisionertest"
	"github.com/rchouinard/pressfleet/control-plane/internal/secrets"
	"github.com/rchouinard/pressfleet/control-plane/internal/secrets/secretstest"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
)

// s3Restic acts like restic on the fake S3 server just enough for verify:
// each repository opens with the password it was initialized with, and its
// config object exists once it is.
type s3Restic struct {
	s3 *fakeS3
	mu sync.Mutex
	// passwords maps a repository's key in the bucket to its password.
	passwords map[string]string
}

func (r *s3Restic) run(_ string, task provisioner.Task) (*provisioner.TaskResult, error) {
	obj, err := parseS3Location(task.Env["RESTIC_REPOSITORY"])
	if err != nil {
		return nil, err
	}
	pw := task.SecretFiles[PasswordFile].Reveal()
	r.mu.Lock()
	defer r.mu.Unlock()
	switch {
	case slices.Contains(task.Command, "init"):
		if _, ok := r.passwords[obj.key]; ok {
			return nil, &provisioner.ExecError{ExitCode: 1, Stderr: "Fatal: create repository failed: config file already exists"}
		}
		r.passwords[obj.key] = pw
		r.s3.mu.Lock()
		r.s3.objects[obj.key+"/config"] = "c"
		r.s3.objects[obj.key+"/keys/k-"+pw] = "k"
		r.s3.mu.Unlock()
	case slices.Contains(task.Command, "cat"):
		want, ok := r.passwords[obj.key]
		switch {
		case !ok:
			return nil, &provisioner.ExecError{ExitCode: 10, Stderr: "Fatal: repository does not exist"}
		case want != pw:
			return nil, &provisioner.ExecError{ExitCode: 12, Stderr: "Fatal: wrong password or no key found"}
		}
	}
	return &provisioner.TaskResult{}, nil
}

// s3VerifyFixture is a Manager whose repositories are on the fake S3
// server under the prefix "pf", with a group whose repository objects are
// already there, made with a password the state database doesn't have:
// what a state restore leaves behind.
func s3VerifyFixture(t *testing.T, db *store.DB) (*Manager, *fakeS3, *s3Restic, *provisionertest.Fake, string) {
	t.Helper()
	f, c := newFakeS3(t, map[string]string{})
	g := store.ClientGroup{Name: "Acme"}
	if err := store.NewClientGroups(db).Create(context.Background(), &g); err != nil {
		t.Fatal(err)
	}
	old := "pf/" + g.ID
	maps.Copy(f.objects, map[string]string{old + "/config": "c", old + "/keys/k-old": "k", old + "/snapshots/s1": "s"})
	r := &s3Restic{s3: f, passwords: map[string]string{old: "old password"}}
	fake := provisionertest.New()
	fake.RunRepositoryTaskFunc = r.run
	m, err := New(Options{
		DB: db, Secrets: secrets.NewStore(db, secretstest.Keyring(t, 1)), Layout: config.Layout{Root: t.TempDir()},
		Backend: BackendS3, Tasks: fake, Logger: slog.New(slog.DiscardHandler),
		S3: S3Config{
			Endpoint: c.endpoint.String(), Bucket: f.bucket, Prefix: "pf",
			AccessKeyID: testKeyID, SecretAccessKey: testSecret,
		},
		HTTPClient: c.http,
	})
	if err != nil {
		t.Fatal(err)
	}
	return m, f, r, fake, g.ID
}

func repositoryTaskCount(fake *provisionertest.Fake) int {
	n := 0
	for _, c := range fake.Calls() {
		if c.Op == "RunRepositoryTask" {
			n++
		}
	}
	return n
}

// On S3, a repository the stored password never opened stays where it is,
// and the group's record moves to a new repository beside it, which is
// recorded in the audit log. Nothing is deleted. Once it is verified,
// backups run no task before their own.
func TestVerifyS3SetsAside(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		ctx := context.Background()
		m, f, r, fake, groupID := s3VerifyFixture(t, db)
		oldObjects := maps.Clone(f.objects)
		oldLocation := s3Location(m.s3, groupID)

		access, err := m.TaskAccess(ctx, groupID)
		if err != nil {
			t.Fatal(err)
		}
		rec, err := m.Get(ctx, groupID)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(rec.Location, oldLocation+"-") || rec.PasswordVerifiedAt.IsZero() {
			t.Fatalf("record = %+v, want it verified at %s-<time>", rec, oldLocation)
		}
		if access.Env["RESTIC_REPOSITORY"] != rec.Location {
			t.Errorf("task repository = %s, want %s", access.Env["RESTIC_REPOSITORY"], rec.Location)
		}
		g, err := store.NewClientGroups(db).Get(ctx, groupID)
		if err != nil || g.BackupRepository != rec.Location {
			t.Errorf("client group backup_repository = %q, %v; want %s", g.BackupRepository, err, rec.Location)
		}
		for k, v := range oldObjects {
			if f.objects[k] != v {
				t.Errorf("object %s of the old repository changed or was deleted", k)
			}
		}
		obj, err := parseS3Location(rec.Location)
		if err != nil {
			t.Fatal(err)
		}
		pw, err := m.password(ctx, rec)
		if err != nil {
			t.Fatal(err)
		}
		if r.passwords[obj.key] != pw.Reveal() {
			t.Errorf("new repository %s wasn't initialized with the stored password", obj.key)
		}

		page, err := store.NewAuditEvents(db).List(ctx, store.AuditFilter{Action: AuditActionSetAside, TargetID: groupID})
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Events) != 1 || page.Events[0].Details["unopenable_location"] != oldLocation || page.Events[0].Details["new_location"] != rec.Location {
			t.Errorf("audit events = %+v", page.Events)
		}

		before := repositoryTaskCount(fake)
		if _, err := m.TaskAccess(ctx, groupID); err != nil {
			t.Fatal(err)
		}
		if n := repositoryTaskCount(fake) - before; n != 0 {
			t.Errorf("TaskAccess of the verified repository ran %d tasks, want 0", n)
		}
	})
}

// An S3 failure is returned and changes nothing, and a retry works; so does
// a retry after a crash between moving the record and creating the new
// repository.
func TestVerifyS3Retries(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		ctx := context.Background()
		m, f, _, _, groupID := s3VerifyFixture(t, db)
		oldLocation := s3Location(m.s3, groupID)

		f.mu.Lock()
		f.fail = http.StatusServiceUnavailable
		f.mu.Unlock()
		if _, err := m.TaskAccess(ctx, groupID); err == nil || errors.Is(err, ErrWrongPassword) {
			t.Fatalf("TaskAccess with S3 down: err = %v, want the S3 error", err)
		}
		f.mu.Lock()
		f.fail = 0
		f.mu.Unlock()
		rec, err := m.Get(ctx, groupID)
		if err != nil || rec.Location != oldLocation || !rec.PasswordVerifiedAt.IsZero() {
			t.Fatalf("record after the failure = %+v, %v; want it unverified at %s", rec, err, oldLocation)
		}

		// A crash after the record moved: init never ran.
		pw, err := m.password(ctx, rec)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := m.setAside(ctx, rec, pw); err != nil {
			t.Fatal(err)
		}
		moved, err := m.Get(ctx, groupID)
		if err != nil || moved.Location == oldLocation || !moved.PasswordVerifiedAt.IsZero() {
			t.Fatalf("record after setAside = %+v, %v", moved, err)
		}

		if _, err := m.TaskAccess(ctx, groupID); err != nil {
			t.Fatalf("retry: %v", err)
		}
		rec, err = m.Get(ctx, groupID)
		if err != nil || rec.Location != moved.Location || rec.PasswordVerifiedAt.IsZero() {
			t.Errorf("record after the retry = %+v, %v; want it verified at %s", rec, err, moved.Location)
		}
		if _, ok := f.objects["pf/"+groupID+"/snapshots/s1"]; !ok {
			t.Error("the old repository's snapshot was deleted")
		}
	})
}
