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

func newTestSite(groupID, slug string, uid int) *store.Site {
	return &store.Site{
		ClientGroupID:          groupID,
		Name:                   "Site " + slug,
		Slug:                   slug,
		Status:                 store.SiteProvisioning,
		PreviewDomain:          slug + ".preview.example",
		PHPVersion:             "8.4",
		WPCoreVersion:          "7.1.2",
		EnableCache:            true,
		CPULimit:               "1.0",
		MemoryReservationMB:    128,
		MemoryLimitMB:          512,
		UID:                    uid,
		WPSaltsSecretRef:       "salts-" + slug,
		AdminUsername:          "admin",
		AdminEmail:             "admin@example.com",
		AdminPasswordSecretRef: "pw-" + slug,
	}
}

func TestSites(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		group := createGroup(ctx, t, db)
		sites := store.NewSites(db)

		before := time.Now().Add(-time.Second)
		s := newTestSite(group, "acme", 20000)
		if err := sites.Create(ctx, s); err != nil {
			t.Fatal(err)
		}
		if !id.Valid(s.ID) || s.CreatedAt.Before(before) || s.UpdatedAt != s.CreatedAt {
			t.Errorf("Create did not fill in ID and timestamps: %+v", s)
		}
		got, err := sites.Get(ctx, s.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got != *s {
			t.Errorf("Get = %+v\nwant %+v", got, *s)
		}

		// Uniqueness and references.
		for name, dup := range map[string]*store.Site{
			"slug":           newTestSite(group, "acme", 20001),
			"uid":            newTestSite(group, "other", 20000),
			"preview domain": func() *store.Site { d := newTestSite(group, "x", 20002); d.PreviewDomain = s.PreviewDomain; return d }(),
		} {
			if err := sites.Create(ctx, dup); !errors.Is(err, store.ErrConflict) {
				t.Errorf("duplicate %s: err = %v, want ErrConflict", name, err)
			}
		}
		if err := sites.Create(ctx, newTestSite(id.New(), "orphan", 20003)); !errors.Is(err, store.ErrReference) {
			t.Errorf("missing group: err = %v, want ErrReference", err)
		}
		if err := sites.Create(ctx, &store.Site{Status: "bogus"}); err == nil {
			t.Error("Create with an invalid status succeeded")
		}
		if _, err := sites.Get(ctx, id.New()); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("Get missing: err = %v, want ErrNotFound", err)
		}

		for _, tt := range []struct {
			slug, domain string
			want         bool
		}{
			{"acme", "new.preview.example", true},
			{"new", "acme.preview.example", true},
			{"new", "new.preview.example", false},
		} {
			if got, err := sites.SlugTaken(ctx, tt.slug, tt.domain); err != nil || got != tt.want {
				t.Errorf("SlugTaken(%q, %q) = %v, %v; want %v", tt.slug, tt.domain, got, err, tt.want)
			}
		}

		// Conditional status changes.
		if ok, err := sites.SetStatus(ctx, s.ID, store.SiteRunning, "", store.SiteDeleting); err != nil || ok {
			t.Errorf("SetStatus from the wrong status = %v, %v", ok, err)
		}
		if ok, err := sites.SetStatus(ctx, s.ID, store.SiteFailed, "it broke", store.SiteProvisioning, store.SiteUpdating); err != nil || !ok {
			t.Errorf("SetStatus = %v, %v", ok, err)
		}
		if got, _ := sites.Get(ctx, s.ID); got.Status != store.SiteFailed || got.StatusMessage != "it broke" || got.UpdatedAt.Before(s.UpdatedAt) {
			t.Errorf("after SetStatus: %+v", got)
		}
		if ok, err := sites.SetStatus(ctx, id.New(), store.SiteRunning, ""); err != nil || ok {
			t.Errorf("SetStatus on a missing site = %v, %v", ok, err)
		}
		if _, err := sites.SetStatus(ctx, s.ID, "bogus", ""); err == nil {
			t.Error("SetStatus to an invalid status succeeded")
		}

		// The admin password is taken once.
		if ok, err := sites.TakeAdminPassword(ctx, s.ID, "wrong"); err != nil || ok {
			t.Errorf("TakeAdminPassword with the wrong ref = %v, %v", ok, err)
		}
		if ok, err := sites.TakeAdminPassword(ctx, s.ID, s.AdminPasswordSecretRef); err != nil || !ok {
			t.Errorf("TakeAdminPassword = %v, %v", ok, err)
		}
		if ok, err := sites.TakeAdminPassword(ctx, s.ID, s.AdminPasswordSecretRef); err != nil || ok {
			t.Errorf("TakeAdminPassword again = %v, %v", ok, err)
		}
		if got, _ := sites.Get(ctx, s.ID); got.AdminPasswordSecretRef != "" || got.WPSaltsSecretRef == "" {
			t.Errorf("after TakeAdminPassword: %+v", got)
		}
		if err := sites.ClearSecretRefs(ctx, s.ID); err != nil {
			t.Fatal(err)
		}
		if got, _ := sites.Get(ctx, s.ID); got.WPSaltsSecretRef != "" {
			t.Errorf("after ClearSecretRefs: %+v", got)
		}

		// Aliases, listing, and deletion.
		if _, err := db.ExecContext(ctx, "INSERT INTO site_aliases (domain, site_id, created_at) VALUES (?, ?, ?), (?, ?, ?)",
			"b.example", s.ID, time.Now(), "a.example", s.ID, time.Now()); err != nil {
			t.Fatal(err)
		}
		if got, err := sites.Aliases(ctx, s.ID); err != nil || !slices.Equal(got, []string{"a.example", "b.example"}) {
			t.Errorf("Aliases = %v, %v", got, err)
		}
		deleting := newTestSite(group, "deleting", 20010)
		deleting.Status = store.SiteDeleting
		if err := sites.Create(ctx, deleting); err != nil {
			t.Fatal(err)
		}
		if got, err := sites.ListIDs(ctx, store.SiteDeleting); err != nil || !slices.Equal(got, []string{s.ID}) {
			t.Errorf("ListIDs except deleting = %v, %v", got, err)
		}
		if got, err := sites.ListIDs(ctx); err != nil || len(got) != 2 {
			t.Errorf("ListIDs = %v, %v", got, err)
		}
		if ok, err := sites.Delete(ctx, s.ID); err != nil || !ok {
			t.Errorf("Delete = %v, %v", ok, err)
		}
		if ok, err := sites.Delete(ctx, s.ID); err != nil || ok {
			t.Errorf("Delete again = %v, %v", ok, err)
		}
		if got, err := sites.Aliases(ctx, s.ID); err != nil || len(got) != 0 {
			t.Errorf("Aliases after Delete = %v, %v", got, err)
		}
	})
}
