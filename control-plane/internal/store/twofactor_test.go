package store_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/id"
	"github.com/rchouinard/pressfleet/control-plane/internal/redact"
	"github.com/rchouinard/pressfleet/control-plane/internal/secrets"
	"github.com/rchouinard/pressfleet/control-plane/internal/secrets/secretstest"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
)

const testTOTPSecret = "JBSWY3DPEHPK3PXPJBSWY3DPEHPK3PXP"

// putTOTPSecret seals a TOTP secret the way the auth code will.
func putTOTPSecret(t *testing.T, db *store.DB) string {
	t.Helper()
	ref, err := secrets.NewStore(db, secretstest.Keyring(t, 1)).Put(context.Background(), secrets.KindTOTPSecret, redact.Secret(testTOTPSecret))
	if err != nil {
		t.Fatal(err)
	}
	return string(ref)
}

func countRows(t *testing.T, db *store.DB, query string, args ...any) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestUserTOTPs(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		totps := store.NewUserTOTPs(db)
		u := createUser(t, db, "a@example.com", store.RoleAdmin)

		secretID := putTOTPSecret(t, db)
		totp := store.TOTP{UserID: u.ID, SecretID: secretID}
		if err := totps.Create(ctx, &totp); err != nil {
			t.Fatal(err)
		}
		if totp.CreatedAt.IsZero() || totp.Confirmed() || totp.LastUsedStep != 0 {
			t.Errorf("Create: %+v", totp)
		}
		got, err := totps.Get(ctx, u.ID)
		if err != nil || fmt.Sprintf("%+v", got) != fmt.Sprintf("%+v", totp) {
			t.Errorf("Get = %+v, %v; want %+v", got, err, totp)
		}

		// The row holds only the secret's ID; the secret itself is
		// encrypted.
		var ciphertext []byte
		var kind string
		if err := db.QueryRowContext(ctx, "SELECT kind, ciphertext FROM secrets WHERE id = ?", got.SecretID).Scan(&kind, &ciphertext); err != nil {
			t.Fatal(err)
		}
		if kind != string(secrets.KindTOTPSecret) || bytes.Contains(ciphertext, []byte(testTOTPSecret)) {
			t.Errorf("TOTP secret stored as kind %q, plaintext visible: %v", kind, bytes.Contains(ciphertext, []byte(testTOTPSecret)))
		}

		// One per user; the user and the secret must exist.
		if err := totps.Create(ctx, &store.TOTP{UserID: u.ID, SecretID: putTOTPSecret(t, db)}); !errors.Is(err, store.ErrConflict) {
			t.Errorf("second TOTP: %v, want ErrConflict", err)
		}
		if err := totps.Create(ctx, &store.TOTP{UserID: id.New(), SecretID: putTOTPSecret(t, db)}); !errors.Is(err, store.ErrReference) {
			t.Errorf("unknown user: %v, want ErrReference", err)
		}
		other := createUser(t, db, "b@example.com", store.RoleViewer)
		if err := totps.Create(ctx, &store.TOTP{UserID: other.ID, SecretID: id.New()}); !errors.Is(err, store.ErrReference) {
			t.Errorf("unknown secret: %v, want ErrReference", err)
		}
		if err := totps.Create(ctx, &store.TOTP{UserID: other.ID}); err == nil {
			t.Error("Create without a secret succeeded")
		}
		if _, err := totps.Get(ctx, other.ID); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("Get without a TOTP: %v, want ErrNotFound", err)
		}
		// The secret can't be deleted from under the TOTP.
		if _, err := db.ExecContext(ctx, "DELETE FROM secrets WHERE id = ?", secretID); !errors.Is(err, store.ErrReference) {
			t.Errorf("deleting the secret of a TOTP: %v, want ErrReference", err)
		}

		// Steps can't be used before confirmation.
		if ok, err := totps.UseStep(ctx, u.ID, 100); err != nil || ok {
			t.Errorf("UseStep before Confirm = %v, %v; want false", ok, err)
		}
		at := time.Now().UTC().Truncate(time.Microsecond)
		if err := totps.Confirm(ctx, u.ID, 100, at); err != nil {
			t.Fatal(err)
		}
		if err := totps.Confirm(ctx, u.ID, 101, at); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("second Confirm: %v, want ErrNotFound", err)
		}
		if err := totps.Confirm(ctx, other.ID, 100, at); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("Confirm without a TOTP: %v, want ErrNotFound", err)
		}
		got, err = totps.Get(ctx, u.ID)
		if err != nil || !got.Confirmed() || !got.ConfirmedAt.Equal(at) || got.LastUsedStep != 100 {
			t.Errorf("after Confirm: %+v, %v", got, err)
		}

		// The confirming code's step and earlier ones are refused.
		for _, tt := range []struct {
			step int64
			want bool
		}{{99, false}, {100, false}, {101, true}, {101, false}, {103, true}, {102, false}} {
			if ok, err := totps.UseStep(ctx, u.ID, tt.step); err != nil || ok != tt.want {
				t.Errorf("UseStep(%d) = %v, %v; want %v", tt.step, ok, err, tt.want)
			}
		}
		if got, _ := totps.Get(ctx, u.ID); got.LastUsedStep != 103 {
			t.Errorf("last used step = %d, want 103", got.LastUsedStep)
		}

		// Delete removes the TOTP and its secret.
		if err := db.InTx(ctx, func(q store.Querier) error { return store.NewUserTOTPs(q).Delete(ctx, u.ID) }); err != nil {
			t.Fatal(err)
		}
		if _, err := totps.Get(ctx, u.ID); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("Get after Delete: %v", err)
		}
		if n := countRows(t, db, "SELECT COUNT(*) FROM secrets WHERE id = ?", secretID); n != 0 {
			t.Error("Delete kept the TOTP secret")
		}
		if err := totps.Delete(ctx, u.ID); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("second Delete: %v, want ErrNotFound", err)
		}
	})
}

