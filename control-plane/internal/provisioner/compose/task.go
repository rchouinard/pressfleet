package compose

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/config"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
	"github.com/rchouinard/pressfleet/control-plane/internal/redact"
)

// ErrInvalidTask is returned by RunTask for a task it can't run as given.
var ErrInvalidTask = errors.New("compose: invalid task")

const (
	// taskMaxOutput bounds the standard output RunTask keeps; more is
	// reported with ErrOutputTruncated. Tasks print summaries (restic
	// --json), not data, which goes through the transfer directory.
	taskMaxOutput = 8 << 20
	// taskMaxStderr bounds the standard error RunTask keeps; the end is kept.
	taskMaxStderr = 64 << 10
	// taskCleanupTimeout bounds removing a task's container, which happens
	// even after ctx has ended.
	taskCleanupTimeout = time.Minute
	// taskWaitDelay bounds the wait for the docker CLI's output pipes after
	// ctx kills it.
	taskWaitDelay = 5 * time.Second
	// siteWPContentPath is where the site's php container mounts wp-content
	// (templates/compose/site.yaml.tmpl); RunTask finds the volume there.
	siteWPContentPath = "/var/www/wordpress/wp-content"
	// taskHome is HOME in a task: restic keeps its cache under it, and the
	// task's root filesystem is read-only apart from this tmpfs.
	taskHome = "/tmp"
)

// Labels on task containers, so leftovers of a crashed control plane can be
// found (docker ps --filter label=pressfleet.task).
const (
	taskLabel       = "pressfleet.task"
	managedLabel    = "pressfleet.managed"
	siteIDLabel     = "pressfleet.site-id"
	taskNamePattern = "pf-%s-task-%s"
)

var (
	envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	// volumeName matches the names Docker gives volumes, which may appear in
	// a --mount value unquoted.
	volumeName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)
	// secretFileName is what Task.SecretFiles may be named: a plain file name.
	secretFileName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)
)

// StreamExecFunc runs the docker CLI with args, writing its standard output
// and standard error to stdout and stderr. Failures are reported as
// *CommandError, whose Stderr the caller already has from stderr.
type StreamExecFunc func(ctx context.Context, stdout, stderr io.Writer, args ...string) error

// DockerStreamExec runs the docker binary found on PATH. When ctx ends the
// CLI is killed, and the wait for its output pipes is bounded.
func DockerStreamExec(ctx context.Context, stdout, stderr io.Writer, args ...string) error {
	// Arguments are passed directly to docker, never through a shell.
	cmd := exec.CommandContext(ctx, "docker", args...) //nolint:gosec // G204: running docker with caller-built arguments is the point
	cmd.Stdout, cmd.Stderr = stdout, stderr
	cmd.WaitDelay = taskWaitDelay
	if err := cmd.Run(); err != nil {
		return &CommandError{Args: args, Err: err}
	}
	return nil
}

// streamAdapter runs a StreamExecFunc's commands through an ExecFunc, which
// only reports standard error when the command fails. Tests that inject
// ProvisionerConfig.Docker get it.
func streamAdapter(docker ExecFunc) StreamExecFunc {
	return func(ctx context.Context, stdout, stderr io.Writer, args ...string) error {
		out, err := docker(ctx, nil, args...)
		_, _ = stdout.Write(out)
		if ce := (*CommandError)(nil); errors.As(err, &ce) {
			_, _ = io.WriteString(stderr, ce.Stderr)
		}
		return err
	}
}

