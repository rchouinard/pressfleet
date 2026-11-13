package repository

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
	"github.com/rchouinard/pressfleet/control-plane/internal/redact"
)

// errRepositoryMissing means restic found no repository at the location.
var errRepositoryMissing = errors.New("repository does not exist")

// restic exit codes (restic 0.17 and later) that callers branch on.
const (
	exitRepositoryMissing = 10
	exitLocked            = 11
	exitWrongPassword     = 12
	// exitKilled is a task killed with SIGKILL, as the kernel does to one
	// that exceeds its memory limit.
	exitKilled = 137
)

// KilledHint says what to do about a restic task killed with SIGKILL (exit
// status 137). restic prints nothing when it is killed, so without this the
// error names no cause. Site tasks (package restic) report it too.
const KilledHint = "killed, probably for exceeding the task's memory limit: raise backup.task_memory_mb"

// maxStderr bounds how much of restic's error output goes into an error.
const maxStderr = 2048

// DefaultMaxTasks is how many repository tasks a Manager runs at once when
// Options.MaxTasks is zero.
const DefaultMaxTasks = 4

// stdinName is the file in the transfer directory that holds the standard
// input of a task run with stdin.
const stdinName = "stdin"

// run runs restic against rec with its password and returns its standard
// output.
func (m *Manager) run(ctx context.Context, rec Repository, password redact.Secret, args ...string) ([]byte, error) {
	var stdout bytes.Buffer
	if err := m.runTo(ctx, rec, password, &stdout, args...); err != nil {
		return nil, err
	}
	return stdout.Bytes(), nil
}

// runTo runs restic against rec with its password, writing its standard
// output to stdout.
func (m *Manager) runTo(ctx context.Context, rec Repository, password redact.Secret, stdout io.Writer, args ...string) error {
	return m.runIO(ctx, rec, password, nil, stdout, args...)
}

// repositoryName is what a task is run for: the client group's ID, or
// provisioner.StateRepository.
func repositoryName(rec Repository) string {
	if rec.ClientGroupID == "" {
		return provisioner.StateRepository
	}
	return rec.ClientGroupID
}

// repositoryDir returns the directory of a local repository: the group's,
// or the state repository's.
func (m *Manager) repositoryDir(rec Repository) (string, error) {
	if rec.ClientGroupID == "" {
		return m.layout.StateBackupRepositoryDir(), nil
	}
	return m.localDir(rec)
}

// resticTask returns the task that runs restic against rec: the
// repository, its password, and for S3 the credentials, all as TaskAccess
// hands them to site tasks, so that secrets reach the container as files and
// never as environment variables or arguments (§6). The caller sets the
// command.
func (m *Manager) resticTask(rec Repository, password redact.Secret) (provisioner.Task, error) {
	a, err := m.taskAccess(rec, password)
	if err != nil {
		return provisioner.Task{}, err
	}
	return provisioner.Task{
		Image:         m.image,
		Env:           a.Env,
		SecretFiles:   a.SecretFiles,
		RepositoryDir: a.RepositoryDir,
		BackupNetwork: a.BackupNetwork,
	}, nil
}

// runIO runs restic against rec with its password in a repository task
// (provisioner.RepositoryTaskRunner), not in the control plane process:
// restic's memory grows with the repository, and every command pays for
// deriving the key (#178). stdin (nil for none) is staged in a transfer
// directory and connected by a shell redirection, since tasks have no
// standard input, and stdout receives the command's output as it is written,
// without the size limit of a task's result.
//
// Group-level commands run without a local cache: they are rare, and a
// cache would need a home of its own.
func (m *Manager) runIO(ctx context.Context, rec Repository, password redact.Secret, stdin io.Reader, stdout io.Writer, args ...string) error {
	sub := "restic"
	if len(args) > 0 {
		sub += " " + args[0]
	}
	task, err := m.resticTask(rec, password)
	if err != nil {
		return err
	}
	if rec.Backend == BackendLocal {
		if err := m.prepareRun(rec, task.RepositoryDir); err != nil {
			if errors.Is(err, errRepositoryMissing) {
				return fmt.Errorf("%s: %w: %s", sub, err, rec.Location)
			}
			return err
		}
	}
	task.Stdout = stdout
	task.Command = slices.Concat([]string{"restic", "--no-cache"}, args)
	if rec.Backend == BackendLocal && len(args) > 0 && args[0] == "init" {
		// What init creates belongs to the task's user, and only its owner
		// or root may change its mode. On Kubernetes the control plane
		// isn't root, so shareLocal can't give the files to the group
		// afterwards (#228): the task does, as shareLocal would. Not the
		// directory itself, which the control plane owns (prepareLocalDir
		// gave it the mode it needs) and the task may not chmod (#347).
		task.Command = slices.Concat([]string{"sh", "-c", `"$@" && find ` + provisioner.TaskRepositoryPath + ` -mindepth 1 -exec chmod g=u {} +`, "sh"}, task.Command)
	}
	if stdin != nil {
		dir, err := m.stageStdin(stdin)
		if err != nil {
			return fmt.Errorf("running %s: %w", sub, err)
		}
		defer func() { _ = os.RemoveAll(dir) }()
		task.TransferDir = dir
		// "$@" is restic and its arguments; the shell is replaced by them.
		task.Command = slices.Concat([]string{"sh", "-c", `exec "$@" < ` + provisioner.TaskTransferPath + "/" + stdinName, "sh"}, task.Command)
	}

	release, err := m.acquire(ctx)
	if err != nil {
		return fmt.Errorf("running %s: waiting for a free task slot: %w", sub, err)
	}
	defer release()
	_, err = m.tasks.RunRepositoryTask(ctx, repositoryName(rec), task)
	return resticError(sub, err)
}

