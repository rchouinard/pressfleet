package compose

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
)

func TestTaskCreateArgs(t *testing.T) {
	base := taskSpec{
		name:            "pf-site1-task-abc",
		siteID:          "site1",
		network:         "pf-site1",
		user:            "20042:20042",
		image:           "pressfleet-control-plane:dev",
		wpContentVolume: "pf-site1_wp-content",
	}
	common := []string{
		"container", "create",
		"--name", "pf-site1-task-abc",
		"--label", "pressfleet.managed=true",
		"--label", "pressfleet.site-id=site1",
		"--label", "pressfleet.task=true",
		"--network", "pf-site1",
		"--user", "20042:20042",
		"--read-only",
		"--tmpfs", "/tmp",
		"--cap-drop", "ALL",
		"--security-opt", "no-new-privileges:true",
		"--init",
		"--log-driver", "none",
	}
	tests := []struct {
		name string
		spec func(*taskSpec)
		task provisioner.Task
		want []string // after common
	}{
		{
			name: "no mounts",
			task: provisioner.Task{Command: []string{"restic", "version"}},
			want: []string{"--entrypoint=restic", "--env", "HOME=/tmp", "--", "pressfleet-control-plane:dev", "version"},
		},
		{
			name: "wp-content read-only and transfer directory",
			spec: func(s *taskSpec) { s.wpContent, s.transferDir = provisioner.MountReadOnly, "/data/tmp/backup-1" },
			task: provisioner.Task{Command: []string{"restic", "backup", "/site/wp-content"}},
			want: []string{
				"--entrypoint=restic", "--env", "HOME=/tmp",
				"--mount", "type=volume,source=pf-site1_wp-content,target=/site/wp-content,readonly",
				"--mount", "type=bind,source=/data/tmp/backup-1,target=/transfer",
				"--", "pressfleet-control-plane:dev", "backup", "/site/wp-content",
			},
		},
		{
			name: "wp-content read-write, repository, env, and secrets",
			spec: func(s *taskSpec) {
				s.wpContent, s.repositoryDir, s.envFile = provisioner.MountReadWrite, "/data/backups/repositories/g1", "/data/tmp/task-env-1"
			},
			task: provisioner.Task{
				Command: []string{"restic", "restore", "latest"},
				Env:     map[string]string{"RESTIC_REPOSITORY": "/repository", "B": "two words", "A": "1"},
			},
			want: []string{
				"--entrypoint=restic", "--env", "HOME=/tmp",
				"--env", "A=1", "--env", "B=two words", "--env", "RESTIC_REPOSITORY=/repository",
				"--env-file", "/data/tmp/task-env-1",
				"--mount", "type=volume,source=pf-site1_wp-content,target=/site/wp-content",
				"--group-add", "60000",
				"--mount", "type=bind,source=/data/backups/repositories/g1,target=/repository",
				"--", "pressfleet-control-plane:dev", "restore", "latest",
			},
		},
		{
			name: "HOME given",
			task: provisioner.Task{Command: []string{"sh"}, Env: map[string]string{"HOME": "/transfer"}},
			want: []string{"--entrypoint=sh", "--env", "HOME=/transfer", "--", "pressfleet-control-plane:dev"},
		},
		{
			name: "hostile arguments stay argv",
			task: provisioner.Task{Command: []string{"-x", "--rm", "; rm -rf /", "$(id)"}},
			want: []string{"--entrypoint=-x", "--env", "HOME=/tmp", "--", "pressfleet-control-plane:dev", "--rm", "; rm -rf /", "$(id)"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			spec := base
			if tc.spec != nil {
				tc.spec(&spec)
			}
			got := taskCreateArgs(spec, tc.task)
			want := append(slices.Clone(common), tc.want...)
			if !slices.Equal(got, want) {
				t.Errorf("args:\n got %q\nwant %q", got, want)
			}
		})
	}
}

func TestDirUnder(t *testing.T) {
	root := t.TempDir()
	tmp := filepath.Join(root, "tmp")
	for _, d := range []string{filepath.Join(tmp, "job-1"), filepath.Join(root, "sites"), filepath.Join(tmp, "with,comma")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(tmp, "escape")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(tmp, "job-1"), filepath.Join(tmp, "alias")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmp, "file"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		dir  string
		want string // "" means rejected
	}{
		{filepath.Join(tmp, "job-1"), filepath.Join(tmp, "job-1")},
		{filepath.Join(tmp, "job-1") + "/", filepath.Join(tmp, "job-1")},
		{filepath.Join(tmp, "alias"), filepath.Join(tmp, "job-1")},
		{tmp, ""},
		{root, ""},
		{filepath.Join(root, "sites"), ""},
		{filepath.Join(tmp, "..", "sites"), ""},
		{filepath.Join(tmp, "escape"), ""},
		{filepath.Join(tmp, "missing"), ""},
		{filepath.Join(tmp, "file"), ""},
		{filepath.Join(tmp, "with,comma"), ""},
		{"tmp/job-1", ""},
		{outside, ""},
	}
	for _, tc := range tests {
		got, err := dirUnder(tmp, tc.dir)
		if tc.want == "" {
			if err == nil {
				t.Errorf("dirUnder(%q) = %q, want an error", tc.dir, got)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("dirUnder(%q) = %q, %v, want %q", tc.dir, got, err, tc.want)
		}
	}
}

func TestSiteUID(t *testing.T) {
	for _, tc := range []struct {
		user string
		want int
	}{
		{"20042:20042", 20042},
		{"20042:20043", 0},
		{"20042", 0},
		{"0:0", 0},
		{"root", 0},
		{"", 0},
	} {
		got, err := siteUID(tc.user)
		if (tc.want == 0) != (err != nil) || got != tc.want {
			t.Errorf("siteUID(%q) = %d, %v, want %d", tc.user, got, err, tc.want)
		}
	}
	if _, err := siteUID("0:0"); err == nil || !strings.Contains(err.Error(), "0:0") {
		t.Errorf("siteUID error doesn't name the user: %v", err)
	}
}
