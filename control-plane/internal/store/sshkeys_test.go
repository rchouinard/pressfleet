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

func TestSSHKeys(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		keys := store.NewSSHKeys(db)
		alice := createUser(t, db, "alice@example.com", store.RoleOperator)
		bob := createUser(t, db, "bob@example.com", store.RoleViewer)

		k1 := store.SSHKey{UserID: alice.ID, Name: "laptop", PublicKey: "ssh-ed25519 AAAA1", Fingerprint: "SHA256:one"}
		if err := keys.Create(ctx, &k1); err != nil {
			t.Fatal(err)
		}
		if !id.Valid(k1.ID) || k1.CreatedAt.IsZero() || k1.LastUsedAt != nil {
			t.Errorf("Create did not fill in the key: %+v", k1)
		}
		k2 := store.SSHKey{UserID: alice.ID, Name: "ci", PublicKey: "ssh-ed25519 AAAA2", Fingerprint: "SHA256:two"}
		if err := keys.Create(ctx, &k2); err != nil {
			t.Fatal(err)
		}

		if got, err := keys.Get(ctx, k1.ID); err != nil || !reflect.DeepEqual(got, k1) {
			t.Errorf("Get = %+v, %v; want %+v", got, err, k1)
		}
		if got, err := keys.GetByFingerprint(ctx, "SHA256:two"); err != nil || !reflect.DeepEqual(got, k2) {
			t.Errorf("GetByFingerprint = %+v, %v; want %+v", got, err, k2)
		}
		// IDs made in the same millisecond needn't sort by creation.
		first, second := k1.ID, k2.ID
		if second < first {
			first, second = second, first
		}
		list, err := keys.ListForUser(ctx, alice.ID, "", 0)
		if err != nil || len(list) != 2 || list[0].ID != first || list[1].ID != second {
			t.Errorf("ListForUser = %+v, %v", list, err)
		}
		if page, err := keys.ListForUser(ctx, alice.ID, first, 1); err != nil || len(page) != 1 || page[0].ID != second {
			t.Errorf("ListForUser page = %+v, %v", page, err)
		}
		if none, err := keys.ListForUser(ctx, bob.ID, "", 0); err != nil || none == nil || len(none) != 0 {
			t.Errorf("ListForUser of a user without keys = %#v, %v", none, err)
		}

		at := time.Now().Add(-time.Minute)
		if err := keys.TouchLastUsed(ctx, k1.ID, at); err != nil {
			t.Fatal(err)
		}
		if got, _ := keys.Get(ctx, k1.ID); got.LastUsedAt == nil || !got.LastUsedAt.Equal(at.UTC().Truncate(time.Microsecond)) {
			t.Errorf("after TouchLastUsed: %+v", got)
		}

		// A key belongs to one user only.
		dup := store.SSHKey{UserID: bob.ID, Name: "stolen", PublicKey: "ssh-ed25519 AAAA1", Fingerprint: "SHA256:one"}
		if err := keys.Create(ctx, &dup); !errors.Is(err, store.ErrConflict) {
			t.Errorf("duplicate fingerprint: %v, want ErrConflict", err)
		}
		orphan := store.SSHKey{UserID: id.New(), Name: "x", PublicKey: "ssh-ed25519 AAAA3", Fingerprint: "SHA256:three"}
		if err := keys.Create(ctx, &orphan); !errors.Is(err, store.ErrReference) {
			t.Errorf("key of a missing user: %v, want ErrReference", err)
		}
		if err := keys.Create(ctx, &store.SSHKey{UserID: alice.ID}); err == nil {
			t.Error("Create without a key succeeded")
		}

		// Only the owner's key is deleted.
		if err := keys.Delete(ctx, bob.ID, k1.ID); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("deleting another user's key: %v, want ErrNotFound", err)
		}
		if err := keys.Delete(ctx, alice.ID, k1.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := keys.GetByFingerprint(ctx, "SHA256:one"); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("deleted key still found: %v", err)
		}
		if err := keys.Delete(ctx, alice.ID, k1.ID); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("deleting twice: %v, want ErrNotFound", err)
		}
		if err := keys.TouchLastUsed(ctx, k1.ID, at); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("touching a deleted key: %v, want ErrNotFound", err)
		}

		// Deleting the user deletes their keys.
		if err := store.NewUsers(db).Delete(ctx, alice.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := keys.Get(ctx, k2.ID); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("key of a deleted user: %v, want ErrNotFound", err)
		}
	})
}
