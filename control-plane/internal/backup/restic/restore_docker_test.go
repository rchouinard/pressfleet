package restic_test

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/backup"
	"github.com/rchouinard/pressfleet/control-plane/internal/id"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner/compose"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
)

// dockerWPContent runs the engine's site tasks in real containers of the
// control plane image against a Docker volume standing for a site's
// wp-content, the way the Compose driver runs them (read-only root
// filesystem, no capabilities, restic's cache in a tmpfs), so the restic
// commands of a scoped restore are checked against the restic that ships in
// the image. Skipped unless PRESSFLEET_DOCKER_TESTS=1 and
// PRESSFLEET_TEST_CONTROL_PLANE_IMAGE names a local control plane image
// (make image).
type dockerWPContent struct {
	t      *testing.T
	image  string
	volume string
}

func newDockerWPContent(t *testing.T) *dockerWPContent {
	t.Helper()
	if os.Getenv("PRESSFLEET_DOCKER_TESTS") != "1" {
		t.Skip("set PRESSFLEET_DOCKER_TESTS=1 to run tests against Docker")
	}
	image := os.Getenv("PRESSFLEET_TEST_CONTROL_PLANE_IMAGE")
	if image == "" {
		t.Skip("set PRESSFLEET_TEST_CONTROL_PLANE_IMAGE to a locally built control plane image (make image)")
	}
	d := &dockerWPContent{t: t, image: image, volume: "pf-test-wpcontent-" + id.New()}
	ctx := context.Background()
	d.docker(ctx, "volume", "create", "--label", "pressfleet.test=true", d.volume)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if _, err := compose.DockerExec(ctx, nil, "volume", "rm", "--force", d.volume); err != nil {
			t.Errorf("cleanup: removing the volume: %v", err)
		}
	})
	return d
}

func (d *dockerWPContent) docker(ctx context.Context, args ...string) string {
	d.t.Helper()
	out, err := compose.DockerExec(ctx, nil, args...)
	if err != nil {
		d.t.Fatalf("docker %q: %v", args, err)
	}
	return string(out)
}

// sh runs a shell script as root with the volume at /site/wp-content.
func (d *dockerWPContent) sh(ctx context.Context, script string) string {
	d.t.Helper()
	return d.docker(ctx, "run", "--rm", "--user", "0:0", "--label", "pressfleet.test=true",
		"--mount", "type=volume,source="+d.volume+",target=/site/wp-content",
		"--entrypoint", "sh", d.image, "-c", script)
}

// files returns the volume's files and their contents.
func (d *dockerWPContent) files(ctx context.Context) map[string]string {
	d.t.Helper()
	out := d.sh(ctx, `cd /site/wp-content && find . -type f | sort | while read -r f; do printf '%s=%s\n' "${f#./}" "$(cat "$f")"; done`)
	files := map[string]string{}
	for line := range strings.Lines(out) {
		name, content, _ := strings.Cut(strings.TrimSuffix(line, "\n"), "=")
		files[name] = content
	}
	return files
}

// runTask runs a site task as the host user, who owns the repository and
// transfer directory.
func (d *dockerWPContent) runTask(ctx context.Context, task provisioner.Task) (*provisioner.TaskResult, error) {
	args := []string{
		"run", "--rm", "--label", "pressfleet.test=true",
		"--user", fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()),
		"--read-only", "--tmpfs", "/tmp", "--cap-drop", "ALL", "--security-opt", "no-new-privileges",
		"--env", "HOME=/tmp",
	}
	switch task.WPContent {
	case provisioner.MountReadOnly:
		args = append(args, "--mount", "type=volume,source="+d.volume+",target="+provisioner.TaskWPContentPath+",readonly")
	case provisioner.MountReadWrite:
		args = append(args, "--mount", "type=volume,source="+d.volume+",target="+provisioner.TaskWPContentPath)
	}
	if task.TransferDir != "" {
		args = append(args, "--mount", "type=bind,source="+task.TransferDir+",target="+provisioner.TaskTransferPath)
	}
	if task.RepositoryDir != "" {
		args = append(args, "--mount", "type=bind,source="+task.RepositoryDir+",target="+provisioner.TaskRepositoryPath)
	}
	if len(task.SecretFiles) > 0 {
		secrets, err := os.MkdirTemp("", "pf-test-secrets-")
		if err != nil {
			return nil, err
		}
		defer os.RemoveAll(secrets)
		if err := os.Chmod(secrets, 0o755); err != nil {
			return nil, err
		}
		for name, v := range task.SecretFiles {
			if err := os.WriteFile(filepath.Join(secrets, name), []byte(v.Reveal()), 0o644); err != nil {
				return nil, err
			}
		}
		args = append(args, "--mount", "type=bind,source="+secrets+",target="+provisioner.TaskSecretsPath+",readonly")
	}
	for k, v := range task.Env {
		args = append(args, "--env", k+"="+v)
	}
	args = append(args, "--entrypoint", task.Command[0], d.image)
	args = append(args, task.Command[1:]...)
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	out, err := compose.DockerExec(ctx, nil, args...)
	if err != nil {
		var ee *exec.ExitError
		var ce *compose.CommandError
		if errors.As(err, &ee) && errors.As(err, &ce) {
			return nil, &provisioner.ExecError{ExitCode: ee.ExitCode(), Stdout: string(out), Stderr: ce.Stderr}
		}
		return nil, err
	}
	return &provisioner.TaskResult{Stdout: string(out)}, nil
}