func TestDeleteUnconfirmedTOTPs(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		totps := store.NewUserTOTPs(db)
		now := time.Now()
		create := func(email string, age time.Duration, confirmed bool) store.TOTP {
			t.Helper()
			totp := store.TOTP{UserID: createUser(t, db, email, store.RoleViewer).ID, SecretID: putTOTPSecret(t, db), CreatedAt: now.Add(-age)}
			if confirmed {
				totp.ConfirmedAt = &totp.CreatedAt
			}
			if err := totps.Create(ctx, &totp); err != nil {
				t.Fatal(err)
			}
			return totp
		}
		stale := create("stale@example.com", time.Hour, false)
		fresh := create("fresh@example.com", time.Minute, false)
		confirmed := create("confirmed@example.com", time.Hour, true)

		if n, err := totps.DeleteUnconfirmedBefore(ctx, now.Add(-15*time.Minute)); err != nil || n != 1 {
			t.Errorf("DeleteUnconfirmedBefore = %d, %v; want 1", n, err)
		}
		if _, err := totps.Get(ctx, stale.UserID); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("stale TOTP kept: %v", err)
		}
		if n := countRows(t, db, "SELECT COUNT(*) FROM secrets WHERE id = ?", stale.SecretID); n != 0 {
			t.Error("stale TOTP's secret kept")
		}
		for _, kept := range []store.TOTP{fresh, confirmed} {
			if _, err := totps.Get(ctx, kept.UserID); err != nil {
				t.Errorf("TOTP of %s deleted: %v", kept.UserID, err)
			}
		}
		if n, err := totps.DeleteUnconfirmedBefore(ctx, now.Add(-15*time.Minute)); err != nil || n != 0 {
			t.Errorf("second DeleteUnconfirmedBefore = %d, %v; want 0", n, err)
		}
	})
}

