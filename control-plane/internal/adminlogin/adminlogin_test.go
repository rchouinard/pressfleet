package adminlogin_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/adminlogin"
	"github.com/rchouinard/pressfleet/control-plane/internal/secrets"
	"github.com/rchouinard/pressfleet/control-plane/internal/secrets/secretstest"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
)

func TestEnsure(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		sec := secrets.NewStore(db, secretstest.Keyring(t, 1))

		first, err := adminlogin.Ensure(ctx, db, sec)
		if err != nil {
			t.Fatal(err)
		}
		second, err := adminlogin.Ensure(ctx, db, sec)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(first.PublicKey(), second.PublicKey()) {
			t.Error("a second Ensure generated a new key")
		}
		assertCounts(t, db, 1)

		var kind string
		if err := db.QueryRowContext(ctx, "SELECT s.kind FROM secrets s JOIN platform_keys k ON k.secret_ref = s.id").
			Scan(&kind); err != nil {
			t.Fatal(err)
		}
		if kind != string(secrets.KindPlatformLoginKey) {
			t.Errorf("secret kind = %q", kind)
		}

		// A control plane with another master key can't read the key, and
		// doesn't replace it.
		other := secrets.NewStore(db, secretstest.Keyring(t, 2))
		if _, err := adminlogin.Ensure(ctx, db, other); !errors.Is(err, adminlogin.ErrKeyUnusable) {
			t.Errorf("Ensure with the wrong master key = %v, want ErrKeyUnusable", err)
		}
		assertCounts(t, db, 1)
	})
}

func TestEnsureConcurrent(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		sec := secrets.NewStore(db, secretstest.Keyring(t, 1))
		const n = 8
		keys := make([]string, n)
		errs := make([]error, n)
		var wg sync.WaitGroup
		for i := range n {
			wg.Add(1)
			go func() {
				defer wg.Done()
				s, err := adminlogin.Ensure(ctx, db, sec)
				if err == nil {
					keys[i] = s.PublicKeyBase64()
				}
				errs[i] = err
			}()
		}
		wg.Wait()
		for i := range n {
			if errs[i] != nil {
				t.Fatalf("Ensure %d: %v", i, errs[i])
			}
			if keys[i] != keys[0] {
				t.Errorf("Ensure %d returned another key", i)
			}
		}
		assertCounts(t, db, 1)
	})
}

// assertCounts checks there is exactly one key row and one login secret, so
// a lost race leaves no orphan.
func assertCounts(t *testing.T, db *store.DB, want int) {
	t.Helper()
	var keys, secretRows int
	if err := db.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM platform_keys").Scan(&keys); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM secrets WHERE kind = 'platform-login-key'").Scan(&secretRows); err != nil {
		t.Fatal(err)
	}
	if keys != want || secretRows != want {
		t.Errorf("platform_keys rows = %d, login secrets = %d; want %d", keys, secretRows, want)
	}
}

