// Package redact keeps secrets out of logs and other output (design doc §6:
// plaintext secrets are never logged).
package redact

import (
	"fmt"
	"log/slog"
)

// Placeholder replaces redacted values in output.
const Placeholder = "[REDACTED]"

// Secret is a string that never reveals its value when formatted, logged, or
// marshaled. Call Reveal to get the plaintext at the point of use.
type Secret string

// Reveal returns the plaintext value.
func (s Secret) Reveal() string { return string(s) }

// String implements fmt.Stringer.
func (Secret) String() string { return Placeholder }

// GoString implements fmt.GoStringer.
func (Secret) GoString() string { return Placeholder }

// Format implements fmt.Formatter so that every verb (%s, %v, %q, %x, ...)
// prints the placeholder.
func (Secret) Format(f fmt.State, _ rune) {
	_, _ = f.Write([]byte(Placeholder))
}

// LogValue implements slog.LogValuer.
func (Secret) LogValue() slog.Value { return slog.StringValue(Placeholder) }

// MarshalText implements encoding.TextMarshaler, which also covers JSON.
func (Secret) MarshalText() ([]byte, error) { return []byte(Placeholder), nil }
