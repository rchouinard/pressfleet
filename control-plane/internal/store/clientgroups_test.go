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

func TestClientGroups(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		groups := store.NewClientGroups(db)

		if list, err := groups.List(ctx, store.ClientGroupFilter{}); err != nil || len(list) != 0 {
			t.Fatalf("List on empty database = %v, %v", list, err)
		}

		before := time.Now().Add(-time.Second)
		acme := &store.ClientGroup{Name: "Acme", Notes: "Big client"}
		if err := groups.Create(ctx, acme); err != nil {
			t.Fatal(err)
		}
		if !id.Valid(acme.ID) || acme.CreatedAt.Before(before) || acme.CreatedAt.Location() != time.UTC || acme.UpdatedAt != acme.CreatedAt {
			t.Errorf("Create did not fill in ID and timestamps: %+v", acme)
		}
		beta := &store.ClientGroup{Name: "Beta"}
		if err := groups.Create(ctx, beta); err != nil {
			t.Fatal(err)
		}

		got, err := groups.Get(ctx, acme.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got != *acme {
			t.Errorf("Get = %+v, want %+v", got, *acme)
		}

		list, err := groups.List(ctx, store.ClientGroupFilter{})
		if err != nil {
			t.Fatal(err)
		}
		if len(list) != 2 || list[0].Name != "Acme" || list[1].Name != "Beta" {
			t.Errorf("List = %+v", list)
		}

		updated := got
		updated.Name, updated.Notes, updated.BackupRepository = "Acme Corp", "", "s3:backups/acme"
		if err := groups.Update(ctx, &updated); err != nil {
			t.Fatal(err)
		}
		if updated.CreatedAt != acme.CreatedAt || updated.UpdatedAt.Before(acme.UpdatedAt) {
			t.Errorf("Update timestamps: %+v", updated)
		}
		if got, err := groups.Get(ctx, acme.ID); err != nil || got != updated {
			t.Errorf("Get after Update = %+v, %v; want %+v", got, err, updated)
		}

		// Errors callers branch on.
		if err := groups.Create(ctx, &store.ClientGroup{Name: "Beta"}); !errors.Is(err, store.ErrConflict) {
			t.Errorf("Create duplicate name: err = %v, want ErrConflict", err)
		}
		dup := *beta
		dup.Name = "Acme Corp"
		if err := groups.Update(ctx, &dup); !errors.Is(err, store.ErrConflict) {
			t.Errorf("Update to duplicate name: err = %v, want ErrConflict", err)
		}
		missing := id.New()
		if _, err := groups.Get(ctx, missing); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("Get missing: err = %v, want ErrNotFound", err)
		}
		if err := groups.Update(ctx, &store.ClientGroup{ID: missing, Name: "x"}); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("Update missing: err = %v, want ErrNotFound", err)
		}
		if err := groups.Delete(ctx, missing); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("Delete missing: err = %v, want ErrNotFound", err)
		}
		if err := groups.Create(ctx, &store.ClientGroup{}); err == nil {
			t.Error("Create without a name succeeded")
		}

		// A group with sites can't be deleted.
		now := time.Now()
		if _, err := db.ExecContext(ctx, `INSERT INTO sites
			(id, client_group_id, name, slug, status, preview_domain, php_version, wp_core_version, uid, created_at, updated_at)
			VALUES (?, ?, 'Site', 'site', 'running', 'site.preview.example', '8.4', '6.9.1', 20000, ?, ?)`,
			id.New(), beta.ID, now, now); err != nil {
			t.Fatal(err)
		}
		if err := groups.Delete(ctx, beta.ID); !errors.Is(err, store.ErrReference) {
			t.Errorf("Delete group with sites: err = %v, want ErrReference", err)
		}

		if err := groups.Delete(ctx, acme.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := groups.Get(ctx, acme.ID); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("Get after Delete: err = %v, want ErrNotFound", err)
		}
	})
}

func TestClientGroupsPagingAndSiteCounts(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		groups := store.NewClientGroups(db)
		// Created out of order, with two names that differ only in case, so
		// the order is by name and then ID.
		for _, name := range []string{"delta", "Alpha", "charlie", "bravo", "alpha"} {
			if err := groups.Create(ctx, &store.ClientGroup{Name: name}); err != nil {
				t.Fatal(err)
			}
		}
		all, err := groups.List(ctx, store.ClientGroupFilter{})
		if err != nil {
			t.Fatal(err)
		}
		if len(all) != 5 {
			t.Fatalf("List = %+v", all)
		}

		var paged []string
		f := store.ClientGroupFilter{Limit: 2}
		for range 5 {
			page, err := groups.List(ctx, f)
			if err != nil {
				t.Fatal(err)
			}
			for _, g := range page {
				paged = append(paged, g.ID)
			}
			if len(page) < f.Limit {
				break
			}
			last := page[len(page)-1]
			f.AfterName, f.AfterID = last.Name, last.ID
		}
		if len(paged) != len(all) {
			t.Fatalf("paged %d groups, want %d", len(paged), len(all))
		}
		for i := range all {
			if paged[i] != all[i].ID {
				t.Errorf("page order %v differs from List order at %d", paged, i)
			}
		}

		// A cursor still works after its group is deleted.
		if err := groups.Delete(ctx, all[1].ID); err != nil {
			t.Fatal(err)
		}
		rest, err := groups.List(ctx, store.ClientGroupFilter{AfterName: all[1].Name, AfterID: all[1].ID})
		if err != nil || len(rest) != 3 || rest[0].ID != all[2].ID {
			t.Errorf("List after deleted cursor = %+v, %v", rest, err)
		}

		// Site counts.
		g := all[0]
		now := time.Now()
		for i, slug := range []string{"one", "two"} {
			if _, err := db.ExecContext(ctx, `INSERT INTO sites
				(id, client_group_id, name, slug, status, preview_domain, php_version, wp_core_version, uid, created_at, updated_at)
				VALUES (?, ?, 'Site', ?, 'running', ?, '8.4', '6.9.1', ?, ?, ?)`,
				id.New(), g.ID, slug, slug+".preview.example", 20000+i, now, now); err != nil {
				t.Fatal(err)
			}
		}
		if got, err := groups.Get(ctx, g.ID); err != nil || got.SiteCount != 2 {
			t.Errorf("Get site count = %d, %v; want 2", got.SiteCount, err)
		}
		list, err := groups.List(ctx, store.ClientGroupFilter{Limit: 2})
		if err != nil || len(list) != 2 || list[0].SiteCount != 2 || list[1].SiteCount != 0 {
			t.Errorf("List site counts = %+v, %v", list, err)
		}
	})
}
