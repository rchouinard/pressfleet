package logging_test

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/rchouinard/pressfleet/control-plane/internal/logging"
)

func TestNewRedactsSensitiveKeys(t *testing.T) {
	for _, format := range []string{logging.FormatText, logging.FormatJSON} {
		t.Run(format, func(t *testing.T) {
			var buf bytes.Buffer
			log, err := logging.New(&buf, "info", format)
			if err != nil {
				t.Fatal(err)
			}
			log.Info("configured",
				"db_password", "pw-value",
				"API_TOKEN", "token-value",
				"Authorization", "Bearer abc",
				"site_id", "site1",
			)
			log.WithGroup("smtp").Info("relay", "secret", "grouped-value")

			out := buf.String()
			for _, leaked := range []string{"pw-value", "token-value", "Bearer abc", "grouped-value"} {
				if strings.Contains(out, leaked) {
					t.Errorf("output leaked %q:\n%s", leaked, out)
				}
			}
			if !strings.Contains(out, "site1") {
				t.Errorf("non-sensitive value was redacted:\n%s", out)
			}
		})
	}
}

func TestNewJSONIsValid(t *testing.T) {
	var buf bytes.Buffer
	log, err := logging.New(&buf, "debug", logging.FormatJSON)
	if err != nil {
		t.Fatal(err)
	}
	log.Debug("hello", "n", 1)
	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("invalid JSON %q: %v", buf.String(), err)
	}
	if rec["msg"] != "hello" || rec["level"] != "DEBUG" {
		t.Errorf("record = %v", rec)
	}
}

func TestNewLevelFiltering(t *testing.T) {
	var buf bytes.Buffer
	log, err := logging.New(&buf, "warn", logging.FormatText)
	if err != nil {
		t.Fatal(err)
	}
	log.Info("hidden")
	log.Warn("shown")
	if strings.Contains(buf.String(), "hidden") || !strings.Contains(buf.String(), "shown") {
		t.Errorf("unexpected output: %s", buf.String())
	}
}

func TestNewRejectsInvalidOptions(t *testing.T) {
	if _, err := logging.New(&bytes.Buffer{}, "loud", logging.FormatText); err == nil {
		t.Error("invalid level accepted")
	}
	if _, err := logging.New(&bytes.Buffer{}, "info", "xml"); err == nil {
		t.Error("invalid format accepted")
	}
}
