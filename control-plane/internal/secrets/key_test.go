package secrets_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/rchouinard/pressfleet/control-plane/internal/redact"
	"github.com/rchouinard/pressfleet/control-plane/internal/secrets"
	"github.com/rchouinard/pressfleet/control-plane/internal/secrets/secretstest"
)

func TestGenerateParseRoundTrip(t *testing.T) {
	k := secrets.GenerateMasterKey()
	text := k.Encode().Reveal()
	if !strings.HasPrefix(text, "pfmk1:") || len(text) != len("pfmk1:")+48 {
		t.Fatalf("encoded key has unexpected shape (length %d)", len(text))
	}
	parsed, err := secrets.ParseMasterKey(redact.Secret("  " + text + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	if parsed.ID() != k.ID() || parsed.Encode() != k.Encode() {
		t.Error("parsed key differs from the generated one")
	}
	if other := secrets.GenerateMasterKey(); other.ID() == k.ID() {
		t.Error("two generated keys have the same ID")
	}
	if id := k.ID(); len(id) != 16 {
		t.Errorf("ID = %q, want 16 hex characters", id)
	}
}

func TestParseMasterKeyErrors(t *testing.T) {
	good := secretstest.KeyText(t, 1)
	body := strings.TrimPrefix(good, "pfmk1:")
	// Change one character in the key bytes, keeping valid base64.
	flipped := []byte(body)
	if flipped[5] == 'A' {
		flipped[5] = 'B'
	} else {
		flipped[5] = 'A'
	}
	tests := []struct {
		name  string
		value string
		want  error
		msg   string
	}{
		{"empty", "  \n", secrets.ErrNoMasterKey, "not configured"},
		{"no prefix", body, secrets.ErrInvalidMasterKey, "not a PressFleet master key"},
		{"future version", "pfmk2:" + body, secrets.ErrInvalidMasterKey, "unsupported format version"},
		{"bad base64", "pfmk1:" + body[:20] + "!!" + body[22:], secrets.ErrInvalidMasterKey, "garbled or truncated"},
		{"truncated", good[:len(good)-4], secrets.ErrInvalidMasterKey, "garbled or truncated"},
		{"typo", "pfmk1:" + string(flipped), secrets.ErrInvalidMasterKey, "checksum mismatch"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := secrets.ParseMasterKey(redact.Secret(tt.value))
			if !errors.Is(err, tt.want) || !strings.Contains(err.Error(), tt.msg) {
				t.Fatalf("err = %v, want %v containing %q", err, tt.want, tt.msg)
			}
			if trimmed := strings.TrimSpace(tt.value); len(trimmed) > 8 && strings.Contains(err.Error(), trimmed[6:]) {
				t.Errorf("error echoes the key: %v", err)
			}
		})
	}
}

func TestNewMasterKeyLength(t *testing.T) {
	if _, err := secrets.NewMasterKey(make([]byte, 16)); !errors.Is(err, secrets.ErrInvalidMasterKey) {
		t.Errorf("err = %v, want ErrInvalidMasterKey", err)
	}
	if _, err := secrets.NewKeyring(secrets.MasterKey{}); !errors.Is(err, secrets.ErrInvalidMasterKey) {
		t.Errorf("NewKeyring(zero) err = %v, want ErrInvalidMasterKey", err)
	}
}

// TestKeyNeverFormatted checks that the key doesn't leak through fmt, slog,
// or JSON, whether held directly, in a keyring, or in a struct.
func TestKeyNeverFormatted(t *testing.T) {
	k := secretstest.Key(t, 7)
	text := k.Encode().Reveal()
	body := strings.TrimPrefix(text, "pfmk1:")
	keyring := secretstest.Keyring(t, 7)
	holder := struct {
		Key     secrets.MasterKey
		Keyring *secrets.Keyring
	}{k, keyring}

	var out bytes.Buffer
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%X"} {
		fmt.Fprintf(&out, verb+"\n", k)
		fmt.Fprintf(&out, verb+"\n", holder)
		fmt.Fprintf(&out, verb+"\n", keyring)
	}
	for _, h := range []slog.Handler{slog.NewTextHandler(&out, nil), slog.NewJSONHandler(&out, nil)} {
		slog.New(h).Info("keys", "key", k, "keyring", keyring, "holder", holder, "encoded", k.Encode())
	}
	data, err := json.Marshal(holder)
	if err != nil {
		t.Fatal(err)
	}
	out.Write(data)

	raw := bytes.Repeat([]byte{7}, secrets.KeySize)
	for _, leak := range []string{text, body, fmt.Sprintf("%x", raw), "7 7 7 7"} {
		if strings.Contains(out.String(), leak) {
			t.Fatalf("output contains key material %q:\n%s", leak, out.String())
		}
	}
	if !strings.Contains(out.String(), keyring.KeyID()) {
		t.Errorf("keyring output should name its key ID:\n%s", out.String())
	}
}