func newCredential(userID string, credentialID string) store.WebAuthnCredential {
	return store.WebAuthnCredential{
		UserID:         userID,
		CredentialID:   []byte(credentialID),
		PublicKey:      []byte{0xa5, 0x01, 0x02, 0x03, 0x26},
		Algorithm:      -7,
		SignCount:      4_000_000_000, // more than an int32 holds
		AAGUID:         bytes.Repeat([]byte{0xad}, 16),
		Transports:     []string{"usb", "nfc"},
		UserVerified:   true,
		BackupEligible: true,
		RPID:           "dashboard.example.com",
		Name:           "YubiKey",
	}
}

func TestWebAuthnCredentials(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		creds := store.NewWebAuthnCredentials(db)
		u := createUser(t, db, "a@example.com", store.RoleAdmin)
		other := createUser(t, db, "b@example.com", store.RoleViewer)

		c1 := newCredential(u.ID, "cred-1")
		if err := creds.Create(ctx, &c1); err != nil {
			t.Fatal(err)
		}
		if !id.Valid(c1.ID) || c1.CreatedAt.IsZero() || c1.LastUsedAt != nil {
			t.Errorf("Create: %+v", c1)
		}
		got, err := creds.Get(ctx, u.ID, c1.ID)
		if err != nil || fmt.Sprintf("%+v", got) != fmt.Sprintf("%+v", c1) {
			t.Errorf("Get = %+v, %v; want %+v", got, err, c1)
		}
		if _, err := creds.Get(ctx, other.ID, c1.ID); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("Get as another user: %v, want ErrNotFound", err)
		}
		if got, err := creds.GetByCredentialID(ctx, []byte("cred-1")); err != nil || got.ID != c1.ID {
			t.Errorf("GetByCredentialID = %+v, %v", got, err)
		}
		if _, err := creds.GetByCredentialID(ctx, []byte("cred-x")); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("GetByCredentialID unknown: %v, want ErrNotFound", err)
		}

		// A minimal credential: no transports or AAGUID.
		c2 := store.WebAuthnCredential{UserID: u.ID, CredentialID: []byte("cred-2"), PublicKey: []byte{1}, Algorithm: -8, RPID: "localhost", Name: "Passkey"}
		if err := creds.Create(ctx, &c2); err != nil {
			t.Fatal(err)
		}
		if got, err := creds.Get(ctx, u.ID, c2.ID); err != nil || len(got.Transports) != 0 || got.Transports == nil || len(got.AAGUID) != 0 {
			t.Errorf("minimal credential = %+v, %v", got, err)
		}

		dup := newCredential(other.ID, "cred-1")
		if err := creds.Create(ctx, &dup); !errors.Is(err, store.ErrConflict) {
			t.Errorf("duplicate credential ID: %v, want ErrConflict", err)
		}
		orphan := newCredential(id.New(), "cred-3")
		if err := creds.Create(ctx, &orphan); !errors.Is(err, store.ErrReference) {
			t.Errorf("unknown user: %v, want ErrReference", err)
		}
		if err := creds.Create(ctx, &store.WebAuthnCredential{UserID: u.ID, CredentialID: []byte("x")}); err == nil {
			t.Error("Create without a public key succeeded")
		}
		c3 := newCredential(other.ID, "cred-3")
		if err := creds.Create(ctx, &c3); err != nil {
			t.Fatal(err)
		}

		list, err := creds.ListForUser(ctx, u.ID)
		want := []string{c1.ID, c2.ID}
		slices.Sort(want)
		if err != nil || len(list) != 2 || list[0].ID != want[0] || list[1].ID != want[1] {
			t.Errorf("ListForUser = %+v, %v; want IDs %v", list, err, want)
		}
		if list, err := creds.ListForUser(ctx, id.New()); err != nil || list == nil || len(list) != 0 {
			t.Errorf("ListForUser without credentials = %v, %v; want empty", list, err)
		}

		if err := creds.Rename(ctx, u.ID, c1.ID, "Desk key"); err != nil {
			t.Fatal(err)
		}
		if err := creds.Rename(ctx, other.ID, c1.ID, "Mine"); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("Rename as another user: %v, want ErrNotFound", err)
		}
		if err := creds.Rename(ctx, u.ID, c1.ID, ""); err == nil {
			t.Error("Rename to empty succeeded")
		}
		at := time.Now().UTC().Truncate(time.Microsecond)
		if err := creds.RecordUse(ctx, c1.ID, 0, true, at); err != nil {
			t.Fatal(err)
		}
		if err := creds.RecordUse(ctx, id.New(), 0, true, at); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("RecordUse unknown: %v, want ErrNotFound", err)
		}
		got, err = creds.Get(ctx, u.ID, c1.ID)
		if err != nil || got.Name != "Desk key" || got.SignCount != 0 || !got.BackupState ||
			got.LastUsedAt == nil || !got.LastUsedAt.Equal(at) || !slices.Equal(got.Transports, []string{"usb", "nfc"}) {
			t.Errorf("after Rename and RecordUse: %+v, %v", got, err)
		}

		if err := creds.Delete(ctx, other.ID, c1.ID); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("Delete as another user: %v, want ErrNotFound", err)
		}
		if err := creds.Delete(ctx, u.ID, c1.ID); err != nil {
			t.Fatal(err)
		}
		if err := creds.Delete(ctx, u.ID, c1.ID); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("second Delete: %v, want ErrNotFound", err)
		}
		if n, err := creds.DeleteAllForUser(ctx, u.ID); err != nil || n != 1 {
			t.Errorf("DeleteAllForUser = %d, %v; want 1", n, err)
		}
		if _, err := creds.Get(ctx, other.ID, c3.ID); err != nil {
			t.Errorf("other user's credential deleted: %v", err)
		}
	})
}

