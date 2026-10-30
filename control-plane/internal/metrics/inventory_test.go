package metrics_test

import (
	"context"
	"testing"

	"github.com/rchouinard/pressfleet/control-plane/internal/metrics"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
)

func TestInventoryCollectorEnvironment(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		ctx := context.Background()
		prod, _ := addSite(t, db, store.SiteRunning)
		staging, _ := addSite(t, db, store.SiteStopped)
		makeStaging(t, db, staging, prod)

		targets, err := metrics.NewSiteTargets(db).Targets(ctx)
		if err != nil {
			t.Fatal(err)
		}
		samples, err := metrics.InventoryCollector{}.Collect(ctx, targets)
		if err != nil {
			t.Fatal(err)
		}
		for _, tc := range []struct{ site, env, parent string }{
			{prod, "production", ""},
			{staging, "staging", prod},
		} {
			if _, ok := findSample(samples, "pressfleet_site_info", "site_id", tc.site, "environment", tc.env, "parent_site_id", tc.parent); !ok {
				t.Errorf("no pressfleet_site_info for %s with environment %q and parent_site_id %q", tc.site, tc.env, tc.parent)
			}
		}
	})
}
