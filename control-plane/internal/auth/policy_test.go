package auth

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
)

func TestTwoFactorPolicyCovers(t *testing.T) {
	tests := []struct {
		required TwoFactorRequirement
		admin    bool
		operator bool
		viewer   bool
	}{
		{"", false, false, false},
		{RequireNone, false, false, false},
		{RequireAdmins, true, false, false},
		{RequireAll, true, true, true},
	}
	for _, tt := range tests {
		p := TwoFactorPolicy{Required: tt.required}
		for role, want := range map[store.Role]bool{store.RoleAdmin: tt.admin, store.RoleOperator: tt.operator, store.RoleViewer: tt.viewer} {
			if got := p.Covers(role); got != want {
				t.Errorf("required %q covers %s = %v, want %v", tt.required, role, got, want)
			}
		}
	}
}

func TestEnsureSecondFactorKept(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		ctx := context.Background()
		users := store.NewUsers(db)
		admin := store.User{Email: "admin@example.com", PasswordHash: "x", Role: store.RoleAdmin}
		if err := users.Create(ctx, &admin); err != nil {
			t.Fatal(err)
		}
		operator := store.User{Email: "operator@example.com", PasswordHash: "x", Role: store.RoleOperator}
		if err := users.Create(ctx, &operator); err != nil {
			t.Fatal(err)
		}
		s := NewService(db, Options{Logger: storetest.Logger(), TwoFactor: TwoFactorPolicy{Required: RequireAdmins}})

		// Neither has a factor: only the covered Admin is refused.
		if err := s.ensureSecondFactorKept(ctx, db, admin); !errors.Is(err, ErrLastSecondFactor) {
			t.Errorf("admin without a factor: %v, want ErrLastSecondFactor", err)
		}
		if err := s.ensureSecondFactorKept(ctx, db, operator); err != nil {
			t.Errorf("operator without a factor: %v, want nil", err)
		}

		// A WebAuthn credential for another relying party ID (a previous
		// dashboard domain) isn't a factor.
		stale := store.WebAuthnCredential{
			UserID: admin.ID, CredentialID: []byte("stale"), PublicKey: []byte("key"), Algorithm: -7,
			AAGUID: make([]byte, 16), RPID: "old.example.com", Name: "old key", CreatedAt: time.Now(),
		}
		if err := store.NewWebAuthnCredentials(db).Create(ctx, &stale); err != nil {
			t.Fatal(err)
		}
		if err := s.ensureSecondFactorKept(ctx, db, admin); !errors.Is(err, ErrLastSecondFactor) {
			t.Errorf("admin with only a stale credential: %v, want ErrLastSecondFactor", err)
		}

		// One for the current relying party ID (localhost here) is.
		cred := store.WebAuthnCredential{
			UserID: admin.ID, CredentialID: []byte("cred"), PublicKey: []byte("key"), Algorithm: -7,
			AAGUID: make([]byte, 16), RPID: "localhost", Name: "key", CreatedAt: time.Now(),
		}
		if err := store.NewWebAuthnCredentials(db).Create(ctx, &cred); err != nil {
			t.Fatal(err)
		}
		if err := s.ensureSecondFactorKept(ctx, db, admin); err != nil {
			t.Errorf("admin with a credential: %v, want nil", err)
		}

		// With nothing required, nobody is refused.
		if err := NewService(db, Options{Logger: storetest.Logger()}).ensureSecondFactorKept(ctx, db, operator); err != nil {
			t.Errorf("no policy: %v, want nil", err)
		}
	})
}
