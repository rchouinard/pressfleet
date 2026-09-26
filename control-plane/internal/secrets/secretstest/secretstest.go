// Package secretstest provides fixed master keys for tests.
package secretstest

import (
	"bytes"
	"testing"

	"github.com/rchouinard/pressfleet/control-plane/internal/secrets"
)

// Key returns a deterministic master key. Different seeds give different
// keys, for wrong-key tests. Never use these keys outside tests.
func Key(t testing.TB, seed byte) secrets.MasterKey {
	t.Helper()
	k, err := secrets.NewMasterKey(bytes.Repeat([]byte{seed}, secrets.KeySize))
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// KeyText returns Key(t, seed) in its encoded form, as it would appear in
// $PRESSFLEET_MASTER_KEY or a key file.
func KeyText(t testing.TB, seed byte) string {
	t.Helper()
	return Key(t, seed).Encode().Reveal()
}

// Keyring returns a keyring whose current key is Key(t, seed).
func Keyring(t testing.TB, seed byte) *secrets.Keyring {
	t.Helper()
	r, err := secrets.NewKeyring(Key(t, seed))
	if err != nil {
		t.Fatal(err)
	}
	return r
}
