package compose_test

import (
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/config"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner/compose"
	"github.com/rchouinard/pressfleet/control-plane/internal/redact"
)

const testGroupID = "01hzzzzzzzzzzzzzzzzzzzzzzz"

// optionValues returns the values of a repeated option in the create argv.
func optionValues(args []string, name string) []string {
	var out []string
	for i, a := range args {
		if a == name && i+1 < len(args) {
			out = append(out, args[i+1])
		}
	}
	return out
}

func TestRunRepositoryTaskLocal(t *testing.T) {
	prov, f, layout := newTaskProvisioner(t, testTaskImage)
	f.stdout = `{"message_type":"summary"}` + "\n"
	repo := mkdir(t, layout.Root+"/backups/repositories/group-"+testGroupID)
	transfer := mkdir(t, layout.TmpDir()+"/state-1")
	task := provisioner.Task{
		Command:       []string{"restic", "--no-cache", "snapshots", "--json"},
		Env:           map[string]string{"RESTIC_REPOSITORY": provisioner.TaskRepositoryPath, "RESTIC_PASSWORD_FILE": provisioner.TaskSecretPath("restic-password")},
		SecretFiles:   map[string]redact.Secret{"restic-password": testTaskSecret},
		RepositoryDir: repo,
		TransferDir:   transfer,
	}
	res, err := prov.RunRepositoryTask(context.Background(), testGroupID, task)
	if err != nil {
		t.Fatal(err)
	}
	if res.Stdout != f.stdout {
		t.Errorf("stdout = %q", res.Stdout)
	}

	args := strings.Join(f.createArgs, " ")
	for _, want := range []string{
		"--label pressfleet.managed=true --label pressfleet.repository=" + testGroupID + " --label pressfleet.task=true",
		"--network none",
		"--user 60000:60000",
		"--read-only --tmpfs /tmp --cap-drop ALL --security-opt no-new-privileges:true --init --log-driver none",
		"--memory 512m --memory-swap 512m",
		"--mount type=bind,source=" + transfer + ",target=/transfer",
		"--mount type=bind,source=" + repo + ",target=/repository",
		"--env RESTIC_PASSWORD_FILE=/run/pressfleet/secrets/restic-password",
		"--entrypoint=restic",
		"-- " + testTaskImage + " --no-cache snapshots --json",
	} {
		if !strings.Contains(args, want) {
			t.Errorf("create args lack %q:\n%s", want, args)
		}
	}
	for _, unwanted := range []string{"pressfleet.site-id", "--group-add", "wp-content", "--env-file"} {
		if strings.Contains(args, unwanted) {
			t.Errorf("create args hold %q:\n%s", unwanted, args)
		}
	}
	for _, c := range f.calls {
		switch {
		case strings.Contains(c, testTaskSecret):
			t.Errorf("secret in docker argv: %s", c)
		case strings.HasPrefix(c, "network "), strings.HasPrefix(c, "compose "):
			t.Errorf("a local repository task touched networks or the site's project: %s", c)
		}
	}
	if f.secrets["restic-password"] != testTaskSecret {
		t.Errorf("secret files at start = %q", f.secrets)
	}
	if _, err := os.Stat(f.secretsDir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("secrets directory still exists: %v", err)
	}
	name := optionValues(f.createArgs, "--name")[0]
	if !strings.HasPrefix(name, "pf-group-"+testGroupID+"-task-") || !slices.Equal(f.removed, []string{name}) {
		t.Errorf("created %s, removed %q", name, f.removed)
	}
}

// Every task container has a host name of its own: restic records it in its
// locks, and treats those of its own host as stale when their process is gone.
func TestRunRepositoryTaskHostnames(t *testing.T) {
	prov, f, layout := newTaskProvisioner(t, testTaskImage)
	repo := mkdir(t, layout.Root+"/backups/repositories/g")
	hosts := map[string]bool{}
	for range 5 {
		if _, err := prov.RunRepositoryTask(context.Background(), testGroupID, provisioner.Task{Command: []string{"restic", "check"}, RepositoryDir: repo}); err != nil {
			t.Fatal(err)
		}
		h := optionValues(f.createArgs, "--hostname")
		if len(h) != 1 || h[0] == "" || len(h[0]) > 63 {
			t.Fatalf("--hostname = %q", h)
		}
		if hosts[h[0]] {
			t.Errorf("host name %s used twice", h[0])
		}
		hosts[h[0]] = true
	}
}

