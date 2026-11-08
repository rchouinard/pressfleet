package cli_test

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/rchouinard/pressfleet/control-plane/internal/store"
)

// recoverByHand does what the recovery steps do to the fake platform: the
// saved project is copied back over the platform's (without removing what
// the new release added, as `cp -a` doesn't), and the previous release's
// control plane starts.
func (e *upgradeEnv) recoverByHand() {
	e.t.Helper()
	prev := e.path("upgrade", "previous-platform")
	err := filepath.WalkDir(prev, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(prev, path)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		target := e.path("platform", rel)
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o600)
	})
	if err != nil {
		e.t.Fatal(err)
	}
	p := e.platform
	p.mu.Lock()
	defer p.mu.Unlock()
	p.version, p.ready = "v1.2.2", true
	p.upErr, p.rollbackUpErr, p.runErr, p.rollbackVersion, p.onUp = nil, nil, nil, "", nil
	p.stops, p.runs, p.ups, p.changesAsked = nil, nil, nil, nil
}

// checkMarkedRecovered checks that --rollback found the platform recovered
// by hand and only marked the record rolled back.
func (e *upgradeEnv) checkMarkedRecovered(r result, prior string) {
	e.t.Helper()
	if r.code != 0 {
		e.t.Fatalf("exit %d, want 0\n%s\n%s", r.code, r.stdout, r.stderr)
	}
	for _, want := range []string{"The platform is back on v1.2.2 already", "Marked the upgrade rolled back (it was " + prior + ")"} {
		if !strings.Contains(r.stdout, want) {
			e.t.Errorf("stdout lacks %q:\n%s", want, r.stdout)
		}
	}
	if len(e.platform.stops) != 0 || len(e.platform.runs) != 0 || len(e.platform.ups) != 0 || len(e.platform.changesAsked) != 0 {
		e.t.Errorf("marking stopped %v, ran %v, started %v, and asked changes %v", e.platform.stops, e.platform.runs,
			e.platform.ups, e.platform.changesAsked)
	}
	status, rb := e.rollback()
	if status != "rolled-back" {
		e.t.Errorf("status %q, want rolled-back", status)
	}
	if rb.StoppedAt != "" || rb.Error != "" || rb.Cause == "" {
		e.t.Errorf("rollback = %+v", rb)
	}
	if got, _ := e.record()["rollback"].(map[string]any); got["recovered_by_hand"] != true {
		e.t.Errorf("the record doesn't say the rollback was finished by hand: %v", got)
	}
}

