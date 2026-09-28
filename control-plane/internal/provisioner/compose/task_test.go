package compose_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/config"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner/compose"
	"github.com/rchouinard/pressfleet/control-plane/internal/redact"
)

const (
	testTaskImage   = "pressfleet-control-plane:test"
	testTaskSecret  = "s3cret-Repository-Password"
	testSiteVolume  = "pf-" + testSiteID + "_wp-content"
	testPHPUser     = "20042:20042"
	testPHPInstance = "pf-" + testSiteID + "-php-1"
)

// taskDocker fakes the docker CLI around RunTask for testSiteID.
type taskDocker struct {
	t      *testing.T
	noSite bool
	// exitCode is the task container's exit code; status is its final
	// state ("exited" unless set).
	exitCode int
	status   string
	// stdout and stderr are what the attached task writes; startErr is
	// what `docker start` returns; block makes it wait for ctx instead.
	stdout, stderr string
	startErr       error
	block          bool
	// connectErr is what `docker network connect` returns.
	connectErr error

	calls      []string
	createArgs []string
	envFile    string // path passed with --env-file
	envContent string // its content during create
	envMode    os.FileMode
	removed    []string
}

func (f *taskDocker) exec(_ context.Context, _ io.Reader, args ...string) ([]byte, error) {
	f.calls = append(f.calls, strings.Join(args, " "))
	switch {
	case args[0] == "compose" && slices.Contains(args, "ps"):
		if f.noSite {
			return nil, nil
		}
		var out []byte
		for _, svc := range []string{"php", "nginx"} {
			b, _ := json.Marshal(compose.Container{Name: "pf-" + testSiteID + "-" + svc + "-1", Service: svc, State: "exited"})
			out = append(append(out, b...), '\n')
		}
		return out, nil
	case args[0] == "container" && args[1] == "inspect":
		return f.inspect(args[len(args)-1])
	case args[0] == "container" && args[1] == "create":
		f.createArgs = args
		if i := slices.Index(args, "--env-file"); i >= 0 {
			f.envFile = args[i+1]
			fi, err := os.Stat(f.envFile)
			if err != nil {
				f.t.Fatalf("env-file: %v", err)
			}
			f.envMode = fi.Mode().Perm()
			b, _ := os.ReadFile(f.envFile)
			f.envContent = string(b)
		}
		return []byte("0123abcd\n"), nil
	case args[0] == "network" && args[1] == "connect":
		return nil, f.connectErr
	case args[0] == "container" && args[1] == "rm":
		f.removed = append(f.removed, args[len(args)-1])
		return nil, nil
	}
	f.t.Fatalf("unexpected docker call: %q", args)
	return nil, nil
}

func (f *taskDocker) stream(ctx context.Context, stdout, stderr io.Writer, args ...string) error {
	f.calls = append(f.calls, strings.Join(args, " "))
	if !slices.Equal(args[:4], []string{"container", "start", "--attach", "--"}) {
		f.t.Fatalf("unexpected streamed docker call: %q", args)
	}
	if f.block {
		<-ctx.Done()
		return &compose.CommandError{Args: args, Err: ctx.Err()}
	}
	_, _ = io.WriteString(stdout, f.stdout)
	_, _ = io.WriteString(stderr, f.stderr)
	return f.startErr
}

func (f *taskDocker) inspect(name string) ([]byte, error) {
	var info map[string]any
	switch {
	case name == testMariaDB:
		info = map[string]any{"NetworkSettings": map[string]any{"Networks": map[string]any{"pf-" + testSiteID: map[string]any{}}}}
	case name == testPHPInstance:
		info = map[string]any{
			"Config": map[string]any{"User": testPHPUser},
			"Mounts": []any{
				map[string]any{"Type": "volume", "Name": "pf-" + testSiteID + "_cache", "Destination": "/var/cache/pressfleet/fastcgi"},
				map[string]any{"Type": "volume", "Name": testSiteVolume, "Destination": "/var/www/wordpress/wp-content"},
			},
		}
	case strings.Contains(name, "-task-"):
		status := f.status
		if status == "" {
			status = "exited"
		}
		info = map[string]any{"State": map[string]any{"Status": status, "ExitCode": f.exitCode}}
	default:
		return nil, errNoSuchContainer
	}
	return json.Marshal([]any{info})
}

// created reports whether a task container was created.
func (f *taskDocker) created() bool { return f.createArgs != nil }

