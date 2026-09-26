package secrets_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/rchouinard/pressfleet/control-plane/internal/redact"
	"github.com/rchouinard/pressfleet/control-plane/internal/secrets"
	"github.com/rchouinard/pressfleet/control-plane/internal/secrets/secretstest"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
)

const plaintext = redact.Secret("correct horse battery staple")

func mustPut(t *testing.T, s secrets.Store, kind secrets.Kind, value redact.Secret) secrets.Ref {
	t.Helper()
	ref, err := s.Put(context.Background(), kind, value)
	if err != nil {
		t.Fatal(err)
	}
	return ref
}

func TestStoreRoundTrip(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		ctx := context.Background()
		s := secrets.NewStore(db, secretstest.Keyring(t, 1))

		ref := mustPut(t, s, secrets.KindSiteDBPassword, plaintext)
		if got, err := s.Get(ctx, ref); err != nil || got != plaintext {
			t.Fatalf("Get = %v, %v", got.Reveal(), err)
		}
		empty := mustPut(t, s, secrets.KindSiteRedisPassword, "")
		if got, err := s.Get(ctx, empty); err != nil || got != "" {
			t.Fatalf("Get(empty) = %q, %v", got.Reveal(), err)
		}

		// The database holds ciphertext, labeled with the key ID.
		var kind, keyID string
		var ciphertext []byte
		if err := db.QueryRowContext(ctx, "SELECT kind, key_id, ciphertext FROM secrets WHERE id = ?", string(ref)).
			Scan(&kind, &keyID, &ciphertext); err != nil {
			t.Fatal(err)
		}
		if kind != string(secrets.KindSiteDBPassword) || keyID != secretstest.Key(t, 1).ID() {
			t.Errorf("row kind %q, key_id %q", kind, keyID)
		}
		if bytes.Contains(ciphertext, []byte(plaintext.Reveal())) {
			t.Error("plaintext stored in the database")
		}
		// Encrypting the same value twice gives different ciphertexts.
		other := mustPut(t, s, secrets.KindSiteDBPassword, plaintext)
		var ciphertext2 []byte
		if err := db.QueryRowContext(ctx, "SELECT ciphertext FROM secrets WHERE id = ?", string(other)).Scan(&ciphertext2); err != nil {
			t.Fatal(err)
		}
		if bytes.Equal(ciphertext, ciphertext2) {
			t.Error("nonce reused: identical ciphertexts")
		}

		if err := s.Update(ctx, ref, "new value"); err != nil {
			t.Fatal(err)
		}
		if got, err := s.Get(ctx, ref); err != nil || got != "new value" {
			t.Fatalf("Get after Update = %q, %v", got.Reveal(), err)
		}

		if err := s.Delete(ctx, ref); err != nil {
			t.Fatal(err)
		}
		if err := s.Delete(ctx, ref); err != nil {
			t.Errorf("second Delete = %v, want nil (idempotent)", err)
		}
		if _, err := s.Get(ctx, ref); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("Get after Delete = %v, want ErrNotFound", err)
		}
		if err := s.Update(ctx, ref, plaintext); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("Update after Delete = %v, want ErrNotFound", err)
		}
	})
}

func TestStoreInvalidKind(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		s := secrets.NewStore(db, secretstest.Keyring(t, 1))
		for _, kind := range []secrets.Kind{"", "Site", "1st", "has space", secrets.Kind(strings.Repeat("a", 65))} {
			if _, err := s.Put(context.Background(), kind, plaintext); !errors.Is(err, secrets.ErrInvalidKind) {
				t.Errorf("Put(%q) = %v, want ErrInvalidKind", kind, err)
			}
		}
	})
}

// TestStoreInTx checks that secrets written in a transaction commit and roll
// back with it, as when provisioning creates a site and its secrets.
func TestStoreInTx(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		ctx := context.Background()
		s := secrets.NewStore(db, secretstest.Keyring(t, 1))

		var committed, rolledBack secrets.Ref
		if err := db.InTx(ctx, func(q store.Querier) error {
			var err error
			committed, err = s.With(q).Put(ctx, secrets.KindSiteDBPassword, plaintext)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		errAbort := errors.New("abort")
		if err := db.InTx(ctx, func(q store.Querier) error {
			var err error
			tx := s.With(q)
			if rolledBack, err = tx.Put(ctx, secrets.KindSiteDBPassword, plaintext); err != nil {
				return err
			}
			if got, err := tx.Get(ctx, rolledBack); err != nil || got != plaintext {
				return fmt.Errorf("Get in transaction = %v, %w", got, err)
			}
			if err := tx.Delete(ctx, committed); err != nil {
				return err
			}
			return errAbort
		}); !errors.Is(err, errAbort) {
			t.Fatal(err)
		}
		if got, err := s.Get(ctx, committed); err != nil || got != plaintext {
			t.Errorf("committed secret: %v, %v", got.Reveal(), err)
		}
		if _, err := s.Get(ctx, rolledBack); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("rolled back secret: err = %v, want ErrNotFound", err)
		}
	})
}

func TestStoreWrongKey(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		ctx := context.Background()
		ref := mustPut(t, secrets.NewStore(db, secretstest.Keyring(t, 1)), secrets.KindSiteDBPassword, plaintext)
		wrong := secrets.NewStore(db, secretstest.Keyring(t, 2))

		_, err := wrong.Get(ctx, ref)
		if !errors.Is(err, secrets.ErrUnknownKey) {
			t.Fatalf("Get with another key = %v, want ErrUnknownKey", err)
		}

		// Relabeling the row with the wrong key's ID must not help.
		if _, err := db.ExecContext(ctx, "UPDATE secrets SET key_id = ? WHERE id = ?", secretstest.Key(t, 2).ID(), string(ref)); err != nil {
			t.Fatal(err)
		}
		if _, err := wrong.Get(ctx, ref); !errors.Is(err, secrets.ErrDecrypt) {
			t.Errorf("Get with another key under its ID = %v, want ErrDecrypt", err)
		}
	})
}