// TestRestoreScopeDocker checks, with the restic and shell of the control
// plane image, that a code-only restore makes the code directories match
// the snapshot (including removing those the snapshot lacks) and leaves
// uploads and the database untouched, and that a database-only restore
// leaves wp-content alone.
func TestRestoreScopeDocker(t *testing.T) {
	d := newDockerWPContent(t)
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		f := newFixture(t, db)
		f.prov.RunTaskFunc = func(_ string, task provisioner.Task) (*provisioner.TaskResult, error) {
			return d.runTask(ctx, task)
		}
		site := f.site(t, f.group(t, "Acme"), "acme")

		uid := strconv.Itoa(os.Getuid()) + ":" + strconv.Itoa(os.Getgid())
		d.sh(ctx, `cd /site/wp-content && rm -rf ./* &&
			mkdir -p plugins/hello themes/twenty uploads/2026/09 &&
			echo v1 > plugins/hello/hello.php && echo v1 > themes/twenty/style.css && echo jpeg > uploads/2026/09/photo.jpg &&
			chown -R `+uid+` .`)
		snapshotted := d.files(ctx)

		snap, err := f.engine.Backup(ctx, site.ID, backup.Options{Kind: "manual", BackupID: id.New()})
		if err != nil {
			t.Fatal(err)
		}
		dumpBefore, _ := f.dbs.Data(site.ID)

		d.sh(ctx, `cd /site/wp-content &&
			echo v2 > plugins/hello/hello.php && mkdir plugins/new && echo new > plugins/new/new.php &&
			mkdir mu-plugins languages && echo mu > mu-plugins/loader.php && echo mo > languages/de_DE.mo &&
			rm themes/twenty/style.css &&
			echo new > uploads/2026/09/new.jpg && echo edited > uploads/2026/09/photo.jpg &&
			chown -R `+uid+` .`)
		f.dbs.SetData(site.ID, []byte("-- changed"))

		if err := f.engine.Restore(ctx, site.ID, snap.ID, site.ID, backup.RestoreOptions{Files: backup.RestoreFilesCode}); err != nil {
			t.Fatal(err)
		}
		want := maps.Clone(snapshotted)
		want["uploads/2026/09/new.jpg"] = "new"
		want["uploads/2026/09/photo.jpg"] = "edited"
		if got := d.files(ctx); !maps.Equal(got, want) {
			t.Errorf("wp-content after a code-only restore = %v, want %v", got, want)
		}
		if got, _ := f.dbs.Data(site.ID); string(got) != "-- changed" {
			t.Errorf("a code-only restore changed the database: %q", got)
		}
		// The removed directories are gone, not left empty.
		if out := d.sh(ctx, `ls /site/wp-content | sort | tr '\n' ' '`); out != "plugins themes uploads " {
			t.Errorf("wp-content has %q, want plugins themes uploads", out)
		}

		// A database-only restore leaves wp-content as it is.
		before := d.files(ctx)
		if err := f.engine.Restore(ctx, site.ID, snap.ID, site.ID, backup.RestoreOptions{Files: backup.RestoreFilesNone, Database: true}); err != nil {
			t.Fatal(err)
		}
		if got := d.files(ctx); !maps.Equal(got, before) {
			t.Errorf("wp-content after a database-only restore = %v, want %v", got, before)
		}
		if got, _ := f.dbs.Data(site.ID); string(got) != string(dumpBefore) {
			t.Errorf("database after a database-only restore = %q, want the snapshot's", got)
		}
	})
}