func newSigner(t *testing.T) *adminlogin.Signer {
	t.Helper()
	db := storetest.Open(t, store.SQLite)
	s, err := adminlogin.Ensure(context.Background(), db, secrets.NewStore(db, secretstest.Keyring(t, 1)))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestSignExactFormat(t *testing.T) {
	signer := newSigner(t)
	before := time.Now().Unix()
	token, jti, exp, err := signer.Sign("01jsite", "admin", "01juser")
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] != "v1" {
		t.Fatalf("token = %q, want v1.<payload>.<signature>", token)
	}
	for _, p := range parts[1:] {
		if strings.ContainsAny(p, "=+/") {
			t.Errorf("part %q isn't base64url without padding", p)
		}
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(payload, &raw); err != nil {
		t.Fatal(err)
	}
	if len(raw) != 6 {
		t.Errorf("payload has %d keys: %s", len(raw), payload)
	}
	var c adminlogin.Claims
	if err := json.Unmarshal(payload, &c); err != nil {
		t.Fatal(err)
	}
	if c.Site != "01jsite" || c.User != "admin" || c.Actor != "01juser" {
		t.Errorf("claims = %+v", c)
	}
	if b, err := hex.DecodeString(c.JTI); err != nil || len(b) != 16 || c.JTI != jti {
		t.Errorf("jti = %q (returned %q), want 32 hex characters", c.JTI, jti)
	}
	if c.IssuedAt < before || c.ExpiresAt-c.IssuedAt != 60 || exp.Unix() != c.ExpiresAt {
		t.Errorf("iat = %d, exp = %d, returned expiry %d", c.IssuedAt, c.ExpiresAt, exp.Unix())
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatal(err)
	}
	pub, _ := base64.StdEncoding.DecodeString(signer.PublicKeyBase64())
	if len(pub) != ed25519.PublicKeySize || !ed25519.Verify(pub, []byte("v1."+parts[1]), sig) {
		t.Error("signature doesn't verify over the ASCII bytes v1.<payload> with the base64 public key")
	}

	_, jti2, _, _ := signer.Sign("01jsite", "admin", "01juser")
	if jti2 == jti {
		t.Error("two tokens share a jti")
	}
}

func TestVerify(t *testing.T) {
	signer := newSigner(t)
	token, _, _, err := signer.Sign("01jsite", "admin", "01juser")
	if err != nil {
		t.Fatal(err)
	}
	v := signer.Verifier()
	c, err := v.Verify(token, "01jsite")
	if err != nil || c.User != "admin" || c.Actor != "01juser" {
		t.Fatalf("Verify = %+v, %v", c, err)
	}

	parts := strings.Split(token, ".")
	otherPayload := base64.RawURLEncoding.EncodeToString([]byte(`{"site":"01jsite","user":"root","actor":"x","jti":"00","iat":1,"exp":61}`))
	otherPub, otherPriv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	forgedSig := base64.RawURLEncoding.EncodeToString(ed25519.Sign(otherPriv, []byte("v1."+parts[1])))
	tests := map[string]struct {
		token, site string
		v           *adminlogin.Verifier
	}{
		"other site":       {token, "01jother", v},
		"empty":            {"", "01jsite", v},
		"wrong version":    {"v2." + parts[1] + "." + parts[2], "01jsite", v},
		"no signature":     {"v1." + parts[1], "01jsite", v},
		"extra part":       {token + ".x", "01jsite", v},
		"tampered payload": {"v1." + otherPayload + "." + parts[2], "01jsite", v},
		"tampered sig":     {"v1." + parts[1] + "." + flip(parts[2]), "01jsite", v},
		"wrong key":        {"v1." + parts[1] + "." + forgedSig, "01jsite", v},
		"padded":           {token + "=", "01jsite", v},
		"other verifier":   {token, "01jsite", adminlogin.NewVerifier(otherPub)},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := tt.v.Verify(tt.token, tt.site); !errors.Is(err, adminlogin.ErrInvalidToken) {
				t.Errorf("Verify = %v, want ErrInvalidToken", err)
			}
		})
	}
}

func TestVerifyExpiry(t *testing.T) {
	signer := newSigner(t)
	token, _, exp, err := signer.Sign("01jsite", "admin", "01juser")
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name string
		now  time.Time
		ok   bool
	}{
		{"just before expiry", exp.Add(-time.Second), true},
		{"at expiry", exp, false},
		{"after expiry", exp.Add(time.Hour), false},
		{"before issue", exp.Add(-2 * adminlogin.TokenLifetime), false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			v := signer.Verifier()
			adminlogin.SetNow(v, func() time.Time { return tt.now })
			_, err := v.Verify(token, "01jsite")
			if (err == nil) != tt.ok {
				t.Errorf("Verify = %v, want ok = %v", err, tt.ok)
			}
		})
	}
}

func TestSignInvalid(t *testing.T) {
	signer := newSigner(t)
	for _, args := range [][3]string{{"", "u", "a"}, {"s", "", "a"}, {"s", "u", ""}} {
		if _, _, _, err := signer.Sign(args[0], args[1], args[2]); !errors.Is(err, adminlogin.ErrInvalidClaims) {
			t.Errorf("Sign(%q) = %v, want ErrInvalidClaims", args, err)
		}
	}
}

func flip(s string) string {
	b := []byte(s)
	if b[0] == 'A' {
		b[0] = 'B'
	} else {
		b[0] = 'A'
	}
	return string(b)
}
