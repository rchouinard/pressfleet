package repository_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/backup/repository"
	"github.com/rchouinard/pressfleet/control-plane/internal/secrets"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
)

// slowS3 is an S3 server that holds a bucket with no objects. Its listing
// can be held (hold), which keeps a repository delete between its check
// for sites and the removal of its records, and made to fail (fail).
type slowS3 struct {
	hold    chan struct{} // closed to release listings; nil means never held
	entered chan struct{} // receives once per held listing
	fail    atomic.Bool
}

func (s *slowS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if s.fail.Load() {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`<Error><Code>InternalError</Code><Message>try again</Message></Error>`))
		return
	}
	if r.Method == http.MethodGet && s.hold != nil {
		s.entered <- struct{}{}
		select {
		case <-s.hold:
		case <-r.Context().Done():
			return
		}
	}
	_, _ = w.Write([]byte(`<ListBucketResult><IsTruncated>false</IsTruncated></ListBucketResult>`))
}

// s3Fixture is a Manager whose repositories live on a slowS3, and a way to
// give a group a repository record there without running restic.
func s3Fixture(t *testing.T, db *store.DB, s3 *slowS3) (fixture, string) {
	t.Helper()
	srv := httptest.NewServer(s3)
	t.Cleanup(srv.Close)
	return newFixture(t, db, repository.Options{
		Backend: repository.BackendS3,
		S3: repository.S3Config{
			Endpoint: srv.URL, Bucket: "pf-backups",
			AccessKeyID: "AKIDTEST", SecretAccessKey: "s3-test-secret-do-not-leak",
		},
		HTTPClient: srv.Client(),
	}), srv.URL
}

// addRepository records a repository for the group, as Ensure does.
func addRepository(t *testing.T, f fixture, url, groupID string) {
	t.Helper()
	ctx := context.Background()
	ref, err := f.secrets.Put(ctx, secrets.KindResticRepoKey, "pw")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.ExecContext(ctx,
		"INSERT INTO backup_repositories (client_group_id, backend, location, secret_ref, created_at) VALUES (?, 's3', ?, ?, ?)",
		groupID, "s3:"+url+"/pf-backups/"+groupID, string(ref), time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := store.NewClientGroups(f.db).SetBackupRepository(ctx, groupID, "s3:"+url+"/pf-backups/"+groupID); err != nil {
		t.Fatal(err)
	}
}

func (f fixture) repositoryExists(t *testing.T, groupID string) bool {
	t.Helper()
	var n int
	if err := f.db.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM backup_repositories WHERE client_group_id = ?", groupID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n == 1
}

var siteSeq atomic.Int64

func createSite(ctx context.Context, db *store.DB, groupID string) error {
	n := siteSeq.Add(1)
	s := store.Site{
		ClientGroupID: groupID, Name: "Site", Slug: fmt.Sprintf("site-%d", n), Status: store.SiteRunning,
		PreviewDomain: fmt.Sprintf("site-%d.preview.example", n), PHPVersion: "8.4", WPCoreVersion: "6.9.1", UID: 20000 + int(n),
	}
	return store.NewSites(db).Create(ctx, &s)
}

// A site can't be created in a group while its repository is being
// deleted, and the delete doesn't lose the repository of a site that was
// created before it checked (#351).
func TestDeleteRefusesSiteCreatedMeanwhile(t *testing.T) {
	t.Parallel()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		ctx := context.Background()
		s3 := &slowS3{hold: make(chan struct{}), entered: make(chan struct{}, 1)}
		f, url := s3Fixture(t, db, s3)
		groupID := f.group(t, "Acme")
		addRepository(t, f, url, groupID)

		done := make(chan error, 1)
		go func() { done <- f.m.Delete(ctx, groupID) }()
		select {
		case <-s3.entered: // the data is being removed
		case <-time.After(30 * time.Second):
			t.Fatal("Delete never started removing the data")
		}

		err := createSite(ctx, db, groupID)
		if !errors.Is(err, store.ErrRepositoryDeleting) {
			t.Errorf("creating a site during the delete: err = %v, want ErrRepositoryDeleting", err)
		}
		close(s3.hold)
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		if f.repositoryExists(t, groupID) {
			t.Error("the repository record is still there after Delete")
		}
		// Nothing is being deleted any more, and there was no site to lose.
		if err := createSite(ctx, db, groupID); err != nil {
			t.Errorf("creating a site after the delete: %v", err)
		}
		if err := f.m.Delete(ctx, groupID); err != nil {
			t.Errorf("Delete of a group without a repository: %v", err)
		}
	})
}

