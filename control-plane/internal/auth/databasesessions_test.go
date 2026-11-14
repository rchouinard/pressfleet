package auth_test

import (
	"context"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/auth"
	"github.com/rchouinard/pressfleet/control-plane/internal/id"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
)

// A role change or disabling ends the user's temporary database sessions,
// so the sweep drops their MariaDB users; other changes leave them.
func TestUpdateUserEndsDatabaseSessions(t *testing.T) {
	ctx := context.Background()
	operator, yes := store.RoleOperator, true
	email := "other@example.com"
	tests := []struct {
		name    string
		changes auth.UserChanges
		ended   bool
	}{
		{"demoted", auth.UserChanges{Role: &operator}, true},
		{"disabled", auth.UserChanges{Disabled: &yes}, true},
		{"unrelated change", auth.UserChanges{Email: &email}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			storetest.Run(t, func(t *testing.T, db *store.DB) {
				svc, _ := newService(t, db)
				mustCreate(t, svc, "root@example.com", store.RoleAdmin)
				admin := mustCreate(t, svc, "admin@example.com", store.RoleAdmin)
				sessions := store.NewDatabaseSessions(db)
				d := &store.DatabaseSession{SiteID: id.New(), UserID: admin.ID, Access: "read_write", ExpiresAt: time.Now().Add(time.Hour)}
				if err := sessions.Create(ctx, d); err != nil {
					t.Fatal(err)
				}
				if _, err := svc.UpdateUser(ctx, admin.ID, tt.changes); err != nil {
					t.Fatal(err)
				}
				got, err := sessions.Get(ctx, d.ID)
				if err != nil {
					t.Fatal(err)
				}
				if ended := got.Status == store.DatabaseSessionEnded; ended != tt.ended {
					t.Errorf("status = %q, ended = %v, want %v", got.Status, ended, tt.ended)
				}
			})
		})
	}
}
