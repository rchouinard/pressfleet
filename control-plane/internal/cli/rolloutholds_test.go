package cli

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRolloutHoldFollowsRecord(t *testing.T) {
	dir := t.TempDir()
	var logs bytes.Buffer
	hold := rolloutHold(dir, slog.New(slog.NewTextHandler(&logs, nil)))
	write := func(content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, upgradeRecordFile), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	if _, held := hold(); held {
		t.Fatal("held without a record")
	}
	write(`{"to":"1.2.0","status":"in-progress"}`)
	for range 3 {
		if reason, held := hold(); !held || !strings.Contains(reason, "1.2.0") {
			t.Fatalf("hold = %q, %v; want held for 1.2.0", reason, held)
		}
	}
	// The same state logs once, not per read.
	if n := strings.Count(logs.String(), "rollouts held until"); n != 1 {
		t.Errorf("logged the hold %d times, want 1:\n%s", n, logs.String())
	}
	write(`{"to":"1.2.0","status":"succeeded"}`)
	if _, held := hold(); held {
		t.Error("still held once the CLI marked the upgrade succeeded")
	}
	write(`not json`)
	if _, held := hold(); !held {
		t.Error("not held with an unreadable record")
	}
	if !strings.Contains(logs.String(), "can't be read") {
		t.Errorf("an unreadable record wasn't warned about:\n%s", logs.String())
	}
}

// The CLI's statuses are the contract's, so the record it writes is one
// the control plane understands.
func TestUpgradeStatusesAreTheContract(t *testing.T) {
	for status, held := range map[string]bool{upgradeInProgress: true, upgradeFailed: true, upgradeSucceeded: false} {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, upgradeRecordFile), []byte(`{"status":"`+status+`"}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, got := rolloutHold(dir, slog.New(slog.DiscardHandler))(); got != held {
			t.Errorf("status %q: held = %v, want %v", status, got, held)
		}
	}
}
