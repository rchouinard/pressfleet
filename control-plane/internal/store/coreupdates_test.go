package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/id"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
)

func TestCorePolicies(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		group := createGroup(ctx, t, db)
		sites := store.NewSites(db)
		s := newTestSite(group, "acme", 20000)
		if err := sites.Create(ctx, s); err != nil {
			t.Fatal(err)
		}
		policies := store.NewCorePolicies(db)

		for _, tt := range []struct {
			name string
			get  func() (store.CorePolicy, error)
			set  func(store.CorePolicy) error
			all  func() (map[string]store.CorePolicy, error)
			id   string
		}{
			{"site", func() (store.CorePolicy, error) { return policies.Site(ctx, s.ID) },
				func(p store.CorePolicy) error { return policies.SetSite(ctx, s.ID, p) }, func() (map[string]store.CorePolicy, error) { return policies.Sites(ctx) }, s.ID},
			{"group", func() (store.CorePolicy, error) { return policies.ClientGroup(ctx, group) },
				func(p store.CorePolicy) error { return policies.SetClientGroup(ctx, group, p) }, func() (map[string]store.CorePolicy, error) { return policies.ClientGroups(ctx) }, group},
		} {
			t.Run(tt.name, func(t *testing.T) {
				p, err := tt.get()
				if err != nil || !p.IsZero() {
					t.Fatalf("new policy = %+v, %v; want none", p, err)
				}
				since := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
				for _, want := range []store.CorePolicy{
					{Hold: true, HoldSince: since},
					{ApprovedBranch: "7.2"},
					{Hold: true, HoldSince: since.Add(time.Hour), ApprovedBranch: "7.2"},
				} {
					if err := tt.set(want); err != nil {
						t.Fatal(err)
					}
					got, err := tt.get()
					if err != nil {
						t.Fatal(err)
					}
					if got.Hold != want.Hold || !got.HoldSince.Equal(want.HoldSince) || got.ApprovedBranch != want.ApprovedBranch || got.UpdatedAt.IsZero() {
						t.Errorf("policy = %+v, want %+v", got, want)
					}
					all, err := tt.all()
					if err != nil || len(all) != 1 || all[tt.id].ApprovedBranch != want.ApprovedBranch {
						t.Errorf("all policies = %+v, %v", all, err)
					}
				}
				if err := tt.set(store.CorePolicy{}); err != nil {
					t.Fatal(err)
				}
				if all, err := tt.all(); err != nil || len(all) != 0 {
					t.Errorf("after removing the policy: %+v, %v", all, err)
				}
			})
		}

		if err := policies.SetSite(ctx, id.New(), store.CorePolicy{Hold: true}); !errors.Is(err, store.ErrReference) {
			t.Errorf("policy of a missing site: err = %v, want ErrReference", err)
		}
		// Policies go with their site and group.
		if err := policies.SetSite(ctx, s.ID, store.CorePolicy{Hold: true}); err != nil {
			t.Fatal(err)
		}
		if err := policies.SetClientGroup(ctx, group, store.CorePolicy{Hold: true}); err != nil {
			t.Fatal(err)
		}
		if _, err := sites.Delete(ctx, s.ID); err != nil {
			t.Fatal(err)
		}
		if err := store.NewClientGroups(db).Delete(ctx, group); err != nil {
			t.Fatal(err)
		}
		for _, table := range []string{"site_core_policies", "client_group_core_policies"} {
			var n int
			if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table).Scan(&n); err != nil || n != 0 {
				t.Errorf("%s rows left: %d, %v", table, n, err)
			}
		}
	})
}

