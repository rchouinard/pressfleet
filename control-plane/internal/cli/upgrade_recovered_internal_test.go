package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/rchouinard/pressfleet/control-plane/internal/platform"
)

func TestDecideRollback(t *testing.T) {
	// onPrevious is the platform after the recovery steps: ready at the
	// release before the upgrade, on its image and project.
	onPrevious := platformState{Ready: true, Version: "v1.2.2", PreviousImage: true, PreviousProject: true}
	with := func(f func(s *platformState)) platformState {
		s := onPrevious
		f(&s)
		return s
	}
	unconfirmed := []string{upgradeInProgress, upgradeFailed, upgradeRollingBack, upgradeRollbackFailed}
	tests := []struct {
		name   string
		status string
		from   string
		to     string
		state  platformState
		want   rollbackAction
	}{
		{name: "recovered by hand", state: onPrevious, want: rollbackMarkRecovered},
		{name: "succeeded, on the new release", status: upgradeSucceeded,
			state: platformState{Ready: true, Version: "v1.2.3"}, want: rollbackRun},
		{name: "succeeded, on the previous release", status: upgradeSucceeded, state: onPrevious, want: rollbackRefuseOnPrevious},
		{name: "succeeded, control plane down", status: upgradeSucceeded, state: platformState{}, want: rollbackRun},
		{name: "succeeded, on neither release", status: upgradeSucceeded,
			state: platformState{Ready: true, Version: "v1.2.4"}, want: rollbackRun},
		{name: "on the new release", state: platformState{Ready: true, Version: "v1.2.3"}, want: rollbackRun},
		{name: "control plane down", state: platformState{}, want: rollbackRun},
		{name: "previous control plane not ready", state: with(func(s *platformState) { s.Ready, s.Version = false, "" }),
			want: rollbackRun},
		{name: "project not put back", state: with(func(s *platformState) { s.PreviousProject = false }), want: rollbackRun},
		{name: "another control plane image", state: with(func(s *platformState) { s.PreviousImage = false }), want: rollbackRun},
		{name: "development image at the previous version, upgraded project",
			state: platformState{Ready: true, Version: "v1.2.2", PreviousImage: false, PreviousProject: false}, want: rollbackRun},
		{name: "on neither release", state: platformState{Ready: true, Version: "v1.2.4"}, want: rollbackRefuseNeither},
		{name: "neither, but not ready", state: platformState{Version: "v1.2.4"}, want: rollbackRun},
		{name: "development CLI", to: "dev", state: platformState{Ready: true, Version: "v1.2.4"}, want: rollbackRun},
		{name: "no from in the record", from: "-", state: onPrevious, want: rollbackRun},
	}
	for _, tt := range tests {
		statuses := unconfirmed
		if tt.status != "" {
			statuses = []string{tt.status}
		}
		for _, status := range statuses {
			t.Run(tt.name+"/"+status, func(t *testing.T) {
				rec := upgradeRecord{From: "v1.2.2", To: "v1.2.3", Status: status}
				if tt.from == "-" {
					rec.From = ""
				}
				if tt.to != "" {
					rec.To = tt.to
				}
				if got := decideRollback(rec, tt.state); got != tt.want {
					t.Errorf("decideRollback(%+v, %+v) = %d, want %d", rec, tt.state, got, tt.want)
				}
			})
		}
	}
}

func TestProjectRestored(t *testing.T) {
	write := func(t *testing.T, dir, rel, content string) {
		t.Helper()
		path := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	route := filepath.Join(platform.TraefikDynamicDir, "site-1234.yaml")
	tests := []struct {
		name string
		dst  map[string]string
		want bool
	}{
		{name: "put back", dst: map[string]string{"compose.yaml": "previous", "conf/redis.conf": "previous"}, want: true},
		{name: "with files the new release added",
			dst: map[string]string{"compose.yaml": "previous", "conf/redis.conf": "previous", "added.conf": "new"}, want: true},
		{name: "with other routes",
			dst: map[string]string{"compose.yaml": "previous", "conf/redis.conf": "previous", route: "rewritten"}, want: true},
		{name: "a file of the new release", dst: map[string]string{"compose.yaml": "new", "conf/redis.conf": "previous"}},
		{name: "a file missing", dst: map[string]string{"compose.yaml": "previous"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src, dst := t.TempDir(), t.TempDir()
			for rel, content := range map[string]string{"compose.yaml": "previous", "conf/redis.conf": "previous", route: "old"} {
				write(t, src, rel, content)
			}
			for rel, content := range tt.dst {
				write(t, dst, rel, content)
			}
			got, err := projectRestored(src, dst)
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Errorf("projectRestored = %t, want %t", got, tt.want)
			}
		})
	}
}
