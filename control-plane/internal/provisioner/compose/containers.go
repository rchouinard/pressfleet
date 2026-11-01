package compose

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

// ErrNoContainer is returned by Project.ServiceContainer when the service
// has no container.
var ErrNoContainer = errors.New("compose: service has no container")

// ContainerDetails is what `docker container inspect` reports about a
// container, as far as diagnostics and tests need it.
type ContainerDetails struct {
	ID   string
	Name string
	// Image is the image reference the container was created from, e.g.
	// ghcr.io/rchouinard/pressfleet/site-php:php8.4-wp7.1.2, or a pinned
	// build of it (Provisioner.PinImage), and ImageID the ID of the image.
	Image   string
	ImageID string
	Running bool
	// Networks maps each network the container is attached to to its IPv4
	// address there.
	Networks map[string]string
	// Mounts are the container's volumes and bind mounts.
	Mounts []Mount
	// Labels are the container's labels.
	Labels map[string]string
}

// Mount is one of a container's volumes or bind mounts.
type Mount struct {
	Type        string // "bind", "volume", "tmpfs"
	Source      string // host path of a bind mount; empty for a tmpfs
	Destination string // path in the container
	RW          bool
}

// ServiceContainer returns the container of one of the project's services.
// It finds the container by its Compose labels rather than through
// `docker compose`, so it works without reading the project directory: a
// site's directory belongs to the control plane (mode 0700), and a process
// running as another user, such as the end-to-end tests, can't read it.
func (p Project) ServiceContainer(ctx context.Context, service string) (ContainerDetails, error) {
	if err := p.validate(); err != nil {
		return ContainerDetails{}, err
	}
	out, err := p.docker()(ctx, nil, "container", "ls", "--all", "--no-trunc",
		"--filter", "label=com.docker.compose.project="+p.Name,
		"--filter", "label=com.docker.compose.service="+service,
		"--format", "{{.ID}}")
	if err != nil {
		return ContainerDetails{}, fmt.Errorf("compose project %s: %w", p.Name, err)
	}
	ids := strings.Fields(string(out))
	switch len(ids) {
	case 0:
		return ContainerDetails{}, fmt.Errorf("compose project %s: %s: %w", p.Name, service, ErrNoContainer)
	case 1:
	default:
		return ContainerDetails{}, fmt.Errorf("compose project %s: service %s has %d containers", p.Name, service, len(ids))
	}
	return InspectContainer(ctx, p.Docker, ids[0])
}

// ExecService runs a command with `docker exec` in the running container of
// one of the project's services, as the container's user, and returns its
// standard output. Like ServiceContainer it doesn't read the project
// directory, which Exec (`docker compose exec`) does. Arguments appear in
// error messages, so never pass secrets as arguments.
func (p Project) ExecService(ctx context.Context, service string, command ...string) ([]byte, error) {
	if len(command) == 0 {
		return nil, fmt.Errorf("%w: exec needs a command", ErrInvalidProject)
	}
	c, err := p.ServiceContainer(ctx, service)
	if err != nil {
		return nil, err
	}
	out, err := p.docker()(ctx, nil, append([]string{"container", "exec", c.ID}, command...)...)
	if err != nil {
		return out, fmt.Errorf("compose project %s: %w", p.Name, err)
	}
	return out, nil
}

// RunOptions configures RunContainer.
type RunOptions struct {
	// Image is the image to run. It must be available locally
	// (EnsureImage): a probe shouldn't stall on a pull.
	Image string
	// Network is the only network the container joins, e.g. a site's
	// network (SiteProjectName). Empty means Docker's default bridge.
	Network string
	// Entrypoint, if set, replaces the image's entrypoint; Command is
	// passed to it as arguments.
	Entrypoint string
	Command    []string
	// User defaults to 65534:65534 (nobody).
	User string
	// Stdin, if set, is connected to the container's standard input, e.g.
	// to hand it a file without mounting anything.
	Stdin io.Reader
}

// RunContainer runs a short-lived container and returns its standard
// output: a client on a site's network sees the site the way the platform's
// own services do, which the end-to-end tests rely on until the host can
// reach sites through Traefik. The container is locked down like a site
// container (non-root, read-only root filesystem, no capabilities, no new
// privileges) and removed when it exits. A nil docker means DockerExec.
func RunContainer(ctx context.Context, docker ExecFunc, opts RunOptions) ([]byte, error) {
	if docker == nil {
		docker = DockerExec
	}
	if opts.Image == "" {
		return nil, fmt.Errorf("%w: run needs an image", ErrInvalidProject)
	}
	user := opts.User
	if user == "" {
		user = "65534:65534"
	}
	args := []string{"container", "run", "--rm", "--user", user, "--read-only", "--tmpfs", "/tmp",
		"--cap-drop", "ALL", "--security-opt", "no-new-privileges:true", "--pull", "never"}
	if opts.Network != "" {
		args = append(args, "--network", opts.Network)
	}
	if opts.Entrypoint != "" {
		args = append(args, "--entrypoint", opts.Entrypoint)
	}
	if opts.Stdin != nil {
		args = append(args, "--interactive")
	}
	args = append(args, "--", opts.Image)
	out, err := docker(ctx, opts.Stdin, append(args, opts.Command...)...)
	if err != nil {
		return out, fmt.Errorf("running %s: %w", opts.Image, err)
	}
	return out, nil
}