// RunTask runs task in a one-off container with access to the site's data
// and waits for it to finish (provisioner.TaskRunner).
//
// The container joins the site's network, so it reaches the platform
// services under the same aliases as the site, and, with Task.BackupNetwork,
// the platform's backup network (ProvisionerConfig.BackupNetwork), where a
// self-hosted S3 service may live. It runs as the site's UID
// and GID (read from its php container), so it reads and writes wp-content
// as the site does. Its root filesystem is read-only, with a tmpfs at /tmp
// (also HOME), all capabilities dropped, and no-new-privileges. The site's
// wp-content volume, TransferDir, and RepositoryDir are mounted at the
// provisioner.Task*Path paths. The site may be running or stopped.
//
// TransferDir must be a directory below config.Layout.TmpDir, and
// RepositoryDir one below the data directory; both are bind-mounted from
// the host by the same path. When the control plane runs as root, RunTask
// hands TransferDir and everything in it to the site's UID, so the task can
// read what the control plane put there and write its own files; the
// control plane, as root, can read them afterwards. RepositoryDir is left
// as it is: it belongs to provisioner.RepositoryGID, which a task with a
// repository gets as a supplementary group.
//
// Task.SecretFiles are written to a directory below config.Layout.TmpDir
// (mode 0700, files 0400, all owned by the site's UID when the control plane
// runs as root) and bind-mounted read-only at provisioner.TaskSecretsPath.
// Nothing secret reaches the docker CLI's arguments, the container's
// environment, or its configuration, which the socket proxy lets Traefik
// read (docker inspect shows the mount's source directory, not its
// contents). The directory is removed when the task ends, also when it fails
// or ctx ends; RemoveStaleTaskSecrets removes what a crash left.
//
// The task's container is removed afterwards whatever happened, including
// when ctx ends or Timeout passes, which kill the task. A non-zero exit
// yields a *provisioner.ExecError; standard output beyond 8 MiB is dropped
// and reported with ErrOutputTruncated, together with the result, unless
// Task.Stdout takes it.
func (p *Provisioner) RunTask(ctx context.Context, siteID string, task provisioner.Task) (*provisioner.TaskResult, error) {
	if err := validSiteID(siteID); err != nil {
		return nil, fmt.Errorf("run task: %w", err)
	}
	spec, err := p.taskSpec(task)
	if err != nil {
		return nil, fmt.Errorf("run task on site %s: %w", siteID, err)
	}
	if task.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, task.Timeout)
		defer cancel()
	}

	user, volume, err := p.taskSiteAccess(ctx, siteID)
	if err != nil {
		return nil, fmt.Errorf("run task: %w", err)
	}
	spec.siteID, spec.network, spec.user, spec.wpContentVolume = siteID, SiteProjectName(siteID), user, volume
	if err := p.attachShared(ctx, siteID); err != nil {
		return nil, fmt.Errorf("run task on site %s: %w", siteID, err)
	}
	if spec.transferDir != "" && os.Geteuid() == 0 {
		uid, _ := siteUID(user) // checked by taskSiteAccess
		if err := chownTree(spec.transferDir, uid); err != nil {
			return nil, fmt.Errorf("run task on site %s: %w", siteID, err)
		}
	}
	name, err := taskContainerName(siteID)
	if err != nil {
		return nil, fmt.Errorf("run task on site %s: %w", siteID, err)
	}
	spec.name = name
	if len(task.SecretFiles) > 0 {
		// Registered before the container's removal, so it runs after it:
		// the secrets outlive the task's last read.
		defer func() { p.removeTaskSecrets(ctx, siteID, spec.secretsDir) }()
		if spec.secretsDir, err = p.writeTaskSecrets(task.SecretFiles, user); err != nil {
			return nil, fmt.Errorf("run task on site %s: %w", siteID, err)
		}
	}

	start := time.Now()
	defer p.removeTask(ctx, siteID, name)
	if err := p.createTask(ctx, spec, task); err != nil {
		return nil, fmt.Errorf("run task on site %s: %w", siteID, err)
	}
	if task.BackupNetwork {
		if err := p.connectBackupNetwork(ctx, siteID, name); err != nil {
			return nil, fmt.Errorf("run task on site %s: %w", siteID, err)
		}
	}
	stdout := &headBuffer{max: taskMaxOutput + 1}
	stderr := &tailBuffer{max: taskMaxStderr}
	var out io.Writer = stdout
	if task.Stdout != nil {
		// Streamed: the result's Stdout stays empty. If the writer fails,
		// the CLI's writes fail and it exits; the container, still
		// running, is then reported as not run to completion and removed.
		out = task.Stdout
	}
	runErr := p.taskDocker(ctx, out, stderr, "container", "start", "--attach", "--", name)
	res, err := p.taskResult(ctx, siteID, name, stdout, stderr, runErr)
	if err != nil {
		p.log.WarnContext(ctx, "task failed", "site_id", siteID, "container", name,
			"command", task.Command[0], "duration", time.Since(start), "error", err)
		return nil, err
	}
	p.log.InfoContext(ctx, "task finished", "site_id", siteID, "container", name,
		"command", task.Command[0], "duration", time.Since(start))
	if len(stdout.buf) > taskMaxOutput {
		res.Stdout = res.Stdout[:taskMaxOutput]
		return res, fmt.Errorf("run task on site %s: %w to the first %d bytes", siteID, ErrOutputTruncated, taskMaxOutput)
	}
	return res, nil
}

