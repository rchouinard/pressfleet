package metrics

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Default locations of the host's procfs. The platform's Compose template
// mounts the host's /proc at [HostProcMount] in the control plane container:
// /proc/stat, /proc/meminfo and /proc/diskstats are not namespaced, but a
// container runtime or lxcfs may still virtualize them, and an explicit
// mount makes the source unambiguous.
const (
	DefaultProcDir = "/proc"
	HostProcMount  = "/host/proc"
)

// userHZ is the unit of /proc/stat's CPU times. It is 100 on every Linux
// architecture Docker supports, and what sysconf(_SC_CLK_TCK) returns.
const userHZ = 100

// diskSectorBytes is the unit of /proc/diskstats' sector counts, whatever
// the device's real sector size.
const diskSectorBytes = 512

// HostCollector reports the host's CPU, memory, filesystem, and disk
// metrics (design doc §7.2 Host): pressfleet_host_cpu_seconds_total (label
// mode), pressfleet_host_memory_total_bytes and _available_bytes,
// pressfleet_host_filesystem_size_bytes and _avail_bytes (label
// mountpoint), and pressfleet_host_disk_read_bytes_total and
// _written_bytes_total. They carry no site labels.
//
// Filesystem usage is measured on the data directory only: it is bind
// mounted from the host, so statfs there reports the host's filesystem, and
// it is where the state, backups, and uploads grow. Docker's own volumes
// live elsewhere and are not visible to the control plane.
type HostCollector struct {
	// Every is the collection interval; zero means the service default.
	Every time.Duration
	// ProcDir is the procfs to read; empty means [DefaultProcDir].
	ProcDir string
	// Mountpoints are the directories to report filesystem usage for. The
	// label value is the path as given.
	Mountpoints []string
	// Statfs reports the size and the space available to unprivileged users
	// of the filesystem holding path; nil means statfs(2). It is a field so
	// tests need no particular filesystem.
	Statfs func(path string) (size, avail uint64, err error)
}

var _ Collector = HostCollector{}

// Name implements [Collector].
func (HostCollector) Name() string { return "host" }

// Interval implements [Collector].
func (c HostCollector) Interval() time.Duration { return c.Every }

// Collect implements [Collector]. A source that can't be read is reported in
// the error while the others still produce samples.
func (c HostCollector) Collect(ctx context.Context, _ []Target) ([]Sample, error) {
	dir := c.ProcDir
	if dir == "" {
		dir = DefaultProcDir
	}
	var out []Sample
	var errs []error
	add := func(file string, parse func([]byte) ([]Sample, error)) {
		if ctx.Err() != nil {
			return
		}
		data, err := os.ReadFile(filepath.Join(dir, file)) //nolint:gosec // G304: a fixed file name under the configured procfs
		if err == nil {
			var s []Sample
			if s, err = parse(data); err == nil {
				out = append(out, s...)
				return
			}
		}
		errs = append(errs, fmt.Errorf("host metrics from %s: %w", file, err))
	}
	add("stat", parseCPUStat)
	add("meminfo", parseMeminfo)
	add("diskstats", parseDiskstats)

	statfs := c.Statfs
	if statfs == nil {
		statfs = statfsBytes
	}
	for _, mp := range c.Mountpoints {
		size, avail, err := statfs(mp)
		if err != nil {
			errs = append(errs, fmt.Errorf("host filesystem metrics for %s: %w", mp, err))
			continue
		}
		out = append(out,
			Sample{Name: "pressfleet_host_filesystem_size_bytes", Labels: map[string]string{"mountpoint": mp}, Value: float64(size)},
			Sample{Name: "pressfleet_host_filesystem_avail_bytes", Labels: map[string]string{"mountpoint": mp}, Value: float64(avail)},
		)
	}
	return out, errors.Join(errs...)
}

// statfsBytes implements HostCollector.Statfs with statfs(2).
func statfsBytes(path string) (size, avail uint64, err error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0, err
	}
	bs := uint64(st.Bsize) //nolint:gosec // G115: a block size is positive
	return st.Blocks * bs, st.Bavail * bs, nil
}

// cpuModes are the /proc/stat columns reported, in order. guest and
// guest_nice are left out: the kernel already counts them in user and nice.
var cpuModes = []string{"user", "nice", "system", "idle", "iowait", "irq", "softirq", "steal"}

// parseCPUStat reads the aggregate "cpu" line of /proc/stat, summed over all
// cores, into one counter per mode.
func parseCPUStat(data []byte) ([]Sample, error) {
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 0 || f[0] != "cpu" {
			continue
		}
		var out []Sample
		for i, mode := range cpuModes {
			// Kernels before 2.6.11 lack the later columns; ignore them.
			if i+1 >= len(f) {
				break
			}
			ticks, err := strconv.ParseUint(f[i+1], 10, 64)
			if err != nil {
				return nil, fmt.Errorf("cpu %s: %w", mode, err)
			}
			out = append(out, Sample{
				Name: "pressfleet_host_cpu_seconds_total", Labels: map[string]string{"mode": mode},
				Value: float64(ticks) / userHZ,
			})
		}
		return out, nil
	}
	return nil, errors.New("no cpu line")
}

// parseMeminfo reads MemTotal and MemAvailable, which /proc/meminfo reports
// in KiB.
func parseMeminfo(data []byte) ([]Sample, error) {
	want := map[string]string{
		"MemTotal":     "pressfleet_host_memory_total_bytes",
		"MemAvailable": "pressfleet_host_memory_available_bytes",
	}
	var out []Sample
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		key, rest, ok := strings.Cut(sc.Text(), ":")
		name, found := want[key]
		if !ok || !found {
			continue
		}
		f := strings.Fields(rest)
		if len(f) == 0 {
			return nil, fmt.Errorf("%s: no value", key)
		}
		kib, err := strconv.ParseUint(f[0], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", key, err)
		}
		out = append(out, Sample{Name: name, Value: float64(kib * 1024)})
	}
	if len(out) != len(want) {
		return nil, errors.New("MemTotal or MemAvailable missing")
	}
	return out, nil
}

// physicalDisk matches whole disks that are backed by hardware or a
// hypervisor. Partitions are excluded because their I/O is already counted
// in the disk's, and so are device-mapper, md, and loop devices because they
// sit on top of other devices and would count the same bytes twice.
var physicalDisk = regexp.MustCompile(`^(?:(?:s|v|xv|h)d[a-z]+|nvme\d+n\d+|mmcblk\d+)$`)

// parseDiskstats sums the bytes read and written over the physical disks.
// With none, it reports zero counters: a host with only virtual devices is
// valid, and the rates should read zero, not be absent.
func parseDiskstats(data []byte) ([]Sample, error) {
	var read, written uint64
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		// major minor name reads merged sectors-read ms writes merged sectors-written ...
		f := strings.Fields(sc.Text())
		if len(f) < 10 || !physicalDisk.MatchString(f[2]) {
			continue
		}
		r, err := strconv.ParseUint(f[5], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("%s sectors read: %w", f[2], err)
		}
		w, err := strconv.ParseUint(f[9], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("%s sectors written: %w", f[2], err)
		}
		read += r * diskSectorBytes
		written += w * diskSectorBytes
	}
	return []Sample{
		{Name: "pressfleet_host_disk_read_bytes_total", Value: float64(read)},
		{Name: "pressfleet_host_disk_written_bytes_total", Value: float64(written)},
	}, nil
}