func TestRecoveryCodes(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		codes := store.NewRecoveryCodes(db)
		u := createUser(t, db, "a@example.com", store.RoleAdmin)
		other := createUser(t, db, "b@example.com", store.RoleViewer)

		c := store.RecoveryCode{UserID: other.ID, CodeHash: "hash-a"}
		if err := codes.Create(ctx, &c); err != nil {
			t.Fatal(err)
		}
		if !id.Valid(c.ID) || c.CreatedAt.IsZero() || c.UsedAt != nil {
			t.Errorf("Create: %+v", c)
		}
		if err := codes.Create(ctx, &store.RecoveryCode{UserID: other.ID, CodeHash: "hash-a"}); !errors.Is(err, store.ErrConflict) {
			t.Errorf("duplicate hash: %v, want ErrConflict", err)
		}
		if err := codes.Create(ctx, &store.RecoveryCode{UserID: id.New(), CodeHash: "hash-b"}); !errors.Is(err, store.ErrReference) {
			t.Errorf("unknown user: %v, want ErrReference", err)
		}
		if err := codes.Create(ctx, &store.RecoveryCode{UserID: u.ID}); err == nil {
			t.Error("Create without a hash succeeded")
		}

		at := time.Now().UTC().Truncate(time.Microsecond)
		var first []store.RecoveryCode
		err := db.InTx(ctx, func(q store.Querier) error {
			var err error
			first, err = store.NewRecoveryCodes(q).Replace(ctx, u.ID, []string{"hash-1", "hash-2", "hash-a"}, at)
			return err
		})
		if err != nil || len(first) != 3 {
			t.Fatalf("Replace = %+v, %v", first, err)
		}
		list, err := codes.ListForUser(ctx, u.ID)
		slices.SortFunc(first, func(a, b store.RecoveryCode) int { return strings.Compare(a.ID, b.ID) })
		if err != nil || fmt.Sprintf("%+v", list) != fmt.Sprintf("%+v", first) {
			t.Errorf("ListForUser = %+v, %v; want %+v", list, err, first)
		}

		if err := codes.Use(ctx, u.ID, "hash-2", at); err != nil {
			t.Fatal(err)
		}
		if err := codes.Use(ctx, u.ID, "hash-2", at); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("reusing a code: %v, want ErrNotFound", err)
		}
		if err := codes.Use(ctx, u.ID, "hash-x", at); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("unknown code: %v, want ErrNotFound", err)
		}
		if n, err := codes.CountUnused(ctx, u.ID); err != nil || n != 2 {
			t.Errorf("CountUnused = %d, %v; want 2", n, err)
		}
		list, _ = codes.ListForUser(ctx, u.ID)
		for _, c := range list {
			if used := c.UsedAt != nil && c.UsedAt.Equal(at); used != (c.CodeHash == "hash-2") {
				t.Errorf("after Use: %+v", c)
			}
		}
		// Codes are per user: using u's "hash-a" leaves the other user's.
		if err := codes.Use(ctx, u.ID, "hash-a", at); err != nil {
			t.Fatal(err)
		}
		if n, _ := codes.CountUnused(ctx, other.ID); n != 1 {
			t.Errorf("other user's codes left = %d, want 1", n)
		}

		// Replacing drops the old set, used codes included.
		second, err := codes.Replace(ctx, u.ID, []string{"hash-3"}, at)
		if err != nil || len(second) != 1 {
			t.Fatalf("second Replace = %+v, %v", second, err)
		}
		if list, _ := codes.ListForUser(ctx, u.ID); len(list) != 1 || list[0].CodeHash != "hash-3" {
			t.Errorf("after second Replace: %+v", list)
		}
		if n, err := codes.DeleteAllForUser(ctx, u.ID); err != nil || n != 1 {
			t.Errorf("DeleteAllForUser = %d, %v; want 1", n, err)
		}
		if list, err := codes.ListForUser(ctx, u.ID); err != nil || list == nil || len(list) != 0 {
			t.Errorf("ListForUser after DeleteAllForUser = %v, %v; want empty", list, err)
		}
		if n, _ := codes.CountUnused(ctx, other.ID); n != 1 {
			t.Errorf("other user's codes deleted: %d left", n)
		}
	})
}