// connectBackupNetwork connects a created task container to the platform's
// backup network. A platform without one (a project rendered before it
// existed, or none at all in development) is not an error: S3 services
// elsewhere are still reachable through the site network.
func (p *Provisioner) connectBackupNetwork(ctx context.Context, siteID, container string) error {
	if p.backupNetwork == "" {
		return nil
	}
	// --gw-priority -1 keeps the site network as the default gateway; the
	// backup network is internal and has no route out anyway.
	_, err := p.docker(ctx, nil, "network", "connect", "--gw-priority", "-1", p.backupNetwork, container)
	switch {
	case err == nil:
		return nil
	case stderrContains(err, "not found", "no such network"):
		p.log.WarnContext(ctx, "the platform's backup network doesn't exist; the task reaches S3 through the site network only",
			"site_id", siteID, "network", p.backupNetwork)
		return nil
	default:
		return fmt.Errorf("connecting the task to the backup network %s: %w", p.backupNetwork, err)
	}
}

// taskResult turns the attached run into RunTask's result. The container's
// own exit code decides, since `docker start --attach` exits non-zero both
// when the task does and when the CLI fails.
func (p *Provisioner) taskResult(ctx context.Context, siteID, name string, stdout *headBuffer, stderr *tailBuffer, runErr error) (*provisioner.TaskResult, error) {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, fmt.Errorf("run task on site %s: %w", siteID, ctxErr)
	}
	info, found, err := p.inspectContainer(ctx, name)
	switch {
	case err != nil:
		return nil, fmt.Errorf("run task on site %s: %w", siteID, err)
	case !found || info.State.Status != "exited":
		if runErr == nil {
			runErr = fmt.Errorf("%w: task container did not run to completion", ErrCommandFailed)
		}
		return nil, fmt.Errorf("run task on site %s: %w: %s", siteID, runErr, strings.TrimSpace(stderr.String()))
	}
	res := &provisioner.TaskResult{Stdout: string(stdout.buf), Stderr: stderr.String()}
	if info.State.ExitCode != 0 {
		out := res.Stdout
		if len(out) > taskMaxOutput {
			out = out[:taskMaxOutput]
		}
		return nil, fmt.Errorf("run task on site %s: %w", siteID,
			&provisioner.ExecError{ExitCode: info.State.ExitCode, Stdout: out, Stderr: res.Stderr})
	}
	return res, nil
}

// taskSiteAccess returns the site's user ("uid:gid") and wp-content volume,
// from its php container, which exists whether or not the site is running.
func (p *Provisioner) taskSiteAccess(ctx context.Context, siteID string) (user, volume string, err error) {
	containers, err := p.project(siteID).Ps(ctx)
	if err != nil {
		return "", "", err
	}
	if len(containers) == 0 {
		return "", "", fmt.Errorf("site %s: %w", siteID, provisioner.ErrSiteNotFound)
	}
	php, ok := findService(containers, provisioner.ComponentPHP)
	if !ok {
		return "", "", fmt.Errorf("site %s: php container is missing", siteID)
	}
	info, found, err := p.inspectContainer(ctx, php.Name)
	if err != nil {
		return "", "", err
	}
	if !found {
		return "", "", fmt.Errorf("site %s: %w", siteID, provisioner.ErrSiteNotFound)
	}
	if _, err := siteUID(info.Config.User); err != nil {
		return "", "", fmt.Errorf("site %s: %w", siteID, err)
	}
	for _, m := range info.Mounts {
		if m.Type == "volume" && m.Destination == siteWPContentPath && volumeName.MatchString(m.Name) {
			return info.Config.User, m.Name, nil
		}
	}
	return "", "", fmt.Errorf("site %s: php container has no wp-content volume", siteID)
}

