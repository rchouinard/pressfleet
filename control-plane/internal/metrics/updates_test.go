package metrics_test

import (
	"context"
	"testing"

	"github.com/rchouinard/pressfleet/control-plane/internal/metrics"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
)

func TestUpdatesCollector(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		ctx := context.Background()
		pending, pendingGroup := addSite(t, db, store.SiteRunning)
		current, _ := addSite(t, db, store.SiteRunning)
		unchecked, _ := addSite(t, db, store.SiteRunning)

		reports := store.NewUpdateReports(db)
		if err := reports.Put(ctx, store.UpdateReport{SiteID: pending, Updates: []store.PendingUpdate{
			{Kind: store.UpdateKindPlugin, Name: "a"}, {Kind: store.UpdateKindPlugin, Name: "b"}, {Kind: store.UpdateKindTheme, Name: "c"},
		}}); err != nil {
			t.Fatal(err)
		}
		if err := reports.Put(ctx, store.UpdateReport{SiteID: current}); err != nil {
			t.Fatal(err)
		}

		targets, err := metrics.NewSiteTargets(db).Targets(ctx)
		if err != nil {
			t.Fatal(err)
		}
		samples, err := metrics.UpdatesCollector{DB: db}.Collect(ctx, targets)
		if err != nil {
			t.Fatal(err)
		}
		const name = "pressfleet_site_pending_updates"
		for _, tt := range []struct {
			site, kind string
			want       float64
		}{
			{pending, "plugin", 2},
			{pending, "theme", 1},
			{current, "plugin", 0},
			{current, "theme", 0},
		} {
			if v, ok := findSample(samples, name, "site_id", tt.site, "kind", tt.kind); !ok || v != tt.want {
				t.Errorf("%s{site_id=%s,kind=%s} = %v, %v; want %v", name, tt.site, tt.kind, v, ok, tt.want)
			}
		}
		if _, ok := findSample(samples, name, "site_id", pending, "client_group_id", pendingGroup); !ok {
			t.Error("samples lack the client group label")
		}
		if _, ok := findSample(samples, name, "site_id", unchecked); ok {
			t.Error("a site that hasn't been checked has samples")
		}
		if len(samples) != 4 {
			t.Errorf("%d samples, want 4: %+v", len(samples), samples)
		}
	})
}
