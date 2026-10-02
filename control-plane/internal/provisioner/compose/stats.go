package compose

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
)

// cgroupStatsScript prints the cgroup v2 values that describe a container's
// usage. The container's cgroup namespace is private, so its files sit at
// /sys/fs/cgroup inside it.
const cgroupStatsScript = "cat /sys/fs/cgroup/cpu.stat; " +
	"echo memory.current $(cat /sys/fs/cgroup/memory.current); " +
	"echo memory.max $(cat /sys/fs/cgroup/memory.max); " +
	"grep -E '^(inactive_file|total_inactive_file) ' /sys/fs/cgroup/memory.stat"

// Stats samples the site's running containers (provisioner.StatsReader).
//
// `docker stats` only reports CPU as a percentage over a short window, but
// the platform stores cumulative CPU time and derives rates itself, so the
// counters are read from the container's cgroup with `docker exec`, and
// the restart count from `docker inspect`. This needs cgroup v2 (the
// default on current hosts); a container that can't be read fails the call.
// Memory is the working set, as `docker stats` reports it: usage minus
// inactive file cache. Stopped containers are left out; a site that was
// never rendered yields provisioner.ErrSiteNotFound.
func (p *Provisioner) Stats(ctx context.Context, siteID string) ([]provisioner.ComponentStats, error) {
	proj, err := p.existing(ctx, siteID)
	if err != nil {
		return nil, fmt.Errorf("read stats: %w", err)
	}
	containers, err := proj.Ps(ctx)
	if err != nil {
		return nil, fmt.Errorf("read stats of site %s: %w", siteID, err)
	}
	var out []provisioner.ComponentStats
	for _, c := range containers {
		if c.State != "running" {
			continue
		}
		s, err := p.containerStats(ctx, c)
		if err != nil {
			return nil, fmt.Errorf("read stats of site %s: %w", siteID, err)
		}
		out = append(out, s)
	}
	return out, nil
}

func (p *Provisioner) containerStats(ctx context.Context, c Container) (provisioner.ComponentStats, error) {
	s := provisioner.ComponentStats{Component: provisioner.Component(c.Service)}
	raw, err := p.docker(ctx, nil, "container", "exec", c.Name, "sh", "-c", cgroupStatsScript)
	if err != nil {
		return s, fmt.Errorf("container %s: %w", c.Name, err)
	}
	s.SampledAt = time.Now()
	if err := parseCgroupStats(string(raw), &s); err != nil {
		return s, fmt.Errorf("container %s: %w", c.Name, err)
	}
	out, err := p.docker(ctx, nil, "container", "inspect", "--format", "{{.RestartCount}}", "--", c.Name)
	if err != nil {
		return s, fmt.Errorf("container %s: %w", c.Name, err)
	}
	text := strings.TrimSpace(string(out))
	s.Restarts, err = strconv.Atoi(text)
	if err != nil {
		return s, fmt.Errorf("container %s: parsing restart count %q: %w", c.Name, text, err)
	}
	return s, nil
}

// parseCgroupStats fills s from the output of cgroupStatsScript.
func parseCgroupStats(out string, s *provisioner.ComponentStats) error {
	var usage, inactive uint64
	var haveCPU, haveMem bool
	for line := range strings.SplitSeq(out, "\n") {
		key, val, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok {
			continue
		}
		if key == "memory.max" && val == "max" {
			continue // unlimited
		}
		n, err := strconv.ParseUint(strings.TrimSpace(val), 10, 64)
		if err != nil {
			return fmt.Errorf("parsing cgroup value %q: %w", line, err)
		}
		switch key {
		case "usage_usec":
			s.CPUUsage = time.Duration(min(n, math.MaxInt64/1000)) * time.Microsecond
			haveCPU = true
		case "memory.current":
			usage, haveMem = n, true
		case "memory.max":
			s.MemoryLimitBytes = n
		case "inactive_file", "total_inactive_file":
			inactive = n
		}
	}
	if !haveCPU || !haveMem {
		return fmt.Errorf("cgroup v2 usage files not found in %q", strings.TrimSpace(out))
	}
	s.MemoryBytes = usage
	if inactive < usage {
		s.MemoryBytes = usage - inactive
	}
	return nil
}
