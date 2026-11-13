package compose

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/outbuf"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
)

// Defaults for ProvisionerConfig's WP-CLI settings.
const (
	DefaultWPCLITimeout   = 10 * time.Minute
	DefaultWPCLIMaxOutput = 8 << 20 // bytes of stdout
)

const (
	// wpcliMaxStderr bounds the stderr kept for ExecError. The end is kept:
	// WP-CLI prints its "Error:" line last, after any PHP notices.
	wpcliMaxStderr = 64 << 10
	// wpcliKillAfter is how long a command run by timedArgv gets to exit
	// after timeout(1) sends SIGTERM before it is killed.
	wpcliKillAfter = 10 * time.Second
	// wpcliWaitDelay bounds the wait for the docker CLI's output pipes after
	// ctx kills it; see DockerExecLimited.
	wpcliWaitDelay = 5 * time.Second
	// wpcliPath is WP-CLI in the php container (images/README.md).
	wpcliPath = "/usr/local/bin/wp"
)

// ErrSiteNotRunning is returned by ExecWPCLI when the site's php container
// exists but is not running. It is provisioner.ErrSiteNotRunning, so
// driver-agnostic callers can test for it.
var ErrSiteNotRunning = provisioner.ErrSiteNotRunning

// ErrOutputTruncated is returned by ExecWPCLI, together with the output that
// was kept, when the command wrote more than the configured maximum.
var ErrOutputTruncated = errors.New("compose: command output truncated")

// ErrInvalidCommand means a command can't be run as given.
var ErrInvalidCommand = errors.New("compose: invalid command")

// ErrUnexpectedUser means the php container does not run as a site UID, so
// WP-CLI is not run in it rather than risk running it as root.
var ErrUnexpectedUser = errors.New("compose: php container does not run as a site UID")

// ExecWPCLI runs WP-CLI in the site's php container as the site's UID and
// returns its standard output.
//
// args reach WP-CLI verbatim as its argv: nothing passes through a shell,
// and they follow a "--" so neither the docker CLI nor timeout(1) reads them
// as options. They should hold no secrets (provisioner.Provisioner says
// why), but they never appear in errors or logs all the same.
//
// The run is bounded by ctx and the configured WP-CLI timeout. Killing the
// docker CLI does not stop a process started with `docker exec`, so WP-CLI
// runs under timeout(1) inside the container with a limit just past the
// deadline, and dies there too. Standard output beyond the configured
// maximum is dropped and reported with ErrOutputTruncated. A non-zero exit
// yields a *provisioner.ExecError carrying the exit code and the end of
// standard error.
func (p *Provisioner) ExecWPCLI(ctx context.Context, siteID string, args []string) (string, error) {
	return p.ExecWPCLIInput(ctx, siteID, args, nil)
}

// ExecWPCLIInput is ExecWPCLI with stdin as WP-CLI's standard input:
// `docker compose exec` forwards the docker CLI's own standard input to the
// process. A nil stdin gives WP-CLI an empty one.
func (p *Provisioner) ExecWPCLIInput(ctx context.Context, siteID string, args []string, stdin io.Reader) (string, error) {
	if err := validSiteID(siteID); err != nil {
		return "", fmt.Errorf("exec wp-cli: %w", err)
	}
	for _, a := range args {
		if strings.IndexByte(a, 0) >= 0 {
			return "", fmt.Errorf("exec wp-cli on site %s: %w: arguments can't contain NUL bytes", siteID, ErrInvalidCommand)
		}
	}
	ctx, cancel := context.WithTimeout(ctx, p.wpcliTimeout)
	defer cancel()

	user, err := p.phpUser(ctx, siteID)
	if err != nil {
		return "", fmt.Errorf("exec wp-cli: %w", err)
	}
	deadline, _ := ctx.Deadline()
	proj := p.project(siteID)
	proj.Docker = p.wpcliDocker
	callID := provisioner.NewWPCLICallID()
	out, err := proj.run(ctx, stdin, wpCLIArgv(user, callID, time.Until(deadline), args)...)
	truncated := len(out) > p.wpcliMaxOutput
	if truncated {
		out = out[:p.wpcliMaxOutput]
	}
	if err != nil {
		if ctx.Err() != nil {
			p.stopWPCLI(ctx, proj, user, callID)
		}
		return "", wpcliError(ctx, siteID, string(out), err)
	}
	if truncated {
		return string(out), fmt.Errorf("exec wp-cli on site %s: %w to the first %d bytes", siteID, ErrOutputTruncated, p.wpcliMaxOutput)
	}
	return string(out), nil
}

// wpCLIArgv is the `docker compose` argv, after the project flags, that runs
// WP-CLI with args as user, in a process group of its own that is stopped
// just after timeout, or by stopWPCLI with the same callID
// (provisioner.WPCLIRunArgv).
func wpCLIArgv(user, callID string, timeout time.Duration, args []string) []string {
	argv := []string{"exec", "--no-TTY", "--user", user, "--", string(provisioner.ComponentPHP)}
	return append(argv, provisioner.WPCLIRunArgv(callID, timeout, append([]string{wpcliPath}, args...)...)...)
}