// If removing the data fails, the repository isn't left marked: sites can
// be created again, and the delete can be retried.
func TestDeleteFailureClearsMark(t *testing.T) {
	t.Parallel()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		ctx := context.Background()
		s3 := &slowS3{}
		f, url := s3Fixture(t, db, s3)
		groupID := f.group(t, "Acme")
		addRepository(t, f, url, groupID)

		s3.fail.Store(true)
		if err := f.m.Delete(ctx, groupID); err == nil {
			t.Fatal("Delete succeeded although S3 failed")
		}
		if !f.repositoryExists(t, groupID) {
			t.Fatal("the repository record is gone after a failed Delete")
		}
		if err := createSite(ctx, db, groupID); err != nil {
			t.Fatalf("creating a site after a failed Delete: %v", err)
		}
		// With the site, a retry is refused; without it, it works.
		if err := f.m.Delete(ctx, groupID); !errors.Is(err, repository.ErrGroupHasSites) {
			t.Fatalf("Delete with a site: err = %v, want ErrGroupHasSites", err)
		}
		if _, err := f.db.ExecContext(ctx, "DELETE FROM sites"); err != nil {
			t.Fatal(err)
		}
		s3.fail.Store(false)
		if err := f.m.Delete(ctx, groupID); err != nil {
			t.Fatalf("retried Delete: %v", err)
		}
		if f.repositoryExists(t, groupID) {
			t.Error("the repository record is still there after the retry")
		}
	})
}

// A mark left by a delete that a crash interrupted is cleared at startup,
// and the delete can be run again.
func TestClearDeleting(t *testing.T) {
	t.Parallel()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		ctx := context.Background()
		f, url := s3Fixture(t, db, &slowS3{})
		groupID := f.group(t, "Acme")
		addRepository(t, f, url, groupID)
		if _, err := db.ExecContext(ctx, "UPDATE backup_repositories SET deleting_at = ?", time.Now()); err != nil {
			t.Fatal(err)
		}
		if err := createSite(ctx, db, groupID); !errors.Is(err, store.ErrRepositoryDeleting) {
			t.Fatalf("creating a site in a marked group: err = %v, want ErrRepositoryDeleting", err)
		}
		if n, err := f.m.ClearDeleting(ctx); err != nil || n != 1 {
			t.Fatalf("ClearDeleting = %d, %v; want 1, nil", n, err)
		}
		if err := createSite(ctx, db, groupID); err != nil {
			t.Errorf("creating a site after ClearDeleting: %v", err)
		}
		if n, err := f.m.ClearDeleting(ctx); err != nil || n != 0 {
			t.Errorf("second ClearDeleting = %d, %v; want 0, nil", n, err)
		}
	})
}

// Creations and deletes that race end in a consistent state: one of them
// wins, or the create runs after the delete. The ordering that loses a
// repository can't be told from the legal one by the end state, so
// TestDeleteRefusesSiteCreatedMeanwhile pins that down; this one checks
// the transactions don't fail or deadlock when they overlap, notably on
// PostgreSQL.
func TestDeleteAndCreateConcurrently(t *testing.T) {
	t.Parallel()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		ctx := context.Background()
		f, url := s3Fixture(t, db, &slowS3{})
		for round := range 25 {
			groupID := f.group(t, fmt.Sprintf("Group %d", round))
			addRepository(t, f, url, groupID)
			var (
				wg                   sync.WaitGroup
				createErr, deleteErr error
				start                = make(chan struct{})
			)
			wg.Add(2)
			go func() {
				defer wg.Done()
				<-start
				createErr = createSite(ctx, db, groupID)
			}()
			go func() {
				defer wg.Done()
				<-start
				deleteErr = f.m.Delete(ctx, groupID)
			}()
			close(start)
			wg.Wait()

			sites := 0
			if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM sites WHERE client_group_id = ?", groupID).Scan(&sites); err != nil {
				t.Fatal(err)
			}
			switch {
			case errors.Is(deleteErr, repository.ErrGroupHasSites) && (sites != 1 || !f.repositoryExists(t, groupID)):
				t.Fatalf("round %d: Delete refused, but sites = %d", round, sites)
			case errors.Is(createErr, store.ErrRepositoryDeleting) && sites != 0:
				t.Fatalf("round %d: create refused, but the group has %d sites", round, sites)
			case createErr == nil && sites != 1:
				t.Fatalf("round %d: the site was created but is gone", round)
			case deleteErr != nil && !errors.Is(deleteErr, repository.ErrGroupHasSites):
				t.Fatalf("round %d: Delete: %v", round, deleteErr)
			case createErr != nil && !errors.Is(createErr, store.ErrRepositoryDeleting):
				t.Fatalf("round %d: create: %v", round, createErr)
			case deleteErr == nil && f.repositoryExists(t, groupID):
				t.Fatalf("round %d: Delete succeeded but the repository record remains", round)
			}
		}
	})
}
