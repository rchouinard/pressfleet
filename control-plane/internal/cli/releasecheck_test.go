package cli_test

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/cli"
	"github.com/rchouinard/pressfleet/control-plane/internal/config"
	"github.com/rchouinard/pressfleet/control-plane/internal/releasecheck"
)

// countingReleases is a release source listing tag, which counts how often
// it was asked.
func countingReleases(tag string) (releasecheck.Source, *atomic.Int32) {
	var calls atomic.Int32
	return releasecheck.SourceFunc(func(context.Context) ([]releasecheck.Release, error) {
		calls.Add(1)
		return []releasecheck.Release{{Tag: tag, URL: "https://github.com/rchouinard/pressfleet/releases/tag/" + tag}}, nil
	}), &calls
}

// serveUntil runs serve against dataDir until ready returns true, then
// stops it.
func serveUntil(t *testing.T, dataDir string, env map[string]string, src releasecheck.Source, ready func(stderr string) bool) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var stderr syncBuffer
	done := make(chan result, 1)
	go func() {
		done <- runOptions(t, ctx, []string{"serve", "--data-dir", dataDir, "--listen", "127.0.0.1:0"}, env,
			cli.Options{Stderr: &stderr, ReleaseSource: src})
	}()
	for deadline := time.Now().Add(10 * time.Second); !ready(stderr.String()); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			cancel()
			t.Fatalf("serve didn't get there; result: %+v", <-done)
		}
	}
	cancel()
	if r := <-done; r.code != 0 {
		t.Fatalf("serve: exit %d, stderr: %s", r.code, r.stderr)
	}
}

// adminRelease runs `pressfleet admin info` on dataDir and returns its
// release field, raw.
func adminRelease(t *testing.T, dataDir string, env map[string]string) json.RawMessage {
	t.Helper()
	env = mergeEnv(map[string]string{"PRESSFLEET_LISTEN": "127.0.0.1:1"}, env)
	r := run(t, context.Background(), []string{"admin", "info", "--data-dir", dataDir}, env)
	if r.code != 0 {
		t.Fatalf("admin info: exit %d, stderr: %s", r.code, r.stderr)
	}
	var info struct {
		Interface int             `json:"interface"`
		Release   json.RawMessage `json:"release"`
	}
	if err := json.Unmarshal([]byte(r.stdout), &info); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, r.stdout)
	}
	if info.Interface != 1 {
		t.Errorf("interface = %d: the release field is an addition to interface 1", info.Interface)
	}
	return info.Release
}

func mergeEnv(base, over map[string]string) map[string]string {
	for k, v := range over {
		base[k] = v
	}
	return base
}

// serve checks for releases at startup, and admin info reports what it
// found.
func TestServeChecksReleases(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "data")
	file := config.Layout{Root: dataDir}.ReleaseCheckFile()
	src, calls := countingReleases("v9.0.0")
	serveUntil(t, dataDir, nil, src, func(string) bool {
		_, err := os.Stat(file)
		return err == nil
	})
	if calls.Load() == 0 {
		t.Error("the release source wasn't asked")
	}

	var got struct {
		LatestVersion   string    `json:"latest_version"`
		ReleaseURL      string    `json:"release_url"`
		CheckedAt       time.Time `json:"checked_at"`
		UpdateAvailable bool      `json:"update_available"`
	}
	raw := adminRelease(t, dataDir, nil)
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("release = %s: %v", raw, err)
	}
	if got.LatestVersion != "v9.0.0" || got.ReleaseURL != "https://github.com/rchouinard/pressfleet/releases/tag/v9.0.0" ||
		got.CheckedAt.IsZero() || !got.UpdateAvailable {
		t.Errorf("release = %s", raw)
	}

	// Switched off, admin info ignores what an earlier run recorded.
	if raw := adminRelease(t, dataDir, map[string]string{"PRESSFLEET_UPDATES_RELEASE_CHECK": "false"}); raw != nil {
		t.Errorf("release with the check off = %s", raw)
	}
}

// With the check off, serve doesn't ask for releases at all.
func TestServeReleaseCheckOff(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "data")
	src, calls := countingReleases("v9.0.0")
	serveUntil(t, dataDir, map[string]string{"PRESSFLEET_UPDATES_RELEASE_CHECK": "false"}, src, func(stderr string) bool {
		return strings.Contains(stderr, "http server listening")
	})
	if n := calls.Load(); n != 0 {
		t.Errorf("the release source was asked %d times", n)
	}
	if _, err := os.Stat(config.Layout{Root: dataDir}.ReleaseCheckFile()); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("release check file: %v", err)
	}
	if raw := adminRelease(t, dataDir, nil); raw != nil {
		t.Errorf("release = %s, want none", raw)
	}
}

func TestVersionShowsUpdate(t *testing.T) {
	withRelease := func(version, latest string, update bool) *fakeControlPlane {
		cp := running(version)
		cp.info = `{"interface":1,"version":"` + version + `","ready":true,"release":{"latest_version":"` + latest +
			`","release_url":"https://example.com/` + latest + `","checked_at":"2026-10-20T12:00:00Z","update_available":` +
			map[bool]string{true: "true", false: "false"}[update] + `}}`
		return cp
	}
	tests := []struct {
		name string
		cp   *fakeControlPlane
		// want is the notice; empty means none.
		want string
	}{
		{"newer than this CLI", withRelease("v1.2.3-test", "v1.3.0", true),
			"A newer PressFleet release is available: v1.3.0 (https://example.com/v1.3.0).\n" +
				"To upgrade, install the v1.3.0 CLI from the release page and run `pressfleet upgrade`.\n"},
		{"this CLI has it", withRelease("v1.2.0", "v1.2.2", true),
			"A newer PressFleet release is available: v1.2.2 (https://example.com/v1.2.2).\n" +
				"To upgrade, run `pressfleet upgrade` with this CLI.\n"},
		{"up to date", withRelease("v1.2.3-test", "v1.2.2", false), ""},
		{"check off or no result", running("v1.2.3-test"), ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := runAgainst(t, tt.cp, nil, "version")
			if r.code != 0 {
				t.Fatalf("exit %d, stderr: %s", r.code, r.stderr)
			}
			if tt.want == "" {
				if strings.Contains(r.stdout, "newer PressFleet release") {
					t.Errorf("unexpected notice:\n%s", r.stdout)
				}
				return
			}
			if !strings.HasSuffix(r.stdout, "\n\n"+tt.want) {
				t.Errorf("stdout lacks the notice %q:\n%s", tt.want, r.stdout)
			}
		})
	}
}
