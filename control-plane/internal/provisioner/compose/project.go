package compose

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// ErrInvalidProject is returned when a Project's name or directory can't be
// used.
var ErrInvalidProject = errors.New("compose: invalid project")

// ErrCommandFailed wraps failures of the docker CLI. Use errors.As with
// *CommandError for the exit details.
var ErrCommandFailed = errors.New("compose: docker command failed")

// CommandError describes a docker CLI invocation that failed.
type CommandError struct {
	Args   []string // arguments after "docker"
	Stderr string   // trimmed standard error
	Err    error    // underlying error, e.g. *exec.ExitError
}

func (e *CommandError) Error() string {
	msg := fmt.Sprintf("docker %s: %v", strings.Join(e.Args, " "), e.Err)
	if e.Stderr != "" {
		msg += ": " + e.Stderr
	}
	return msg
}

// Unwrap lets errors.Is match both ErrCommandFailed and the underlying error.
func (e *CommandError) Unwrap() []error { return []error{ErrCommandFailed, e.Err} }

// ExecFunc runs the docker CLI with args and returns its standard output.
// Failures are reported as *CommandError. Tests inject a fake to avoid
// needing Docker.
type ExecFunc func(ctx context.Context, args ...string) ([]byte, error)

// DockerExec runs the docker binary found on PATH.
func DockerExec(ctx context.Context, args ...string) ([]byte, error) {
	// Arguments are passed directly to docker, never through a shell.
	cmd := exec.CommandContext(ctx, "docker", args...) //nolint:gosec // G204: running docker with caller-built arguments is the point
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return stdout.Bytes(), &CommandError{Args: args, Stderr: strings.TrimSpace(stderr.String()), Err: err}
	}
	return stdout.Bytes(), nil
}

// projectName matches the names Compose accepts for -p.
var projectName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

// Project is one Compose project: a rendered directory containing
// compose.yaml, applied under a fixed project name.
type Project struct {
	// Name is the Compose project name (-p), e.g. "pressfleet" or "pf-<site-id>".
	Name string
	// Dir is the absolute project directory holding compose.yaml. Relative
	// paths in the file resolve against it.
	Dir string
	// Docker runs the docker CLI; nil means DockerExec.
	Docker ExecFunc
}

// UpOptions configures Up.
type UpOptions struct {
	// Wait blocks until every service is running and healthy (Compose's
	// --wait), failing if one exits or becomes unhealthy.
	Wait bool
	// WaitTimeout bounds Wait; zero means no limit beyond ctx.
	WaitTimeout time.Duration
}

// Up creates or updates the project's containers in the background,
// recreating only services whose configuration changed. Services no longer
// in compose.yaml are removed.
func (p Project) Up(ctx context.Context, opts UpOptions) error {
	args := []string{"up", "--detach", "--remove-orphans"}
	if opts.Wait {
		args = append(args, "--wait")
		if opts.WaitTimeout > 0 {
			args = append(args, "--wait-timeout", strconv.Itoa(int(opts.WaitTimeout.Round(time.Second).Seconds())))
		}
	}
	_, err := p.run(ctx, args...)
	return err
}

// DownOptions configures Down.
type DownOptions struct {
	// Volumes also removes the project's named volumes, destroying their data.
	Volumes bool
}

// Down stops and removes the project's containers and networks.
func (p Project) Down(ctx context.Context, opts DownOptions) error {
	args := []string{"down", "--remove-orphans"}
	if opts.Volumes {
		args = append(args, "--volumes")
	}
	_, err := p.run(ctx, args...)
	return err
}

// Container is the state of one container in a project.
type Container struct {
	Name     string `json:"Name"`
	Service  string `json:"Service"`
	State    string `json:"State"`  // e.g. "running", "exited"
	Health   string `json:"Health"` // "", "starting", "healthy", "unhealthy"
	ExitCode int    `json:"ExitCode"`
}

// Ps lists the project's containers, including stopped ones.
func (p Project) Ps(ctx context.Context) ([]Container, error) {
	out, err := p.run(ctx, "ps", "--all", "--format", "json")
	if err != nil {
		return nil, err
	}
	return parsePs(out)
}

// Exec runs a command in a running service container without a TTY and
// returns its standard output. It is how the host CLI runs state commands
// inside the control plane (§4.7). Arguments appear in error messages, so
// never pass secrets as arguments; use stdin or a file instead.
func (p Project) Exec(ctx context.Context, service string, command ...string) ([]byte, error) {
	if service == "" || len(command) == 0 {
		return nil, fmt.Errorf("%w: exec needs a service and a command", ErrInvalidProject)
	}
	return p.run(ctx, append([]string{"exec", "--no-TTY", service}, command...)...)
}

func (p Project) run(ctx context.Context, args ...string) ([]byte, error) {
	if !projectName.MatchString(p.Name) {
		return nil, fmt.Errorf("%w: name %q must be lowercase letters, digits, '-' or '_'", ErrInvalidProject, p.Name)
	}
	if !filepath.IsAbs(p.Dir) {
		return nil, fmt.Errorf("%w: directory %q must be absolute", ErrInvalidProject, p.Dir)
	}
	execFn := p.Docker
	if execFn == nil {
		execFn = DockerExec
	}
	full := append([]string{"compose", "--project-name", p.Name, "--project-directory", p.Dir}, args...)
	out, err := execFn(ctx, full...)
	if err != nil {
		return out, fmt.Errorf("compose project %s: %w", p.Name, err)
	}
	return out, nil
}

// parsePs accepts both output formats of `docker compose ps --format json`:
// one object per line (Compose 2.21 and later) or a single array (earlier).
func parsePs(out []byte) ([]Container, error) {
	out = bytes.TrimSpace(out)
	if len(out) == 0 {
		return nil, nil
	}
	var containers []Container
	if out[0] == '[' {
		if err := json.Unmarshal(out, &containers); err != nil {
			return nil, fmt.Errorf("parsing compose ps output: %w", err)
		}
		return containers, nil
	}
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var c Container
		if err := json.Unmarshal(line, &c); err != nil {
			return nil, fmt.Errorf("parsing compose ps output: %w", err)
		}
		containers = append(containers, c)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("parsing compose ps output: %w", err)
	}
	return containers, nil
}
