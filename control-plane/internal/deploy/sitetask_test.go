package deploy_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/rchouinard/pressfleet/control-plane/internal/deploy"
)

const testJob = "01j9zq3v6t8k4m2n5p7r9s1x3y"

// stageTheme writes a theme package and unpacks it into wp-content.
func stageTheme(t *testing.T, wpContent, version string) deploy.UnpackResult {
	t.Helper()
	archive := filepath.Join(t.TempDir(), "package")
	if err := os.WriteFile(archive, zipOf(t, member{name: "acme/style.css", body: version}), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := deploy.Unpack(wpContent, deploy.KindTheme, "acme", testJob, archive)
	if err != nil {
		t.Fatal(err)
	}
	if res.Inode == 0 || res.Manifest.Files != 1 {
		t.Fatalf("unpack result = %+v", res)
	}
	return res
}

// installTheme puts version in themes/acme, as a deployment before.
func installTheme(t *testing.T, wpContent, version string) {
	t.Helper()
	dir := filepath.Join(wpContent, "themes", "acme")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "style.css"), []byte(version), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestSwapAndCleanup(t *testing.T) {
	wc := t.TempDir()
	installTheme(t, wc, "v1")
	res := stageTheme(t, wc, "v2")
	if got := tree(t, filepath.Join(wc, "themes")); got["acme/style.css"] != "v1" || got[".pressfleet-deploy-"+testJob+"/style.css"] != "v2" {
		t.Fatalf("after unpacking: %v", got)
	}
	for i := range 2 { // idempotent
		sw, err := deploy.Swap(wc, deploy.KindTheme, "acme", testJob, res.Inode)
		if err != nil {
			t.Fatal(err)
		}
		if !sw.Previous || !sw.Atomic {
			t.Errorf("swap %d = %+v", i, sw)
		}
	}
	want := map[string]string{"acme/": "", "acme/style.css": "v2", ".pressfleet-previous-" + testJob + "/": "", ".pressfleet-previous-" + testJob + "/style.css": "v1"}
	if got := tree(t, filepath.Join(wc, "themes")); !mapsEqual(got, want) {
		t.Errorf("after swapping: %v", got)
	}
	for range 2 {
		if err := deploy.Cleanup(wc, deploy.KindTheme, "acme", testJob, res.Inode); err != nil {
			t.Fatal(err)
		}
	}
	if got := tree(t, filepath.Join(wc, "themes")); !mapsEqual(got, map[string]string{"acme/": "", "acme/style.css": "v2"}) {
		t.Errorf("after cleanup: %v", got)
	}
}

func TestRevert(t *testing.T) {
	previous := ".pressfleet-previous-" + testJob
	staging := ".pressfleet-deploy-" + testJob
	tests := []struct {
		name string
		// setup installs v1 unless first, stages v2, and brings the
		// directory to the state a crash left.
		first bool
		state func(t *testing.T, themes string, staged uint64)
		want  deploy.RevertResult
		files map[string]string
	}{
		{
			name:  "after a swap",
			state: swapped,
			want:  deploy.RevertResult{Reverted: true, Restored: true},
			files: map[string]string{"acme/": "", "acme/style.css": "v1"},
		},
		{
			name:  "first deployment",
			first: true,
			state: swapped,
			want:  deploy.RevertResult{Reverted: true},
			files: map[string]string{},
		},
		{
			name: "exchanged but not yet renamed",
			state: func(t *testing.T, themes string, _ uint64) {
				// What an exchange leaves: the names traded places.
				rename(t, themes, "acme", "tmp")
				rename(t, themes, staging, "acme")
				rename(t, themes, "tmp", staging)
			},
			want:  deploy.RevertResult{Reverted: true, Restored: true},
			files: map[string]string{"acme/": "", "acme/style.css": "v1"},
		},
		{
			name: "between the renames of a non-atomic swap",
			state: func(t *testing.T, themes string, _ uint64) {
				rename(t, themes, "acme", previous)
			},
			want:  deploy.RevertResult{Restored: true},
			files: map[string]string{"acme/": "", "acme/style.css": "v1"},
		},
		{
			name:  "never swapped",
			state: func(*testing.T, string, uint64) {},
			files: map[string]string{"acme/": "", "acme/style.css": "v1"},
		},
		{
			name: "a revert that stopped after its exchange",
			state: func(t *testing.T, themes string, staged uint64) {
				swapped(t, themes, staged)
				// The revert's exchange: v1 back in place, v2 under the
				// previous name.
				rename(t, themes, "acme", "tmp")
				rename(t, themes, previous, "acme")
				rename(t, themes, "tmp", previous)
			},
			files: map[string]string{"acme/": "", "acme/style.css": "v1"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			wc := t.TempDir()
			if !tc.first {
				installTheme(t, wc, "v1")
			}
			res := stageTheme(t, wc, "v2")
			themes := filepath.Join(wc, "themes")
			tc.state(t, themes, res.Inode)
			got, err := deploy.Revert(wc, deploy.KindTheme, "acme", testJob, res.Inode)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("revert = %+v, want %+v", got, tc.want)
			}
			// Again: nothing left to do.
			if again, err := deploy.Revert(wc, deploy.KindTheme, "acme", testJob, res.Inode); err != nil || again.Reverted {
				t.Errorf("second revert = %+v, %v", again, err)
			}
			if files := tree(t, themes); !mapsEqual(files, tc.files) {
				t.Errorf("after reverting: %v, want %v", files, tc.files)
			}
		})
	}
}