func TestWebAuthnUserHandle(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		users := store.NewUsers(db)
		u := createUser(t, db, "a@example.com", store.RoleAdmin)
		other := createUser(t, db, "b@example.com", store.RoleViewer)

		if h, err := users.WebAuthnUserHandle(ctx, u.ID); err != nil || h != nil {
			t.Errorf("handle before registration = %x, %v; want nil", h, err)
		}
		h, err := users.EnsureWebAuthnUserHandle(ctx, u.ID)
		if err != nil || len(h) != store.WebAuthnUserHandleSize {
			t.Fatalf("EnsureWebAuthnUserHandle = %x, %v", h, err)
		}
		if again, err := users.EnsureWebAuthnUserHandle(ctx, u.ID); err != nil || !bytes.Equal(again, h) {
			t.Errorf("second EnsureWebAuthnUserHandle = %x, %v; want %x", again, err, h)
		}
		if got, err := users.WebAuthnUserHandle(ctx, u.ID); err != nil || !bytes.Equal(got, h) {
			t.Errorf("WebAuthnUserHandle = %x, %v", got, err)
		}
		if oh, err := users.EnsureWebAuthnUserHandle(ctx, other.ID); err != nil || bytes.Equal(oh, h) {
			t.Errorf("other user's handle = %x, %v", oh, err)
		}
		if got, err := users.GetByWebAuthnUserHandle(ctx, h); err != nil || got.ID != u.ID {
			t.Errorf("GetByWebAuthnUserHandle = %+v, %v", got, err)
		}
		for _, unknown := range [][]byte{nil, bytes.Repeat([]byte{1}, store.WebAuthnUserHandleSize)} {
			if _, err := users.GetByWebAuthnUserHandle(ctx, unknown); !errors.Is(err, store.ErrNotFound) {
				t.Errorf("GetByWebAuthnUserHandle(%x): %v, want ErrNotFound", unknown, err)
			}
		}
		for _, missing := range []func() error{
			func() error { _, err := users.WebAuthnUserHandle(ctx, id.New()); return err },
			func() error { _, err := users.EnsureWebAuthnUserHandle(ctx, id.New()); return err },
		} {
			if err := missing(); !errors.Is(err, store.ErrNotFound) {
				t.Errorf("unknown user: %v, want ErrNotFound", err)
			}
		}
		// The handle is unique.
		if _, err := db.ExecContext(ctx, "UPDATE users SET webauthn_user_handle = ? WHERE id = ?", h, other.ID); !errors.Is(err, store.ErrConflict) {
			t.Errorf("duplicate handle: %v, want ErrConflict", err)
		}
	})
}

