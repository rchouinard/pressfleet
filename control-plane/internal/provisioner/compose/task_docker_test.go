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

// TestRunTaskContainerDocker checks how the Compose driver runs a site
// task as a container: while it runs, its argv holds no secret, it has the
// default memory limit with no swap on top, and it runs as the site's
// UID and GID. What a task sees and does is in the conformance suite.
func TestRunTaskContainerDocker(t *testing.T) {
	s := newDockerSite(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	siteID := s.cfg.ID
	if _, err := s.prov.CreateSite(ctx, s.cfg); err != nil {
		t.Fatal(err)
	}
	image := s.docker(t, ctx, "inspect", "--format", "{{.Config.Image}}", s.project.Name+"-php-1")

	const secret = "pf-task-secret-Value-9"
	runCtx, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		_, err := s.prov.RunTask(runCtx, siteID, provisioner.Task{
			Image:       image,
			Command:     []string{"sh", "-c", "sleep 300"},
			SecretFiles: map[string]redact.Secret{"pf-secret": secret},
			WPContent:   provisioner.MountReadOnly,
		})
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
	// Site tasks have the memory limit too, with no swap on top.
	if mem := s.docker(t, ctx, "inspect", "--format", "{{.HostConfig.Memory}} {{.HostConfig.MemorySwap}}", name); mem != "536870912 536870912" {
		t.Errorf("task container memory and swap limits = %s, want 512 MiB for both", mem)
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
			Env:           map[string]string{"RESTIC_REPOSITORY": provisioner.TaskRepositoryPath, "RESTIC_PASSWORD_FILE": provisioner.TaskSecretPath("restic-password")},
			SecretFiles:   map[string]redact.Secret{"restic-password": password},
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

	// A repository task, which runs as the group instead of a site, reads
	// what the sites wrote and writes into what they share: after it prunes,
	// a site still reads the repository.
	repoTask := func(args ...string) string {
		t.Helper()
		res, err := s.prov.RunRepositoryTask(ctx, s.cfg.ClientGroupID, provisioner.Task{
			Image:         cpImage,
			Command:       append([]string{"restic", "--no-cache"}, args...),
			Env:           map[string]string{"RESTIC_REPOSITORY": provisioner.TaskRepositoryPath, "RESTIC_PASSWORD_FILE": provisioner.TaskSecretPath("restic-password")},
			SecretFiles:   map[string]redact.Secret{"restic-password": password},
			RepositoryDir: repo,
			Timeout:       2 * time.Minute,
		})
		if err != nil {
			t.Fatalf("repository task: restic %q: %v", args, err)
		}
		return res.Stdout
	}
	out := repoTask("snapshots", "--json")
	for _, site := range sites {
		if !strings.Contains(out, "site:"+site.ID) {
			t.Errorf("the repository task doesn't see site %s's snapshot:\n%s", site.ID, out)
		}
	}
	repoTask("check")
	repoTask("forget", "--tag", "site:"+other.ID, "--keep-last", "1", "--prune")
	repoTask("unlock")
	restic(s.cfg, "check")
	if got := restic(s.cfg, "dump", "--tag", "site:"+s.cfg.ID, "latest", provisioner.TaskWPContentPath+"/pf-site.txt"); got != s.cfg.ID+"\n" {
		t.Errorf("site %d restores %q after a repository task pruned", s.cfg.UID, got)
	}

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
		if strings.HasPrefix(e.Name(), "task-secrets-") {
			t.Errorf("secrets directory %s left behind", e.Name())
		}
	}
}

// TestRunTaskSecretsOutOfInspectDocker is the test for finding F6: the socket
// proxy lets Traefik call `GET /containers/<id>/json`, which returns the
// container's whole configuration, environment included. While a task runs,
// nothing in that output may hold a secret, yet the task must read its
// secrets, and they must be gone, with their directory, once it ends.
func TestRunTaskSecretsOutOfInspectDocker(t *testing.T) {
	s := newDockerSite(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	if _, err := s.prov.CreateSite(ctx, s.cfg); err != nil {
		t.Fatal(err)
	}
	image := s.docker(t, ctx, "inspect", "--format", "{{.Config.Image}}", s.project.Name+"-php-1")

	// Signals from the task. Tests that don't run as root can't hand the
	// directory to the site's UID, so open it up.
	transfer := filepath.Join(s.layout.TmpDir(), "secrets-"+s.cfg.ID)
	if err := os.Mkdir(transfer, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(transfer, 0o777); err != nil {
		t.Fatal(err)
	}
	// Distinctive values, and one with characters JSON escapes, which
	// inspect would show escaped.
	const (
		password = "pf-F6-restic-Password-7c1d"
		keyID    = "AKIAPFF6TESTKEYID"
		secret   = "pf-F6/secret+key\"with=quotes"
	)
	credentials := "[default]\naws_access_key_id = " + keyID + "\naws_secret_access_key = " + secret + "\n"
	pwPath, credPath := provisioner.TaskSecretPath("restic-password"), provisioner.TaskSecretPath("aws-credentials")

	for file, content := range map[string]string{"want-password": password, "want-credentials": credentials} {
		if err := os.WriteFile(filepath.Join(transfer, file), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	done := make(chan error, 1)
	runCtx, stop := context.WithCancel(ctx)
	defer stop()
	go func() {
		_, err := s.prov.RunTask(runCtx, s.cfg.ID, provisioner.Task{
			Image: image,
			// Compares both files, as the task's user, with what the test
			// left in the transfer directory (not in the command, which
			// inspect shows), and says so; then waits to be inspected.
			Command: []string{"sh", "-c", `
				cmp "$RESTIC_PASSWORD_FILE" /transfer/want-password &&
				cmp "$AWS_SHARED_CREDENTIALS_FILE" /transfer/want-credentials &&
				[ ! -w "$RESTIC_PASSWORD_FILE" ] &&
				stat -c %a "$RESTIC_PASSWORD_FILE" > /transfer/mode &&
				touch /transfer/read-ok
				sleep 300`},
			Env: map[string]string{
				"RESTIC_PASSWORD_FILE":        pwPath,
				"AWS_SHARED_CREDENTIALS_FILE": credPath,
			},
			SecretFiles: map[string]redact.Secret{"restic-password": password, "aws-credentials": redact.Secret(credentials)},
			TransferDir: transfer,
			Timeout:     5 * time.Minute,
		})
		done <- err
	}()

	var name string
	for deadline := time.Now().Add(2 * time.Minute); ; time.Sleep(200 * time.Millisecond) {
		if _, err := os.Stat(filepath.Join(transfer, "read-ok")); err == nil {
			name = s.taskContainer(t, ctx)
			break
		}
		select {
		case err := <-done:
			t.Fatalf("task ended before reading its secrets: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("the task never read its secrets")
		}
	}
	if mode, _ := os.ReadFile(filepath.Join(transfer, "mode")); strings.TrimSpace(string(mode)) != "400" && os.Geteuid() == 0 {
		t.Errorf("secret file mode in the task = %q, want 400", mode)
	}

	// Everything the socket proxy would return, and the Docker CLI's other
	// view of it.
	inspect := s.docker(t, ctx, "inspect", name)
	for _, v := range []string{password, keyID, secret, strings.ReplaceAll(secret, `"`, `\"`), "aws_secret_access_key"} {
		if strings.Contains(inspect, v) {
			t.Errorf("docker inspect of the running task holds %q:\n%s", v, inspect)
		}
	}
	env := s.docker(t, ctx, "inspect", "--format", "{{json .Config.Env}}", name)
	for _, want := range []string{"RESTIC_PASSWORD_FILE=" + pwPath, "AWS_SHARED_CREDENTIALS_FILE=" + credPath} {
		if !strings.Contains(env, want) {
			t.Errorf("task environment %s lacks %s", env, want)
		}
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
}
