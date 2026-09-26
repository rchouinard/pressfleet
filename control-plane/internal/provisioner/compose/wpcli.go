package compose

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os/exec"
	"strconv"
	"strings"
	"time"

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
	// wpcliKillAfter is how long WP-CLI gets to exit after timeout(1) sends
	// SIGTERM before it is killed.
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
// as options. They may hold secrets (e.g. `user create --user_pass=...`), so
// they never appear in errors or logs.
//
// The run is bounded by ctx and the configured WP-CLI timeout. Killing the
// docker CLI does not stop a process started with `docker exec`, so WP-CLI
// runs under timeout(1) inside the container with a limit just past the
// deadline, and dies there too. Standard output beyond the configured
// maximum is dropped and reported with ErrOutputTruncated. A non-zero exit
// yields a *provisioner.ExecError carrying the exit code and the end of
// standard error.
func (p *Provisioner) ExecWPCLI(ctx context.Context, siteID string, args []string) (string, error) {
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
	out, err := proj.run(ctx, nil, wpCLIArgv(user, time.Until(deadline), args)...)
	truncated := len(out) > p.wpcliMaxOutput
	if truncated {
		out = out[:p.wpcliMaxOutput]
	}
	if err != nil {
		return "", wpcliError(ctx, siteID, string(out), err)
	}
	if truncated {
		return string(out), fmt.Errorf("exec wp-cli on site %s: %w to the first %d bytes", siteID, ErrOutputTruncated, p.wpcliMaxOutput)
	}
	return string(out), nil
}

// wpCLIArgv is the `docker compose` argv, after the project flags, that runs
// WP-CLI with args as user, killed after timeout (rounded up, plus a second,
// so the caller's context expires first).
func wpCLIArgv(user string, timeout time.Duration, args []string) []string {
	secs := int(math.Ceil(max(timeout, 0).Seconds())) + 1
	argv := []string{
		"exec", "--no-TTY", "--user", user, "--", string(provisioner.ComponentPHP),
		"timeout", "--kill-after=" + strconv.Itoa(int(wpcliKillAfter.Seconds())) + "s", strconv.Itoa(secs) + "s",
		wpcliPath,
	}
	return append(argv, args...)
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
	uid, gid, _ := strings.Cut(info.Config.User, ":")
	n, err := strconv.Atoi(uid)
	if err != nil || gid != uid || n < provisioner.MinSiteUID || n > provisioner.MaxSiteUID {
		return "", fmt.Errorf("site %s: %w: user is %q", siteID, ErrUnexpectedUser, info.Config.User)
	}
	return info.Config.User, nil
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
		stdout := &headBuffer{max: stdoutMax + 1}
		stderr := &tailBuffer{max: stderrMax}
		cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr
		cmd.WaitDelay = wpcliWaitDelay
		if err := cmd.Run(); err != nil {
			return stdout.buf, &CommandError{Args: args, Stderr: strings.TrimSpace(stderr.String()), Err: err}
		}
		return stdout.buf, nil
	}
}

// headBuffer keeps the first max bytes written to it and discards the rest.
type headBuffer struct {
	buf []byte
	max int
}

func (b *headBuffer) Write(p []byte) (int, error) {
	if room := b.max - len(b.buf); room > 0 {
		b.buf = append(b.buf, p[:min(room, len(p))]...)
	}
	return len(p), nil
}

// tailBuffer keeps the last max bytes written to it.
type tailBuffer struct {
	buf     []byte
	max     int
	dropped int
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	b.buf = append(b.buf, p...)
	// Trim only once the buffer holds twice the limit, so trimming costs
	// amortized constant time per byte.
	if len(b.buf) > 2*b.max {
		b.trim()
	}
	return len(p), nil
}

func (b *tailBuffer) trim() {
	if cut := len(b.buf) - b.max; cut > 0 {
		b.dropped += cut
		b.buf = append(b.buf[:0], b.buf[cut:]...)
	}
}

// String returns the kept bytes, prefixed with a note if some were dropped.
func (b *tailBuffer) String() string {
	b.trim()
	if b.dropped == 0 {
		return string(b.buf)
	}
	return fmt.Sprintf("[%d bytes truncated]\n%s", b.dropped, bytes.ToValidUTF8(b.buf, nil))
}
