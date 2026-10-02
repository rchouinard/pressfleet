package store_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/id"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
)

func createUser(t *testing.T, db *store.DB, email string, role store.Role) store.User {
	t.Helper()
	u := store.User{Email: email, PasswordHash: "$argon2id$hash", Role: role}
	if err := store.NewUsers(db).Create(context.Background(), &u); err != nil {
		t.Fatal(err)
	}
	return u
}

func TestUserDisplayName(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		users := store.NewUsers(db)
		u := createUser(t, db, "a@example.com", store.RoleViewer)
		if u.DisplayName != "" {
			t.Errorf("new user's display name = %q, want empty", u.DisplayName)
		}
		u.DisplayName = "Alex Rivera"
		if err := users.Update(ctx, &u, time.Now()); err != nil {
			t.Fatal(err)
		}
		if got, err := users.Get(ctx, u.ID); err != nil || got.DisplayName != "Alex Rivera" {
			t.Errorf("Get after Update = %q, %v", got.DisplayName, err)
		}
		listed, err := users.List(ctx, store.UserFilter{})
		if err != nil || len(listed) != 1 || listed[0].DisplayName != "Alex Rivera" {
			t.Errorf("List = %+v, %v", listed, err)
		}
	})
}

func TestUsers(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		users := store.NewUsers(db)

		admin := createUser(t, db, "admin@example.com", store.RoleAdmin)
		if !id.Valid(admin.ID) || admin.CreatedAt.IsZero() || admin.UpdatedAt != admin.CreatedAt || admin.Disabled() || admin.LastLoginAt != nil {
			t.Errorf("Create did not fill in the user: %+v", admin)
		}
		viewer := createUser(t, db, "viewer@example.com", store.RoleViewer)

		got, err := users.Get(ctx, admin.ID)
		if err != nil || fmt.Sprintf("%+v", got) != fmt.Sprintf("%+v", admin) {
			t.Errorf("Get = %+v, %v; want %+v", got, err, admin)
		}
		if got.PasswordHash.Reveal() != "$argon2id$hash" {
			t.Errorf("password hash = %q", got.PasswordHash.Reveal())
		}
		if got, err := users.GetByEmail(ctx, "viewer@example.com"); err != nil || got.ID != viewer.ID {
			t.Errorf("GetByEmail = %+v, %v", got, err)
		}

		// Update, including disabling and changing role.
		disabledAt := time.Now().Add(-time.Minute)
		upd := viewer
		upd.Email, upd.Role, upd.DisabledAt, upd.PasswordHash = "v@example.com", store.RoleOperator, &disabledAt, "$argon2id$new"
		at := time.Now()
		if err := users.Update(ctx, &upd, at); err != nil {
			t.Fatal(err)
		}
		reread, err := users.Get(ctx, viewer.ID)
		if err != nil {
			t.Fatal(err)
		}
		if reread.Email != "v@example.com" || reread.Role != store.RoleOperator || !reread.Disabled() ||
			!reread.DisabledAt.Equal(*upd.DisabledAt) || reread.PasswordHash != "$argon2id$new" ||
			!reread.UpdatedAt.Equal(upd.UpdatedAt) || !reread.CreatedAt.Equal(viewer.CreatedAt) {
			t.Errorf("after Update: %+v (in memory %+v)", reread, upd)
		}
		reread.DisabledAt = nil
		if err := users.Update(ctx, &reread, time.Now()); err != nil {
			t.Fatal(err)
		}
		if u, _ := users.Get(ctx, viewer.ID); u.Disabled() {
			t.Error("re-enabling did not clear disabled_at")
		}

		if err := users.SetLastLogin(ctx, admin.ID, at); err != nil {
			t.Fatal(err)
		}
		if u, _ := users.Get(ctx, admin.ID); u.LastLoginAt == nil || !u.LastLoginAt.Equal(at.UTC().Truncate(time.Microsecond)) || !u.UpdatedAt.Equal(admin.UpdatedAt) {
			t.Errorf("SetLastLogin: %+v", u)
		}

		// List with filter and pagination.
		third := createUser(t, db, "third@example.com", store.RoleAdmin)
		all, err := users.List(ctx, store.UserFilter{})
		if err != nil || len(all) != 3 || all[0].ID >= all[1].ID || all[1].ID >= all[2].ID {
			t.Errorf("List = %+v, %v", all, err)
		}
		page, err := users.List(ctx, store.UserFilter{After: all[0].ID, Limit: 1})
		if err != nil || len(page) != 1 || page[0].ID != all[1].ID {
			t.Errorf("List page = %+v, %v", page, err)
		}
		admins, err := users.List(ctx, store.UserFilter{Role: store.RoleAdmin})
		if err != nil || len(admins) != 2 {
			t.Errorf("List admins = %+v, %v", admins, err)
		}

		// Errors callers branch on.
		if err := users.Create(ctx, &store.User{Email: "admin@example.com", PasswordHash: "x", Role: store.RoleViewer}); !errors.Is(err, store.ErrConflict) {
			t.Errorf("duplicate email: %v", err)
		}
		if err := users.Create(ctx, &store.User{Email: "x@example.com", PasswordHash: "x", Role: "root"}); err == nil {
			t.Error("invalid role accepted")
		}
		missing := id.New()
		if _, err := users.Get(ctx, missing); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("Get missing: %v", err)
		}
		if _, err := users.GetByEmail(ctx, "nobody@example.com"); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("GetByEmail missing: %v", err)
		}
		if err := users.Update(ctx, &store.User{ID: missing, Email: "m@example.com", PasswordHash: "x", Role: store.RoleViewer}, at); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("Update missing: %v", err)
		}
		dup := third
		dup.Email = "admin@example.com"
		if err := users.Update(ctx, &dup, at); !errors.Is(err, store.ErrConflict) {
			t.Errorf("Update to duplicate email: %v", err)
		}

		// Deleting a user deletes their sessions and tokens.
		now := time.Now()
		sess := store.Session{UserID: third.ID, TokenHash: "h1", CreatedAt: now, ExpiresAt: now.Add(time.Hour)}
		if err := store.NewSessions(db).Create(ctx, &sess); err != nil {
			t.Fatal(err)
		}
		tok := store.APIToken{UserID: third.ID, Name: "ci", TokenHash: "t1", Scope: store.ScopeFull}
		if err := store.NewAPITokens(db).Create(ctx, &tok); err != nil {
			t.Fatal(err)
		}
		if err := users.Delete(ctx, third.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := store.NewSessions(db).GetByTokenHash(ctx, "h1"); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("session survived user deletion: %v", err)
		}
		if _, err := store.NewAPITokens(db).GetByTokenHash(ctx, "t1"); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("token survived user deletion: %v", err)
		}
		if err := users.Delete(ctx, third.ID); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("Delete missing: %v", err)
		}
	})
}

