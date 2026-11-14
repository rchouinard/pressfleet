package repository

import (
	"context"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/rchouinard/pressfleet/control-plane/internal/config"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner/provisionertest"
)

// restic's GOMEMLIMIT follows backup.task_memory_mb, with the default the
// drivers use for zero (#281).
func TestTaskGOMEMLIMIT(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		memoryMB int
		want     string
	}{
		{"default", 0, "384MiB"},
		{"explicit default", config.DefaultBackupTaskMemoryMB, "384MiB"},
		{"configured", 2048, "1536MiB"},
		{"minimum", config.MinBackupTaskMemoryMB, "96MiB"},
	} {
		for _, backend := range []string{BackendLocal, BackendS3} {
			t.Run(tc.name+"/"+backend, func(t *testing.T) {
				t.Parallel()
				opts := Options{
					Layout: config.Layout{Root: t.TempDir()}, Logger: slog.New(slog.DiscardHandler),
					Tasks: provisionertest.New(), TaskMemoryMB: tc.memoryMB,
				}
				rec := Repository{ClientGroupID: "g1", Backend: backend}
				if backend == BackendS3 {
					opts.Backend = BackendS3
					opts.S3 = S3Config{Endpoint: "http://minio:9000", Bucket: "b", AccessKeyID: testKeyID, SecretAccessKey: testSecret}
					rec.Location = "s3:http://minio:9000/b/g1"
				} else {
					rec.Location, _ = opts.Layout.BackupRepositoryDir("g1")
				}
				m, err := New(opts)
				if err != nil {
					t.Fatal(err)
				}
				a, err := m.taskAccess(rec, "password")
				if err != nil {
					t.Fatal(err)
				}
				if got := a.Env["GOMEMLIMIT"]; got != tc.want {
					t.Errorf("GOMEMLIMIT = %q, want %q", got, tc.want)
				}
			})
		}
	}
}

// The export's check task builds its own environment, and gets the limit
// too.
func TestCheckExportGOMEMLIMIT(t *testing.T) {
	t.Parallel()
	fake := provisionertest.New()
	m, err := New(Options{
		Layout: config.Layout{Root: t.TempDir()}, Logger: slog.New(slog.DiscardHandler),
		Tasks: fake, TaskMemoryMB: 1024,
	})
	if err != nil {
		t.Fatal(err)
	}
	var env map[string]string
	fake.RunRepositoryTaskFunc = func(_ string, task provisioner.Task) (*provisioner.TaskResult, error) {
		env = task.Env
		return &provisioner.TaskResult{}, nil
	}
	dir := t.TempDir()
	e := Export{Dir: dir, RepositoryDir: filepath.Join(dir, ExportRepositoryDir), PasswordFile: filepath.Join(dir, ExportPasswordFile)}
	if err := m.checkExport(context.Background(), "g1", e); err != nil {
		t.Fatal(err)
	}
	if got := env["GOMEMLIMIT"]; got != "768MiB" {
		t.Errorf("GOMEMLIMIT = %q, want 768MiB", got)
	}
}
