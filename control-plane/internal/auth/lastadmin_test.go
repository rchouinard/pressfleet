package auth_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/rchouinard/pressfleet/control-plane/internal/auth"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
)

func TestLastAdminProtection(t *testing.T) {
	ctx := context.Background()
	viewer, yes, no := store.RoleViewer, true, false
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		svc, _ := newService(t, db)
		admin := mustCreate(t, svc, "admin@example.com", store.RoleAdmin)
		op := mustCreate(t, svc, "op@example.com", store.RoleOperator)

		for name, c := range map[string]auth.UserChanges{
			"demote":  {Role: &viewer},
			"disable": {Disabled: &yes},
		} {
			if _, err := svc.UpdateUser(ctx, admin.ID, c); !errors.Is(err, auth.ErrLastAdmin) {
				t.Errorf("%s the last admin: err = %v, want ErrLastAdmin", name, err)
			}
		}
		if err := svc.DeleteUser(ctx, admin.ID); !errors.Is(err, auth.ErrLastAdmin) {
			t.Errorf("delete the last admin: err = %v, want ErrLastAdmin", err)
		}
		// Changes that keep the admin an admin are fine.
		email := "boss@example.com"
		if _, err := svc.UpdateUser(ctx, admin.ID, auth.UserChanges{Email: &email, Disabled: &no}); err != nil {
			t.Errorf("harmless change to the last admin: %v", err)
		}
		// Other users aren't affected.
		if _, err := svc.UpdateUser(ctx, op.ID, auth.UserChanges{Role: &viewer, Disabled: &yes}); err != nil {
			t.Errorf("demote and disable an operator: %v", err)
		}

		// A disabled admin doesn't count as an admin.
		second := mustCreate(t, svc, "second@example.com", store.RoleAdmin)
		if _, err := svc.UpdateUser(ctx, second.ID, auth.UserChanges{Disabled: &yes}); err != nil {
			t.Fatal(err)
		}
		if err := svc.DeleteUser(ctx, admin.ID); !errors.Is(err, auth.ErrLastAdmin) {
			t.Errorf("delete the last enabled admin: err = %v, want ErrLastAdmin", err)
		}
		// With another enabled admin, the first can go.
		if _, err := svc.UpdateUser(ctx, second.ID, auth.UserChanges{Disabled: &no}); err != nil {
			t.Fatal(err)
		}
		if _, err := svc.UpdateUser(ctx, admin.ID, auth.UserChanges{Role: &viewer}); err != nil {
			t.Errorf("demote an admin while another exists: %v", err)
		}
		if err := svc.DeleteUser(ctx, second.ID); !errors.Is(err, auth.ErrLastAdmin) {
			t.Errorf("delete the new last admin: err = %v, want ErrLastAdmin", err)
		}
		if err := svc.DeleteUser(ctx, admin.ID); err != nil {
			t.Errorf("delete a former admin: %v", err)
		}
		if err := svc.DeleteUser(ctx, admin.ID); !errors.Is(err, auth.ErrUserNotFound) {
			t.Errorf("delete twice: err = %v, want ErrUserNotFound", err)
		}
	})
}

// TestLastAdminConcurrent removes admins concurrently, each request alone
// being allowed. Without locking, two requests could each see the other
// admin and together leave none.
func TestLastAdminConcurrent(t *testing.T) {
	ctx := context.Background()
	viewer, yes := store.RoleViewer, true
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		svc, _ := newService(t, db)
		for round := range 5 {
			const n = 4
			admins := make([]store.User, n)
			for i := range admins {
				admins[i] = mustCreate(t, svc, "admin"+string(rune('a'+round))+string(rune('0'+i))+"@example.com", store.RoleAdmin)
			}
			var (
				wg        sync.WaitGroup
				mu        sync.Mutex
				succeeded int
			)
			for i, u := range admins {
				wg.Go(func() {
					var err error
					switch i % 3 {
					case 0:
						_, err = svc.UpdateUser(ctx, u.ID, auth.UserChanges{Role: &viewer})
					case 1:
						_, err = svc.UpdateUser(ctx, u.ID, auth.UserChanges{Disabled: &yes})
					default:
						err = svc.DeleteUser(ctx, u.ID)
					}
					switch {
					case err == nil:
						mu.Lock()
						succeeded++
						mu.Unlock()
					case !errors.Is(err, auth.ErrLastAdmin):
						t.Errorf("round %d: removing admin %d: %v", round, i, err)
					}
				})
			}
			wg.Wait()
			left, err := store.NewUsers(db).LockActiveAdmins(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if len(left) != 1 || succeeded != n-1 {
				t.Fatalf("round %d: %d removals succeeded, %d admins left (%v); want %d and 1", round, succeeded, len(left), left, n-1)
			}
			// Clean up so the next round starts with no admins.
			if _, err := db.ExecContext(ctx, "DELETE FROM users"); err != nil {
				t.Fatal(err)
			}
		}
	})
}
