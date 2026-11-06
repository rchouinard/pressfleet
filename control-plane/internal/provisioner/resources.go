package provisioner

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// ContainerResources are one site container's share of the site's
// resources (ResourceLimits).
type ContainerResources struct {
	// CPUs is the CPU limit as a decimal number of CPUs, e.g. "0.75": a
	// Compose "cpus" value, and a Kubernetes CPU quantity as it is.
	CPUs                string
	MemoryLimitMB       int
	MemoryReservationMB int
}

// ResourceSplit is how a site's resources are divided between its
// containers, plus the PHP settings derived from php's share.
type ResourceSplit struct {
	Nginx, PHP       ContainerResources
	PHPMemoryLimitMB int // PHP_MEMORY_LIMIT
	PHPMaxChildren   int // PHP_FPM_MAX_CHILDREN, PHP-FPM's pm.max_children
}

// Constants of the resource split formulas (see SplitResources).
const (
	// phpOverheadMB is php's memory apart from its workers: the FPM
	// master, and OPcache's shared memory (PHP_OPCACHE_MEMORY_CONSUMPTION,
	// 64 MiB by default), which workers share rather than each hold.
	phpOverheadMB = 64
	// maxPHPMemoryLimitMB is PHP_MEMORY_LIMIT when php has room for it; it
	// is the image default and what wp-admin asks for (WP_MAX_MEMORY_LIMIT).
	maxPHPMemoryLimitMB = 256
	minPHPMaxChildren   = 2
	maxPHPMaxChildren   = 32
)

// SplitResources divides a site's resources between nginx and php, the same
// way for every driver (§4.1). nginx (one worker, static files, FastCGI
// proxying) needs little and its needs barely grow with the site, so it gets
// a small, bounded share and php the rest:
//
//	nginx memory limit       = clamp(limit/8, 32, 128) MiB
//	php memory limit         = limit - nginx memory limit
//	nginx memory reservation = max(clamp(reservation/8, 8, 32), reservation - php memory limit) MiB
//	php memory reservation   = reservation - nginx memory reservation
//	nginx CPU                = clamp(floor(cpu/4, 0.01), 0.1, 0.5)
//	php CPU                  = cpu - nginx CPU
//
// nginx's CPU is a whole number of hundredths because Docker's systemd cgroup
// driver (the default on most hosts) can only express a CPU limit in
// multiples of 10 ms per second and rounds every container's limit up to the
// next one. A share like 0.187 would run as 0.19, and 0.563 as 0.57: a site
// set to 0.75 CPUs would get 0.76. With nginx on a multiple of 0.01, php's
// share is one too whenever the site's total is, so the site gets exactly
// what was set. A total that is not a multiple of 0.01 (0.333) still gets
// rounded up by up to 0.01, in php's share alone.
//
// The second term of the nginx reservation only matters when the reservation
// is close to the limit; it keeps php's reservation within php's limit. The
// shares always add up to the site's totals exactly. For the defaults (1 CPU,
// 128 MiB reserved, 512 MiB limit) nginx gets 0.25 CPU, 16/64 MiB and php
// 0.75 CPU, 112/448 MiB.
//
// PHP settings follow from php's memory limit P:
//
//	PHP_MEMORY_LIMIT     = min(256, floor32(P - 64)) MiB
//	PHP_FPM_MAX_CHILDREN = clamp((P - 64) / max(PHP_MEMORY_LIMIT/4, 32), 2, 32)
//
// 64 MiB is the FPM master and OPcache. One request may use the whole
// memory_limit, but a typical WordPress worker uses about a quarter of it,
// so max_children sizes workers at a quarter (at least 32 MiB) instead of
// assuming every worker peaks at once. If they all do, the kernel's OOM
// killer ends a worker, which FPM replaces; the site stays up. Idle sites
// hold no workers (pm = ondemand). The default site gets PHP_MEMORY_LIMIT
// 256M and 6 children.
//
// Limits below the platform minimums (MinCPUMillis, MinMemoryLimitMB,
// MinMemoryReservationMB) are an error.
func SplitResources(r ResourceLimits) (ResourceSplit, error) {
	var errs []error
	cpu, err := ParseCPUMillis(r.CPU)
	if err != nil {
		errs = append(errs, err)
	} else if cpu < MinCPUMillis {
		errs = append(errs, fmt.Errorf("resources: CPU must be at least %s", FormatCPUMillis(MinCPUMillis)))
	}
	if r.MemoryLimitMB < MinMemoryLimitMB {
		errs = append(errs, fmt.Errorf("resources: memory limit must be at least %d MiB", MinMemoryLimitMB))
	}
	if r.MemoryReservationMB < MinMemoryReservationMB {
		errs = append(errs, fmt.Errorf("resources: memory reservation must be at least %d MiB", MinMemoryReservationMB))
	}
	if r.MemoryReservationMB > r.MemoryLimitMB {
		errs = append(errs, errors.New("resources: memory reservation must not exceed the limit"))
	}
	if err := errors.Join(errs...); err != nil {
		return ResourceSplit{}, err
	}

	var s ResourceSplit
	s.Nginx.MemoryLimitMB = clamp(r.MemoryLimitMB/8, 32, 128)
	s.PHP.MemoryLimitMB = r.MemoryLimitMB - s.Nginx.MemoryLimitMB
	s.Nginx.MemoryReservationMB = max(clamp(r.MemoryReservationMB/8, 8, 32), r.MemoryReservationMB-s.PHP.MemoryLimitMB)
	s.PHP.MemoryReservationMB = r.MemoryReservationMB - s.Nginx.MemoryReservationMB

	nginxCPU := clamp(cpu/4/cpuQuotaStepMillis*cpuQuotaStepMillis, 100, 500)
	s.Nginx.CPUs = FormatCPUMillis(nginxCPU)
	s.PHP.CPUs = FormatCPUMillis(cpu - nginxCPU)

	workerBudget := s.PHP.MemoryLimitMB - phpOverheadMB
	s.PHPMemoryLimitMB = min(maxPHPMemoryLimitMB, workerBudget/32*32)
	perWorker := max(s.PHPMemoryLimitMB/4, 32)
	s.PHPMaxChildren = clamp(workerBudget/perWorker, minPHPMaxChildren, maxPHPMaxChildren)
	return s, nil
}

// cpuQuotaStepMillis is the granularity, in thousandths of a CPU, of the CPU
// limits that Docker's systemd cgroup driver applies exactly (10 ms of CPU
// time per second).
const cpuQuotaStepMillis = 10

func clamp(v, lo, hi int) int { return min(max(v, lo), hi) }

// ParseCPUMillis parses a decimal CPU count ("1.0", "0.5", "2") into
// thousandths of a CPU.
func ParseCPUMillis(s string) (int, error) {
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) || f <= 0 || f > 1024 {
		return 0, fmt.Errorf("resources: CPU %q must be a positive decimal number of CPUs", s)
	}
	return int(math.Round(f * 1000)), nil
}

// FormatCPUMillis renders thousandths of a CPU as a decimal ("0.75", "1").
func FormatCPUMillis(m int) string {
	s := fmt.Sprintf("%d.%03d", m/1000, m%1000)
	return strings.TrimSuffix(strings.TrimRight(s, "0"), ".")
}
