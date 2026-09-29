package store_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
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
		if !reflect.DeepEqual(got, *s) {
			t.Errorf("Get = %+v\nwant %+v", got, *s)
		}
		if got, err := sites.GetBySlug(ctx, "acme"); err != nil || !reflect.DeepEqual(got, *s) {
			t.Errorf("GetBySlug = %+v, %v\nwant %+v", got, err, *s)
		}
		if _, err := sites.GetBySlug(ctx, "nope"); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("GetBySlug of a missing slug: %v, want ErrNotFound", err)
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

func TestSiteListAndSettings(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		groupA, groupB := createGroup(ctx, t, db), createGroup(ctx, t, db)
		sites := store.NewSites(db)
		var created []string
		for i := range 5 {
			group := groupA
			if i%2 == 1 {
				group = groupB
			}
			s := newTestSite(group, fmt.Sprintf("site-%d", i), 20000+i)
			if i == 4 {
				s.Status = store.SiteRunning
			}
			if err := sites.Create(ctx, s); err != nil {
				t.Fatal(err)
			}
			created = append(created, s.ID)
		}
		newestFirst := slices.Clone(created)
		slices.Sort(newestFirst)
		slices.Reverse(newestFirst)
		idsOf := func(p store.SitePage) []string {
			var out []string
			for _, s := range p.Sites {
				out = append(out, s.ID)
			}
			return out
		}

		// Paging through everything visits every site once, newest first.
		var all []string
		cursor := ""
		for range 10 {
			page, err := sites.List(ctx, store.SiteFilter{Before: cursor, Limit: 2})
			if err != nil {
				t.Fatal(err)
			}
			all = append(all, idsOf(page)...)
			if cursor = page.NextCursor; cursor == "" {
				break
			}
		}
		if !slices.Equal(all, newestFirst) {
			t.Errorf("paged = %v\nwant %v", all, newestFirst)
		}
		page, err := sites.List(ctx, store.SiteFilter{ClientGroupID: groupB})
		if err != nil || len(page.Sites) != 2 || page.NextCursor != "" {
			t.Errorf("group filter = %v, %v", idsOf(page), err)
		}
		page, err = sites.List(ctx, store.SiteFilter{ClientGroupID: groupA, Status: store.SiteRunning})
		if err != nil || !slices.Equal(idsOf(page), []string{created[4]}) {
			t.Errorf("group and status filter = %v, %v", idsOf(page), err)
		}
		if page, err := sites.List(ctx, store.SiteFilter{ClientGroupID: id.New()}); err != nil || page.Sites == nil || len(page.Sites) != 0 {
			t.Errorf("unknown group = %+v, %v", page, err)
		}

		// Aliases for a page of sites.
		now := time.Now()
		if _, err := db.ExecContext(ctx, "INSERT INTO site_aliases (domain, site_id, created_at) VALUES (?, ?, ?), (?, ?, ?), (?, ?, ?)",
			"z.example", created[0], now, "a.example", created[0], now, "b.example", created[1], now); err != nil {
			t.Fatal(err)
		}
		aliases, err := sites.AliasesForSites(ctx, created[:3])
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(aliases[created[0]], []string{"a.example", "z.example"}) || !slices.Equal(aliases[created[1]], []string{"b.example"}) ||
			len(aliases[created[2]]) != 0 {
			t.Errorf("AliasesForSites = %v", aliases)
		}
		if got, err := sites.AliasesForSites(ctx, nil); err != nil || len(got) != 0 {
			t.Errorf("AliasesForSites(nil) = %v, %v", got, err)
		}

		// Settings.
		before, _ := sites.Get(ctx, created[0])
		want := store.SiteSettings{PHPVersion: "8.3", CPULimit: "2.5", MemoryReservationMB: 256, MemoryLimitMB: 1024,
			PreviewProtection: true, PreviewUsername: "preview"}
		if ok, err := sites.SetSettings(ctx, created[0], want); err != nil || !ok {
			t.Fatalf("SetSettings = %v, %v", ok, err)
		}
		got, _ := sites.Get(ctx, created[0])
		if got.Settings() != want || got.UpdatedAt.Before(before.UpdatedAt) || before.WithSettings(want).Settings() != want {
			t.Errorf("after SetSettings: %+v", got)
		}
		if ok, err := sites.SetSettings(ctx, id.New(), want); err != nil || ok {
			t.Errorf("SetSettings on a missing site = %v, %v", ok, err)
		}
	})
}

// TestSitePreviewPassword covers the preview protection columns (#107):
// the password reference and hash, and their removal with the site's other
// secret references.
func TestSitePreviewPassword(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		sites := store.NewSites(db)
		s := newTestSite(createGroup(ctx, t, db), "acme", 20000)
		if err := sites.Create(ctx, s); err != nil {
			t.Fatal(err)
		}
		if s.PreviewProtection || s.PreviewUsername != "" || s.PreviewPasswordHash != "" || s.PreviewPasswordSecretRef != "" {
			t.Errorf("new site has preview protection: %+v", s)
		}
		if ok, err := sites.SetPreviewPassword(ctx, s.ID, "pw-ref", "$2a$04$hash"); err != nil || !ok {
			t.Fatalf("SetPreviewPassword = %v, %v", ok, err)
		}
		set := s.Settings()
		set.PreviewProtection, set.PreviewUsername = true, "preview"
		if _, err := sites.SetSettings(ctx, s.ID, set); err != nil {
			t.Fatal(err)
		}
		got, err := sites.Get(ctx, s.ID)
		if err != nil {
			t.Fatal(err)
		}
		if !got.PreviewProtection || got.PreviewUsername != "preview" || got.PreviewPasswordSecretRef != "pw-ref" ||
			got.PreviewPasswordHash != "$2a$04$hash" {
			t.Errorf("after SetPreviewPassword and SetSettings: %+v", got)
		}
		if _, err := sites.SetPreviewPassword(ctx, s.ID, "", "x"); err == nil {
			t.Error("SetPreviewPassword accepted an empty reference")
		}
		if ok, err := sites.SetPreviewPassword(ctx, id.New(), "r", "h"); err != nil || ok {
			t.Errorf("SetPreviewPassword on a missing site = %v, %v", ok, err)
		}

		if err := sites.ClearSecretRefs(ctx, s.ID); err != nil {
			t.Fatal(err)
		}
		got, _ = sites.Get(ctx, s.ID)
		if got.PreviewProtection || got.PreviewPasswordSecretRef != "" || got.PreviewPasswordHash != "" ||
			got.WPSaltsSecretRef != "" || got.AdminPasswordSecretRef != "" {
			t.Errorf("after ClearSecretRefs: %+v", got)
		}
	})
}