// resticError maps the failure of a restic task the way the control plane
// always has: restic's exit codes 10, 11, and 12 become the sentinels
// callers branch on, with the tail and head of restic's own words.
func resticError(sub string, err error) error {
	if err == nil {
		return nil
	}
	var execErr *provisioner.ExecError
	if !errors.As(err, &execErr) {
		return fmt.Errorf("running %s: %w", sub, err)
	}
	msg := strings.TrimSpace(execErr.Stderr)
	if len(msg) > maxStderr {
		// Both ends: restic says what it was doing first and ends with
		// its fatal error.
		msg = msg[:maxStderr/2] + "\n...\n" + msg[len(msg)-maxStderr/2:]
	}
	var sentinel error
	switch execErr.ExitCode {
	case exitRepositoryMissing:
		sentinel = errRepositoryMissing
	case exitLocked:
		sentinel = ErrLocked
	case exitWrongPassword:
		sentinel = ErrWrongPassword
	}
	if sentinel != nil {
		return fmt.Errorf("%s: %w: %s", sub, sentinel, msg)
	}
	if execErr.ExitCode == exitKilled {
		msg += " (" + KilledHint + ")"
	}
	return fmt.Errorf("%s: exit status %d: %s", sub, execErr.ExitCode, strings.TrimSpace(msg))
}

// acquire waits for one of the Manager's task slots (Options.MaxTasks) and
// returns the function that gives it back.
func (m *Manager) acquire(ctx context.Context) (func(), error) {
	select {
	case m.slots <- struct{}{}:
		return func() { <-m.slots }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// stageStdin copies r into a new transfer directory and returns it. The
// caller removes it.
func (m *Manager) stageStdin(r io.Reader) (string, error) {
	tmp := m.layout.TmpDir()
	if err := os.MkdirAll(tmp, 0o700); err != nil {
		return "", fmt.Errorf("creating the transfer directory: %w", err)
	}
	dir, err := os.MkdirTemp(tmp, "restic-stdin-")
	if err != nil {
		return "", fmt.Errorf("creating the transfer directory: %w", err)
	}
	if err := writeFrom(filepath.Join(dir, stdinName), func(w io.Writer) error {
		_, err := io.Copy(w, r)
		return err
	}); err != nil {
		_ = os.RemoveAll(dir)
		return "", fmt.Errorf("staging standard input: %w", err)
	}
	return dir, nil
}

// prepareRun readies a local repository for a task, which runs as the
// repository group and not as root. A missing repository directory is
// errRepositoryMissing, as restic reports it, since there is nothing to
// bind-mount; creating it is initialize's job. A repository whose files
// don't yet belong to the group, such as the state repository an earlier
// release created from the control plane process, is shared with it.
func (m *Manager) prepareRun(rec Repository, dir string) error {
	if rec.Backend != BackendLocal {
		return nil
	}
	if _, err := os.Stat(filepath.Join(dir, "config")); err != nil {
		if _, derr := os.Stat(dir); errors.Is(derr, fs.ErrNotExist) {
			return errRepositoryMissing
		}
		// A directory without a config: init is about to fill it, or the
		// command will report the missing repository itself.
		return nil
	}
	if isShared(dir) {
		return nil
	}
	return m.shareLocal(dir)
}
