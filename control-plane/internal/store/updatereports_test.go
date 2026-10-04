package store_test

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/id"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
)

func TestUpdateReports(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		group := createGroup(ctx, t, db)
		sites := store.NewSites(db)
		s := newTestSite(group, "acme", 20000)
		if err := sites.Create(ctx, s); err != nil {
			t.Fatal(err)
		}
		reports := store.NewUpdateReports(db)

		if _, err := reports.Get(ctx, s.ID); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("Get before any check: err = %v, want ErrNotFound", err)
		}
		if all, err := reports.Counts(ctx); err != nil || len(all) != 0 {
			t.Fatalf("Counts before any check = %v, %v; want none", all, err)
		}

		checked := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
		want := store.UpdateReport{
			SiteID:       s.ID,
			UpdateCounts: store.UpdateCounts{Plugins: 2, Themes: 1, CheckedAt: checked},
			Updates: []store.PendingUpdate{
				{Kind: store.UpdateKindPlugin, Name: "akismet", Title: "Akismet", Version: "5.3", UpdateVersion: "5.4"},
				{Kind: store.UpdateKindPlugin, Name: "hello", Version: "1.7.2", UpdateVersion: "1.7.3"},
				{Kind: store.UpdateKindTheme, Name: "twentytwentyfive", Title: "Twenty Twenty-Five", Version: "1.0", UpdateVersion: "1.1"},
			},
		}
		// The counts come from the updates, whatever the caller says.
		in := want
		in.Plugins, in.Themes = 0, 0
		if err := reports.Put(ctx, in); err != nil {
			t.Fatal(err)
		}
		got, err := reports.Get(ctx, s.ID)
		if err != nil {
			t.Fatal(err)
		}
		if !got.CheckedAt.Equal(checked) {
			t.Errorf("CheckedAt = %v, want %v", got.CheckedAt, checked)
		}
		got.CheckedAt = checked
		if !reflect.DeepEqual(got, want) {
			t.Errorf("Get = %+v, want %+v", got, want)
		}
		all, err := reports.Counts(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if c := all[s.ID]; len(all) != 1 || c.Plugins != 2 || c.Themes != 1 || c.Total() != 3 || !c.CheckedAt.Equal(checked) {
			t.Errorf("Counts = %+v", all)
		}

		// A later check replaces the report; nothing pending is an empty
		// list, not a missing report.
		if err := reports.Put(ctx, store.UpdateReport{SiteID: s.ID}); err != nil {
			t.Fatal(err)
		}
		got, err = reports.Get(ctx, s.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Total() != 0 || got.Updates == nil || len(got.Updates) != 0 || !got.CheckedAt.After(checked) {
			t.Errorf("Get after an empty check = %+v", got)
		}

		if err := reports.Put(ctx, store.UpdateReport{SiteID: s.ID, Updates: []store.PendingUpdate{{Kind: "core"}}}); err == nil {
			t.Error("Put with an unknown kind: err = nil")
		}
		if err := reports.Put(ctx, store.UpdateReport{SiteID: id.New()}); !errors.Is(err, store.ErrReference) {
			t.Errorf("Put for a missing site: err = %v, want ErrReference", err)
		}

		// Reports go with their site.
		if _, err := sites.Delete(ctx, s.ID); err != nil {
			t.Fatal(err)
		}
		if all, err := reports.Counts(ctx); err != nil || len(all) != 0 {
			t.Errorf("Counts after deleting the site = %v, %v; want none", all, err)
		}
	})
}
