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
	"strings"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
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
// services under the same aliases as the site, and runs as the site's UID
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
// as it is, and must be writable by the site's UID.
//
// Secret environment variables reach the docker CLI through an env-file
// (mode 0600 in the data directory's tmp/, removed as soon as the container
// is created), so they never appear in argv, errors, or logs. Docker still
// keeps them in the container's configuration until the container is
// removed, as it does for any environment variable.
//
// The task's container is removed afterwards whatever happened, including
// when ctx ends or Timeout passes, which kill the task. A non-zero exit
// yields a *provisioner.ExecError; standard output beyond 8 MiB is dropped
// and reported with ErrOutputTruncated, together with the result.
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

	start := time.Now()
	defer p.removeTask(ctx, siteID, name)
	if err := p.createTask(ctx, spec, task); err != nil {
		return nil, fmt.Errorf("run task on site %s: %w", siteID, err)
	}
	stdout := &headBuffer{max: taskMaxOutput + 1}
	stderr := &tailBuffer{max: taskMaxStderr}
	runErr := p.taskDocker(ctx, stdout, stderr, "container", "start", "--attach", "--", name)
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
	envFile         string // holds SecretEnv; "" if there is none
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
	for name, v := range task.SecretEnv {
		if !envName.MatchString(name) {
			errs = append(errs, fmt.Errorf("environment variable name %q is invalid", name))
		}
		if _, dup := task.Env[name]; dup {
			errs = append(errs, fmt.Errorf("environment variable %s is both secret and not", name))
		}
		// An env-file holds one variable per line. The value is not quoted.
		if strings.ContainsAny(v.Reveal(), "\x00\r\n") {
			errs = append(errs, fmt.Errorf("secret environment variable %s can't contain line breaks or NUL bytes", name))
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

// createTask creates the task's container, passing SecretEnv through an
// env-file that exists only while `docker create` runs.
func (p *Provisioner) createTask(ctx context.Context, spec taskSpec, task provisioner.Task) error {
	if len(task.SecretEnv) > 0 {
		f, err := os.CreateTemp(p.layout.TmpDir(), "task-env-*") // mode 0600
		if err != nil {
			return fmt.Errorf("writing task environment: %w", err)
		}
		defer func() { _ = os.Remove(f.Name()) }()
		var b strings.Builder
		for _, name := range slices.Sorted(maps.Keys(task.SecretEnv)) {
			b.WriteString(name + "=" + task.SecretEnv[name].Reveal() + "\n")
		}
		_, werr := f.WriteString(b.String())
		if err := errors.Join(werr, f.Close()); err != nil {
			return fmt.Errorf("writing task environment: %w", err)
		}
		spec.envFile = f.Name()
	}
	if _, err := p.docker(ctx, nil, taskCreateArgs(spec, task)...); err != nil {
		return fmt.Errorf("creating task container: %w", err)
	}
	return nil
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
		if _, ok := task.SecretEnv["HOME"]; !ok {
			args = append(args, "--env", "HOME="+taskHome)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(task.Env)) {
		args = append(args, "--env", name+"="+task.Env[name])
	}
	if spec.envFile != "" {
		args = append(args, "--env-file", spec.envFile)
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
	if spec.repositoryDir != "" {
		args = append(args, "--mount", "type=bind,source="+spec.repositoryDir+",target="+provisioner.TaskRepositoryPath)
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
