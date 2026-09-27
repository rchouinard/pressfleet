package store_test

import (
	"context"
	"testing"

	"github.com/rchouinard/pressfleet/control-plane/internal/id"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
)

// TestSiteMaintenance covers the maintenance flag (#44), including the
// conditional update.
func TestSiteMaintenance(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		sites := store.NewSites(db)
		s := newTestSite(createGroup(ctx, t, db), "acme", 20000)
		if err := sites.Create(ctx, s); err != nil {
			t.Fatal(err)
		}
		if s.Maintenance {
			t.Error("a new site is in maintenance mode")
		}
		for _, step := range []struct {
			enabled bool
			from    []bool
			changed bool
			want    bool
		}{
			{true, nil, true, true},
			{false, []bool{false}, false, true}, // it isn't false: no change
			{false, []bool{true}, true, false},
			{false, nil, true, false}, // unconditional updates always touch the row
		} {
			changed, err := sites.SetMaintenance(ctx, s.ID, step.enabled, step.from...)
			if err != nil {
				t.Fatal(err)
			}
			got, err := sites.Get(ctx, s.ID)
			if err != nil {
				t.Fatal(err)
			}
			if changed != step.changed || got.Maintenance != step.want {
				t.Errorf("SetMaintenance(%v, %v) = %v, maintenance %v; want %v, %v", step.enabled, step.from, changed, got.Maintenance, step.changed, step.want)
			}
		}
		if changed, err := sites.SetMaintenance(ctx, id.New(), true); err != nil || changed {
			t.Errorf("SetMaintenance on a missing site = %v, %v", changed, err)
		}
	})
}