func TestHasSecondFactor(t *testing.T) {
	ctx := context.Background()
	const rpID = "dashboard.example.com" // newCredential's
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		users := store.NewUsers(db)
		check := func(userID, rpID string, want bool) {
			t.Helper()
			if got, err := users.HasSecondFactor(ctx, userID, rpID); err != nil || got != want {
				t.Errorf("HasSecondFactor(%s) = %v, %v; want %v", rpID, got, err, want)
			}
			// IDsWithSecondFactor agrees.
			if ids, err := users.IDsWithSecondFactor(ctx, rpID); err != nil || ids[userID] != want {
				t.Errorf("IDsWithSecondFactor(%s) = %v, %v; want %s: %v", rpID, ids, err, userID, want)
			}
		}
		u := createUser(t, db, "a@example.com", store.RoleAdmin)
		check(u.ID, rpID, false)
		check(id.New(), rpID, false)

		// Recovery codes and an unconfirmed TOTP don't count.
		if _, err := store.NewRecoveryCodes(db).Replace(ctx, u.ID, []string{"h1"}, time.Now()); err != nil {
			t.Fatal(err)
		}
		totps := store.NewUserTOTPs(db)
		if err := totps.Create(ctx, &store.TOTP{UserID: u.ID, SecretID: putTOTPSecret(t, db)}); err != nil {
			t.Fatal(err)
		}
		check(u.ID, rpID, false)
		if err := totps.Confirm(ctx, u.ID, 1, time.Now()); err != nil {
			t.Fatal(err)
		}
		check(u.ID, rpID, true)
		// A TOTP counts whatever the relying party.
		check(u.ID, "new.example.com", true)

		// A credential counts only for its own relying party ID.
		v := createUser(t, db, "b@example.com", store.RoleViewer)
		c := newCredential(v.ID, "cred")
		if err := store.NewWebAuthnCredentials(db).Create(ctx, &c); err != nil {
			t.Fatal(err)
		}
		check(v.ID, rpID, true)
		check(v.ID, "new.example.com", false)
		check(v.ID, "", false)
		// A user with both factors is listed once.
		if err := store.NewWebAuthnCredentials(db).Create(ctx, &c); err == nil {
			t.Fatal("credential ID reused")
		}
		c2 := newCredential(u.ID, "cred2")
		if err := store.NewWebAuthnCredentials(db).Create(ctx, &c2); err != nil {
			t.Fatal(err)
		}
		if ids, err := users.IDsWithSecondFactor(ctx, rpID); err != nil || len(ids) != 2 || !ids[u.ID] || !ids[v.ID] {
			t.Errorf("IDsWithSecondFactor = %v, %v; want %s and %s", ids, err, u.ID, v.ID)
		}
		if ids, err := users.IDsWithSecondFactor(ctx, "new.example.com"); err != nil || len(ids) != 1 || !ids[u.ID] {
			t.Errorf("IDsWithSecondFactor(new domain) = %v, %v; want only %s", ids, err, u.ID)
		}

		// A credential for the new domain beside a stale one counts.
		c3 := newCredential(v.ID, "cred3")
		c3.RPID = "new.example.com"
		if err := store.NewWebAuthnCredentials(db).Create(ctx, &c3); err != nil {
			t.Fatal(err)
		}
		check(v.ID, "new.example.com", true)
	})
}

