// Package logging builds the control plane's structured logger.
package logging

import (
	"fmt"
	"io"
	"log/slog"
	"strings"

	"github.com/rchouinard/pressfleet/control-plane/internal/redact"
)

// Log formats.
const (
	FormatText = "text"
	FormatJSON = "json"
)

// sensitiveKeyParts are substrings of attribute keys whose values are always
// redacted. This is a safety net; secrets should be wrapped in redact.Secret.
var sensitiveKeyParts = []string{
	"password", "passwd", "secret", "token", "authorization", "cookie", "private_key", "master_key",
}

// ParseLevel parses a log level name: debug, info, warn, or error.
func ParseLevel(s string) (slog.Level, error) {
	var l slog.Level
	if err := l.UnmarshalText([]byte(s)); err != nil {
		return 0, fmt.Errorf("invalid log level %q (want debug, info, warn, or error)", s)
	}
	return l, nil
}

// New returns a logger writing to w in the given format ("text" or "json")
// at the given level.
func New(w io.Writer, level, format string) (*slog.Logger, error) {
	lvl, err := ParseLevel(level)
	if err != nil {
		return nil, err
	}
	opts := &slog.HandlerOptions{Level: lvl, ReplaceAttr: redactSensitive}
	switch format {
	case FormatText:
		return slog.New(slog.NewTextHandler(w, opts)), nil
	case FormatJSON:
		return slog.New(slog.NewJSONHandler(w, opts)), nil
	default:
		return nil, fmt.Errorf("invalid log format %q (want %s or %s)", format, FormatText, FormatJSON)
	}
}

// redactSensitive replaces the value of any attribute whose key looks
// sensitive.
func redactSensitive(_ []string, a slog.Attr) slog.Attr {
	if a.Value.Kind() == slog.KindGroup {
		return a
	}
	key := strings.ToLower(a.Key)
	for _, part := range sensitiveKeyParts {
		if strings.Contains(key, part) {
			return slog.String(a.Key, redact.Placeholder)
		}
	}
	return a
}
