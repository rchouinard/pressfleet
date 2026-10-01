package metrics_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/id"
	"github.com/rchouinard/pressfleet/control-plane/internal/metrics"
	"github.com/rchouinard/pressfleet/control-plane/internal/metrics/metricstest"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
)

// addSite inserts a site row in a new client group.
func addSite(t *testing.T, db *store.DB, status store.SiteStatus) (siteID, groupID string) {
	t.Helper()
	ctx := context.Background()
	group := &store.ClientGroup{Name: "group-" + id.New()}
	if err := store.NewClientGroups(db).Create(ctx, group); err != nil {
		t.Fatal(err)
	}
	siteID = id.New()
	uid, err := store.AllocateSiteUID(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if _, err := db.ExecContext(ctx, `INSERT INTO sites
		(id, client_group_id, name, slug, status, preview_domain, php_version, wp_core_version, uid, created_at, updated_at)
		VALUES (?, ?, 'Site', ?, ?, ?, '8.4', '7.1.2', ?, ?, ?)`,
		siteID, group.ID, siteID, string(status), siteID+".preview.example", uid, now, now); err != nil {
		t.Fatal(err)
	}
	return siteID, group.ID
}

func siteIDs(ts []metrics.Target) []string {
	var out []string
	for _, t := range ts {
		out = append(out, t.SiteID)
	}
	slices.Sort(out)
	return out
}

func TestSiteTargets(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		ctx := context.Background()
		src := metrics.NewSiteTargets(db)
		if got, err := src.Targets(ctx); err != nil || len(got) != 0 {
			t.Fatalf("empty database: %v, %v", got, err)
		}
		a, ga := addSite(t, db, store.SiteRunning)
		b, _ := addSite(t, db, store.SiteStopped)
		addSite(t, db, store.SiteDeleting)

		got, err := src.Targets(ctx)
		if err != nil {
			t.Fatal(err)
		}
		want := []string{a, b}
		slices.Sort(want)
		if !slices.Equal(siteIDs(got), want) {
			t.Errorf("targets = %v, want %v", siteIDs(got), want)
		}
		for _, tg := range got {
			if tg.SiteID == a && (tg.ClientGroupID != ga || tg.Site.PHPVersion != "8.4") {
				t.Errorf("target = %+v", tg)
			}
		}
	})
}

func TestSiteTargetsPaginates(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		const n = 205
		for range n {
			addSite(t, db, store.SiteRunning)
		}
		got, err := metrics.NewSiteTargets(db).Targets(context.Background())
		if err != nil || len(got) != n {
			t.Fatalf("got %d targets, err %v; want %d", len(got), err, n)
		}
	})
}

// TestNewSitesAreCollectedWithoutRestart is the acceptance criterion of #67:
// a running service picks up sites created, and stops for sites deleted,
// after it started.
func TestNewSitesAreCollectedWithoutRestart(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		w := &metricstest.Writer{}
		start(t, newService(t, w, metrics.NewSiteTargets(db), metrics.InventoryCollector{}))

		first, firstGroup := addSite(t, db, store.SiteRunning)
		eventually(t, "first site", func() bool { return siteInfoCount(w, first) > 0 })

		second, secondGroup := addSite(t, db, store.SiteRunning)
		eventually(t, "site created after start", func() bool { return siteInfoCount(w, second) > 0 })
		for _, s := range w.Named("pressfleet_site_info") {
			want := map[string]string{first: firstGroup, second: secondGroup}[s.Labels["site_id"]]
			if s.Labels["client_group_id"] != want || s.Labels["status"] != "running" {
				t.Errorf("sample labels = %v, want client_group_id %s", s.Labels, want)
			}
		}

		if _, err := store.NewSites(db).Delete(context.Background(), first); err != nil {
			t.Fatal(err)
		}
		runsAfter(t, w, 2)
		before := siteInfoCount(w, first)
		runsAfter(t, w, 5)
		if after := siteInfoCount(w, first); after != before {
			t.Errorf("deleted site still collected: %d samples became %d", before, after)
		}
	})
}