// swapped completes the swap of the staged version.
func swapped(t *testing.T, themes string, staged uint64) {
	t.Helper()
	if _, err := deploy.Swap(filepath.Dir(themes), deploy.KindTheme, "acme", testJob, staged); err != nil {
		t.Fatal(err)
	}
}

func rename(t *testing.T, dir, from, to string) {
	t.Helper()
	if err := os.Rename(filepath.Join(dir, from), filepath.Join(dir, to)); err != nil {
		t.Fatal(err)
	}
}

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || w != v {
			return false
		}
	}
	return true
}

func TestSwapCompletesAnInterruptedExchange(t *testing.T) {
	wc := t.TempDir()
	installTheme(t, wc, "v1")
	res := stageTheme(t, wc, "v2")
	themes := filepath.Join(wc, "themes")
	staging := ".pressfleet-deploy-" + testJob
	rename(t, themes, "acme", "tmp")
	rename(t, themes, staging, "acme")
	rename(t, themes, "tmp", staging)
	sw, err := deploy.Swap(wc, deploy.KindTheme, "acme", testJob, res.Inode)
	if err != nil || !sw.Previous {
		t.Fatalf("swap = %+v, %v", sw, err)
	}
	if got := tree(t, themes); got["acme/style.css"] != "v2" || got[".pressfleet-previous-"+testJob+"/style.css"] != "v1" || len(got) != 4 {
		t.Errorf("after swapping: %v", got)
	}
}

func TestSwapRefuses(t *testing.T) {
	t.Run("a target that is a file", func(t *testing.T) {
		wc := t.TempDir()
		if err := os.MkdirAll(filepath.Join(wc, "themes"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(wc, "themes", "acme"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
		archive := filepath.Join(t.TempDir(), "package")
		if err := os.WriteFile(archive, zipOf(t, member{name: "style.css", body: "x"}), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := deploy.Unpack(wc, deploy.KindTheme, "acme", testJob, archive); !errors.Is(err, deploy.ErrTargetNotDirectory) {
			t.Errorf("Unpack = %v", err)
		}
	})
	t.Run("nothing staged", func(t *testing.T) {
		wc := t.TempDir()
		installTheme(t, wc, "v1")
		if _, err := deploy.Swap(wc, deploy.KindTheme, "acme", testJob, 12345); !errors.Is(err, deploy.ErrNotStaged) || !deploy.IsPermanentTaskError(err) {
			t.Errorf("Swap = %v", err)
		}
	})
	t.Run("a link out of wp-content", func(t *testing.T) {
		wc := t.TempDir()
		outside := t.TempDir()
		if err := os.Symlink(outside, filepath.Join(wc, "themes")); err != nil {
			t.Fatal(err)
		}
		archive := filepath.Join(t.TempDir(), "package")
		if err := os.WriteFile(archive, zipOf(t, member{name: "style.css", body: "x"}), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := deploy.Unpack(wc, deploy.KindTheme, "acme", testJob, archive); err == nil {
			t.Error("Unpack followed a link out of wp-content")
		}
		if entries, _ := os.ReadDir(outside); len(entries) != 0 {
			t.Errorf("wrote outside wp-content: %v", entries)
		}
	})
	t.Run("an invalid name", func(t *testing.T) {
		if _, err := deploy.Swap(t.TempDir(), deploy.KindTheme, "../acme", testJob, 1); err == nil {
			t.Error("Swap accepted ../acme")
		}
	})
}

func TestRunTaskCommand(t *testing.T) {
	wc := t.TempDir()
	transfer := t.TempDir()
	if err := os.WriteFile(filepath.Join(transfer, deploy.PackageFile), tarGzOf(t, member{name: "acme/style.css", body: "v1"}), 0o600); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) (string, error) {
		var out bytes.Buffer
		err := deploy.RunTaskCommand(args, &out)
		return out.String(), err
	}
	out, err := run("deploy-unpack", "--wp-content", wc, "--archive", filepath.Join(transfer, deploy.PackageFile),
		"--kind", "plugin", "--name", "acme", "--job", testJob)
	if err != nil {
		t.Fatal(err)
	}
	var res deploy.UnpackResult
	if err := json.Unmarshal([]byte(out), &res); err != nil || res.Inode == 0 || res.Manifest.Format != deploy.FormatTarGz {
		t.Fatalf("unpack output %q: %v", out, err)
	}
	if _, err := os.Stat(filepath.Join(wc, "plugins", ".pressfleet-deploy-"+testJob, "style.css")); err != nil {
		t.Error(err)
	}
	if _, err := run("deploy-swap", "--wp-content", wc, "--kind", "plugin", "--name", "acme", "--job", testJob, "--inode", "1"); !deploy.IsPermanentTaskError(err) {
		t.Errorf("swap with the wrong inode = %v", err)
	}
	for _, args := range [][]string{nil, {"nope"}, {"deploy-swap", "--bogus"}, {"deploy-swap", "--kind", "theme", "--name", "../x", "--job", testJob}} {
		if _, err := run(args...); err == nil || deploy.IsPermanentTaskError(err) {
			t.Errorf("%q = %v", args, err)
		}
	}
}