// taskSpec is a validated task, and where it runs.
type taskSpec struct {
	name            string // container name
	siteID          string
	network         string
	user            string // "uid:gid"
	image           string
	wpContentVolume string
	wpContent       provisioner.MountMode
	transferDir     string // host path, symlinks below the data directory resolved
	repositoryDir   string // likewise
	secretsDir      string // host path of the directory holding SecretFiles; "" if there are none
}

// taskSpec validates task and resolves its image and directories.
func (p *Provisioner) taskSpec(task provisioner.Task) (taskSpec, error) {
	var errs []error
	spec := taskSpec{image: task.Image, wpContent: task.WPContent}
	if spec.image == "" {
		spec.image = p.taskImage
	}
	if spec.image == "" {
		errs = append(errs, errors.New("no image given and no task image configured"))
	} else if err := validImage(spec.image); err != nil {
		errs = append(errs, err)
	}
	if len(task.Command) == 0 || task.Command[0] == "" {
		errs = append(errs, errors.New("a command is required"))
	}
	for _, a := range task.Command {
		if strings.IndexByte(a, 0) >= 0 {
			errs = append(errs, errors.New("command arguments can't contain NUL bytes"))
			break
		}
	}
	for name, v := range task.Env {
		if !envName.MatchString(name) {
			errs = append(errs, fmt.Errorf("environment variable name %q is invalid", name))
		}
		if strings.IndexByte(v, 0) >= 0 {
			errs = append(errs, fmt.Errorf("environment variable %s can't contain NUL bytes", name))
		}
	}
	for name, v := range task.SecretFiles {
		if !secretFileName.MatchString(name) {
			errs = append(errs, fmt.Errorf("secret file name %q is invalid", name))
		}
		if strings.IndexByte(v.Reveal(), 0) >= 0 {
			errs = append(errs, fmt.Errorf("secret file %s can't contain NUL bytes", name))
		}
	}
	switch task.WPContent {
	case provisioner.MountNone, provisioner.MountReadOnly, provisioner.MountReadWrite:
	default:
		errs = append(errs, fmt.Errorf("unknown wp-content mount mode %d", task.WPContent))
	}
	if task.Timeout < 0 {
		errs = append(errs, errors.New("timeout must not be negative"))
	}
	var err error
	if task.TransferDir != "" {
		if spec.transferDir, err = dirUnder(p.layout.TmpDir(), task.TransferDir); err != nil {
			errs = append(errs, fmt.Errorf("transfer directory: %w", err))
		}
	}
	if task.RepositoryDir != "" {
		if spec.repositoryDir, err = dirUnder(p.layout.Root, task.RepositoryDir); err != nil {
			errs = append(errs, fmt.Errorf("repository directory: %w", err))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return taskSpec{}, fmt.Errorf("%w: %w", ErrInvalidTask, err)
	}
	return spec, nil
}

// validImage accepts what can safely follow "--" as the image of docker
// create; Docker itself checks the reference.
func validImage(image string) error {
	if image == "" || strings.HasPrefix(image, "-") || strings.ContainsFunc(image, func(r rune) bool { return r <= ' ' || r == 0x7f }) {
		return fmt.Errorf("task image %q is not an image reference", image)
	}
	return nil
}

// dirUnder checks that dir is an existing directory strictly below root,
// also after resolving symbolic links, and returns it as root joined with
// the resolved relative path. Root itself stays as given: on the host it is
// the same path, and resolving it here, inside the control plane container,
// could give a path the host doesn't have.
func dirUnder(root, dir string) (string, error) {
	if !filepath.IsAbs(dir) {
		return "", fmt.Errorf("%q must be an absolute path", dir)
	}
	rel, err := filepath.Rel(root, dir)
	if err != nil || rel == "." || !filepath.IsLocal(rel) {
		return "", fmt.Errorf("%q must be below %s", dir, root)
	}
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	realDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", err
	}
	rel, err = filepath.Rel(realRoot, realDir)
	if err != nil || rel == "." || !filepath.IsLocal(rel) {
		return "", fmt.Errorf("%q must be below %s, also after resolving symbolic links", dir, root)
	}
	fi, err := os.Stat(realDir)
	if err != nil {
		return "", err
	}
	if !fi.IsDir() {
		return "", fmt.Errorf("%q is not a directory", dir)
	}
	resolved := filepath.Join(root, rel)
	// --mount takes comma-separated key=value pairs.
	if strings.ContainsAny(resolved, ",\"\r\n") {
		return "", fmt.Errorf("%q can't contain commas, quotes, or line breaks", dir)
	}
	return resolved, nil
}