// InspectContainer returns the details of a container, by name or ID. A nil
// docker means DockerExec.
func InspectContainer(ctx context.Context, docker ExecFunc, container string) (ContainerDetails, error) {
	if docker == nil {
		docker = DockerExec
	}
	out, err := docker(ctx, nil, "container", "inspect", "--", container)
	if err != nil {
		return ContainerDetails{}, fmt.Errorf("inspecting container %s: %w", container, err)
	}
	var infos []struct {
		ID     string `json:"Id"`
		Name   string `json:"Name"`
		Image  string `json:"Image"`
		Config struct {
			Image  string            `json:"Image"`
			Labels map[string]string `json:"Labels"`
		} `json:"Config"`
		State struct {
			Running bool `json:"Running"`
		} `json:"State"`
		NetworkSettings struct {
			Networks map[string]struct {
				IPAddress string `json:"IPAddress"`
			} `json:"Networks"`
		} `json:"NetworkSettings"`
		Mounts []Mount `json:"Mounts"`
	}
	if err := json.Unmarshal(out, &infos); err != nil {
		return ContainerDetails{}, fmt.Errorf("parsing inspect output for container %s: %w", container, err)
	}
	if len(infos) != 1 {
		return ContainerDetails{}, fmt.Errorf("inspecting container %s: got %d results", container, len(infos))
	}
	info := infos[0]
	d := ContainerDetails{
		ID:       info.ID,
		Name:     strings.TrimPrefix(info.Name, "/"),
		Image:    info.Config.Image,
		ImageID:  info.Image,
		Running:  info.State.Running,
		Networks: map[string]string{},
		Mounts:   info.Mounts,
		Labels:   info.Config.Labels,
	}
	for name, n := range info.NetworkSettings.Networks {
		d.Networks[name] = n.IPAddress
	}
	return d, nil
}

// HealthProbe is one run of a container's healthcheck.
type HealthProbe struct {
	Start    time.Time `json:"Start"`
	End      time.Time `json:"End"`
	ExitCode int       `json:"ExitCode"`
	// Output is what the probe printed, cut to maxProbeOutput bytes.
	Output string `json:"Output"`
}

// maxProbeOutput bounds the output kept per probe. Docker itself keeps at
// most 4 KiB of it.
const maxProbeOutput = 2048

// ContainerHealth is the state of a container that has a healthcheck, as
// far as explaining why it isn't healthy needs it.
type ContainerHealth struct {
	Name string
	// Status is the container's state: "running", "exited", "restarting".
	Status       string
	ExitCode     int
	OOMKilled    bool
	RestartCount int
	// Health is the healthcheck's status: "starting", "healthy", or
	// "unhealthy".
	Health        string
	FailingStreak int
	// Probes are the most recent runs of the healthcheck, oldest first
	// (Docker keeps five).
	Probes []HealthProbe
}

// Healthchecks reports the health of each container of the project (running
// or not) that has a healthcheck. Docker forgets a failed probe's output
// with the container, so a test that wants to know why a container never
// became healthy has to ask before the project is taken down. Only the
// container's state is read, never its configuration, which holds the
// environment and so secrets.
func (p Project) Healthchecks(ctx context.Context) ([]ContainerHealth, error) {
	if err := p.validate(); err != nil {
		return nil, err
	}
	out, err := p.docker()(ctx, nil, "container", "ls", "--all", "--no-trunc",
		"--filter", "label=com.docker.compose.project="+p.Name, "--format", "{{.ID}}")
	if err != nil {
		return nil, fmt.Errorf("compose project %s: %w", p.Name, err)
	}
	ids := strings.Fields(string(out))
	if len(ids) == 0 {
		return nil, nil
	}
	args := append([]string{"container", "inspect", "--format",
		`{"Name":{{json .Name}},"RestartCount":{{json .RestartCount}},"State":{{json .State}}}`, "--"}, ids...)
	out, err = p.docker()(ctx, nil, args...)
	if err != nil {
		return nil, fmt.Errorf("compose project %s: inspecting containers: %w", p.Name, err)
	}
	var all []ContainerHealth
	for line := range strings.Lines(string(out)) {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var info struct {
			Name         string
			RestartCount int
			State        struct {
				Status    string
				ExitCode  int
				OOMKilled bool
				Health    *struct {
					Status        string
					FailingStreak int
					Log           []HealthProbe
				}
			}
		}
		if err := json.Unmarshal([]byte(line), &info); err != nil {
			return nil, fmt.Errorf("compose project %s: parsing inspect output: %w", p.Name, err)
		}
		if info.State.Health == nil {
			continue
		}
		h := ContainerHealth{
			Name:          strings.TrimPrefix(info.Name, "/"),
			Status:        info.State.Status,
			ExitCode:      info.State.ExitCode,
			OOMKilled:     info.State.OOMKilled,
			RestartCount:  info.RestartCount,
			Health:        info.State.Health.Status,
			FailingStreak: info.State.Health.FailingStreak,
			Probes:        info.State.Health.Log,
		}
		for i := range h.Probes {
			if o := h.Probes[i].Output; len(o) > maxProbeOutput {
				h.Probes[i].Output = o[:maxProbeOutput] + "... (cut)"
			}
		}
		all = append(all, h)
	}
	return all, nil
}
