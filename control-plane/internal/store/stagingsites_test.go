package store_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/id"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
)

func sorted(ids ...string) []string {
	slices.Sort(ids)
	return ids
}

func TestStagingSites(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		group := createGroup(ctx, t, db)
		sites := store.NewSites(db)
		prod := newTestSite(group, "acme", 21000)
		other := newTestSite(group, "other", 21001)
		for _, s := range []*store.Site{prod, other} {
			if err := sites.Create(ctx, s); err != nil {
				t.Fatal(err)
			}
		}
		if prod.IsStaging() {
			t.Error("a site without a parent is staging")
		}

		refreshed := time.Date(2026, 10, 30, 12, 0, 0, 123456000, time.UTC)
		pushed := refreshed.Add(time.Hour)
		stg := newTestSite(group, "acme-staging", 21002)
		stg.ParentSiteID, stg.MailDisabled = prod.ID, true
		stg.StagingRefreshedAt, stg.StagingSnapshotID, stg.StagingPushedAt = refreshed, "abc123", pushed
		if err := sites.Create(ctx, stg); err != nil {
			t.Fatal(err)
		}
		bare := newTestSite(group, "acme-staging-2", 21003)
		bare.ParentSiteID = prod.ID
		if err := sites.Create(ctx, bare); err != nil {
			t.Fatal(err)
		}

		staged, err := sites.StagingIDs(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(staged) != 2 || !staged[stg.ID] || !staged[bare.ID] || staged[prod.ID] || staged[other.ID] {
			t.Errorf("StagingIDs = %v, want only %s and %s", staged, stg.ID, bare.ID)
		}

		got, err := sites.Get(ctx, stg.ID)
		if err != nil {
			t.Fatal(err)
		}
		if !got.IsStaging() || got.ParentSiteID != prod.ID || !got.MailDisabled || got.StagingSnapshotID != "abc123" ||
			!got.StagingRefreshedAt.Equal(refreshed) || !got.StagingPushedAt.Equal(pushed) {
			t.Errorf("staging site = %+v", got)
		}
		if got, _ := sites.Get(ctx, bare.ID); !got.IsStaging() || !got.StagingRefreshedAt.IsZero() ||
			got.StagingSnapshotID != "" || !got.StagingPushedAt.IsZero() || got.MailDisabled {
			t.Errorf("never refreshed = %+v", got)
		}
		if got, _ := sites.Get(ctx, prod.ID); got.IsStaging() {
			t.Errorf("production site = %+v", got)
		}

		// A parent has to exist.
		orphan := newTestSite(group, "orphan", 21004)
		orphan.ParentSiteID = id.New()
		if err := sites.Create(ctx, orphan); !errors.Is(err, store.ErrReference) {
			t.Errorf("missing parent: err = %v, want ErrReference", err)
		}

		ids := func(list []store.Site) []string {
			var out []string
			for _, s := range list {
				out = append(out, s.ID)
			}
			slices.Sort(out) // IDs made in the same millisecond aren't ordered
			return out
		}
		kids, err := sites.Children(ctx, prod.ID)
		if err != nil || !slices.Equal(ids(kids), sorted(stg.ID, bare.ID)) {
			t.Errorf("Children = %v, %v", ids(kids), err)
		}
		if kids, err := sites.Children(ctx, other.ID); err != nil || len(kids) != 0 {
			t.Errorf("Children of a site without any = %v, %v", ids(kids), err)
		}

		for _, tc := range []struct {
			name string
			f    store.SiteFilter
			want []string
		}{
			{"parent", store.SiteFilter{ParentSiteID: prod.ID}, sorted(bare.ID, stg.ID)},
			{"other parent", store.SiteFilter{ParentSiteID: other.ID}, nil},
			{"production", store.SiteFilter{Environment: store.EnvProduction}, sorted(other.ID, prod.ID)},
			{"staging", store.SiteFilter{Environment: store.EnvStaging}, sorted(bare.ID, stg.ID)},
			{"production with a parent", store.SiteFilter{Environment: store.EnvProduction, ParentSiteID: prod.ID}, nil},
		} {
			page, err := sites.List(ctx, tc.f)
			if err != nil || !slices.Equal(ids(page.Sites), tc.want) {
				t.Errorf("%s: List = %v, %v; want %v", tc.name, ids(page.Sites), err, tc.want)
			}
		}

		// The mail switch is a setting, kept with the others.
		set := got.Settings()
		set.MailDisabled = false
		if ok, err := sites.SetSettings(ctx, stg.ID, set); err != nil || !ok {
			t.Fatalf("SetSettings = %v, %v", ok, err)
		}
		if got, _ := sites.Get(ctx, stg.ID); got.MailDisabled || got.ParentSiteID != prod.ID {
			t.Errorf("after SetSettings = %+v", got)
		}

		// A parent with children can't be deleted.
		if _, err := sites.Delete(ctx, prod.ID); !errors.Is(err, store.ErrReference) {
			t.Errorf("delete parent: err = %v, want ErrReference", err)
		}
	})
}