func TestRollouts(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		group := createGroup(ctx, t, db)
		sites := store.NewSites(db)
		var ids []string
		for i, slug := range []string{"a", "b", "c"} {
			s := newTestSite(group, slug, 20000+i)
			if err := sites.Create(ctx, s); err != nil {
				t.Fatal(err)
			}
			ids = append(ids, s.ID)
		}
		rollouts := store.NewRollouts(db)

		ro, err := rollouts.Get(ctx, "core")
		if err != nil || ro.Name != "core" || ro.Halted() || ro.ConsecutiveFailures != 0 {
			t.Fatalf("new rollout = %+v, %v", ro, err)
		}
		if ro, err = rollouts.Sync(ctx, "core", "7.2.1 7.1.4"); err != nil || ro.Fingerprint != "7.2.1 7.1.4" {
			t.Fatalf("Sync = %+v, %v", ro, err)
		}

		// A failure started by hand doesn't count.
		if ro, err = rollouts.RecordFailure(ctx, "core", store.RolloutFailure{SiteID: ids[0], Target: "7.2.1", Message: "boom"}, false, 2); err != nil || ro.ConsecutiveFailures != 0 {
			t.Fatalf("uncounted failure: %+v, %v", ro, err)
		}
		if f, ok, err := rollouts.Failure(ctx, "core", ids[0]); err != nil || !ok || f.Target != "7.2.1" || f.Message != "boom" || f.FailedAt.IsZero() {
			t.Errorf("Failure = %+v, %t, %v", f, ok, err)
		}
		// Two in a row halt it with a limit of two.
		if ro, err = rollouts.RecordFailure(ctx, "core", store.RolloutFailure{SiteID: ids[0], Target: "7.2.1"}, true, 2); err != nil || ro.ConsecutiveFailures != 1 || ro.Halted() {
			t.Fatalf("first failure: %+v, %v", ro, err)
		}
		if err := rollouts.RecordSuccess(ctx, "core", ids[0]); err != nil {
			t.Fatal(err)
		}
		if _, ok, _ := rollouts.Failure(ctx, "core", ids[0]); ok {
			t.Error("a success kept the site's failure record")
		}
		for i, siteID := range ids[1:] {
			if ro, err = rollouts.RecordFailure(ctx, "core", store.RolloutFailure{SiteID: siteID, Target: "7.2.1"}, true, 2); err != nil {
				t.Fatal(err)
			}
			if ro.ConsecutiveFailures != i+1 || ro.Halted() != (i == 1) {
				t.Errorf("after failure %d: %+v", i+1, ro)
			}
		}
		if ro.HaltReason == "" {
			t.Error("halted without a reason")
		}
		failures, err := rollouts.Failures(ctx, "core")
		if err != nil || len(failures) != 2 || failures[ids[1]].Target != "7.2.1" {
			t.Errorf("Failures = %+v, %v", failures, err)
		}

		// Syncing the same fingerprint keeps the halt; a new one lifts it.
		if ro, err = rollouts.Sync(ctx, "core", "7.2.1 7.1.4"); err != nil || !ro.Halted() {
			t.Errorf("Sync(same) = %+v, %v", ro, err)
		}
		if ro, err = rollouts.Sync(ctx, "core", "7.2.2 7.1.4"); err != nil || ro.Halted() || ro.ConsecutiveFailures != 0 {
			t.Errorf("Sync(new) = %+v, %v", ro, err)
		}
		if _, err := rollouts.RecordFailure(ctx, "core", store.RolloutFailure{SiteID: ids[1], Target: "7.2.2"}, true, 1); err != nil {
			t.Fatal(err)
		}
		if ro, err = rollouts.Resume(ctx, "core"); err != nil || ro.Halted() || ro.ConsecutiveFailures != 0 {
			t.Errorf("Resume = %+v, %v", ro, err)
		}
		if err := rollouts.ClearFailure(ctx, "core", ids[1]); err != nil {
			t.Fatal(err)
		}
		if _, err := rollouts.RecordFailure(ctx, "core", store.RolloutFailure{SiteID: id.New(), Target: "7.2.2"}, true, 1); !errors.Is(err, store.ErrReference) {
			t.Errorf("failure of a missing site: err = %v, want ErrReference", err)
		}
		// Failure records go with their site.
		if _, err := sites.Delete(ctx, ids[2]); err != nil {
			t.Fatal(err)
		}
		if failures, err := rollouts.Failures(ctx, "core"); err != nil || len(failures) != 0 {
			t.Errorf("Failures after deleting the site = %+v, %v", failures, err)
		}
	})
}

func TestMigratePinnedSitesToHolds(t *testing.T) {
	ctx := context.Background()
	for _, d := range storetest.Dialects {
		t.Run(string(d), func(t *testing.T) {
			db := storetest.OpenUnmigrated(t, storetest.Config(t, d))
			if _, err := db.MigrateTo(ctx, t.TempDir(), 18); err != nil {
				t.Fatal(err)
			}
			group := createGroup(ctx, t, db)
			at := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
			for _, s := range []struct {
				slug   string
				pinned bool
			}{{"pinned", true}, {"free", false}} {
				if _, err := db.ExecContext(ctx, "INSERT INTO sites (id, client_group_id, name, slug, status, preview_domain, "+
					"php_version, wp_core_version, wp_core_pinned, uid, created_at, updated_at) VALUES (?, ?, ?, ?, 'running', ?, '8.4', '7.1.2', ?, ?, ?, ?)",
					s.slug, group, s.slug, s.slug, s.slug+".example.test", s.pinned, map[bool]int{true: 20000, false: 20001}[s.pinned], at, at); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := db.Migrate(ctx, t.TempDir()); err != nil {
				t.Fatal(err)
			}
			policies := store.NewCorePolicies(db)
			if p, err := policies.Site(ctx, "pinned"); err != nil || !p.Hold || !p.HoldSince.Equal(at) || p.ApprovedBranch != "" {
				t.Errorf("pinned site's policy = %+v, %v; want a hold since %v", p, err, at)
			}
			if p, err := policies.Site(ctx, "free"); err != nil || !p.IsZero() {
				t.Errorf("other site's policy = %+v, %v; want none", p, err)
			}
		})
	}
}
