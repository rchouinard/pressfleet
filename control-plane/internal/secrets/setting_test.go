package secrets_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/rchouinard/pressfleet/control-plane/internal/redact"
	"github.com/rchouinard/pressfleet/control-plane/internal/secrets"
	"github.com/rchouinard/pressfleet/control-plane/internal/secrets/secretstest"
)

func TestSealSettingRoundTrip(t *testing.T) {
	keys := secretstest.Keyring(t, 1)
	const plain = "relay pa$$word: ' \\ \n"
	sealed := keys.SealSetting("smtp.password", plain)
	if !strings.HasPrefix(sealed, "pfenc1:"+keys.KeyID()+":") {
		t.Errorf("sealed value %q lacks the format prefix and key ID", sealed)
	}
	if strings.Contains(sealed, "word") || strings.ContainsAny(sealed, " \n'\\\"$") {
		t.Errorf("sealed value %q is not opaque, single-token text", sealed)
	}
	if !secrets.IsSealedSetting(sealed) {
		t.Error("IsSealedSetting(sealed) = false")
	}
	if again := keys.SealSetting("smtp.password", plain); again == sealed {
		t.Error("sealing twice gave the same ciphertext; nonces must be random")
	}
	got, err := keys.OpenSetting("smtp.password", sealed)
	if err != nil {
		t.Fatal(err)
	}
	if got.Reveal() != plain {
		t.Errorf("opened %q, want %q", got.Reveal(), plain)
	}
}

func TestOpenSettingErrors(t *testing.T) {
	keys := secretstest.Keyring(t, 1)
	sealed := keys.SealSetting("smtp.password", redact.Secret("pw"))
	prefix, body, _ := strings.Cut(strings.TrimPrefix(sealed, "pfenc1:"), ":")
	tampered := []byte(body)
	if tampered[len(tampered)-2] == 'A' {
		tampered[len(tampered)-2] = 'B'
	} else {
		tampered[len(tampered)-2] = 'A'
	}
	tests := []struct {
		name, setting, value string
		keys                 *secrets.Keyring
		want                 error
	}{
		{"plaintext", "smtp.password", "hunter2", keys, secrets.ErrNotSealed},
		{"empty", "smtp.password", "", keys, secrets.ErrNotSealed},
		{"no key ID", "smtp.password", "pfenc1:" + body, keys, secrets.ErrNotSealed},
		{"bad base64", "smtp.password", "pfenc1:" + prefix + ":!!", keys, secrets.ErrNotSealed},
		{"other setting", "dns.token", sealed, keys, secrets.ErrDecrypt},
		{"tampered", "smtp.password", "pfenc1:" + prefix + ":" + string(tampered), keys, secrets.ErrDecrypt},
		{"other master key", "smtp.password", sealed, secretstest.Keyring(t, 2), secrets.ErrUnknownKey},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.keys.OpenSetting(tc.setting, tc.value)
			if !errors.Is(err, tc.want) {
				t.Errorf("err = %v, want %v", err, tc.want)
			}
			if err != nil && strings.Contains(err.Error(), "hunter2") {
				t.Errorf("error %q reveals the value", err)
			}
		})
	}
	if secrets.IsSealedSetting("hunter2") {
		t.Error("IsSealedSetting(plaintext) = true")
	}
}
