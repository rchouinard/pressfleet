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

	"github.com/rchouinard/pressfleet/control-plane/internal/id"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner/compose"
	"github.com/rchouinard/pressfleet/control-plane/internal/redact"
	"github.com/rchouinard/pressfleet/control-plane/internal/sitedb"
)

// TestRunTaskDocker proves issue #50's acceptance criterion against real
// Docker: a task reads a site's wp-content read-only through the driver. It
// also checks that writes fail in read-only mode and work in read-write
// mode, that the transfer directory is shared, that secrets reach the task
// but not the container's argv, that non-zero exits and cancellation are
// reported, that tasks work on a stopped site, and that every task container
// is removed. The tasks use the site's php image, which is always present
// here; TestRunTaskRepositoryDocker runs restic from the control plane image.
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

	s.noTaskContainers(t, ctx)
}

// TestRunTaskRepositoryDocker checks that sites with different UIDs share a
// client group's local restic repository through provisioner.RepositoryGID:
// with the repository set up as the backup engine sets it up (root-owned,
// group RepositoryGID, setgid directories, a group-readable config), two
// sites back up into it in turn, either can read the other's snapshots,
// and `restic check` passes. It needs the control plane image, which ships
// restic and mariadb-dump: set PRESSFLEET_TEST_CONTROL_PLANE_IMAGE (make
// image).
func TestRunTaskRepositoryDocker(t *testing.T) {
	s := newDockerSite(t)
	cpImage := os.Getenv("PRESSFLEET_TEST_CONTROL_PLANE_IMAGE")
	if cpImage == "" {
		t.Skip("set PRESSFLEET_TEST_CONTROL_PLANE_IMAGE to a locally built control plane image (make image)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	// A second site of the same provisioner, with its own UID.
	other := *s.cfg
	other.ID = id.New()
	other.UID = s.cfg.UID + 1
	other.DBSecretRef, other.WPSaltsSecretRef = siteSecrets(t, s.secrets, other.ID,
		sitedb.Credentials{Database: "wordpress", Username: "wordpress", Password: "unused"})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if err := s.prov.DeleteSite(ctx, other.ID); err != nil {
			t.Errorf("cleanup: delete second site: %v", err)
		}
	})
	sites := []*provisioner.SiteConfig{s.cfg, &other}
	for _, cfg := range sites {
		if _, err := s.prov.CreateSite(ctx, cfg); err != nil {
			t.Fatal(err)
		}
		proj := compose.Project{Name: compose.SiteProjectName(cfg.ID), Dir: filepath.Join(s.layout.SitesDir(), cfg.ID)}
		if _, err := proj.Exec(ctx, "php", "sh", "-c", "echo "+cfg.ID+" > /var/www/wordpress/wp-content/pf-site.txt"); err != nil {
			t.Fatal(err)
		}
	}

	// The repository, set up as root the way the backup engine does it. The
	// test doesn't run as root, so a root container does the chown.
	parent := filepath.Join(s.layout.Root, "backups", "repositories")
	repo := filepath.Join(parent, "group-"+s.cfg.ClientGroupID)
	if err := os.MkdirAll(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	asRoot := func(script string) {
		t.Helper()
		s.docker(t, ctx, "run", "--rm", "--user", "0:0", "--label", "pressfleet.test=true",
			"--mount", "type=bind,source="+parent+",target=/repositories",
			"--entrypoint", "sh", cpImage, "-c", script)
	}
	gid := strconv.Itoa(provisioner.RepositoryGID)
	t.Cleanup(func() {
		// The repository's files belong to the sites' UIDs.
		if _, err := compose.DockerExec(context.Background(), nil, "run", "--rm", "--user", "0:0",
			"--mount", "type=bind,source="+parent+",target=/repositories",
			"--entrypoint", "rm", cpImage, "-rf", "/repositories/"+filepath.Base(repo)); err != nil {
			t.Errorf("cleanup: removing repository: %v", err)
		}
	})
	asRoot("chown 0:" + gid + " /repositories/" + filepath.Base(repo) + " && chmod 2770 /repositories/" + filepath.Base(repo))

	const password = "pf-shared-repo-Password-1"
	restic := func(cfg *provisioner.SiteConfig, args ...string) string {
		t.Helper()
		res, err := s.prov.RunTask(ctx, cfg.ID, provisioner.Task{
			Image:         cpImage,
			Command:       append([]string{"restic"}, args...),
			Env:           map[string]string{"RESTIC_REPOSITORY": provisioner.TaskRepositoryPath},
			SecretEnv:     map[string]redact.Secret{"RESTIC_PASSWORD": password},
			WPContent:     provisioner.MountReadOnly,
			RepositoryDir: repo,
			Timeout:       2 * time.Minute,
		})
		if err != nil {
			t.Fatalf("site %d: restic %q: %v", cfg.UID, args, err)
		}
		return res.Stdout
	}
	restic(s.cfg, "init")
	// Group bits on the config make restic create files group-readable and
	// directories group-writable from now on.
	asRoot("chmod -R g+rwX /repositories/" + filepath.Base(repo))

	for _, cfg := range sites {
		restic(cfg, "backup", "--tag", "site:"+cfg.ID, provisioner.TaskWPContentPath)
	}
	for _, cfg := range sites {
		out := restic(cfg, "snapshots", "--json")
		for _, site := range sites {
			if !strings.Contains(out, "site:"+site.ID) {
				t.Errorf("site %d doesn't see site %s's snapshot:\n%s", cfg.UID, site.ID, out)
			}
		}
	}
	restic(&other, "check")
	if got := restic(s.cfg, "dump", "--tag", "site:"+other.ID, "latest", provisioner.TaskWPContentPath+"/pf-site.txt"); got != other.ID+"\n" {
		t.Errorf("site %d restores %q from site %d's snapshot", s.cfg.UID, got, other.UID)
	}

	// Everything restic created kept the group and is readable by it, and
	// directories stay setgid and group-writable. Files needn't be
	// group-writable: restic never changes one in place.
	listing := s.docker(t, ctx, "run", "--rm", "--user", "0:0", "--label", "pressfleet.test=true",
		"--mount", "type=bind,source="+repo+",target=/repository,readonly",
		"--entrypoint", "find", cpImage, "/repository", "-printf", `%y %m %g %u %p\n`)
	var files strings.Builder
	for line := range strings.Lines(listing) {
		f := strings.Fields(line)
		if f[0] != "d" {
			files.WriteString(line)
		}
		mode, _ := strconv.ParseUint(f[1], 8, 32)
		ok := f[2] == gid && mode&0o040 != 0
		if f[0] == "d" {
			ok = ok && mode&0o070 == 0o070 && mode&0o2000 != 0
		}
		if !ok {
			t.Errorf("repository entry without the group's access: %s", line)
		}
	}
	t.Logf("repository files after two sites' backups:\n%s", files.String())

	res, err := s.prov.RunTask(ctx, s.cfg.ID, provisioner.Task{Image: cpImage, Command: []string{"mariadb-dump", "--version"}})
	if err != nil || !strings.Contains(res.Stdout, "mariadb-dump") {
		t.Errorf("mariadb-dump --version = %+v, %v", res, err)
	}
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
