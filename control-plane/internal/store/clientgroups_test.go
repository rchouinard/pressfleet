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

		if list, err := groups.List(ctx); err != nil || len(list) != 0 {
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

		list, err := groups.List(ctx)
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
			(id, client_group_id, name, slug, status, preview_domain, php_version, wp_core_version, created_at, updated_at)
			VALUES (?, ?, 'Site', 'site', 'running', 'site.preview.example', '8.4', '6.9.1', ?, ?)`,
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