func TestRunRepositoryTaskS3(t *testing.T) {
	const backup = "pressfleet_backup"
	prov, f, _ := newTaskProvisioner(t, testTaskImage, func(c *compose.ProvisionerConfig) { c.BackupNetwork = backup })
	_, err := prov.RunRepositoryTask(context.Background(), testGroupID, provisioner.Task{
		Command:       []string{"restic", "init"},
		Env:           map[string]string{"RESTIC_REPOSITORY": "s3:http://s3:9000/bucket/" + testGroupID},
		SecretFiles:   map[string]redact.Secret{"aws-credentials": "[default]\naws_secret_access_key = " + testTaskSecret + "\n"},
		BackupNetwork: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	name := optionValues(f.createArgs, "--name")[0]
	create := slices.IndexFunc(f.calls, func(c string) bool { return strings.HasPrefix(c, "container create") })
	network := slices.IndexFunc(f.calls, func(c string) bool { return strings.HasPrefix(c, "network create") })
	connect := slices.Index(f.calls, "network connect --gw-priority -1 "+backup+" "+name)
	start := slices.IndexFunc(f.calls, func(c string) bool { return strings.HasPrefix(c, "container start") })
	if network < 0 || create < network || connect < create || start < connect {
		t.Fatalf("calls = %q; want network create, container create, network connect, start", f.calls)
	}
	// A plain bridge for repository tasks, labeled, that keeps them from
	// reaching each other.
	for _, want := range []string{"--driver bridge", "--label pressfleet.managed=true", "enable_icc=false", " pf_repository_egress"} {
		if !strings.Contains(f.calls[network], want) {
			t.Errorf("%q lacks %q", f.calls[network], want)
		}
	}
	if strings.Contains(f.calls[network], "--internal") {
		t.Errorf("the egress network is internal: %q", f.calls[network])
	}
	args := strings.Join(f.createArgs, " ")
	if !strings.Contains(args, "--network pf_repository_egress") || strings.Contains(args, "--network none") {
		t.Errorf("create args = %s", args)
	}
	if !strings.Contains(args, "--user 60000:60000") || strings.Contains(args, "target=/repository") {
		t.Errorf("create args = %s", args)
	}
	for _, c := range f.calls {
		if strings.Contains(c, testTaskSecret) {
			t.Errorf("secret in docker argv: %s", c)
		}
	}
	// Only a site task's network is a site's.
	if strings.Contains(strings.Join(f.calls, "\n"), "pf-"+testSiteID) {
		t.Errorf("the repository task touched a site: %q", f.calls)
	}
}

func TestRunRepositoryTaskNetworkCreate(t *testing.T) {
	for _, tc := range []struct {
		name    string
		err     error
		wantErr bool
	}{
		{"exists", &compose.CommandError{Stderr: "Error response from daemon: network with name pf_repository_egress already exists", Err: errors.New("exit status 1")}, false},
		{"fails", &compose.CommandError{Stderr: "Error response from daemon: all predefined address pools have been fully subnetted", Err: errors.New("exit status 1")}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prov, f, _ := newTaskProvisioner(t, testTaskImage)
			f.networkErr = tc.err
			_, err := prov.RunRepositoryTask(context.Background(), testGroupID, provisioner.Task{Command: []string{"restic", "init"}, BackupNetwork: true})
			if (err != nil) != tc.wantErr {
				t.Fatalf("RunRepositoryTask = %v, want error %v", err, tc.wantErr)
			}
			if tc.wantErr && f.created() {
				t.Error("a container was created without its network")
			}
		})
	}
}

func TestRunRepositoryTaskState(t *testing.T) {
	prov, f, layout := newTaskProvisioner(t, testTaskImage)
	repo := mkdir(t, layout.Root+"/backups/repositories/state")
	if _, err := prov.RunRepositoryTask(context.Background(), provisioner.StateRepository, provisioner.Task{Command: []string{"restic", "check"}, RepositoryDir: repo}); err != nil {
		t.Fatal(err)
	}
	name := optionValues(f.createArgs, "--name")[0]
	if !strings.HasPrefix(name, "pf-state-task-") {
		t.Errorf("container name = %s", name)
	}
	if l := optionValues(f.createArgs, "--label"); !slices.Contains(l, "pressfleet.repository=state") {
		t.Errorf("labels = %q", l)
	}
}

func TestRunRepositoryTaskMemory(t *testing.T) {
	for _, tc := range []struct {
		name       string
		configured int
		task       int
		want       string
	}{
		{"default", 0, 0, "512m"},
		{"configured", 768, 0, "768m"},
		{"task's own", 768, 256, "256m"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prov, f, _ := newTaskProvisioner(t, testTaskImage, func(c *compose.ProvisionerConfig) { c.TaskMemoryMB = tc.configured })
			if _, err := prov.RunRepositoryTask(context.Background(), testGroupID, provisioner.Task{Command: []string{"restic", "init"}, BackupNetwork: true, MemoryLimitMB: tc.task}); err != nil {
				t.Fatal(err)
			}
			if m, s := optionValues(f.createArgs, "--memory"), optionValues(f.createArgs, "--memory-swap"); !slices.Equal(m, []string{tc.want}) || !slices.Equal(s, m) {
				t.Errorf("--memory %q --memory-swap %q, want %s for both", m, s, tc.want)
			}
		})
	}
}