func TestStoreTampering(t *testing.T) {
	type rows struct{ a, b secrets.Ref }
	tests := []struct {
		name string
		// tamper modifies the stored rows and returns the row to read.
		tamper func(t *testing.T, db *store.DB, r rows) secrets.Ref
	}{
		{"flipped ciphertext byte", func(t *testing.T, db *store.DB, r rows) secrets.Ref {
			c := ciphertextOf(t, db, r.a)
			c[len(c)-1] ^= 1
			setCiphertext(t, db, r.a, c)
			return r.a
		}},
		{"truncated ciphertext", func(t *testing.T, db *store.DB, r rows) secrets.Ref {
			setCiphertext(t, db, r.a, ciphertextOf(t, db, r.a)[:10])
			return r.a
		}},
		{"unknown format", func(t *testing.T, db *store.DB, r rows) secrets.Ref {
			c := ciphertextOf(t, db, r.a)
			c[0] = 99
			setCiphertext(t, db, r.a, c)
			return r.a
		}},
		{"ciphertext swapped between rows", func(t *testing.T, db *store.DB, r rows) secrets.Ref {
			setCiphertext(t, db, r.b, ciphertextOf(t, db, r.a))
			return r.b
		}},
		{"kind changed", func(t *testing.T, db *store.DB, r rows) secrets.Ref {
			if _, err := db.ExecContext(context.Background(), "UPDATE secrets SET kind = ? WHERE id = ?",
				string(secrets.KindResticRepoKey), string(r.a)); err != nil {
				t.Fatal(err)
			}
			return r.a
		}},
	}
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		s := secrets.NewStore(db, secretstest.Keyring(t, 1))
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				r := rows{
					a: mustPut(t, s, secrets.KindSiteDBPassword, plaintext),
					b: mustPut(t, s, secrets.KindSiteDBPassword, "another password"),
				}
				ref := tt.tamper(t, db, r)
				got, err := s.Get(context.Background(), ref)
				if !errors.Is(err, secrets.ErrDecrypt) {
					t.Fatalf("Get = %q, %v; want ErrDecrypt", got.Reveal(), err)
				}
				if strings.Contains(err.Error(), plaintext.Reveal()) {
					t.Errorf("error leaks plaintext: %v", err)
				}
			})
		}
	})
}

func ciphertextOf(t *testing.T, db *store.DB, ref secrets.Ref) []byte {
	t.Helper()
	var c []byte
	if err := db.QueryRowContext(context.Background(), "SELECT ciphertext FROM secrets WHERE id = ?", string(ref)).Scan(&c); err != nil {
		t.Fatal(err)
	}
	return c
}

func setCiphertext(t *testing.T, db *store.DB, ref secrets.Ref, c []byte) {
	t.Helper()
	if _, err := db.ExecContext(context.Background(), "UPDATE secrets SET ciphertext = ? WHERE id = ?", c, string(ref)); err != nil {
		t.Fatal(err)
	}
}

func TestVerify(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		ctx := context.Background()
		keys := secretstest.Keyring(t, 1)

		first, err := secrets.Verify(ctx, db, keys)
		if err != nil || !first {
			t.Fatalf("first Verify = %v, %v; want true, nil", first, err)
		}
		mustPut(t, secrets.NewStore(db, keys), secrets.KindSiteDBPassword, plaintext)
		first, err = secrets.Verify(ctx, db, secretstest.Keyring(t, 1))
		if err != nil || first {
			t.Fatalf("second Verify = %v, %v; want false, nil", first, err)
		}

		_, err = secrets.Verify(ctx, db, secretstest.Keyring(t, 2))
		if !errors.Is(err, secrets.ErrKeyMismatch) || !strings.Contains(err.Error(), "master key does not match this installation") {
			t.Fatalf("Verify with another key = %v, want ErrKeyMismatch", err)
		}
		if !strings.Contains(err.Error(), keys.KeyID()) {
			t.Errorf("error should name the installation's key ID: %v", err)
		}

		// A tampered check value is also a mismatch.
		if _, err := db.ExecContext(ctx, "UPDATE master_keys SET check_value = ?", []byte("bogus")); err != nil {
			t.Fatal(err)
		}
		if _, err := secrets.Verify(ctx, db, keys); !errors.Is(err, secrets.ErrKeyMismatch) {
			t.Errorf("Verify with tampered check value = %v, want ErrKeyMismatch", err)
		}

		// Secrets without a recorded key: refuse to adopt a new key.
		if _, err := db.ExecContext(ctx, "DELETE FROM master_keys"); err != nil {
			t.Fatal(err)
		}
		if _, err := secrets.Verify(ctx, db, keys); !errors.Is(err, secrets.ErrKeyMismatch) {
			t.Errorf("Verify with orphaned secrets = %v, want ErrKeyMismatch", err)
		}
	})
}

func TestOnlyOneCurrentKey(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		ctx := context.Background()
		if _, err := secrets.Verify(ctx, db, secretstest.Keyring(t, 1)); err != nil {
			t.Fatal(err)
		}
		// The partial unique index is what stops two control planes from
		// registering different keys at the same moment.
		_, err := db.ExecContext(ctx, "INSERT INTO master_keys (id, check_value, is_current, created_at) VALUES (?, ?, ?, CURRENT_TIMESTAMP)",
			"other", []byte("x"), true)
		if !errors.Is(err, store.ErrConflict) {
			t.Errorf("second current key: err = %v, want ErrConflict", err)
		}
	})
}
