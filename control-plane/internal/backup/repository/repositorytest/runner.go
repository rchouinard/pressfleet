// Package repositorytest provides a provisioner.RepositoryTaskRunner for
// tests that need real restic without Docker.
package repositorytest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
)

// Call is one repository task a LocalRunner ran.
type Call struct {
	// Repository is the client group ID or provisioner.StateRepository.
	Repository string
	// Task is the task as it was given, before any path was rewritten.
	Task provisioner.Task
}

// LocalRunner runs repository tasks as processes of the test, in place of a
// container: the task's command runs with the mounts replaced by the
// directories they would be bind-mounted from (provisioner.TaskRepositoryPath
// by RepositoryDir, TaskTransferPath by TransferDir, TaskSecretsPath by a
// temporary directory holding SecretFiles), and with nothing of the test's
// environment but PATH. Unlike a container, it runs as the test's own user,
// so the ownership a repository task has (provisioner.RepositoryGID) is not
// exercised; compose's Docker tests cover that.
type LocalRunner struct {
	// Restic is the binary run for a command that starts with "restic"; empty
	// means the first "restic" on PATH.
	Restic string

	mu    sync.Mutex
	calls []Call
}

var _ provisioner.RepositoryTaskRunner = (*LocalRunner)(nil)

// ResticBinary returns the restic on PATH, which mise.toml pins, so
// `mise exec -- make test` has it.
func ResticBinary(t testing.TB) string {
	t.Helper()
	p, err := exec.LookPath("restic")
	if err != nil {
		t.Fatal("restic is not on PATH; run the tests with `mise exec -- make test` after `mise install`")
	}
	return p
}

// NewLocalRunner returns a LocalRunner for the restic on PATH.
func NewLocalRunner(t testing.TB) *LocalRunner {
	t.Helper()
	return &LocalRunner{Restic: ResticBinary(t)}
}

// Calls returns the tasks run so far, oldest first.
func (r *LocalRunner) Calls() []Call {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Call(nil), r.calls...)
}

// RunRepositoryTask implements provisioner.RepositoryTaskRunner.
func (r *LocalRunner) RunRepositoryTask(ctx context.Context, repository string, task provisioner.Task) (*provisioner.TaskResult, error) {
	r.mu.Lock()
	r.calls = append(r.calls, Call{Repository: repository, Task: task})
	r.mu.Unlock()

	if task.WPContent != provisioner.MountNone {
		return nil, errors.New("a repository task has no wp-content")
	}
	if task.RepositoryDir == "" && !task.BackupNetwork {
		return nil, errors.New("a repository task needs RepositoryDir or BackupNetwork")
	}
	if len(task.Command) == 0 {
		return nil, errors.New("a task needs a command")
	}
	if task.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, task.Timeout)
		defer cancel()
	}

	secrets, err := os.MkdirTemp("", "repositorytest-secrets-")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(secrets) }()
	for name, content := range task.SecretFiles {
		if name != filepath.Base(name) {
			return nil, fmt.Errorf("secret file name %q is not a plain file name", name)
		}
		if err := os.WriteFile(filepath.Join(secrets, name), []byte(content.Reveal()), 0o600); err != nil {
			return nil, err
		}
	}
	rw := rewriter{
		provisioner.TaskRepositoryPath: task.RepositoryDir,
		provisioner.TaskTransferPath:   task.TransferDir,
		provisioner.TaskSecretsPath:    secrets,
	}

	argv := make([]string, len(task.Command))
	for i, a := range task.Command {
		argv[i] = rw.rewrite(a)
	}
	if argv[0] == "restic" && r.Restic != "" {
		argv[0] = r.Restic
	}
	home, err := os.MkdirTemp("", "repositorytest-home-")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(home) }()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...) //nolint:gosec // G204: a test's own command
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home}
	for k, v := range task.Env {
		cmd.Env = append(cmd.Env, k+"="+rw.rewrite(v))
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if task.Stdout != nil {
		cmd.Stdout = task.Stdout
	}
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && ctx.Err() == nil {
			return nil, fmt.Errorf("run task on repository %s: %w", repository,
				&provisioner.ExecError{ExitCode: exitErr.ExitCode(), Stdout: stdout.String(), Stderr: stderr.String()})
		}
		if ctx.Err() != nil {
			err = ctx.Err()
		}
		return nil, fmt.Errorf("run task on repository %s: %w", repository, err)
	}
	return &provisioner.TaskResult{Stdout: stdout.String(), Stderr: stderr.String()}, nil
}

// rewriter replaces mount points in paths and shell scripts with the
// directories behind them.
type rewriter map[string]string

func (rw rewriter) rewrite(s string) string {
	for mount, dir := range rw {
		if dir == "" {
			continue
		}
		// The mount point as a whole path: at the start or after a space,
		// quote, or equals sign, and followed by a slash, the end, or a
		// space or quote. A real path that merely contains it stays.
		re := regexp.MustCompile(`(^|[\s='":])` + regexp.QuoteMeta(mount) + `($|[/\s'"])`)
		s = re.ReplaceAllStringFunc(s, func(m string) string {
			return strings.Replace(m, mount, dir, 1)
		})
	}
	return s
}
