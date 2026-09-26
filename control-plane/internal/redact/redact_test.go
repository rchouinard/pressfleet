package redact_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/rchouinard/pressfleet/control-plane/internal/redact"
)

const plaintext = "hunter2"

func TestSecretNeverPrints(t *testing.T) {
	s := redact.Secret(plaintext)
	for _, verb := range []string{"%s", "%v", "%+v", "%#v", "%q", "%x", "%X", "%10s"} {
		if out := fmt.Sprintf(verb, s); strings.Contains(out, plaintext) {
			t.Errorf("Sprintf(%q) leaked the secret: %q", verb, out)
		}
	}
	if out := fmt.Sprintf("%+v", struct{ Password redact.Secret }{s}); strings.Contains(out, plaintext) {
		t.Errorf("struct formatting leaked the secret: %q", out)
	}
	if s.Reveal() != plaintext {
		t.Errorf("Reveal() = %q", s.Reveal())
	}
}

func TestSecretJSON(t *testing.T) {
	out, err := json.Marshal(struct{ Password redact.Secret }{redact.Secret(plaintext)})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), plaintext) {
		t.Errorf("JSON leaked the secret: %s", out)
	}
}

func TestSecretSlog(t *testing.T) {
	var buf bytes.Buffer
	for _, h := range []slog.Handler{slog.NewTextHandler(&buf, nil), slog.NewJSONHandler(&buf, nil)} {
		slog.New(h).Info("connect", "credential", redact.Secret(plaintext))
	}
	if strings.Contains(buf.String(), plaintext) {
		t.Errorf("slog leaked the secret: %s", buf.String())
	}
	if !strings.Contains(buf.String(), redact.Placeholder) {
		t.Errorf("slog output missing placeholder: %s", buf.String())
	}
}