func TestSetStagingRefreshed(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		group := createGroup(ctx, t, db)
		sites := store.NewSites(db)
		prod := newTestSite(group, "acme", 21100)
		if err := sites.Create(ctx, prod); err != nil {
			t.Fatal(err)
		}
		stg := newTestSite(group, "acme-staging", 21101)
		stg.ParentSiteID = prod.ID
		if err := sites.Create(ctx, stg); err != nil {
			t.Fatal(err)
		}
		at := time.Date(2026, 10, 30, 12, 0, 0, 123456000, time.FixedZone("EST", -5*3600))
		if ok, err := sites.SetStagingRefreshed(ctx, stg.ID, "abc123", at); err != nil || !ok {
			t.Fatalf("SetStagingRefreshed = %v, %v", ok, err)
		}
		got, err := sites.Get(ctx, stg.ID)
		if err != nil || got.StagingSnapshotID != "abc123" || !got.StagingRefreshedAt.Equal(at) || !got.StagingPushedAt.IsZero() {
			t.Errorf("after SetStagingRefreshed = %+v, %v", got, err)
		}
		if ok, err := sites.SetStagingRefreshed(ctx, id.New(), "abc123", at); err != nil || ok {
			t.Errorf("missing site: %v, %v", ok, err)
		}
		for _, bad := range []struct {
			snap string
			at   time.Time
		}{{"", at}, {"abc123", time.Time{}}} {
			if _, err := sites.SetStagingRefreshed(ctx, stg.ID, bad.snap, bad.at); err == nil {
				t.Errorf("SetStagingRefreshed(%q, %v) accepted", bad.snap, bad.at)
			}
		}
	})
}

func TestSetStagingPushed(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		group := createGroup(ctx, t, db)
		sites := store.NewSites(db)
		prod := newTestSite(group, "acme", 21200)
		if err := sites.Create(ctx, prod); err != nil {
			t.Fatal(err)
		}
		stg := newTestSite(group, "acme-staging", 21201)
		stg.ParentSiteID = prod.ID
		if err := sites.Create(ctx, stg); err != nil {
			t.Fatal(err)
		}
		at := time.Date(2026, 10, 30, 12, 0, 0, 123456000, time.FixedZone("EST", -5*3600))
		if ok, err := sites.SetStagingPushed(ctx, stg.ID, at); err != nil || !ok {
			t.Fatalf("SetStagingPushed = %v, %v", ok, err)
		}
		got, err := sites.Get(ctx, stg.ID)
		if err != nil || !got.StagingPushedAt.Equal(at) || !got.StagingRefreshedAt.IsZero() || got.StagingSnapshotID != "" {
			t.Errorf("after SetStagingPushed = %+v, %v", got, err)
		}
		// A staging site can be deleted while its push runs.
		if ok, err := sites.SetStagingPushed(ctx, id.New(), at); err != nil || ok {
			t.Errorf("missing site: %v, %v", ok, err)
		}
		if _, err := sites.SetStagingPushed(ctx, stg.ID, time.Time{}); err == nil {
			t.Error("SetStagingPushed accepted no time")
		}
	})
}

// TestSiteLock checks that a transaction that locks a site sees what
// another one that locked it first committed: on PostgreSQL through the row
// lock, on SQLite because write transactions run one at a time.
func TestSiteLock(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		group := createGroup(ctx, t, db)
		sites := store.NewSites(db)
		s := newTestSite(group, "acme", 21200)
		s.Status = store.SiteRunning
		if err := sites.Create(ctx, s); err != nil {
			t.Fatal(err)
		}
		if _, err := sites.Lock(ctx, id.New()); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("Lock of a missing site: %v", err)
		}

		locked := make(chan struct{})
		seen := make(chan store.SiteStatus, 1)
		errs := make(chan error, 2)
		go func() {
			errs <- db.InTx(ctx, func(q store.Querier) error {
				if _, err := store.NewSites(q).Lock(ctx, s.ID); err != nil {
					return err
				}
				close(locked)
				// Give the other transaction time to reach its lock.
				time.Sleep(100 * time.Millisecond)
				_, err := store.NewSites(q).SetStatus(ctx, s.ID, store.SiteDeleting, "")
				return err
			})
		}()
		<-locked
		go func() {
			errs <- db.InTx(ctx, func(q store.Querier) error {
				got, err := store.NewSites(q).Lock(ctx, s.ID)
				seen <- got.Status
				return err
			})
		}()
		for range 2 {
			if err := <-errs; err != nil {
				t.Fatal(err)
			}
		}
		if got := <-seen; got != store.SiteDeleting {
			t.Errorf("the second transaction saw %s, want deleting", got)
		}
	})
}
