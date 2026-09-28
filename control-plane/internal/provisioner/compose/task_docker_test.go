package compose_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
	"github.com/rchouinard/pressfleet/control-plane/internal/redact"
)

// TestRunTaskDocker proves issue #50's acceptance criterion against real
// Docker: a task reads a site's wp-content read-only through the driver. It
// also checks that writes fail in read-only mode and work in read-write
// mode, that the transfer directory is shared, that secrets reach the task
// but not the container's argv, that non-zero exits and cancellation are
// reported, that tasks work on a stopped site, and that every task container
// is removed. The tasks use the site's php image, which is always present
// here; with PRESSFLEET_TEST_CONTROL_PLANE_IMAGE set (make test-e2e builds
// one), restic in the control plane image also backs up wp-content.
func TestRunTaskDocker(t *testing.T) {
	s := newDockerSite(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	siteID := s.cfg.ID
	if _, err := s.prov.CreateSite(ctx, s.cfg); err != nil {
		t.Fatal(err)
	}
	image := s.docker(t, ctx, "inspect", "--format", "{{.Config.Image}}", s.project.Name+"-php-1")
	s.exec(t, ctx, "php", "sh", "-c", "echo hello-from-wp-content > /var/www/wordpress/wp-content/pf-task.txt")

	// Tests don't run as root, so RunTask can't give the transfer directory
	// to the site's UID; open it up instead.
	transfer := filepath.Join(s.layout.TmpDir(), "task-"+siteID)
	if err := os.Mkdir(transfer, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(transfer, 0o777); err != nil {
		t.Fatal(err)
	}
	const secret = "pf-task-secret-Value-9"
	run := func(ctx context.Context, mode provisioner.MountMode, script string) (*provisioner.TaskResult, error) {
		return s.prov.RunTask(ctx, siteID, provisioner.Task{
			Image:       image,
			Command:     []string{"sh", "-c", script},
			Env:         map[string]string{"PF_PLAIN": "plain value"},
			SecretEnv:   map[string]redact.Secret{"PF_SECRET": secret},
			WPContent:   mode,
			TransferDir: transfer,
			Timeout:     2 * time.Minute,
		})
	}

	// Read-only: reads wp-content, can't write it, shares the transfer
	// directory, and sees its environment.
	res, err := run(ctx, provisioner.MountReadOnly, `
		cat /site/wp-content/pf-task.txt
		if touch /site/wp-content/pf-task-write 2>/dev/null; then echo wp-content is writable; exit 1; fi
		cp /site/wp-content/pf-task.txt /transfer/copy.txt
		printf '%s' "$PF_SECRET" > /transfer/secret.txt
		echo "uid=$(id -u) plain=$PF_PLAIN"
		echo to-stderr >&2`)
	if err != nil {
		t.Fatal(err)
	}
	want := "hello-from-wp-content\nuid=" + strconv.Itoa(s.cfg.UID) + " plain=plain value\n"
	if res.Stdout != want || res.Stderr != "to-stderr\n" {
		t.Errorf("read-only task = %+v, want stdout %q", res, want)
	}
	if b, err := os.ReadFile(filepath.Join(transfer, "copy.txt")); err != nil || string(b) != "hello-from-wp-content\n" {
		t.Errorf("transfer copy = %q, %v", b, err)
	}
	if b, err := os.ReadFile(filepath.Join(transfer, "secret.txt")); err != nil || string(b) != secret {
		t.Errorf("secret in task = %q, %v", b, err)
	}
	s.noTaskContainers(t, ctx)

	// Read-write: the site sees what the task writes.
	if _, err := run(ctx, provisioner.MountReadWrite, `echo restored > /site/wp-content/pf-task-rw.txt`); err != nil {
		t.Fatal(err)
	}
	if got := s.exec(t, ctx, "php", "cat", "/var/www/wordpress/wp-content/pf-task-rw.txt"); got != "restored" {
		t.Errorf("site sees %q after a read-write task", got)
	}

	// No mount: no wp-content.
	if _, err := run(ctx, provisioner.MountNone, `test ! -e /site/wp-content`); err != nil {
		t.Errorf("wp-content visible without a mount: %v", err)
	}

	// A non-zero exit is an ExecError.
	_, err = run(ctx, provisioner.MountNone, `echo oops >&2; exit 4`)
	var ee *provisioner.ExecError
	if !errors.As(err, &ee) || ee.ExitCode != 4 || ee.Stderr != "oops\n" {
		t.Errorf("failing task error = %v", err)
	}
	s.noTaskContainers(t, ctx)

	// While a task runs, its argv holds no secret. Canceling ctx kills it
	// and removes its container.
	runCtx, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		_, err := run(runCtx, provisioner.MountReadOnly, `sleep 300`)
		done <- err
	}()
	var name string
	for deadline := time.Now().Add(time.Minute); name == ""; time.Sleep(200 * time.Millisecond) {
		name = s.taskContainer(t, ctx)
		if time.Now().After(deadline) {
			t.Fatal("task container never appeared")
		}
	}
	for deadline := time.Now().Add(time.Minute); s.docker(t, ctx, "inspect", "--format", "{{.State.Status}}", name) != "running"; time.Sleep(200 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("task container never ran")
		}
	}
	argv := s.docker(t, ctx, "inspect", "--format", "{{json .Path}} {{json .Args}} {{json .Config.Cmd}} {{json .Config.Entrypoint}}", name)
	if strings.Contains(argv, secret) || !strings.Contains(argv, "sleep 300") {
		t.Errorf("task container argv = %s", argv)
	}
	if user := s.docker(t, ctx, "inspect", "--format", "{{.Config.User}}", name); user != strconv.Itoa(s.cfg.UID)+":"+strconv.Itoa(s.cfg.UID) {
		t.Errorf("task container user = %s", user)
	}
	stop()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("canceled task error = %v", err)
		}
	case <-time.After(2 * time.Minute):
		t.Fatal("canceled task didn't return")
	}
	s.noTaskContainers(t, ctx)

	// A stopped site's data is reachable too.
	if err := s.prov.StopSite(ctx, siteID); err != nil {
		t.Fatal(err)
	}
	if res, err := run(ctx, provisioner.MountReadOnly, `cat /site/wp-content/pf-task.txt`); err != nil || res.Stdout != "hello-from-wp-content\n" {
		t.Errorf("task on a stopped site = %+v, %v", res, err)
	}

	if _, err := s.prov.RunTask(ctx, "pf-no-such-site", provisioner.Task{Image: image, Command: []string{"true"}}); !errors.Is(err, provisioner.ErrSiteNotFound) {
		t.Errorf("task on a missing site: %v", err)
	}

	t.Run("restic", func(t *testing.T) {
		cpImage := os.Getenv("PRESSFLEET_TEST_CONTROL_PLANE_IMAGE")
		if cpImage == "" {
			t.Skip("set PRESSFLEET_TEST_CONTROL_PLANE_IMAGE to a locally built control plane image (make image)")
		}
		repo := filepath.Join(s.layout.Root, "backups", "repositories", "group-"+siteID)
		if err := os.MkdirAll(repo, 0o700); err != nil {
			t.Fatal(err)
		}
		// RunTask leaves the repository's ownership alone, and restic runs
		// as the site's UID.
		if err := os.Chmod(repo, 0o777); err != nil {
			t.Fatal(err)
		}
		restic := func(args ...string) *provisioner.TaskResult {
			t.Helper()
			res, err := s.prov.RunTask(ctx, siteID, provisioner.Task{
				Image:         cpImage,
				Command:       append([]string{"restic"}, args...),
				Env:           map[string]string{"RESTIC_REPOSITORY": provisioner.TaskRepositoryPath},
				SecretEnv:     map[string]redact.Secret{"RESTIC_PASSWORD": secret},
				WPContent:     provisioner.MountReadOnly,
				TransferDir:   transfer,
				RepositoryDir: repo,
				Timeout:       2 * time.Minute,
			})
			if err != nil {
				t.Fatalf("restic %q: %v", args, err)
			}
			return res
		}
		// The repository's files belong to the site's UID; remove them as it.
		t.Cleanup(func() {
			if _, err := s.prov.RunTask(context.Background(), siteID, provisioner.Task{
				Image: cpImage, Command: []string{"sh", "-c", "rm -rf /repository/*"}, RepositoryDir: repo,
			}); err != nil {
				t.Errorf("cleanup: %v", err)
			}
		})
		restic("init")
		restic("backup", "--tag", "site:"+siteID, provisioner.TaskWPContentPath, provisioner.TaskTransferPath)
		ls := restic("ls", "latest")
		for _, want := range []string{"/site/wp-content/pf-task.txt", "/transfer/copy.txt"} {
			if !strings.Contains(ls.Stdout, want) {
				t.Errorf("snapshot lacks %s:\n%s", want, ls.Stdout)
			}
		}
		if res, err := s.prov.RunTask(ctx, siteID, provisioner.Task{Image: cpImage, Command: []string{"mariadb-dump", "--version"}}); err != nil || !strings.Contains(res.Stdout, "mariadb-dump") {
			t.Errorf("mariadb-dump --version = %+v, %v", res, err)
		}
	})
	s.noTaskContainers(t, ctx)
}

// taskContainer returns the name of the site's task container, or "".
func (s *dockerSite) taskContainer(t *testing.T, ctx context.Context) string {
	t.Helper()
	out := s.docker(t, ctx, "container", "ls", "--all", "--format", "{{.Names}}",
		"--filter", "label=pressfleet.task=true", "--filter", "label=pressfleet.site-id="+s.cfg.ID)
	name, _, _ := strings.Cut(out, "\n")
	return name
}

func (s *dockerSite) noTaskContainers(t *testing.T, ctx context.Context) {
	t.Helper()
	if name := s.taskContainer(t, ctx); name != "" {
		t.Errorf("task container %s left behind", name)
	}
	entries, err := os.ReadDir(s.layout.TmpDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "task-env-") {
			t.Errorf("env-file %s left behind", e.Name())
		}
	}
}
