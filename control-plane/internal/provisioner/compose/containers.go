package compose

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
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
	// ghcr.io/rchouinard/pressfleet/site-php:php8.4-wp7.1.2.
	Image   string
	Running bool
	// Networks maps each network the container is attached to to its IPv4
	// address there.
	Networks map[string]string
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
		Config struct {
			Image string `json:"Image"`
		} `json:"Config"`
		State struct {
			Running bool `json:"Running"`
		} `json:"State"`
		NetworkSettings struct {
			Networks map[string]struct {
				IPAddress string `json:"IPAddress"`
			} `json:"Networks"`
		} `json:"NetworkSettings"`
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
		Running:  info.State.Running,
		Networks: map[string]string{},
	}
	for name, n := range info.NetworkSettings.Networks {
		d.Networks[name] = n.IPAddress
	}
	return d, nil
}
