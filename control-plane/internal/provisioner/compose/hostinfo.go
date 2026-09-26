package compose

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/netip"
	"os"
	"os/exec"
	"strings"
	"time"
)

// DefaultDaemonConfig is where dockerd reads daemon.json on Linux hosts.
const DefaultDaemonConfig = "/etc/docker/daemon.json"

// ErrDockerCLINotFound means the docker CLI is not installed or not on PATH.
var ErrDockerCLINotFound = errors.New("docker CLI not found")

// inspectTimeout bounds each docker CLI call, so a wedged daemon makes
// preflight fail instead of hang.
const inspectTimeout = 15 * time.Second

// AddressPool is one entry of the daemon's default-address-pools setting:
// networks are carved out of Base in subnets of prefix length Size.
type AddressPool struct {
	Base netip.Prefix
	Size int
}

// EngineInfo holds the Docker Engine facts that host preflight checks need.
type EngineInfo struct {
	ServerVersion string
	OSType        string // "linux" or "windows"
	Architecture  string // kernel architecture, e.g. "x86_64"
	CgroupVersion string // "1" or "2"
	// DefaultAddressPools is empty when the daemon uses Docker's built-in
	// pools; the Engine API only reports pools configured by the operator.
	DefaultAddressPools []AddressPool
	LoggingDriver       string

	// The Engine API doesn't expose default log options, so they come from
	// daemon.json. DaemonConfigFound is false when that file doesn't exist,
	// which is normal both on a stock install and inside a container.
	DaemonConfigPath  string
	DaemonConfigFound bool
	// DaemonConfigErr is set when daemon.json exists but can't be read or
	// parsed, e.g. because it is only readable by root.
	DaemonConfigErr error
	// LogOpts are daemon.json's default "log-opts".
	LogOpts map[string]string
}

// HostInspector reads Docker facts about the local host by running the docker
// CLI. The zero value uses "docker" from PATH and DefaultDaemonConfig.
type HostInspector struct {
	// Docker is the docker CLI to run; empty means "docker".
	Docker string
	// DaemonConfig is the daemon.json path; empty means DefaultDaemonConfig.
	DaemonConfig string
}

// Engine returns facts about the Docker Engine. It fails if the docker CLI
// is missing or the daemon is unreachable.
func (h HostInspector) Engine(ctx context.Context) (EngineInfo, error) {
	out, err := h.run(ctx, "info", "--format", "{{json .}}")
	if err != nil {
		return EngineInfo{}, err
	}
	info, err := parseEngineInfo(out)
	if err != nil {
		return EngineInfo{}, err
	}
	path := h.DaemonConfig
	if path == "" {
		path = DefaultDaemonConfig
	}
	info.DaemonConfigPath = path
	data, err := os.ReadFile(path) //nolint:gosec // G304: the path is the daemon.json location, not user input
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		info.DaemonConfigFound, info.DaemonConfigErr = true, err
	default:
		info.DaemonConfigFound = true
		info.LogOpts, info.DaemonConfigErr = parseDaemonLogOpts(data)
	}
	return info, nil
}

// ComposeVersion returns the Compose CLI plugin's version, e.g. "2.29.1".
// It fails if the plugin is not installed.
func (h HostInspector) ComposeVersion(ctx context.Context) (string, error) {
	out, err := h.run(ctx, "compose", "version", "--short")
	if err != nil {
		return "", err
	}
	return strings.TrimPrefix(strings.TrimSpace(string(out)), "v"), nil
}

func (h HostInspector) run(ctx context.Context, args ...string) ([]byte, error) {
	docker := h.Docker
	if docker == "" {
		docker = "docker"
	}
	ctx, cancel := context.WithTimeout(ctx, inspectTimeout)
	defer cancel()
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, docker, args...) //nolint:gosec // G204: fixed docker subcommands
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		// A bare name that isn't on PATH gives ErrNotFound; a missing
		// explicit path gives ErrNotExist.
		if errors.Is(err, exec.ErrNotFound) || errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("%w: %w", ErrDockerCLINotFound, err)
		}
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return nil, fmt.Errorf("docker %s: %w: %s", strings.Join(args, " "), err, msg)
		}
		return nil, fmt.Errorf("docker %s: %w", strings.Join(args, " "), err)
	}
	return stdout.Bytes(), nil
}

// parseEngineInfo decodes `docker info --format '{{json .}}'` output.
func parseEngineInfo(data []byte) (EngineInfo, error) {
	var raw struct {
		ServerVersion       string
		OSType              string
		Architecture        string
		CgroupVersion       string
		LoggingDriver       string
		ServerErrors        []string
		DefaultAddressPools []struct {
			Base string
			Size int
		}
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return EngineInfo{}, fmt.Errorf("parsing docker info: %w", err)
	}
	if len(raw.ServerErrors) > 0 {
		return EngineInfo{}, fmt.Errorf("docker info: %s", strings.Join(raw.ServerErrors, "; "))
	}
	info := EngineInfo{
		ServerVersion: raw.ServerVersion,
		OSType:        raw.OSType,
		Architecture:  raw.Architecture,
		CgroupVersion: raw.CgroupVersion,
		LoggingDriver: raw.LoggingDriver,
	}
	for _, p := range raw.DefaultAddressPools {
		base, err := netip.ParsePrefix(p.Base)
		if err != nil {
			return EngineInfo{}, fmt.Errorf("parsing docker info: address pool base: %w", err)
		}
		info.DefaultAddressPools = append(info.DefaultAddressPools, AddressPool{Base: base, Size: p.Size})
	}
	return info, nil
}

// parseDaemonLogOpts extracts "log-opts" from daemon.json.
func parseDaemonLogOpts(data []byte) (map[string]string, error) {
	var cfg struct {
		LogOpts map[string]string `json:"log-opts"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing daemon.json: %w", err)
	}
	return cfg.LogOpts, nil
}