// Site tasks have the limit too.
func TestRunTaskMemory(t *testing.T) {
	for _, tc := range []struct {
		name       string
		configured int
		task       int
		want       string
	}{
		{"default", 0, 0, "512m"},
		{"configured", 1024, 0, "1024m"},
		{"task's own", 1024, 300, "300m"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prov, f, _ := newTaskProvisioner(t, testTaskImage, func(c *compose.ProvisionerConfig) { c.TaskMemoryMB = tc.configured })
			if _, err := prov.RunTask(context.Background(), testSiteID, provisioner.Task{Command: []string{"restic", "backup"}, MemoryLimitMB: tc.task}); err != nil {
				t.Fatal(err)
			}
			if m, s := optionValues(f.createArgs, "--memory"), optionValues(f.createArgs, "--memory-swap"); !slices.Equal(m, []string{tc.want}) || !slices.Equal(s, m) {
				t.Errorf("--memory %q --memory-swap %q, want %s for both", m, s, tc.want)
			}
		})
	}
}

func TestTaskMemoryInvalid(t *testing.T) {
	for _, mb := range []int{-1, config.MinBackupTaskMemoryMB - 1, config.MaxBackupTaskMemoryMB + 1} {
		prov, f, _ := newTaskProvisioner(t, testTaskImage)
		task := provisioner.Task{Command: []string{"restic"}, BackupNetwork: true, MemoryLimitMB: mb}
		if _, err := prov.RunTask(context.Background(), testSiteID, task); !errors.Is(err, compose.ErrInvalidTask) {
			t.Errorf("RunTask with %d MiB: %v, want ErrInvalidTask", mb, err)
		}
		if _, err := prov.RunRepositoryTask(context.Background(), testGroupID, task); !errors.Is(err, compose.ErrInvalidTask) {
			t.Errorf("RunRepositoryTask with %d MiB: %v, want ErrInvalidTask", mb, err)
		}
		if len(f.calls) != 0 {
			t.Errorf("docker calls for an invalid task: %q", f.calls)
		}
	}
	layout := config.Layout{Root: t.TempDir()}
	for _, mb := range []int{-1, config.MinBackupTaskMemoryMB - 1, config.MaxBackupTaskMemoryMB + 1} {
		_, err := compose.NewProvisioner(compose.ProvisionerConfig{Layout: layout, Secrets: memSecrets{}, Images: imageMap{}, MariaDBContainer: testMariaDB, TaskMemoryMB: mb})
		if err == nil {
			t.Errorf("NewProvisioner accepted a task memory limit of %d MiB", mb)
		}
	}
}

