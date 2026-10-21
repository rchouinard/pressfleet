package compose

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
)

// ErrInvalidSession is returned by RunSession for a session it can't run as
// given.
var ErrInvalidSession = errors.New("compose: invalid session")

// Limits of a session worker. It serves one SFTP client, which needs little
// memory (the server streams files in 32 KiB packets) and few processes.
const (
	sessionMemory     = "128m"
	sessionPidsLimit  = "64"
	sessionNameFormat = "pf-%s-session-%s"
	// sessionLabel marks session workers, like taskLabel marks tasks, so
	// leftovers of a crashed control plane can be found.
	sessionLabel = "pressfleet.session"
	// sessionMaxStderr bounds the standard error RunSession keeps for its
	// error when s.Stderr is nil; the end is kept.
	sessionMaxStderr = 16 << 10
)

// AttachExecFunc runs the docker CLI with args, connecting its standard
// streams to stdin, stdout, and stderr. Failures are reported as
// *CommandError.
type AttachExecFunc func(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer, args ...string) error

// DockerAttachExec runs the docker binary found on PATH with its standard
// streams connected. When ctx ends the CLI is killed, and the wait for its
// streams is bounded: a client that never closes its end mustn't keep the
// call from returning.
func DockerAttachExec(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer, args ...string) error {
	// Arguments are passed directly to docker, never through a shell.
	cmd := exec.CommandContext(ctx, "docker", args...) //nolint:gosec // G204: running docker with caller-built arguments is the point
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr
	cmd.WaitDelay = taskWaitDelay
	if err := cmd.Run(); err != nil {
		return &CommandError{Args: args, Err: err}
	}
	return nil
}

// attachAdapter runs an AttachExecFunc's commands through an ExecFunc,
// which buffers standard output and only reports standard error when the
// command fails. Tests that inject ProvisionerConfig.Docker get it.
func attachAdapter(docker ExecFunc) AttachExecFunc {
	return func(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer, args ...string) error {
		out, err := docker(ctx, stdin, args...)
		_, _ = stdout.Write(out)
		if ce := (*CommandError)(nil); errors.As(err, &ce) {
			_, _ = io.WriteString(stderr, ce.Stderr)
		}
		return err
	}
}

// RunSession runs a developer session in a one-off worker container for the
// site and waits for it to exit (provisioner.SessionRunner).
//
// The container runs as the site's UID and GID (read from its php
// container) with the site's wp-content volume at
// provisioner.TaskWPContentPath and no other mount, no network
// (--network none), a read-only root filesystem with a tmpfs at /tmp, all
// capabilities dropped, no-new-privileges, and memory and process limits.
// Its standard input and output are the session's streams, attached with
// `docker start --attach --interactive`; with no TTY they carry binary data
// unchanged, and the end of s.Stdin closes the container's standard input
// (the container is created with --interactive, which sets StdinOnce).
//
// The container is removed afterwards whatever happened, including when
// ctx ends, which kills it.
func (p *Provisioner) RunSession(ctx context.Context, siteID string, s provisioner.Session) error {
	if err := validSiteID(siteID); err != nil {
		return fmt.Errorf("run session: %w", err)
	}
	image, err := p.sessionSpec(s)
	if err != nil {
		return fmt.Errorf("run session on site %s: %w", siteID, err)
	}
	user, volume, err := p.taskSiteAccess(ctx, siteID)
	if err != nil {
		return fmt.Errorf("run session: %w", err)
	}
	name, err := sessionContainerName(siteID)
	if err != nil {
		return fmt.Errorf("run session on site %s: %w", siteID, err)
	}

	start := time.Now()
	defer p.removeTask(ctx, p.log.With("site_id", siteID), name)
	if _, err := p.docker(ctx, nil, sessionCreateArgs(name, siteID, user, volume, image, s.Command)...); err != nil {
		return fmt.Errorf("run session on site %s: creating the worker: %w", siteID, err)
	}
	stderr := s.Stderr
	tail := &tailBuffer{max: sessionMaxStderr}
	if stderr == nil {
		stderr = tail
	}
	runErr := p.sessionDocker(ctx, s.Stdin, s.Stdout, stderr, "container", "start", "--attach", "--interactive", "--", name)
	err = p.sessionResult(ctx, siteID, name, tail, runErr)
	p.log.InfoContext(ctx, "session worker finished", "site_id", siteID, "container", name,
		"duration", time.Since(start), "error", err)
	return err
}

