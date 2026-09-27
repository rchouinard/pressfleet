package store_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/rchouinard/pressfleet/control-plane/internal/id"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
)

// TestSiteDomains covers a site's custom domains (#45, #46): setting and
// replacing them, uniqueness across sites, finding a domain's site, and
// the conditional clearing of the URL rewrite list.
func TestSiteDomains(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		sites := store.NewSites(db)
		group := createGroup(ctx, t, db)
		a := newTestSite(group, "alpha", 20000)
		b := newTestSite(group, "beta", 20001)
		for _, s := range []*store.Site{a, b} {
			if err := sites.Create(ctx, s); err != nil {
				t.Fatal(err)
			}
		}
		if d, err := sites.Domains(ctx, a.ID); err != nil || d.Primary != "" || len(d.Aliases) != 0 || len(d.URLRewriteFrom) != 0 {
			t.Fatalf("a new site's domains = %+v, %v", d, err)
		}

		want := store.SiteDomains{
			Primary:        "www.alpha.example",
			Aliases:        []string{"alpha.example", "shop.alpha.example"},
			URLRewriteFrom: []string{"alpha.preview.example", "old.alpha.example"},
		}
		if ok, err := sites.SetDomains(ctx, a.ID, want); err != nil || !ok {
			t.Fatalf("SetDomains = %v, %v", ok, err)
		}
		got, err := sites.Domains(ctx, a.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Primary != want.Primary || !slices.Equal(got.Aliases, want.Aliases) || !slices.Equal(got.URLRewriteFrom, want.URLRewriteFrom) {
			t.Errorf("Domains = %+v, want %+v", got, want)
		}
		if site, _ := sites.Get(ctx, a.ID); site.PrimaryDomain != want.Primary || !slices.Equal(site.URLRewriteFrom, want.URLRewriteFrom) {
			t.Errorf("Get = %+v", site)
		}

		// Replacing drops aliases that are no longer listed.
		want.Aliases = []string{"alpha.example"}
		if _, err := sites.SetDomains(ctx, a.ID, want); err != nil {
			t.Fatal(err)
		}
		if got, _ := sites.Aliases(ctx, a.ID); !slices.Equal(got, want.Aliases) {
			t.Errorf("aliases after replacing = %v", got)
		}

		// Another site can't take the same primary domain or alias.
		for _, d := range []store.SiteDomains{
			{Primary: "www.alpha.example"},
			{Primary: "www.beta.example", Aliases: []string{"alpha.example"}},
		} {
			err := db.InTx(ctx, func(q store.Querier) error {
				_, err := store.NewSites(q).SetDomains(ctx, b.ID, d)
				return err
			})
			if !errors.Is(err, store.ErrConflict) {
				t.Errorf("SetDomains(%+v) on another site: %v, want ErrConflict", d, err)
			}
		}
		if got, _ := sites.Domains(ctx, b.ID); got.Primary != "" || len(got.Aliases) != 0 {
			t.Errorf("a refused change left %+v", got)
		}

		for domain, owner := range map[string]string{
			"www.alpha.example":    a.ID,
			"alpha.example":        a.ID,
			"beta.preview.example": b.ID,
			"shop.alpha.example":   "",
			"nothing.example":      "",
		} {
			if got, err := sites.DomainOwner(ctx, domain); err != nil || got != owner {
				t.Errorf("DomainOwner(%s) = %q, %v; want %q", domain, got, err, owner)
			}
		}

		// The rewrite list is cleared only if it is still what the job
		// rewrote.
		if ok, err := sites.ClearURLRewrite(ctx, a.ID, []string{"alpha.preview.example"}); err != nil || ok {
			t.Errorf("clearing another list = %v, %v", ok, err)
		}
		if ok, err := sites.ClearURLRewrite(ctx, a.ID, want.URLRewriteFrom); err != nil || !ok {
			t.Errorf("ClearURLRewrite = %v, %v", ok, err)
		}
		if site, _ := sites.Get(ctx, a.ID); len(site.URLRewriteFrom) != 0 {
			t.Errorf("rewrite list after clearing = %q", site.URLRewriteFrom)
		}

		// Going back to the preview domain alone (a rolled back go-live).
		if _, err := sites.SetDomains(ctx, a.ID, store.SiteDomains{}); err != nil {
			t.Fatal(err)
		}
		if got, _ := sites.Domains(ctx, a.ID); got.Primary != "" || len(got.Aliases) != 0 {
			t.Errorf("after clearing: %+v", got)
		}
		if ok, err := sites.SetDomains(ctx, id.New(), want); err != nil || ok {
			t.Errorf("SetDomains on a missing site = %v, %v", ok, err)
		}

		// Deleting a site frees its domains.
		if _, err := sites.SetDomains(ctx, a.ID, want); err != nil {
			t.Fatal(err)
		}
		if _, err := sites.Delete(ctx, a.ID); err != nil {
			t.Fatal(err)
		}
		if owner, _ := sites.DomainOwner(ctx, "alpha.example"); owner != "" {
			t.Errorf("a deleted site still owns its alias")
		}
	})
}
