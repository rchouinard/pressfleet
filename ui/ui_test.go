package ui_test

import (
	"io/fs"
	"testing"

	"github.com/rchouinard/pressfleet/ui"
)

// TestFiles holds for both builds (placeholder and real UI).
func TestFiles(t *testing.T) {
	if _, err := fs.Stat(ui.Files(), "index.html"); err != nil {
		t.Fatalf("embedded UI has no index.html: %v", err)
	}
}