// sessionResult turns the attached run into RunSession's result, going by
// the container's own exit code as taskResult does.
func (p *Provisioner) sessionResult(ctx context.Context, siteID, name string, stderr *tailBuffer, runErr error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return fmt.Errorf("run session on site %s: %w", siteID, ctxErr)
	}
	info, found, err := p.inspectContainer(ctx, name)
	switch {
	case err != nil:
		return fmt.Errorf("run session on site %s: %w", siteID, err)
	case !found || info.State.Status != "exited":
		if runErr == nil {
			runErr = fmt.Errorf("%w: session worker did not run to completion", ErrCommandFailed)
		}
		return fmt.Errorf("run session on site %s: %w", siteID, runErr)
	case info.State.ExitCode != 0:
		return fmt.Errorf("run session on site %s: %w", siteID,
			&provisioner.ExecError{ExitCode: info.State.ExitCode, Stderr: stderr.String()})
	}
	return nil
}

// SessionWorkers lists the names of the site's session worker containers,
// running or not. RunSession removes each worker when it ends, so outside
// a session the list is empty; tests check that it is.
func SessionWorkers(ctx context.Context, docker ExecFunc, siteID string) ([]string, error) {
	if err := validSiteID(siteID); err != nil {
		return nil, err
	}
	if docker == nil {
		docker = DockerExec
	}
	out, err := docker(ctx, nil, "container", "ls", "--all",
		"--filter", "label="+sessionLabel+"=true", "--filter", "label="+siteIDLabel+"="+siteID,
		"--format", "{{.Names}}")
	if err != nil {
		return nil, fmt.Errorf("listing session workers of site %s: %w", siteID, err)
	}
	return strings.Fields(string(out)), nil
}

// sessionSpec validates s and returns its image.
func (p *Provisioner) sessionSpec(s provisioner.Session) (string, error) {
	var errs []error
	image := s.Image
	if image == "" {
		image = p.taskImage
	}
	if image == "" {
		errs = append(errs, errors.New("no image given and no task image configured"))
	} else if err := validImage(image); err != nil {
		errs = append(errs, err)
	}
	if len(s.Command) == 0 || s.Command[0] == "" {
		errs = append(errs, errors.New("a command is required"))
	}
	for _, a := range s.Command {
		if strings.IndexByte(a, 0) >= 0 {
			errs = append(errs, errors.New("command arguments can't contain NUL bytes"))
			break
		}
	}
	if s.Stdin == nil || s.Stdout == nil {
		errs = append(errs, errors.New("standard input and output are required"))
	}
	if err := errors.Join(errs...); err != nil {
		return "", fmt.Errorf("%w: %w", ErrInvalidSession, err)
	}
	return image, nil
}

// sessionContainerName returns a unique container name for a session on
// the site.
func sessionContainerName(siteID string) (string, error) {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return fmt.Sprintf(sessionNameFormat, siteID, hex.EncodeToString(b)), nil
}

// sessionCreateArgs is the `docker container create` argv for a session
// worker.
func sessionCreateArgs(name, siteID, user, volume, image string, command []string) []string {
	args := []string{
		"container", "create",
		"--name", name,
		"--label", managedLabel + "=true",
		"--label", siteIDLabel + "=" + siteID,
		"--label", sessionLabel + "=true",
		// Standard input stays open for the attached client, and closes
		// when it ends.
		"--interactive",
		// The gateway relays everything over the streams; the worker needs
		// no network, and so can't reach the platform services or other
		// sites.
		"--network", "none",
		"--user", user,
		"--read-only",
		"--tmpfs", taskHome,
		"--env", "HOME=" + taskHome,
		"--cap-drop", "ALL",
		"--security-opt", "no-new-privileges:true",
		"--memory", sessionMemory,
		"--pids-limit", sessionPidsLimit,
		"--init",
		"--log-driver", "none",
		"--mount", "type=volume,source=" + volume + ",target=" + provisioner.TaskWPContentPath,
		"--entrypoint=" + command[0],
		"--", image,
	}
	return append(args, command[1:]...)
}
