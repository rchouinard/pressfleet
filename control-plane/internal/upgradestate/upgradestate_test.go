package upgradestate_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rchouinard/pressfleet/control-plane/internal/upgradestate"
)

func writeRecord(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, upgradestate.RecordFile), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestCheckStatuses(t *testing.T) {
	tests := []struct {
		name, status string
		held         bool
	}{
		{"in progress", upgradestate.InProgress, true},
		{"failed", upgradestate.Failed, true},
		{"rolling back", upgradestate.RollingBack, true},
		{"rolling back with an underscore", "rolling_back", true},
		{"rollback failed", upgradestate.RollbackFailed, true},
		{"empty", "", true},
		{"unknown", "something-new", true},
		{"succeeded", upgradestate.Succeeded, false},
		{"rolled back", upgradestate.RolledBack, false},
		{"rolled back with an underscore", "rolled_back", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := writeRecord(t, `{"from":"1.0.0","to":"1.1.0","status":"`+tt.status+`","extra":1}`)
			h := upgradestate.Check(dir)
			if h.Held != tt.held {
				t.Fatalf("Held = %v, want %v (%s)", h.Held, tt.held, h.Reason)
			}
			if tt.held && (h.Reason == "" || h.Err != nil) {
				t.Errorf("held without a reason, or with an error: %+v", h)
			}
			if !tt.held && h.Reason != "" {
				t.Errorf("released with a reason: %q", h.Reason)
			}
		})
	}
}

func TestCheckReasonNamesRelease(t *testing.T) {
	h := upgradestate.Check(writeRecord(t, `{"to":"1.2.0","status":"failed"}`))
	if !strings.Contains(h.Reason, "1.2.0") {
		t.Errorf("Reason = %q, want it to name the release", h.Reason)
	}
}

func TestCheckNoRecord(t *testing.T) {
	h := upgradestate.Check(t.TempDir())
	if h.Held || h.Err != nil {
		t.Errorf("Check without a record = %+v, want not held", h)
	}
	if _, err := upgradestate.ReadRecord(t.TempDir()); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("ReadRecord error = %v, want ErrNotExist", err)
	}
}

func TestCheckUnreadable(t *testing.T) {
	asDir := t.TempDir()
	if err := os.Mkdir(filepath.Join(asDir, upgradestate.RecordFile), 0o700); err != nil {
		t.Fatal(err)
	}
	for name, dir := range map[string]string{
		"invalid JSON": writeRecord(t, `{"status": `),
		"a directory":  asDir,
	} {
		t.Run(name, func(t *testing.T) {
			h := upgradestate.Check(dir)
			if !h.Held || !errors.Is(h.Err, upgradestate.ErrUnreadable) {
				t.Errorf("Check = %+v, want held with ErrUnreadable", h)
			}
		})
	}
}

func TestCheckResumesWithoutRestart(t *testing.T) {
	dir := writeRecord(t, `{"to":"1.1.0","status":"in-progress"}`)
	if !upgradestate.Check(dir).Held {
		t.Fatal("not held while in progress")
	}
	if err := os.WriteFile(filepath.Join(dir, upgradestate.RecordFile), []byte(`{"to":"1.1.0","status":"succeeded"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if upgradestate.Check(dir).Held {
		t.Error("still held after the record says succeeded")
	}
}
