package siteimport

import "testing"

func TestUnescape(t *testing.T) {
	tests := map[byte]byte{
		'n': '\n', 'r': '\r', 't': '\t', '0': 0, 'Z': 0x1a, 'b': '\b',
		// Anything else stands for itself, including the quote and
		// backslash themselves.
		'\'': '\'', '"': '"', '\\': '\\', 'x': 'x', '%': '%',
	}
	for in, want := range tests {
		if got := unescape(in); got != want {
			t.Errorf("unescape(%q) = %q, want %q", in, got, want)
		}
	}
}