func TestRunRepositoryTaskInvalid(t *testing.T) {
	tests := []struct {
		name       string
		repository string
		task       func(config.Layout) provisioner.Task
	}{
		{"wp-content", testGroupID, func(config.Layout) provisioner.Task {
			return provisioner.Task{Command: []string{"restic"}, BackupNetwork: true, WPContent: provisioner.MountReadOnly}
		}},
		{"no repository", testGroupID, func(config.Layout) provisioner.Task { return provisioner.Task{Command: []string{"restic"}} }},
		{"no command", testGroupID, func(config.Layout) provisioner.Task { return provisioner.Task{BackupNetwork: true} }},
		{"repository outside the data directory", testGroupID, func(config.Layout) provisioner.Task {
			return provisioner.Task{Command: []string{"restic"}, RepositoryDir: t.TempDir()}
		}},
		{"transfer directory outside tmp", testGroupID, func(l config.Layout) provisioner.Task {
			return provisioner.Task{Command: []string{"restic"}, BackupNetwork: true, TransferDir: l.SitesDir()}
		}},
		{"empty repository", "", func(config.Layout) provisioner.Task {
			return provisioner.Task{Command: []string{"restic"}, BackupNetwork: true}
		}},
		{"repository with a path", "../x", func(config.Layout) provisioner.Task {
			return provisioner.Task{Command: []string{"restic"}, BackupNetwork: true}
		}},
		{"repository with an uppercase letter", "Group", func(config.Layout) provisioner.Task {
			return provisioner.Task{Command: []string{"restic"}, BackupNetwork: true}
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			prov, f, layout := newTaskProvisioner(t, testTaskImage)
			_, err := prov.RunRepositoryTask(context.Background(), tc.repository, tc.task(layout))
			if !errors.Is(err, compose.ErrInvalidTask) {
				t.Fatalf("RunRepositoryTask error = %v, want ErrInvalidTask", err)
			}
			if len(f.calls) != 0 {
				t.Errorf("docker calls for an invalid task: %q", f.calls)
			}
		})
	}
}

// A repository task cleans up after every kind of failure: its container and
// its secrets are gone.
func TestRunRepositoryTaskCleanup(t *testing.T) {
	task := provisioner.Task{
		Command:       []string{"restic", "backup"},
		BackupNetwork: true,
		SecretFiles:   map[string]redact.Secret{"restic-password": testTaskSecret},
	}
	withTimeout := task
	withTimeout.Timeout = 50 * time.Millisecond
	for _, tc := range []struct {
		name  string
		setup func(*taskDocker)
		task  provisioner.Task
		check func(error) bool
	}{
		{"non-zero exit", func(f *taskDocker) { f.exitCode = 10 }, task, func(err error) bool {
			var ee *provisioner.ExecError
			return errors.As(err, &ee) && ee.ExitCode == 10
		}},
		{"start failure", func(f *taskDocker) { f.status, f.startErr = "created", &compose.CommandError{Err: exitError(1)} }, task, nil},
		{"connect failure", func(f *taskDocker) {
			f.connectErr = &compose.CommandError{Stderr: "permission denied", Err: exitError(1)}
		}, task, nil},
		{"create failure", func(f *taskDocker) { f.createErr = &compose.CommandError{Stderr: "no space", Err: exitError(1)} }, task, nil},
		{"timeout", func(f *taskDocker) { f.block = true }, withTimeout, func(err error) bool { return errors.Is(err, context.DeadlineExceeded) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prov, f, layout := newTaskProvisioner(t, testTaskImage, func(c *compose.ProvisionerConfig) { c.BackupNetwork = "pressfleet_backup" })
			tc.setup(f)
			_, err := prov.RunRepositoryTask(context.Background(), testGroupID, tc.task)
			if err == nil || tc.check != nil && !tc.check(err) {
				t.Fatalf("RunRepositoryTask error = %v", err)
			}
			if f.created() && len(f.removed) != 1 {
				t.Errorf("removed %q, want the task container", f.removed)
			}
			if entries, _ := os.ReadDir(layout.TmpDir()); len(entries) != 0 {
				t.Errorf("%d entries left behind in tmp/", len(entries))
			}
		})
	}

	t.Run("context canceled", func(t *testing.T) {
		prov, f, layout := newTaskProvisioner(t, testTaskImage)
		f.block = true
		ctx, cancel := context.WithCancel(context.Background())
		time.AfterFunc(50*time.Millisecond, cancel)
		_, err := prov.RunRepositoryTask(ctx, testGroupID, task)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("RunRepositoryTask error = %v, want context.Canceled", err)
		}
		if len(f.removed) != 1 {
			t.Errorf("removed %q, want the task container even after ctx ended", f.removed)
		}
		if entries, _ := os.ReadDir(layout.TmpDir()); len(entries) != 0 {
			t.Errorf("%d entries left behind in tmp/", len(entries))
		}
	})
}
