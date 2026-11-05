package compose_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/config"
	"github.com/rchouinard/pressfleet/control-plane/internal/id"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner/compose"
	"github.com/rchouinard/pressfleet/control-plane/internal/redact"
)

// repoDocker is a Compose provisioner against the real Docker daemon, for
// repository tasks, which need no site: only the control plane image, which
// ships restic.
type repoDocker struct {
	prov    *compose.Provisioner
	layout  config.Layout
	image   string
	group   string // a client group ID
	repo    string // the group's local repository directory
	backups string
}

const repoPassword = "pf-repo-task-Password-3"

// newRepoDocker skips unless PRESSFLEET_DOCKER_TESTS=1 and
// PRESSFLEET_TEST_CONTROL_PLANE_IMAGE names a local control plane image
// (make image). backupNetwork is the platform's backup network.
func newRepoDocker(t *testing.T, backupNetwork string) *repoDocker {
	t.Helper()
	if os.Getenv("PRESSFLEET_DOCKER_TESTS") != "1" {
		t.Skip("set PRESSFLEET_DOCKER_TESTS=1 to run tests against Docker")
	}
	image := os.Getenv("PRESSFLEET_TEST_CONTROL_PLANE_IMAGE")
	if image == "" {
		t.Skip("set PRESSFLEET_TEST_CONTROL_PLANE_IMAGE to a locally built control plane image (make image)")
	}
	layout := config.Layout{Root: t.TempDir()}
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	prov, err := compose.NewProvisioner(compose.ProvisionerConfig{
		Layout: layout, Secrets: memSecrets{}, Images: imageMap{}, MariaDBContainer: "pf-test-no-mariadb",
		TaskImage: image, BackupNetwork: backupNetwork,
	})
	if err != nil {
		t.Fatal(err)
	}
	r := &repoDocker{prov: prov, layout: layout, image: image, group: id.New(), backups: filepath.Join(layout.Root, "backups", "repositories")}
	r.repo = filepath.Join(r.backups, "group-"+r.group)
	if err := os.MkdirAll(r.repo, 0o700); err != nil {
		t.Fatal(err)
	}
	// The repository's files belong to UID 60000, which the test isn't.
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if _, err := compose.DockerExec(ctx, nil, "run", "--rm", "--user", "0:0", "--label", "pressfleet.test=true",
			"--mount", "type=bind,source="+layout.Root+",target=/data", "--entrypoint", "rm", image, "-rf", "/data/backups", "/data/tmp"); err != nil {
			t.Errorf("cleanup: removing repositories: %v", err)
		}
		// Whatever a failed test left running.
		if out, err := compose.DockerExec(ctx, nil, "container", "ls", "--all", "--quiet", "--filter", "label=pressfleet.repository="+r.group); err == nil {
			for _, c := range strings.Fields(string(out)) {
				_, _ = compose.DockerExec(ctx, nil, "container", "rm", "--force", c)
			}
		}
	})
	return r
}

func (r *repoDocker) docker(t *testing.T, ctx context.Context, args ...string) string {
	t.Helper()
	out, err := compose.DockerExec(ctx, nil, args...)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}

// shareRepository sets up the repository directory the way the backup engine does:
// owned by root and provisioner.RepositoryGID, setgid, group-writable.
func (r *repoDocker) shareRepository(t *testing.T, ctx context.Context) {
	t.Helper()
	gid := strconv.Itoa(provisioner.RepositoryGID)
	r.docker(t, ctx, "run", "--rm", "--user", "0:0", "--label", "pressfleet.test=true",
		"--mount", "type=bind,source="+r.backups+",target=/repositories",
		"--entrypoint", "sh", r.image, "-c",
		"chown 0:"+gid+" /repositories/group-"+r.group+" && chmod 2770 /repositories/group-"+r.group)
}

// restic runs restic against the local repository in a repository task.
func (r *repoDocker) restic(t *testing.T, ctx context.Context, args ...string) string {
	t.Helper()
	res, err := r.prov.RunRepositoryTask(ctx, r.group, provisioner.Task{
		Command:       append([]string{"restic", "--no-cache"}, args...),
		Env:           map[string]string{"RESTIC_REPOSITORY": provisioner.TaskRepositoryPath, "RESTIC_PASSWORD_FILE": provisioner.TaskSecretPath("restic-password")},
		SecretFiles:   map[string]redact.Secret{"restic-password": repoPassword},
		RepositoryDir: r.repo,
		Timeout:       3 * time.Minute,
	})
	if err != nil {
		t.Fatalf("restic %q: %v", args, err)
	}
	return res.Stdout
}