func TestSessions(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		sessions := store.NewSessions(db)
		u := createUser(t, db, "a@example.com", store.RoleAdmin)
		other := createUser(t, db, "b@example.com", store.RoleAdmin)

		now := time.Now().UTC().Truncate(time.Microsecond)
		s1 := store.Session{UserID: u.ID, TokenHash: "hash-1", SourceIP: "192.0.2.1", UserAgent: "curl", CreatedAt: now, ExpiresAt: now.Add(time.Hour)}
		if err := sessions.Create(ctx, &s1); err != nil {
			t.Fatal(err)
		}
		if !id.Valid(s1.ID) || !s1.LastSeenAt.Equal(now) {
			t.Errorf("Create: %+v", s1)
		}
		got, err := sessions.GetByTokenHash(ctx, "hash-1")
		if err != nil || got != s1 {
			t.Errorf("GetByTokenHash = %+v, %v; want %+v", got, err, s1)
		}
		later := now.Add(time.Minute)
		if err := sessions.Touch(ctx, s1.ID, later); err != nil {
			t.Fatal(err)
		}
		if got, _ := sessions.GetByTokenHash(ctx, "hash-1"); !got.LastSeenAt.Equal(later) {
			t.Errorf("Touch: last seen %v, want %v", got.LastSeenAt, later)
		}

		if err := sessions.Create(ctx, &store.Session{UserID: u.ID, TokenHash: "hash-1", CreatedAt: now, ExpiresAt: now}); !errors.Is(err, store.ErrConflict) {
			t.Errorf("duplicate token hash: %v", err)
		}
		if err := sessions.Create(ctx, &store.Session{UserID: id.New(), TokenHash: "hash-x", CreatedAt: now, ExpiresAt: now}); !errors.Is(err, store.ErrReference) {
			t.Errorf("unknown user: %v", err)
		}

		s2 := store.Session{UserID: u.ID, TokenHash: "hash-2", CreatedAt: now, ExpiresAt: now.Add(time.Hour)}
		s3 := store.Session{UserID: u.ID, TokenHash: "hash-3", CreatedAt: now, ExpiresAt: now.Add(time.Hour)}
		s4 := store.Session{UserID: other.ID, TokenHash: "hash-4", CreatedAt: now, ExpiresAt: now.Add(time.Hour)}
		for _, s := range []*store.Session{&s2, &s3, &s4} {
			if err := sessions.Create(ctx, s); err != nil {
				t.Fatal(err)
			}
		}
		// Delete every session of u except s1.
		if n, err := sessions.DeleteForUser(ctx, u.ID, s1.ID); err != nil || n != 2 {
			t.Errorf("DeleteForUser = %d, %v; want 2", n, err)
		}
		if _, err := sessions.GetByTokenHash(ctx, "hash-1"); err != nil {
			t.Errorf("kept session was deleted: %v", err)
		}
		if _, err := sessions.GetByTokenHash(ctx, "hash-4"); err != nil {
			t.Errorf("other user's session was deleted: %v", err)
		}

		// Expired (absolute) and idle sessions are cleaned up.
		expired := store.Session{UserID: other.ID, TokenHash: "hash-5", CreatedAt: now.Add(-2 * time.Hour), ExpiresAt: now.Add(-time.Hour)}
		idle := store.Session{UserID: other.ID, TokenHash: "hash-6", CreatedAt: now.Add(-3 * time.Hour), ExpiresAt: now.Add(time.Hour)}
		for _, s := range []*store.Session{&expired, &idle} {
			if err := sessions.Create(ctx, s); err != nil {
				t.Fatal(err)
			}
		}
		if n, err := sessions.DeleteExpired(ctx, now, now.Add(-2*time.Hour)); err != nil || n != 2 {
			t.Errorf("DeleteExpired = %d, %v; want 2", n, err)
		}

		if err := sessions.Delete(ctx, s1.ID); err != nil {
			t.Fatal(err)
		}
		if err := sessions.Delete(ctx, s1.ID); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("Delete missing: %v", err)
		}
	})
}

