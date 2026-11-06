package compose

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

// ErrNotPublished is returned by Port when the container port has no
// published host port.
var ErrNotPublished = errors.New("compose: port not published")

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

// ExecFunc runs the docker CLI with args, connecting stdin (nil for none) to
// its standard input, and returns its standard output. Failures are reported
// as *CommandError. Tests inject a fake to avoid needing Docker.
type ExecFunc func(ctx context.Context, stdin io.Reader, args ...string) ([]byte, error)

// DockerExec runs the docker binary found on PATH.
func DockerExec(ctx context.Context, stdin io.Reader, args ...string) ([]byte, error) {
	// Arguments are passed directly to docker, never through a shell.
	cmd := exec.CommandContext(ctx, "docker", args...) //nolint:gosec // G204: running docker with caller-built arguments is the point
	var stdout, stderr bytes.Buffer
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, &stdout, &stderr
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
	// Files are the absolute paths of the Compose files when they aren't
	// Dir/compose.yaml and its compose.override.yaml, merged in order.
	// Relative paths in them still resolve against Dir, which is how a
	// platform upgrade inspects a newly rendered file in a staging directory
	// as if it were the project's own.
	Files []string
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
	// NoStart creates or recreates containers without starting them, so a
	// stopped project stays stopped. Wait is ignored.
	NoStart bool
	// ForceRecreate recreates containers even if their configuration is
	// unchanged. Compose can't see changes to the content of bind-mounted
	// files, such as a site's wp-config.php.
	ForceRecreate bool
	// Services limits Up to these services; empty means all of them.
	Services []string
}

// Up creates or updates the project's containers in the background,
// recreating only services whose configuration changed. Services no longer
// in compose.yaml are removed.
func (p Project) Up(ctx context.Context, opts UpOptions) error {
	args := []string{"up"}
	if opts.NoStart {
		args = append(args, "--no-start", "--remove-orphans")
	} else {
		args = append(args, "--detach", "--remove-orphans")
		if opts.Wait {
			args = append(args, "--wait")
			if opts.WaitTimeout > 0 {
				args = append(args, "--wait-timeout", waitSeconds(opts.WaitTimeout))
			}
		}
	}
	if opts.ForceRecreate {
		args = append(args, "--force-recreate")
	}
	if len(opts.Services) > 0 {
		args = append(append(args, "--"), opts.Services...)
	}
	_, err := p.run(ctx, nil, args...)
	return err
}

// StartOptions configures Start.
type StartOptions struct {
	// Wait blocks until every service is running and healthy.
	Wait bool
	// WaitTimeout bounds Wait; zero means no limit beyond ctx.
	WaitTimeout time.Duration
}

// Start starts the project's existing containers.
func (p Project) Start(ctx context.Context, opts StartOptions) error {
	args := []string{"start"}
	if opts.Wait {
		args = append(args, "--wait")
		if opts.WaitTimeout > 0 {
			args = append(args, "--wait-timeout", waitSeconds(opts.WaitTimeout))
		}
	}
	_, err := p.run(ctx, nil, args...)
	return err
}

// Stop stops the project's containers without removing them.
func (p Project) Stop(ctx context.Context) error {
	_, err := p.run(ctx, nil, "stop")
	return err
}

// StopServices stops the named services' containers without removing them.
func (p Project) StopServices(ctx context.Context, services ...string) error {
	if len(services) == 0 {
		return fmt.Errorf("%w: stop needs a service", ErrInvalidProject)
	}
	_, err := p.run(ctx, nil, append([]string{"stop", "--"}, services...)...)
	return err
}

// RunService runs a command in a new, removed-after container of a service
// (compose run --rm --no-deps), without a TTY, and returns its standard
// output. It is how the host CLI runs a state command while the service's
// own container is stopped, e.g. restoring the control plane's state
// database (§4.7). As with Exec, never pass secrets as arguments.
func (p Project) RunService(ctx context.Context, service string, command ...string) ([]byte, error) {
	if service == "" {
		return nil, fmt.Errorf("%w: run needs a service", ErrInvalidProject)
	}
	return p.run(ctx, nil, append([]string{"run", "--rm", "--no-deps", "--no-TTY", service}, command...)...)
}

// ListDir lists the names of the entries of a directory as a new container
// of a service sees it (compose run --rm --no-deps, with ls as the
// entrypoint). The host CLI uses it for directories only the container's
// root can read, such as the state backups (§4.7). The service's image must
// ship ls.
func (p Project) ListDir(ctx context.Context, service, dir string) ([]string, error) {
	if service == "" {
		return nil, fmt.Errorf("%w: ls needs a service", ErrInvalidProject)
	}
	out, err := p.run(ctx, nil, "run", "--rm", "--no-deps", "--no-TTY", "--entrypoint", "ls", service, "-1", dir)
	if err != nil {
		return nil, err
	}
	return strings.Fields(string(out)), nil
}

// ReadFile returns a file's contents as a new container of a service sees
// it (compose run --rm --no-deps, with cat as the entrypoint). Like ListDir,
// it is for files only the container's root can read, such as the state
// database's migration markers (§4.7). The service's image must ship cat.
func (p Project) ReadFile(ctx context.Context, service, path string) ([]byte, error) {
	if service == "" {
		return nil, fmt.Errorf("%w: cat needs a service", ErrInvalidProject)
	}
	return p.run(ctx, nil, "run", "--rm", "--no-deps", "--no-TTY", "--entrypoint", "cat", service, "--", path)
}