func TestDeleteUserRemovesSecondFactors(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		u := createUser(t, db, "a@example.com", store.RoleAdmin)
		other := createUser(t, db, "b@example.com", store.RoleAdmin)
		for _, userID := range []string{u.ID, other.ID} {
			if err := store.NewUserTOTPs(db).Create(ctx, &store.TOTP{UserID: userID, SecretID: putTOTPSecret(t, db)}); err != nil {
				t.Fatal(err)
			}
			c := newCredential(userID, "cred-"+userID)
			if err := store.NewWebAuthnCredentials(db).Create(ctx, &c); err != nil {
				t.Fatal(err)
			}
			if _, err := store.NewRecoveryCodes(db).Replace(ctx, userID, []string{"h1", "h2"}, time.Now()); err != nil {
				t.Fatal(err)
			}
		}
		totp, err := store.NewUserTOTPs(db).Get(ctx, u.ID)
		if err != nil {
			t.Fatal(err)
		}

		if err := db.InTx(ctx, func(q store.Querier) error { return store.NewUsers(q).Delete(ctx, u.ID) }); err != nil {
			t.Fatal(err)
		}
		for _, tt := range []struct {
			query string
			arg   string
		}{
			{"SELECT COUNT(*) FROM user_totp WHERE user_id = ?", u.ID},
			{"SELECT COUNT(*) FROM secrets WHERE id = ?", totp.SecretID},
			{"SELECT COUNT(*) FROM webauthn_credentials WHERE user_id = ?", u.ID},
			{"SELECT COUNT(*) FROM recovery_codes WHERE user_id = ?", u.ID},
		} {
			if n := countRows(t, db, tt.query, tt.arg); n != 0 {
				t.Errorf("%s: %d rows left after deleting the user", tt.query, n)
			}
		}
		// The other user's factors stay.
		if ok, err := store.NewUsers(db).HasSecondFactor(ctx, other.ID, "dashboard.example.com"); err != nil || !ok {
			t.Errorf("other user's factors: %v, %v", ok, err)
		}
		if n := countRows(t, db, "SELECT COUNT(*) FROM secrets"); n != 1 {
			t.Errorf("%d secrets left, want the other user's 1", n)
		}
		if err := store.NewUsers(db).Delete(ctx, u.ID); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("deleting again: %v, want ErrNotFound", err)
		}
	})
}

func TestSessionVerifiedAtMigrationBackfill(t *testing.T) {
	ctx := context.Background()
	for _, d := range storetest.Dialects {
		t.Run(string(d), func(t *testing.T) {
			db := storetest.OpenUnmigrated(t, storetest.Config(t, d))
			backups := filepath.Join(t.TempDir(), "backups")
			if _, err := db.MigrateTo(ctx, backups, 29); err != nil {
				t.Fatal(err)
			}
			u := createUser(t, db, "a@example.com", store.RoleAdmin)
			created := time.Now().Add(-time.Hour).UTC().Truncate(time.Microsecond)
			if _, err := db.ExecContext(ctx, `INSERT INTO sessions
				(id, user_id, token_hash, created_at, last_seen_at, expires_at) VALUES (?, ?, 'h', ?, ?, ?)`,
				id.New(), u.ID, created, time.Now(), time.Now().Add(time.Hour)); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Migrate(ctx, backups); err != nil {
				t.Fatal(err)
			}
			s, err := store.NewSessions(db).GetByTokenHash(ctx, "h")
			if err != nil || !s.VerifiedAt.Equal(created) {
				t.Errorf("migrated session = %+v, %v; want verified at %v", s, err, created)
			}
			if h, err := store.NewUsers(db).WebAuthnUserHandle(ctx, u.ID); err != nil || h != nil {
				t.Errorf("migrated user's handle = %x, %v; want nil", h, err)
			}
		})
	}
}