// noContainers checks that no repository task container or secrets
// directory is left.
func (r *repoDocker) noContainers(t *testing.T, ctx context.Context) {
	t.Helper()
	if out := r.docker(t, ctx, "container", "ls", "--all", "--format", "{{.Names}}", "--filter", "label=pressfleet.repository="+r.group); out != "" {
		t.Errorf("task containers left behind: %s", out)
	}
	entries, err := os.ReadDir(r.layout.TmpDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "task-secrets-") {
			t.Errorf("secrets directory %s left behind", e.Name())
		}
	}
}

// waitRunning returns the name of the group's running task container.
func (r *repoDocker) waitRunning(t *testing.T, ctx context.Context) string {
	t.Helper()
	for deadline := time.Now().Add(2 * time.Minute); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
		out := r.docker(t, ctx, "container", "ls", "--format", "{{.Names}}", "--filter", "status=running", "--filter", "label=pressfleet.repository="+r.group)
		if name, _, _ := strings.Cut(out, "\n"); name != "" {
			return name
		}
	}
	t.Fatal("the task container never ran")
	return ""
}

// TestRunRepositoryTaskResticDocker runs restic init, snapshots, backup,
// and check in repository tasks against a local repository, as the
// repository's group. What a repository task sees is in the conformance
// suite.
func TestRunRepositoryTaskResticDocker(t *testing.T) {
	r := newRepoDocker(t, "")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	r.shareRepository(t, ctx)

	r.restic(t, ctx, "init")
	if out := strings.TrimSpace(r.restic(t, ctx, "snapshots", "--json")); out != "[]" && out != "null" {
		t.Errorf("snapshots of a new repository = %s", out)
	}
	// The state repository's kind of work: back up a file handed over in a
	// transfer directory.
	transfer := filepath.Join(r.layout.TmpDir(), "state-1")
	if err := os.Mkdir(transfer, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(transfer, "state.db"), []byte("state database"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(transfer, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(transfer, "state.db"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := r.prov.RunRepositoryTask(ctx, r.group, provisioner.Task{
		Command:       []string{"restic", "--no-cache", "backup", "--tag", "state", provisioner.TaskTransferPath + "/state.db"},
		Env:           map[string]string{"RESTIC_REPOSITORY": provisioner.TaskRepositoryPath, "RESTIC_PASSWORD_FILE": provisioner.TaskSecretPath("restic-password")},
		SecretFiles:   map[string]redact.Secret{"restic-password": repoPassword},
		RepositoryDir: r.repo,
		TransferDir:   transfer,
	}); err != nil {
		t.Fatal(err)
	}
	if out := r.restic(t, ctx, "snapshots", "--json"); !strings.Contains(out, `"state"`) {
		t.Errorf("snapshots after a backup = %s", out)
	}
	r.restic(t, ctx, "check")
	r.restic(t, ctx, "stats", "--mode", "raw-data")

	// The wrong password is restic's exit code 12, reported as an ExecError.
	_, err := r.prov.RunRepositoryTask(ctx, r.group, provisioner.Task{
		Command:       []string{"restic", "--no-cache", "snapshots"},
		Env:           map[string]string{"RESTIC_REPOSITORY": provisioner.TaskRepositoryPath, "RESTIC_PASSWORD_FILE": provisioner.TaskSecretPath("restic-password")},
		SecretFiles:   map[string]redact.Secret{"restic-password": "wrong"},
		RepositoryDir: r.repo,
	})
	var ee *provisioner.ExecError
	if !errors.As(err, &ee) || ee.ExitCode != 12 {
		t.Errorf("wrong password: %v, want exit code 12", err)
	}
	r.noContainers(t, ctx)
}

// TestRunRepositoryTaskMemoryDocker checks that the memory limit reaches the
// container (the default, the configured value, and a task's own). That the
// kernel enforces it is in the conformance suite.
func TestRunRepositoryTaskMemoryDocker(t *testing.T) {
	r := newRepoDocker(t, "")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	limits := func(task provisioner.Task) (memory, swap string) {
		t.Helper()
		task.Command, task.RepositoryDir = []string{"sleep", "300"}, r.repo
		runCtx, stop := context.WithCancel(ctx)
		done := make(chan error, 1)
		go func() { _, err := r.prov.RunRepositoryTask(runCtx, r.group, task); done <- err }()
		name := r.waitRunning(t, ctx)
		out := r.docker(t, ctx, "inspect", "--format", "{{.HostConfig.Memory}} {{.HostConfig.MemorySwap}}", name)
		stop()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Errorf("canceled task error = %v", err)
		}
		memory, swap, _ = strings.Cut(out, " ")
		return memory, swap
	}
	mib := func(n int) string { return strconv.Itoa(n << 20) }
	if m, s := limits(provisioner.Task{}); m != mib(512) || s != mib(512) {
		t.Errorf("default limits = %s, %s; want 512 MiB, no swap", m, s)
	}
	if m, s := limits(provisioner.Task{MemoryLimitMB: 192}); m != mib(192) || s != mib(192) {
		t.Errorf("task's own limits = %s, %s; want 192 MiB, no swap", m, s)
	}

	// A provisioner configured with another default.
	prov, err := compose.NewProvisioner(compose.ProvisionerConfig{
		Layout: r.layout, Secrets: memSecrets{}, Images: imageMap{}, MariaDBContainer: "pf-test-no-mariadb", TaskImage: r.image, TaskMemoryMB: 256,
	})
	if err != nil {
		t.Fatal(err)
	}
	r.prov = prov
	if m, _ := limits(provisioner.Task{}); m != mib(256) {
		t.Errorf("configured limit = %s, want 256 MiB", m)
	}

	r.noContainers(t, ctx)
}

// TestRunRepositoryTaskSecretsDocker is the F6 test for repository tasks: the
// socket proxy lets Traefik read `docker inspect`, so nothing in it may hold
// a secret while the task runs, and the task must still read them.
func TestRunRepositoryTaskSecretsDocker(t *testing.T) {
	r := newRepoDocker(t, "")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	transfer := filepath.Join(r.layout.TmpDir(), "secrets-1")
	if err := os.Mkdir(transfer, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(transfer, 0o777); err != nil {
		t.Fatal(err)
	}
	const (
		keyID  = "AKIAPFREPOTESTKEYID"
		secret = "pf-repo/secret+key\"with=quotes"
	)
	credentials := "[default]\naws_access_key_id = " + keyID + "\naws_secret_access_key = " + secret + "\n"
	if err := os.WriteFile(filepath.Join(transfer, "want-credentials"), []byte(credentials), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(transfer, "want-password"), []byte(repoPassword), 0o644); err != nil {
		t.Fatal(err)
	}
	pwPath, credPath := provisioner.TaskSecretPath("restic-password"), provisioner.TaskSecretPath("aws-credentials")

	runCtx, stop := context.WithCancel(ctx)
	defer stop()
	done := make(chan error, 1)
	go func() {
		_, err := r.prov.RunRepositoryTask(runCtx, r.group, provisioner.Task{
			Command: []string{"sh", "-c", `
				cmp "$RESTIC_PASSWORD_FILE" /transfer/want-password &&
				cmp "$AWS_SHARED_CREDENTIALS_FILE" /transfer/want-credentials &&
				touch /transfer/read-ok
				sleep 300`},
			Env:           map[string]string{"RESTIC_PASSWORD_FILE": pwPath, "AWS_SHARED_CREDENTIALS_FILE": credPath},
			SecretFiles:   map[string]redact.Secret{"restic-password": repoPassword, "aws-credentials": redact.Secret(credentials)},
			RepositoryDir: r.repo,
			TransferDir:   transfer,
			Timeout:       5 * time.Minute,
		})
		done <- err
	}()
	for deadline := time.Now().Add(2 * time.Minute); ; time.Sleep(200 * time.Millisecond) {
		if _, err := os.Stat(filepath.Join(transfer, "read-ok")); err == nil {
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
	name := r.waitRunning(t, ctx)
	inspect := r.docker(t, ctx, "inspect", name)
	for _, v := range []string{repoPassword, keyID, secret, strings.ReplaceAll(secret, `"`, `\"`), "aws_secret_access_key"} {
		if strings.Contains(inspect, v) {
			t.Errorf("docker inspect of the running task holds %q:\n%s", v, inspect)
		}
	}
	env := r.docker(t, ctx, "inspect", "--format", "{{json .Config.Env}}", name)
	for _, want := range []string{"RESTIC_PASSWORD_FILE=" + pwPath, "AWS_SHARED_CREDENTIALS_FILE=" + credPath} {
		if !strings.Contains(env, want) {
			t.Errorf("task environment %s lacks %s", env, want)
		}
	}

	// Canceling kills the task, and its container and secrets go.
	stop()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("canceled task error = %v", err)
		}
	case <-time.After(2 * time.Minute):
		t.Fatal("canceled task didn't return")
	}
	entries, err := os.ReadDir(r.layout.TmpDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "task-secrets-") {
			t.Errorf("secrets directory %s left behind", e.Name())
		}
	}
	if out := r.docker(t, ctx, "container", "ls", "--all", "--format", "{{.Names}}", "--filter", "label=pressfleet.repository="+r.group); out != "" {
		t.Errorf("task containers left behind: %s", out)
	}
}

// TestRunRepositoryTaskNetworkDocker checks the egress network a task on S3
// gets: how it is made (concurrently, again after it was removed), what it
// has, and that a task on it reaches neither the platform nor a site by
// name.
func TestRunRepositoryTaskNetworkDocker(t *testing.T) {
	r := newRepoDocker(t, "")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	// Only this test's driver may own it.
	remove := func() { _, _ = compose.DockerExec(context.Background(), nil, "network", "rm", "pf_repository_egress") }
	remove()
	t.Cleanup(remove)

	var wg sync.WaitGroup
	errs := make([]error, 4)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = r.prov.RunRepositoryTask(ctx, r.group, provisioner.Task{Command: []string{"true"}, BackupNetwork: true})
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("concurrent task #%d: %v", i, err)
		}
	}
	got := r.docker(t, ctx, "network", "inspect", "--format",
		`{{.Driver}} internal={{.Internal}} icc={{index .Options "com.docker.network.bridge.enable_icc"}} {{index .Labels "pressfleet.managed"}} containers={{len .Containers}}`,
		"pf_repository_egress")
	if got != "bridge internal=false icc=false true containers=0" {
		t.Errorf("repository network = %q", got)
	}

	res, err := r.prov.RunRepositoryTask(ctx, r.group, provisioner.Task{
		Command:       []string{"sh", "-c", `ls /sys/class/net | tr '\n' ' '; getent hosts mariadb redis traefik control-plane >/dev/null && echo reaches-platform || echo no-platform`},
		BackupNetwork: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := "eth0 lo no-platform"; strings.Join(strings.Fields(res.Stdout), " ") != want {
		t.Errorf("task on the repository network saw %q, want %q", res.Stdout, want)
	}
	r.noContainers(t, ctx)
}

// s3Fixture runs an S3-compatible server (the one the repository package's
// S3 test uses) on a network of its own, and also on a published port.
type s3Fixture struct {
	backup string // the platform's backup network: internal, with the server as "s3"
	port   string // host port
}

func startS3Fixture(t *testing.T, ctx context.Context) s3Fixture {
	t.Helper()
	const image = "rustfs/rustfs:1.0.0@sha256:8cc9801755448b71a786705ce76692c77e14936cccd87cf2fc31842e58f4d1ff"
	if _, err := compose.DockerExec(ctx, nil, "image", "inspect", image); err != nil {
		if _, err := compose.DockerExec(ctx, nil, "pull", "--quiet", image); err != nil {
			t.Skipf("S3 test server image not available: %v", err)
		}
	}
	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	f := s3Fixture{backup: "pftest-backup-" + suffix}
	server := "pftest-s3-" + suffix
	run := func(args ...string) string {
		t.Helper()
		out, err := compose.DockerExec(ctx, nil, args...)
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(string(out))
	}
	run("network", "create", "--internal", f.backup)
	t.Cleanup(func() {
		_, _ = compose.DockerExec(context.Background(), nil, "container", "rm", "--force", "--volumes", server)
		_, _ = compose.DockerExec(context.Background(), nil, "network", "rm", f.backup)
	})
	// Published ports need a network that isn't internal, so the server is
	// on the default bridge too.
	run("container", "run", "--detach", "--name", server, "--label", "pressfleet.test=true",
		"--env", "RUSTFS_ACCESS_KEY="+s3Access, "--env", "RUSTFS_SECRET_KEY="+s3Secret,
		"--publish", "0.0.0.0::9000", image)
	run("network", "connect", "--alias", "s3", f.backup, server)
	f.port = strings.TrimPrefix(run("container", "port", server, "9000/tcp"), "0.0.0.0:")
	if _, port, ok := strings.Cut(f.port, ":"); ok {
		f.port = port
	}
	return f
}

const (
	s3Access = "pftest-access"
	s3Secret = "pftest-secret-key-0123456789"
)

// s3Restic runs restic against the bucket at endpoint in a repository task
// of the group.
func (r *repoDocker) s3Restic(t *testing.T, ctx context.Context, prov *compose.Provisioner, endpoint string, args ...string) (*provisioner.TaskResult, error) {
	t.Helper()
	deadline := time.Now().Add(time.Minute)
	for {
		res, err := prov.RunRepositoryTask(ctx, r.group, provisioner.Task{
			Command: append([]string{"restic", "--no-cache"}, args...),
			Env: map[string]string{
				"RESTIC_REPOSITORY":           "s3:" + endpoint + "/pf-backups/" + r.group,
				"RESTIC_PASSWORD_FILE":        provisioner.TaskSecretPath("restic-password"),
				"AWS_SHARED_CREDENTIALS_FILE": provisioner.TaskSecretPath("aws-credentials"),
				"AWS_DEFAULT_REGION":          "us-east-1",
			},
			SecretFiles: map[string]redact.Secret{
				"restic-password": repoPassword,
				"aws-credentials": redact.Secret("[default]\naws_access_key_id = " + s3Access + "\naws_secret_access_key = " + s3Secret + "\n"),
			},
			BackupNetwork: true,
			Timeout:       3 * time.Minute,
		})
		// The server needs a moment before it answers.
		if err != nil && args[0] == "init" && time.Now().Before(deadline) && !errors.Is(err, context.Canceled) {
			time.Sleep(2 * time.Second)
			continue
		}
		return res, err
	}
}

// TestRunRepositoryTaskS3Docker runs restic against an S3 server in two
// ways: one on the platform's backup network (reached by name, with the
// egress network as the default route), and, with no backup network, one
// reached over the egress network's route to the host.
func TestRunRepositoryTaskS3Docker(t *testing.T) {
	r := newRepoDocker(t, "")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	s3 := startS3Fixture(t, ctx)
	t.Cleanup(func() { _, _ = compose.DockerExec(context.Background(), nil, "network", "rm", "pf_repository_egress") })

	// On the backup network.
	prov, err := compose.NewProvisioner(compose.ProvisionerConfig{
		Layout: r.layout, Secrets: memSecrets{}, Images: imageMap{}, MariaDBContainer: "pf-test-no-mariadb",
		TaskImage: r.image, BackupNetwork: s3.backup,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init"}, {"snapshots", "--json"}, {"check"}} {
		if _, err := r.s3Restic(t, ctx, prov, "http://s3:9000", args...); err != nil {
			t.Fatalf("restic %q on the backup network: %v", args, err)
		}
	}
	// Two interfaces: the egress network and the backup network.
	res, err := prov.RunRepositoryTask(ctx, r.group, provisioner.Task{Command: []string{"sh", "-c", `ls /sys/class/net | tr '\n' ' '`}, BackupNetwork: true})
	if err != nil || strings.Join(strings.Fields(res.Stdout), " ") != "eth0 eth1 lo" {
		t.Errorf("interfaces with a backup network = %+v, %v", res, err)
	}

	// Without a backup network the server is reached the way a remote S3
	// service is: by an address the host answers on, over the egress
	// network's route (its gateway is the host).
	gateway := r.docker(t, ctx, "network", "inspect", "--format", "{{(index .IPAM.Config 0).Gateway}}", "pf_repository_egress")
	plain, err := compose.NewProvisioner(compose.ProvisionerConfig{
		Layout: r.layout, Secrets: memSecrets{}, Images: imageMap{}, MariaDBContainer: "pf-test-no-mariadb", TaskImage: r.image,
	})
	if err != nil {
		t.Fatal(err)
	}
	out, err := r.s3Restic(t, ctx, plain, fmt.Sprintf("http://%s:%s", gateway, s3.port), "snapshots", "--json")
	if err != nil {
		t.Fatalf("restic over the egress network's route: %v", err)
	}
	if got := strings.TrimSpace(out.Stdout); got != "[]" && got != "null" {
		t.Errorf("snapshots over the egress route = %s", got)
	}
	// ...and it can't find the self-hosted server by name there.
	_, err = plain.RunRepositoryTask(ctx, r.group, provisioner.Task{Command: []string{"getent", "hosts", "s3"}, BackupNetwork: true, Timeout: time.Minute})
	if ee := (*provisioner.ExecError)(nil); !errors.As(err, &ee) {
		t.Errorf("a task without the backup network resolving the S3 service by name: %v, want a failed lookup", err)
	}
	r.noContainers(t, ctx)
}