func TestAPITokens(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		tokens := store.NewAPITokens(db)
		u := createUser(t, db, "a@example.com", store.RoleOperator)
		other := createUser(t, db, "b@example.com", store.RoleOperator)

		expires := time.Now().Add(24 * time.Hour)
		full := store.APIToken{UserID: u.ID, Name: "deploys", TokenHash: "th-1", Prefix: "pft_abcd", Scope: store.ScopeFull, ExpiresAt: &expires}
		if err := tokens.Create(ctx, &full); err != nil {
			t.Fatal(err)
		}
		ro := store.APIToken{UserID: u.ID, Name: "metrics", TokenHash: "th-2", Prefix: "pft_efgh", Scope: store.ScopeReadOnly}
		if err := tokens.Create(ctx, &ro); err != nil {
			t.Fatal(err)
		}
		got, err := tokens.GetByTokenHash(ctx, "th-2")
		if err != nil || got.Scope != store.ScopeReadOnly || got.Prefix != "pft_efgh" || got.ExpiresAt != nil || got.LastUsedAt != nil || got.RevokedAt != nil || !got.CreatedAt.Equal(ro.CreatedAt) {
			t.Errorf("GetByTokenHash = %+v, %v", got, err)
		}
		if got, _ := tokens.GetByTokenHash(ctx, "th-1"); got.ExpiresAt == nil || !got.ExpiresAt.Equal(*full.ExpiresAt) || got.Scope != store.ScopeFull {
			t.Errorf("expiry/scope not stored: %+v", got)
		}

		used := time.Now()
		if err := tokens.TouchLastUsed(ctx, ro.ID, used); err != nil {
			t.Fatal(err)
		}
		if got, _ := tokens.GetByTokenHash(ctx, "th-2"); got.LastUsedAt == nil || !got.LastUsedAt.Equal(used.UTC().Truncate(time.Microsecond)) {
			t.Errorf("TouchLastUsed: %+v", got)
		}

		list, err := tokens.ListActiveForUser(ctx, u.ID, "", 0)
		if err != nil || len(list) != 2 || list[0].ID >= list[1].ID {
			t.Errorf("ListActiveForUser = %+v, %v", list, err)
		}
		if page, _ := tokens.ListActiveForUser(ctx, u.ID, list[0].ID, 1); len(page) != 1 || page[0].ID != list[1].ID {
			t.Errorf("ListActiveForUser page = %+v", page)
		}

		// Only the owner can revoke, and only once.
		if err := tokens.Revoke(ctx, other.ID, full.ID, time.Now()); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("Revoke by another user: %v", err)
		}
		if err := tokens.Revoke(ctx, u.ID, full.ID, time.Now()); err != nil {
			t.Fatal(err)
		}
		if err := tokens.Revoke(ctx, u.ID, full.ID, time.Now()); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("Revoke twice: %v", err)
		}
		if got, _ := tokens.GetByTokenHash(ctx, "th-1"); got.RevokedAt == nil {
			t.Error("revoked token has no revoked_at")
		}
		if list, _ := tokens.ListActiveForUser(ctx, u.ID, "", 0); len(list) != 1 || list[0].ID != ro.ID {
			t.Errorf("revoked token still listed: %+v", list)
		}

		otherTok := store.APIToken{UserID: other.ID, Name: "x", TokenHash: "th-3", Scope: store.ScopeFull}
		if err := tokens.Create(ctx, &otherTok); err != nil {
			t.Fatal(err)
		}
		if n, err := tokens.RevokeAllForUser(ctx, u.ID, time.Now()); err != nil || n != 1 {
			t.Errorf("RevokeAllForUser = %d, %v; want 1", n, err)
		}
		if got, _ := tokens.GetByTokenHash(ctx, "th-3"); got.RevokedAt != nil {
			t.Error("RevokeAllForUser revoked another user's token")
		}

		if err := tokens.Create(ctx, &store.APIToken{UserID: u.ID, Name: "bad", TokenHash: "th-4", Scope: "admin"}); err == nil {
			t.Error("invalid scope accepted")
		}
		if err := tokens.Create(ctx, &store.APIToken{UserID: u.ID, Name: "dup", TokenHash: "th-2", Scope: store.ScopeFull}); !errors.Is(err, store.ErrConflict) {
			t.Errorf("duplicate hash: %v", err)
		}
	})
}