// taskContainerName returns a unique container name for a task on the site.
func taskContainerName(siteID string) (string, error) {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return fmt.Sprintf(taskNamePattern, siteID, hex.EncodeToString(b)), nil
}

// createTask creates the task's container.
func (p *Provisioner) createTask(ctx context.Context, spec taskSpec, task provisioner.Task) error {
	if _, err := p.docker(ctx, nil, taskCreateArgs(spec, task)...); err != nil {
		return fmt.Errorf("creating task container: %w", err)
	}
	return nil
}

// secretsDirPrefix starts the names of the directories below
// config.Layout.TmpDir that hold a running task's secret files.
const secretsDirPrefix = "task-secrets-"

// writeTaskSecrets writes files into a new directory below the temporary
// directory and returns it, removing it again if it can't finish. The
// directory and files belong to the task's user, user ("uid:gid"), so the
// task reads them and nobody else can; changing the owner needs the control
// plane to run as root, as it does in the platform. Otherwise (development
// and tests) the secrets are readable by every local user for as long as the
// task runs, unless the task's user is the control plane's own.
func (p *Provisioner) writeTaskSecrets(files map[string]redact.Secret, user string) (string, error) {
	tmp := p.layout.TmpDir()
	if err := os.MkdirAll(tmp, 0o700); err != nil {
		return "", fmt.Errorf("writing task secrets: %w", err)
	}
	dir, err := os.MkdirTemp(tmp, secretsDirPrefix+"*")
	if err != nil {
		return "", fmt.Errorf("writing task secrets: %w", err)
	}
	if strings.ContainsAny(dir, ",\"\r\n") { // --mount takes comma-separated key=value pairs
		_ = os.RemoveAll(dir)
		return "", fmt.Errorf("writing task secrets: %q can't contain commas, quotes, or line breaks", dir)
	}
	fail := func(err error) (string, error) {
		_ = os.RemoveAll(dir)
		return "", fmt.Errorf("writing task secrets: %w", err)
	}
	uid, err := siteUID(user) // checked by taskSiteAccess
	if err != nil {
		return fail(err)
	}
	dirMode, fileMode := os.FileMode(0o700), os.FileMode(0o400)
	switch {
	case os.Geteuid() == 0:
		if err := os.Chown(dir, uid, uid); err != nil {
			return fail(err)
		}
	case uid != os.Geteuid():
		dirMode, fileMode = 0o755, 0o444
	}
	if err := os.Chmod(dir, dirMode); err != nil {
		return fail(err)
	}
	for _, name := range slices.Sorted(maps.Keys(files)) {
		file := filepath.Join(dir, name)
		if err := os.WriteFile(file, []byte(files[name].Reveal()), fileMode); err != nil {
			return fail(err)
		}
		if os.Geteuid() == 0 {
			if err := os.Chown(file, uid, uid); err != nil {
				return fail(err)
			}
		}
	}
	return dir, nil
}

// removeTaskSecrets removes the task's secret files, after ctx may have
// ended. A failure is logged without the path's contents: the directory
// stays for RemoveStaleTaskSecrets.
func (p *Provisioner) removeTaskSecrets(ctx context.Context, siteID, dir string) {
	if dir == "" {
		return
	}
	if err := os.RemoveAll(dir); err != nil {
		p.log.ErrorContext(ctx, "removing task secrets failed", "site_id", siteID, "dir", dir, "error", err)
	}
}