// timedArgv is the `docker compose` argv, after the project flags, that runs
// command as user under timeout(1), killed after timeout (rounded up, plus
// a second, so the caller's context expires first). It serves the short
// commands that run no children, unlike WP-CLI.
func timedArgv(user string, timeout time.Duration, command ...string) []string {
	secs := int(math.Ceil(max(timeout, 0).Seconds())) + 1
	argv := []string{
		"exec", "--no-TTY", "--user", user, "--", string(provisioner.ComponentPHP),
		"timeout", "--kill-after=" + strconv.Itoa(int(wpcliKillAfter.Seconds())) + "s", strconv.Itoa(secs) + "s",
	}
	return append(argv, command...)
}

// stopWPCLI stops the WP-CLI call whose context has ended, with a second
// exec in the container: killing the docker CLI doesn't signal the process
// it started. It outlives ctx, and bounds itself. If it fails, the run
// script's own timer ends the call at its deadline.
func (p *Provisioner) stopWPCLI(ctx context.Context, proj Project, user, callID string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), provisioner.WPCLIStopTimeout)
	defer cancel()
	argv := append([]string{"exec", "--no-TTY", "--user", user, "--", string(provisioner.ComponentPHP)},
		provisioner.WPCLIStopArgv(callID)...)
	if _, err := proj.run(ctx, nil, argv...); err != nil {
		p.log.WarnContext(ctx, "stopping WP-CLI after its call ended failed", "project", proj.Name, "error", err)
	}
}

// phpUser returns the user ("uid:gid") the site's running php container
// runs as, checked to be a site UID.
func (p *Provisioner) phpUser(ctx context.Context, siteID string) (string, error) {
	containers, err := p.project(siteID).Ps(ctx)
	if err != nil {
		return "", err
	}
	if len(containers) == 0 {
		return "", fmt.Errorf("site %s: %w", siteID, provisioner.ErrSiteNotFound)
	}
	php, ok := findService(containers, provisioner.ComponentPHP)
	if !ok || php.State != "running" {
		return "", fmt.Errorf("site %s: %w", siteID, ErrSiteNotRunning)
	}
	info, found, err := p.inspectContainer(ctx, php.Name)
	if err != nil {
		return "", err
	}
	if !found || !info.State.Running {
		return "", fmt.Errorf("site %s: %w", siteID, ErrSiteNotRunning)
	}
	if _, err := siteUID(info.Config.User); err != nil {
		return "", fmt.Errorf("site %s: %w", siteID, err)
	}
	return info.Config.User, nil
}

// siteUID checks that a container's user ("uid:gid") is a site UID, with
// the GID equal to the UID as the site template sets it, and returns the UID.
func siteUID(user string) (int, error) {
	uid, gid, _ := strings.Cut(user, ":")
	n, err := strconv.Atoi(uid)
	if err != nil || gid != uid || n < provisioner.MinSiteUID || n > provisioner.MaxSiteUID {
		return 0, fmt.Errorf("%w: user is %q", ErrUnexpectedUser, user)
	}
	return n, nil
}

// exitCoder is implemented by *exec.ExitError, and by test fakes.
type exitCoder interface{ ExitCode() int }

// wpcliError turns a failed WP-CLI run into the error ExecWPCLI returns.
// It never wraps the *CommandError itself, whose message lists the
// arguments.
func wpcliError(ctx context.Context, siteID, stdout string, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return fmt.Errorf("exec wp-cli on site %s: %w", siteID, ctxErr)
	}
	var ce *CommandError
	if !errors.As(err, &ce) {
		return fmt.Errorf("exec wp-cli on site %s: %w", siteID, err)
	}
	var ec exitCoder
	if errors.As(ce.Err, &ec) && ec.ExitCode() > 0 {
		return fmt.Errorf("exec wp-cli on site %s: %w", siteID,
			&provisioner.ExecError{ExitCode: ec.ExitCode(), Stdout: stdout, Stderr: ce.Stderr})
	}
	return fmt.Errorf("exec wp-cli on site %s: %w: %w", siteID, ErrCommandFailed, ce.Err)
}

// DockerExecLimited is DockerExec with bounded memory, for commands whose
// output size the platform doesn't control. It keeps at most stdoutMax+1
// bytes of standard output, so a caller can tell whether output was cut, and
// the last stderrMax bytes of standard error.
//
// When ctx ends, the docker CLI is killed, but `docker compose` runs as a
// child process that may keep the output pipes open; the wait for them is
// bounded rather than lasting as long as that child.
func DockerExecLimited(stdoutMax, stderrMax int) ExecFunc {
	return func(ctx context.Context, stdin io.Reader, args ...string) ([]byte, error) {
		// Arguments are passed directly to docker, never through a shell.
		cmd := exec.CommandContext(ctx, "docker", args...) //nolint:gosec // G204: running docker with caller-built arguments is the point
		stdout := &outbuf.Head{Max: stdoutMax + 1}
		stderr := &outbuf.Tail{Max: stderrMax}
		cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr
		cmd.WaitDelay = wpcliWaitDelay
		if err := cmd.Run(); err != nil {
			return stdout.Bytes(), &CommandError{Args: args, Stderr: strings.TrimSpace(stderr.String()), Err: err}
		}
		return stdout.Bytes(), nil
	}
}
