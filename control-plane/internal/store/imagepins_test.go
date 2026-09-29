package store_test

import (
	"context"
	"errors"
	"maps"
	"testing"

	"github.com/rchouinard/pressfleet/control-plane/internal/id"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
)

func TestImagePins(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		group := createGroup(ctx, t, db)
		sites := store.NewSites(db)
		a, b := newTestSite(group, "a", 20000), newTestSite(group, "b", 20001)
		for _, s := range []*store.Site{a, b} {
			if err := sites.Create(ctx, s); err != nil {
				t.Fatal(err)
			}
		}
		pins := store.NewImagePins(db)
		if got, err := pins.Site(ctx, a.ID); err != nil || len(got) != 0 {
			t.Fatalf("pins of a new site = %v, %v", got, err)
		}
		const php, nginx = "r/site-php:php8.4-wp7.1.2", "r/site-nginx:wp7.1.2"
		for _, p := range []struct{ site, image, pinned string }{
			{a.ID, php, "r/site-php@sha256:1"},
			{a.ID, nginx, "r/site-nginx@sha256:2"},
			{b.ID, php, "r/site-php@sha256:1"},
			{a.ID, php, "r/site-php@sha256:3"}, // replaces the first
		} {
			if err := pins.Set(ctx, p.site, p.image, p.pinned); err != nil {
				t.Fatal(err)
			}
		}
		want := map[string]string{php: "r/site-php@sha256:3", nginx: "r/site-nginx@sha256:2"}
		if got, err := pins.Site(ctx, a.ID); err != nil || !maps.Equal(got, want) {
			t.Errorf("pins of a = %v, %v; want %v", got, err, want)
		}
		all, err := pins.All(ctx)
		if err != nil || len(all) != 2 || !maps.Equal(all[a.ID], want) || all[b.ID][php] != "r/site-php@sha256:1" {
			t.Errorf("all pins = %v, %v", all, err)
		}
		if err := pins.Set(ctx, id.New(), php, "x"); !errors.Is(err, store.ErrReference) {
			t.Errorf("pin of a missing site: err = %v, want ErrReference", err)
		}
		// Pins go with their site.
		if _, err := sites.Delete(ctx, a.ID); err != nil {
			t.Fatal(err)
		}
		if all, err := pins.All(ctx); err != nil || len(all) != 1 || all[a.ID] != nil {
			t.Errorf("pins after deleting a site = %v, %v", all, err)
		}
	})
}