// RemoveStaleTaskSecrets removes the secret directories of tasks that a
// crash left behind, from the temporary directory below layout. Call it when
// the control plane starts, before it runs tasks, and from one process only:
// it can't tell a stale directory from a running task's.
func RemoveStaleTaskSecrets(layout config.Layout) error {
	matches, err := filepath.Glob(filepath.Join(layout.TmpDir(), secretsDirPrefix+"*"))
	if err != nil {
		return err
	}
	var errs []error
	for _, m := range matches {
		errs = append(errs, os.RemoveAll(m))
	}
	return errors.Join(errs...)
}

// taskCreateArgs is the `docker container create` argv for the task.
func taskCreateArgs(spec taskSpec, task provisioner.Task) []string {
	args := []string{
		"container", "create",
		"--name", spec.name,
		"--label", managedLabel + "=true",
		"--label", siteIDLabel + "=" + spec.siteID,
		"--label", taskLabel + "=true",
		"--network", spec.network,
		"--user", spec.user,
		"--read-only",
		"--tmpfs", taskHome,
		"--cap-drop", "ALL",
		"--security-opt", "no-new-privileges:true",
		// Reaps whatever the command starts, e.g. mariadb-dump under a
		// wrapper; output reaches RunTask by attaching, so no log is kept.
		"--init",
		"--log-driver", "none",
		"--entrypoint=" + task.Command[0],
	}
	if _, ok := task.Env["HOME"]; !ok {
		args = append(args, "--env", "HOME="+taskHome)
	}
	for _, name := range slices.Sorted(maps.Keys(task.Env)) {
		args = append(args, "--env", name+"="+task.Env[name])
	}
	switch spec.wpContent {
	case provisioner.MountReadOnly:
		args = append(args, "--mount", "type=volume,source="+spec.wpContentVolume+",target="+provisioner.TaskWPContentPath+",readonly")
	case provisioner.MountReadWrite:
		args = append(args, "--mount", "type=volume,source="+spec.wpContentVolume+",target="+provisioner.TaskWPContentPath)
	}
	if spec.transferDir != "" {
		args = append(args, "--mount", "type=bind,source="+spec.transferDir+",target="+provisioner.TaskTransferPath)
	}
	if spec.secretsDir != "" {
		args = append(args, "--mount", "type=bind,source="+spec.secretsDir+",target="+provisioner.TaskSecretsPath+",readonly")
	}
	if spec.repositoryDir != "" {
		// Still the site's UID, but in the group that owns the repositories,
		// which the client group's sites share.
		args = append(args,
			"--group-add", strconv.Itoa(provisioner.RepositoryGID),
			"--mount", "type=bind,source="+spec.repositoryDir+",target="+provisioner.TaskRepositoryPath)
	}
	args = append(args, "--", spec.image)
	return append(args, task.Command[1:]...)
}

// removeTask force-removes the task's container, killing it if it still
// runs. It runs after ctx may have ended, so it has its own deadline.
func (p *Provisioner) removeTask(ctx context.Context, siteID, name string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), taskCleanupTimeout)
	defer cancel()
	if _, err := p.docker(ctx, nil, "container", "rm", "--force", "--volumes", "--", name); err != nil && !stderrContains(err, "no such container") {
		p.log.ErrorContext(ctx, "removing task container failed", "site_id", siteID, "container", name, "error", err)
	}
}

// chownTree gives dir and everything below it to uid (and GID uid), without
// following symbolic links.
func chownTree(dir string, uid int) error {
	// Walking through an os.Root keeps a symbolic link swapped in during
	// the walk from reaching outside dir.
	root, err := os.OpenRoot(dir)
	if err != nil {
		return fmt.Errorf("handing transfer directory to the site's user: %w", err)
	}
	defer func() { _ = root.Close() }()
	err = fs.WalkDir(root.FS(), ".", func(path string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		return root.Lchown(path, uid, uid)
	})
	if err != nil {
		return fmt.Errorf("handing transfer directory to the site's user: %w", err)
	}
	return nil
}