// waitSeconds formats a wait timeout for Compose, which takes whole seconds
// and reads 0 as no limit, so a positive timeout never rounds down to 0.
func waitSeconds(d time.Duration) string {
	return strconv.Itoa(max(1, int(d.Round(time.Second).Seconds())))
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
	_, err := p.run(ctx, nil, args...)
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
	out, err := p.run(ctx, nil, "ps", "--all", "--format", "json")
	if err != nil {
		return nil, err
	}
	return parsePs(out)
}

// Top lists the processes running in the service's containers, as
// `docker compose top` prints them: for each container, its name and a table
// whose last column is the command line. It is how end-to-end tests check
// what a container runs.
func (p Project) Top(ctx context.Context, service string) ([]byte, error) {
	if service == "" {
		return nil, fmt.Errorf("%w: top needs a service", ErrInvalidProject)
	}
	return p.run(ctx, nil, "top", service)
}

// Exec runs a command in a running service container without a TTY and
// returns its standard output. It is how the host CLI runs state commands
// inside the control plane (§4.7). Arguments appear in error messages, so
// never pass secrets as arguments; use ExecInput or a file instead.
func (p Project) Exec(ctx context.Context, service string, command ...string) ([]byte, error) {
	return p.ExecInput(ctx, nil, service, command...)
}

// ExecInput is Exec with stdin connected to the command's standard input,
// which is how secrets such as passwords are handed to a command.
func (p Project) ExecInput(ctx context.Context, stdin io.Reader, service string, command ...string) ([]byte, error) {
	if service == "" || len(command) == 0 {
		return nil, fmt.Errorf("%w: exec needs a service and a command", ErrInvalidProject)
	}
	return p.run(ctx, stdin, append([]string{"exec", "--no-TTY", service}, command...)...)
}

// Logs returns the timestamped output of every container in the project.
func (p Project) Logs(ctx context.Context) ([]byte, error) {
	return p.run(ctx, nil, "logs", "--no-color", "--timestamps")
}

// Port returns the host address ("host:port") on which a service's
// container port is published. It returns ErrNotPublished if there is none.
func (p Project) Port(ctx context.Context, service string, containerPort int) (string, error) {
	if service == "" || containerPort <= 0 {
		return "", fmt.Errorf("%w: port needs a service and a container port", ErrInvalidProject)
	}
	out, err := p.run(ctx, nil, "port", service, strconv.Itoa(containerPort))
	if err != nil {
		return "", err
	}
	// One address per line if the port is published more than once.
	addr, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
	if addr == "" || strings.HasSuffix(addr, ":0") {
		return "", fmt.Errorf("%w: compose project %s: %s port %d", ErrNotPublished, p.Name, service, containerPort)
	}
	return addr, nil
}

// Resources are the Docker objects labelled as belonging to a project.
type Resources struct {
	Containers []string // names
	Networks   []string // names
	Volumes    []string // names
}

// Empty reports whether there are no resources.
func (r Resources) Empty() bool {
	return len(r.Containers) == 0 && len(r.Networks) == 0 && len(r.Volumes) == 0
}

// Resources lists the containers, networks, and volumes that carry the
// project's Compose label. It asks the daemon rather than reading
// compose.yaml, so it also finds objects the file no longer describes; after
// Down with Volumes it is empty, which is how teardown is verified.
func (p Project) Resources(ctx context.Context) (Resources, error) {
	if err := p.validate(); err != nil {
		return Resources{}, err
	}
	filter := "label=com.docker.compose.project=" + p.Name
	var res Resources
	for _, q := range []struct {
		args []string
		dst  *[]string
	}{
		{[]string{"container", "ls", "--all", "--filter", filter, "--format", "{{.Names}}"}, &res.Containers},
		{[]string{"network", "ls", "--filter", filter, "--format", "{{.Name}}"}, &res.Networks},
		{[]string{"volume", "ls", "--filter", filter, "--format", "{{.Name}}"}, &res.Volumes},
	} {
		out, err := p.docker()(ctx, nil, q.args...)
		if err != nil {
			return Resources{}, fmt.Errorf("compose project %s: %w", p.Name, err)
		}
		*q.dst = strings.Fields(string(out))
	}
	return res, nil
}

func (p Project) validate() error {
	if !projectName.MatchString(p.Name) {
		return fmt.Errorf("%w: name %q must be lowercase letters, digits, '-' or '_'", ErrInvalidProject, p.Name)
	}
	if !filepath.IsAbs(p.Dir) {
		return fmt.Errorf("%w: directory %q must be absolute", ErrInvalidProject, p.Dir)
	}
	for _, f := range p.Files {
		if !filepath.IsAbs(f) {
			return fmt.Errorf("%w: file %q must be absolute", ErrInvalidProject, f)
		}
	}
	return nil
}

func (p Project) docker() ExecFunc {
	if p.Docker == nil {
		return DockerExec
	}
	return p.Docker
}

func (p Project) run(ctx context.Context, stdin io.Reader, args ...string) ([]byte, error) {
	if err := p.validate(); err != nil {
		return nil, err
	}
	full := []string{"compose", "--project-name", p.Name, "--project-directory", p.Dir}
	for _, f := range p.Files {
		full = append(full, "--file", f)
	}
	full = append(full, args...)
	out, err := p.docker()(ctx, stdin, full...)
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
