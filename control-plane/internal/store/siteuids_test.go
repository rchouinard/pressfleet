package store_test

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/id"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
)

// insertSiteWithUID inserts a minimal site row. uid may be nil.
func insertSiteWithUID(ctx context.Context, q store.Querier, groupID string, uid any) (string, error) {
	siteID := id.New()
	now := time.Now()
	_, err := q.ExecContext(ctx, `INSERT INTO sites
		(id, client_group_id, name, slug, status, preview_domain, php_version, wp_core_version, uid, created_at, updated_at)
		VALUES (?, ?, ?, ?, 'provisioning', ?, '8.4', '7.1.2', ?, ?, ?)`,
		siteID, groupID, siteID, siteID, siteID+".preview.example", uid, now, now)
	return siteID, err
}

func createGroup(ctx context.Context, t *testing.T, db *store.DB) string {
	t.Helper()
	g := &store.ClientGroup{Name: "Acme " + id.New()}
	if err := store.NewClientGroups(db).Create(ctx, g); err != nil {
		t.Fatal(err)
	}
	return g.ID
}

func TestAllocateSiteUID(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		group := createGroup(ctx, t, db)

		first, err := store.AllocateSiteUID(ctx, db)
		if err != nil {
			t.Fatal(err)
		}
		if first != provisioner.MinSiteUID {
			t.Errorf("first UID = %d, want %d", first, provisioner.MinSiteUID)
		}
		siteID, err := insertSiteWithUID(ctx, db, group, first)
		if err != nil {
			t.Fatal(err)
		}

		// A deleted site's UID is not handed out again.
		if _, err := db.ExecContext(ctx, "DELETE FROM sites WHERE id = ?", siteID); err != nil {
			t.Fatal(err)
		}
		second, err := store.AllocateSiteUID(ctx, db)
		if err != nil {
			t.Fatal(err)
		}
		if second != first+1 {
			t.Errorf("UID after deleting a site = %d, want %d", second, first+1)
		}

		// A rolled-back transaction doesn't use up a UID.
		rollback := errors.New("rollback")
		err = db.InTx(ctx, func(q store.Querier) error {
			if _, err := store.AllocateSiteUID(ctx, q); err != nil {
				return err
			}
			return rollback
		})
		if !errors.Is(err, rollback) {
			t.Fatal(err)
		}
		if got, err := store.AllocateSiteUID(ctx, db); err != nil || got != second+1 {
			t.Errorf("UID after rollback = %d, %v; want %d", got, err, second+1)
		}

		// The schema enforces a unique, non-null UID.
		if _, err := insertSiteWithUID(ctx, db, group, second); err != nil {
			t.Fatal(err)
		}
		if _, err := insertSiteWithUID(ctx, db, group, second); !errors.Is(err, store.ErrConflict) {
			t.Errorf("duplicate UID: err = %v, want ErrConflict", err)
		}
		if _, err := insertSiteWithUID(ctx, db, group, nil); err == nil {
			t.Error("site without a UID accepted")
		}
		if _, err := db.ExecContext(ctx, "UPDATE sites SET uid = NULL"); err == nil {
			t.Error("UID set to NULL")
		}

		// The range ends at MaxSiteUID.
		if _, err := db.ExecContext(ctx, "UPDATE site_uid_counter SET last_uid = ?", provisioner.MaxSiteUID-1); err != nil {
			t.Fatal(err)
		}
		if got, err := store.AllocateSiteUID(ctx, db); err != nil || got != provisioner.MaxSiteUID {
			t.Errorf("last UID = %d, %v; want %d", got, err, provisioner.MaxSiteUID)
		}
		for range 2 {
			if _, err := store.AllocateSiteUID(ctx, db); !errors.Is(err, store.ErrSiteUIDsExhausted) {
				t.Errorf("past the range: err = %v, want ErrSiteUIDsExhausted", err)
			}
		}
	})
}

// TestAllocateSiteUIDConcurrent creates sites from many goroutines at once,
// as concurrent provisioning jobs would, and checks every UID is distinct.
func TestAllocateSiteUIDConcurrent(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		group := createGroup(ctx, t, db)
		const n = 16
		var (
			wg   sync.WaitGroup
			mu   sync.Mutex
			uids []int
		)
		for range n {
			wg.Go(func() {
				err := db.InTx(ctx, func(q store.Querier) error {
					uid, err := store.AllocateSiteUID(ctx, q)
					if err != nil {
						return err
					}
					if _, err := insertSiteWithUID(ctx, q, group, uid); err != nil {
						return err
					}
					mu.Lock()
					uids = append(uids, uid)
					mu.Unlock()
					return nil
				})
				if err != nil {
					t.Error(err)
				}
			})
		}
		wg.Wait()
		slices.Sort(uids)
		if len(uids) != n || uids[0] != provisioner.MinSiteUID || uids[n-1] != provisioner.MinSiteUID+n-1 {
			t.Errorf("UIDs = %v, want %d consecutive UIDs from %d", uids, n, provisioner.MinSiteUID)
		}
	})
}

// TestSiteUIDMigrationBackfill migrates a database that already has sites
// (development databases) and checks they get UIDs and the counter follows.
func TestSiteUIDMigrationBackfill(t *testing.T) {
	ctx := context.Background()
	for _, d := range storetest.Dialects {
		t.Run(string(d), func(t *testing.T) {
			db := storetest.OpenUnmigrated(t, storetest.Config(t, d))
			backups := filepath.Join(t.TempDir(), "backups")
			if _, err := db.MigrateTo(ctx, backups, 4); err != nil {
				t.Fatal(err)
			}
			group := createGroup(ctx, t, db)
			var ids []string
			for range 3 {
				siteID := id.New()
				now := time.Now()
				if _, err := db.ExecContext(ctx, `INSERT INTO sites
					(id, client_group_id, name, slug, status, preview_domain, php_version, wp_core_version, created_at, updated_at)
					VALUES (?, ?, ?, ?, 'running', ?, '8.4', '7.1.2', ?, ?)`,
					siteID, group, siteID, siteID, siteID+".preview.example", now, now); err != nil {
					t.Fatal(err)
				}
				ids = append(ids, siteID)
			}
			if _, err := db.Migrate(ctx, backups); err != nil {
				t.Fatal(err)
			}
			slices.Sort(ids) // IDs made in the same millisecond needn't sort in creation order
			for i, siteID := range ids {
				var uid int
				var ref string
				if err := db.QueryRowContext(ctx, "SELECT uid, wp_salts_secret_ref FROM sites WHERE id = ?", siteID).Scan(&uid, &ref); err != nil {
					t.Fatal(err)
				}
				if uid != provisioner.MinSiteUID+i || ref != "" {
					t.Errorf("site %d: uid = %d, salts ref = %q", i, uid, ref)
				}
			}
			if got, err := store.AllocateSiteUID(ctx, db); err != nil || got != provisioner.MinSiteUID+3 {
				t.Errorf("first allocation after backfill = %d, %v", got, err)
			}
		})
	}
}