// After the recovery steps return the platform to the previous release,
// their last step, --rollback, marks the upgrade rolled back without
// changing anything else, however the upgrade was left unconfirmed (#310).
func TestRollbackAfterRecoveryByHand(t *testing.T) {
	tests := []struct {
		name   string
		status string
		fail   func(t *testing.T, e *upgradeEnv)
	}{
		{name: "failed with no-rollback", status: "failed", fail: func(t *testing.T, e *upgradeEnv) {
			if _, r := e.failedUpgrade(); r.code != 1 {
				t.Fatalf("upgrade: exit %d\n%s\n%s", r.code, r.stdout, r.stderr)
			}
		}},
		{name: "rollback stopped", status: "rollback-failed", fail: func(t *testing.T, e *upgradeEnv) {
			e.migrateOnUp(store.SQLite)
			e.platform.upErr = errors.New("unhealthy")
			e.platform.runErr = errors.New("no such image")
			if r := e.upgrade("", "--yes"); r.code != 1 || !strings.Contains(r.stderr, "step 2") {
				t.Fatalf("upgrade: exit %d\n%s\n%s", r.code, r.stdout, r.stderr)
			}
		}},
		// With PostgreSQL and a migration, the rollback refuses, and the
		// operator restores their own backup; the previous control plane
		// being ready says they did.
		{name: "migrated PostgreSQL", status: "failed", fail: func(t *testing.T, e *upgradeEnv) {
			e.usePostgres()
			e.migrateOnUp(store.Postgres)
			e.platform.upErr = errors.New("unhealthy")
			if r := e.upgrade("", "--yes"); r.code != 1 || !strings.Contains(r.stderr, "Restore the PostgreSQL state database") {
				t.Fatalf("upgrade: exit %d\n%s\n%s", r.code, r.stdout, r.stderr)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newUpgradeEnv(t, "v1.2.2")
			tt.fail(t, e)
			if status, _ := e.rollback(); status != tt.status {
				t.Fatalf("status %q, want %s", status, tt.status)
			}
			e.recoverByHand()
			// Blocking changes don't matter: nothing is restored.
			e.platform.changes = blockingChanges

			// No --yes: marking changes nothing to confirm.
			r := e.upgrade("", "--rollback")
			e.checkMarkedRecovered(r, tt.status)

			r = e.upgrade("", "--rollback")
			if r.code != 1 || !strings.Contains(r.stderr, "rolled back already") {
				t.Errorf("second --rollback: exit %d\n%s\n%s", r.code, r.stdout, r.stderr)
			}
			// The next upgrade starts afresh.
			r = e.upgrade("", "--yes")
			if r.code != 0 || strings.Contains(r.stdout, "didn't finish") {
				t.Errorf("upgrade after the recovery: exit %d\n%s\n%s", r.code, r.stdout, r.stderr)
			}
		})
	}
}

// The recovery steps end by running this CLI's --rollback, with the config
// file it was given.
func TestRecoveryStepsEndWithRollback(t *testing.T) {
	e := newUpgradeEnv(t, "v1.2.2")
	_, r := e.failedUpgrade()
	if r.code != 1 {
		t.Fatalf("exit %d\n%s\n%s", r.code, r.stdout, r.stderr)
	}
	for _, want := range []string{
		"5. Mark the upgrade rolled back with this CLI (v1.2.3-test)",
		"\n    pressfleet upgrade --rollback --config " + e.configPath + "\n\nThen use the pressfleet CLI of v1.2.2",
	} {
		if !strings.Contains(r.stderr, want) {
			t.Errorf("recovery steps lack %q:\n%s", want, r.stderr)
		}
	}
}

func TestRollbackRefusesWhenOnNeitherRelease(t *testing.T) {
	e := newUpgradeEnv(t, "v1.2.2")
	if _, r := e.failedUpgrade(); r.code != 1 {
		t.Fatalf("upgrade: exit %d\n%s\n%s", r.code, r.stdout, r.stderr)
	}
	e.recoverByHand()
	e.platform.version = "v1.2.4"

	r := e.upgrade("", "--rollback", "--yes")
	if r.code != 1 || !strings.Contains(r.stderr, "neither release") || !strings.Contains(r.stderr, "control plane runs v1.2.4") {
		t.Errorf("exit %d\n%s\n%s", r.code, r.stdout, r.stderr)
	}
	if status, _ := e.rollback(); status != "failed" {
		t.Errorf("status %q, want failed", status)
	}
	if len(e.platform.stops) != 0 || len(e.platform.runs) != 0 || len(e.platform.ups) != 0 {
		t.Errorf("a refused rollback stopped %v, ran %v, and started %v", e.platform.stops, e.platform.runs, e.platform.ups)
	}
}

// The platform counts as recovered only when the previous control plane is
// ready: one that isn't, as when it refuses a schema the state restore was
// skipped for, is rolled back instead.
func TestRollbackAfterIncompleteRecovery(t *testing.T) {
	e := newUpgradeEnv(t, "v1.2.2")
	original := readFile(t, e.path("platform", "compose.yaml"))
	if _, r := e.failedUpgrade(); r.code != 1 {
		t.Fatalf("upgrade: exit %d\n%s\n%s", r.code, r.stdout, r.stderr)
	}
	e.recoverByHand()
	e.platform.ready = false

	r := e.upgrade("", "--rollback", "--yes")
	e.checkRolledBackExit(r, original, 0)
	if len(e.platform.runs) != 1 || !slices.Contains(e.platform.runs[0].command, "restore") {
		t.Errorf("runs = %+v, want the state restore", e.platform.runs)
	}
}

func TestRollbackRefusesSucceededUpgradeOnPreviousRelease(t *testing.T) {
	e, _, _ := upgraded(t, store.SQLite)
	e.recoverByHand()

	r := e.upgrade("", "--rollback", "--yes")
	if r.code != 1 || !strings.Contains(r.stderr, "succeeded, but the platform runs v1.2.2 already") {
		t.Errorf("exit %d\n%s\n%s", r.code, r.stdout, r.stderr)
	}
	if status, _ := e.rollback(); status != "succeeded" {
		t.Errorf("status %q, want succeeded", status)
	}
	if len(e.platform.stops) != 0 || len(e.platform.runs) != 0 || len(e.platform.changesAsked) != 0 {
		t.Errorf("a refused rollback stopped %v, ran %v, and asked changes %v", e.platform.stops, e.platform.runs, e.platform.changesAsked)
	}
}
