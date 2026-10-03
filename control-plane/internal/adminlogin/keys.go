// Package adminlogin signs and verifies the one-click login tokens that log a
// platform user in to a site's WordPress admin (design doc §5.9).
//
// The control plane holds an Ed25519 key, generated on first start by
// [Ensure] and stored encrypted with the master key in the state database
// (so state database backups carry it, and it can only be read with the
// master key, like every other secret). Sites hold only the public half, in
// wp-config.php (PRESSFLEET_LOGIN_PUBLIC_KEY). [Signer.Sign] builds the v1
// token the site-side must-use plugin checks; [Verifier] is the same check
// in Go, used by the tests (and any future Go consumer).
package adminlogin

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/redact"
	"github.com/rchouinard/pressfleet/control-plane/internal/secrets"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
)

// loginKeyName is the platform_keys row of the login signing key.
const loginKeyName = "login"

// ErrKeyUnusable means the stored login key can't be loaded: it doesn't
// decrypt (wrong master key) or doesn't match its recorded public key.
var ErrKeyUnusable = errors.New("platform login key is unusable")

// Ensure returns the platform's login signer, generating and storing the key
// first if the state database has none. It is idempotent and safe to call
// from concurrent control planes on SQLite and PostgreSQL: the key is
// created in one transaction behind a primary key, and a caller that loses
// the race uses the winner's key.
func Ensure(ctx context.Context, db *store.DB, sec secrets.Store) (*Signer, error) {
	s, err := load(ctx, db, sec)
	if err == nil || !errors.Is(err, store.ErrNotFound) {
		return s, err
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generating login key: %w", err)
	}
	err = db.InTx(ctx, func(q store.Querier) error {
		ref, err := sec.With(q).Put(ctx, secrets.KindPlatformLoginKey,
			redact.Secret(base64.StdEncoding.EncodeToString(priv.Seed())))
		if err != nil {
			return err
		}
		_, err = q.ExecContext(ctx,
			"INSERT INTO platform_keys (name, public_key, secret_ref, created_at) VALUES (?, ?, ?, ?)",
			loginKeyName, base64.StdEncoding.EncodeToString(pub), string(ref), time.Now())
		return err
	})
	if err != nil && !errors.Is(err, store.ErrConflict) {
		return nil, fmt.Errorf("storing login key: %w", err)
	}
	// On a conflict the transaction (and the orphaned secret) rolled back;
	// the winner's row is there now.
	return load(ctx, db, sec)
}

// load reads and decrypts the key. It returns store.ErrNotFound if there is
// none.
func load(ctx context.Context, db *store.DB, sec secrets.Store) (*Signer, error) {
	var pubText, ref string
	err := db.QueryRowContext(ctx, "SELECT public_key, secret_ref FROM platform_keys WHERE name = ?", loginKeyName).
		Scan(&pubText, &ref)
	if err != nil {
		return nil, fmt.Errorf("reading login key: %w", err)
	}
	seedText, err := sec.Get(ctx, secrets.Ref(ref))
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrKeyUnusable, err)
	}
	seed, err := base64.StdEncoding.DecodeString(seedText.Reveal())
	if err != nil || len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("%w: malformed private key", ErrKeyUnusable)
	}
	priv := ed25519.NewKeyFromSeed(seed)
	pub := priv.Public().(ed25519.PublicKey) //nolint:forcetypeassert // always ed25519.PublicKey
	if base64.StdEncoding.EncodeToString(pub) != pubText {
		return nil, fmt.Errorf("%w: public key doesn't match", ErrKeyUnusable)
	}
	return &Signer{priv: priv, pub: pub, now: time.Now, rand: rand.Reader}, nil
}