// newTaskProvisioner returns a provisioner on a fake docker CLI; edit
// adjusts its configuration.
func newTaskProvisioner(t *testing.T, taskImage string, edit ...func(*compose.ProvisionerConfig)) (*compose.Provisioner, *taskDocker, config.Layout) {
	t.Helper()
	layout := config.Layout{Root: t.TempDir()}
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	f := &taskDocker{t: t}
	cfg := compose.ProvisionerConfig{
		Layout:           layout,
		Secrets:          memSecrets{},
		Images:           imageMap{},
		MariaDBContainer: testMariaDB,
		Docker:           f.exec,
		DockerStream:     f.stream,
		TaskImage:        taskImage,
	}
	for _, e := range edit {
		e(&cfg)
	}
	prov, err := compose.NewProvisioner(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return prov, f, layout
}

func mkdir(t *testing.T, dir string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestRunTask(t *testing.T) {
	prov, f, layout := newTaskProvisioner(t, testTaskImage)
	f.stdout, f.stderr = `{"message_type":"summary"}`+"\n", "scanned 3 files\n"
	transfer := mkdir(t, filepath.Join(layout.TmpDir(), "backup-1"))
	repo := mkdir(t, filepath.Join(layout.Root, "backups", "repositories", "group-1"))

	res, err := prov.RunTask(context.Background(), testSiteID, provisioner.Task{
		Command:       []string{"restic", "backup", provisioner.TaskWPContentPath},
		Env:           map[string]string{"RESTIC_REPOSITORY": provisioner.TaskRepositoryPath},
		SecretEnv:     map[string]redact.Secret{"RESTIC_PASSWORD": testTaskSecret},
		WPContent:     provisioner.MountReadOnly,
		TransferDir:   transfer,
		RepositoryDir: repo,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Stdout != f.stdout || res.Stderr != f.stderr {
		t.Errorf("result = %+v", res)
	}

	args := strings.Join(f.createArgs, " ")
	for _, want := range []string{
		"--network pf-" + testSiteID,
		"--user " + testPHPUser,
		"--mount type=volume,source=" + testSiteVolume + ",target=/site/wp-content,readonly",
		"--mount type=bind,source=" + transfer + ",target=/transfer",
		"--group-add 60000 --mount type=bind,source=" + repo + ",target=/repository",
		"--env RESTIC_REPOSITORY=/repository",
		"--entrypoint=restic",
		"-- " + testTaskImage + " backup /site/wp-content",
	} {
		if !strings.Contains(args, want) {
			t.Errorf("create args lack %q:\n%s", want, args)
		}
	}
	// The secret reaches docker only through the env-file, which is private
	// and gone once the container exists.
	for _, c := range f.calls {
		if strings.Contains(c, testTaskSecret) {
			t.Errorf("secret in docker argv: %s", c)
		}
	}
	if f.envContent != "RESTIC_PASSWORD="+testTaskSecret+"\n" || f.envMode != 0o600 {
		t.Errorf("env-file = %q (mode %v)", f.envContent, f.envMode)
	}
	if !strings.HasPrefix(f.envFile, layout.TmpDir()+string(filepath.Separator)) {
		t.Errorf("env-file %s is not in the data directory's tmp/", f.envFile)
	}
	if _, err := os.Stat(f.envFile); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("env-file still exists: %v", err)
	}
	name := f.createArgs[slices.Index(f.createArgs, "--name")+1]
	if !strings.HasPrefix(name, "pf-"+testSiteID+"-task-") || !slices.Equal(f.removed, []string{name}) {
		t.Errorf("created %s, removed %q", name, f.removed)
	}
}

func TestRunTaskImage(t *testing.T) {
	for _, tc := range []struct {
		name, configured, task, want string
	}{
		{"configured", testTaskImage, "", testTaskImage},
		{"task overrides", testTaskImage, "other:1", "other:1"},
		{"task only", "", "other:1", "other:1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prov, f, _ := newTaskProvisioner(t, tc.configured)
			if _, err := prov.RunTask(context.Background(), testSiteID, provisioner.Task{Image: tc.task, Command: []string{"true"}}); err != nil {
				t.Fatal(err)
			}
			if i := slices.Index(f.createArgs, "--"); i < 0 || f.createArgs[i+1] != tc.want {
				t.Errorf("create args = %q, want image %s", f.createArgs, tc.want)
			}
			if slices.ContainsFunc(f.createArgs, func(a string) bool { return strings.HasPrefix(a, "type=volume") }) {
				t.Errorf("MountNone task mounts wp-content: %q", f.createArgs)
			}
			if slices.Contains(f.createArgs, "--group-add") {
				t.Errorf("task without a repository joins the repository group: %q", f.createArgs)
			}
		})
	}
}

func TestRunTaskBackupNetwork(t *testing.T) {
	const network = "pressfleet_backup"
	for _, tc := range []struct {
		name       string
		configured string
		join       bool
		connectErr error
		wantErr    bool
		connected  bool
	}{
		{name: "joins", configured: network, join: true, connected: true},
		{name: "not asked", configured: network},
		{name: "no network configured", join: true},
		{name: "network missing", configured: network, join: true, connected: true,
			connectErr: &compose.CommandError{Stderr: "Error response from daemon: network " + network + " not found", Err: errors.New("exit status 1")}},
		{name: "connect fails", configured: network, join: true, connected: true, wantErr: true,
			connectErr: &compose.CommandError{Stderr: "Error response from daemon: permission denied", Err: errors.New("exit status 1")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prov, f, _ := newTaskProvisioner(t, testTaskImage, func(c *compose.ProvisionerConfig) { c.BackupNetwork = tc.configured })
			f.connectErr = tc.connectErr
			_, err := prov.RunTask(context.Background(), testSiteID, provisioner.Task{Command: []string{"restic", "snapshots"}, BackupNetwork: tc.join})
			if (err != nil) != tc.wantErr {
				t.Fatalf("RunTask = %v, want error %v", err, tc.wantErr)
			}
			name := f.createArgs[slices.Index(f.createArgs, "--name")+1]
			want := "network connect --gw-priority -1 " + network + " " + name
			connectAt := slices.Index(f.calls, want)
			if (connectAt >= 0) != tc.connected {
				t.Errorf("calls = %q; want connected %v", f.calls, tc.connected)
			}
			// Connected before it starts, and removed whatever happened.
			startAt := slices.IndexFunc(f.calls, func(c string) bool { return strings.HasPrefix(c, "container start") })
			if tc.connected && !tc.wantErr && (startAt < 0 || startAt < connectAt) {
				t.Errorf("calls = %q; want connect before start", f.calls)
			}
			if tc.wantErr && startAt >= 0 {
				t.Errorf("started after connecting failed: %q", f.calls)
			}
			if !slices.Equal(f.removed, []string{name}) {
				t.Errorf("removed %q, want %s", f.removed, name)
			}
		})
	}
}

func TestRunTaskExitCode(t *testing.T) {
	prov, f, _ := newTaskProvisioner(t, testTaskImage)
	f.exitCode, f.stdout, f.stderr = 3, "partial\n", "Fatal: repository does not exist\n"
	f.startErr = &compose.CommandError{Err: exitError(3)}

	_, err := prov.RunTask(context.Background(), testSiteID, provisioner.Task{Command: []string{"restic", "snapshots"}})
	var ee *provisioner.ExecError
	if !errors.As(err, &ee) || ee.ExitCode != 3 || ee.Stdout != "partial\n" || !strings.Contains(ee.Stderr, "does not exist") {
		t.Fatalf("RunTask error = %v (%+v)", err, ee)
	}
	if len(f.removed) != 1 {
		t.Errorf("removed %q, want the task container", f.removed)
	}
}

func TestRunTaskStartFailure(t *testing.T) {
	prov, f, _ := newTaskProvisioner(t, testTaskImage)
	f.status, f.stderr = "created", "OCI runtime create failed"
	f.startErr = &compose.CommandError{Err: exitError(1)}

	_, err := prov.RunTask(context.Background(), testSiteID, provisioner.Task{Command: []string{"restic"}})
	var ee *provisioner.ExecError
	if err == nil || errors.As(err, &ee) || !errors.Is(err, compose.ErrCommandFailed) || !strings.Contains(err.Error(), "OCI runtime") {
		t.Fatalf("RunTask error = %v, want a docker failure with its stderr", err)
	}
	if len(f.removed) != 1 {
		t.Errorf("removed %q, want the task container", f.removed)
	}
}

func TestRunTaskTimeout(t *testing.T) {
	prov, f, _ := newTaskProvisioner(t, testTaskImage)
	f.block = true
	_, err := prov.RunTask(context.Background(), testSiteID, provisioner.Task{Command: []string{"sleep", "600"}, Timeout: 50 * time.Millisecond})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("RunTask error = %v, want a deadline error", err)
	}
	if len(f.removed) != 1 {
		t.Errorf("removed %q, want the task container even after the deadline", f.removed)
	}
}

func TestRunTaskSiteNotFound(t *testing.T) {
	prov, f, _ := newTaskProvisioner(t, testTaskImage)
	f.noSite = true
	_, err := prov.RunTask(context.Background(), testSiteID, provisioner.Task{Command: []string{"restic"}})
	if !errors.Is(err, provisioner.ErrSiteNotFound) {
		t.Fatalf("RunTask error = %v, want ErrSiteNotFound", err)
	}
	if f.created() {
		t.Error("a task container was created for a missing site")
	}
}

func TestRunTaskInvalid(t *testing.T) {
	outside := t.TempDir()
	tests := []struct {
		name  string
		image string // configured task image
		task  func(l config.Layout) provisioner.Task
	}{
		{"no command", testTaskImage, func(config.Layout) provisioner.Task { return provisioner.Task{} }},
		{"empty command", testTaskImage, func(config.Layout) provisioner.Task { return provisioner.Task{Command: []string{""}} }},
		{"NUL in argument", testTaskImage, func(config.Layout) provisioner.Task { return provisioner.Task{Command: []string{"a", "b\x00"}} }},
		{"no image", "", func(config.Layout) provisioner.Task { return provisioner.Task{Command: []string{"restic"}} }},
		{"option as image", "", func(config.Layout) provisioner.Task {
			return provisioner.Task{Image: "--privileged", Command: []string{"restic"}}
		}},
		{"bad env name", testTaskImage, func(config.Layout) provisioner.Task {
			return provisioner.Task{Command: []string{"restic"}, Env: map[string]string{"A=B": "c"}}
		}},
		{"bad secret env name", testTaskImage, func(config.Layout) provisioner.Task {
			return provisioner.Task{Command: []string{"restic"}, SecretEnv: map[string]redact.Secret{"1X": "c"}}
		}},
		{"secret with line break", testTaskImage, func(config.Layout) provisioner.Task {
			return provisioner.Task{Command: []string{"restic"}, SecretEnv: map[string]redact.Secret{"P": "a\nB=c"}}
		}},
		{"env both secret and not", testTaskImage, func(config.Layout) provisioner.Task {
			return provisioner.Task{Command: []string{"restic"}, Env: map[string]string{"P": "a"}, SecretEnv: map[string]redact.Secret{"P": "b"}}
		}},
		{"unknown mount mode", testTaskImage, func(config.Layout) provisioner.Task {
			return provisioner.Task{Command: []string{"restic"}, WPContent: 7}
		}},
		{"negative timeout", testTaskImage, func(config.Layout) provisioner.Task {
			return provisioner.Task{Command: []string{"restic"}, Timeout: -time.Second}
		}},
		{"transfer dir outside tmp", testTaskImage, func(l config.Layout) provisioner.Task {
			return provisioner.Task{Command: []string{"restic"}, TransferDir: l.SitesDir()}
		}},
		{"transfer dir is tmp", testTaskImage, func(l config.Layout) provisioner.Task {
			return provisioner.Task{Command: []string{"restic"}, TransferDir: l.TmpDir()}
		}},
		{"transfer dir outside data directory", testTaskImage, func(config.Layout) provisioner.Task {
			return provisioner.Task{Command: []string{"restic"}, TransferDir: outside}
		}},
		{"repository outside data directory", testTaskImage, func(config.Layout) provisioner.Task {
			return provisioner.Task{Command: []string{"restic"}, RepositoryDir: outside}
		}},
		{"repository is the data directory", testTaskImage, func(l config.Layout) provisioner.Task {
			return provisioner.Task{Command: []string{"restic"}, RepositoryDir: l.Root}
		}},
		{"relative repository", testTaskImage, func(config.Layout) provisioner.Task {
			return provisioner.Task{Command: []string{"restic"}, RepositoryDir: "tmp"}
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			prov, f, layout := newTaskProvisioner(t, tc.image)
			_, err := prov.RunTask(context.Background(), testSiteID, tc.task(layout))
			if !errors.Is(err, compose.ErrInvalidTask) {
				t.Fatalf("RunTask error = %v, want ErrInvalidTask", err)
			}
			if len(f.calls) != 0 {
				t.Errorf("docker calls for an invalid task: %q", f.calls)
			}
			if strings.Contains(err.Error(), "a\nB=c") {
				t.Errorf("error reveals a secret: %v", err)
			}
		})
	}
}

// TestRunTaskAdapter checks RunTask through the plain ExecFunc that most
// tests inject, which only reports stderr on failure.
func TestRunTaskAdapter(t *testing.T) {
	layout := config.Layout{Root: t.TempDir()}
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	f := &taskDocker{t: t}
	exec := func(ctx context.Context, stdin io.Reader, args ...string) ([]byte, error) {
		if args[0] == "container" && args[1] == "start" {
			f.calls = append(f.calls, strings.Join(args, " "))
			return []byte("hello\n"), nil
		}
		return f.exec(ctx, stdin, args...)
	}
	prov, err := compose.NewProvisioner(compose.ProvisionerConfig{
		Layout: layout, Secrets: memSecrets{}, Images: imageMap{}, MariaDBContainer: testMariaDB,
		Docker: exec, TaskImage: testTaskImage,
	})
	if err != nil {
		t.Fatal(err)
	}
	res, err := prov.RunTask(context.Background(), testSiteID, provisioner.Task{Command: []string{"echo", "hello"}})
	if err != nil || res.Stdout != "hello\n" {
		t.Fatalf("RunTask = %+v, %v", res, err)
	}
}
