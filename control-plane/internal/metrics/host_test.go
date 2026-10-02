package metrics

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

const statFixture = `cpu  1000 50 500 8000 200 10 20 30 0 0
cpu0 500 25 250 4000 100 5 10 15 0 0
intr 12345
`

const meminfoFixture = `MemTotal:        2048000 kB
MemFree:          100000 kB
MemAvailable:    1024000 kB
Buffers:           1000 kB
`

const diskstatsFixture = `   7       0 loop0 10 0 100 0 0 0 0 0 0 0 0
   8       0 sda 100 0 2000 5 200 0 4000 9 0 0 0
   8       1 sda1 90 0 1800 4 190 0 3800 8 0 0 0
 259       0 nvme0n1 10 0 80 1 20 0 160 2 0 0 0
 259       1 nvme0n1p1 10 0 80 1 20 0 160 2 0 0 0
 253       0 dm-0 50 0 999 1 50 0 999 1 0 0 0
`

func values(samples []Sample) map[string]float64 {
	m := map[string]float64{}
	for _, s := range samples {
		key := s.Name
		for _, k := range []string{"mode", "mountpoint"} {
			if v, ok := s.Labels[k]; ok {
				key += "{" + v + "}"
			}
		}
		m[key] = s.Value
	}
	return m
}

func TestHostCollector(t *testing.T) {
	dir := t.TempDir()
	for name, content := range map[string]string{"stat": statFixture, "meminfo": meminfoFixture, "diskstats": diskstatsFixture} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	c := HostCollector{
		ProcDir:     dir,
		Mountpoints: []string{"/data"},
		Statfs: func(path string) (uint64, uint64, error) {
			if path != "/data" {
				t.Errorf("statfs path = %q", path)
			}
			return 1000, 250, nil
		},
	}
	samples, err := c.Collect(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	got := values(samples)
	want := map[string]float64{
		"pressfleet_host_cpu_seconds_total{user}":       10,
		"pressfleet_host_cpu_seconds_total{nice}":       0.5,
		"pressfleet_host_cpu_seconds_total{system}":     5,
		"pressfleet_host_cpu_seconds_total{idle}":       80,
		"pressfleet_host_cpu_seconds_total{iowait}":     2,
		"pressfleet_host_cpu_seconds_total{irq}":        0.1,
		"pressfleet_host_cpu_seconds_total{softirq}":    0.2,
		"pressfleet_host_cpu_seconds_total{steal}":      0.3,
		"pressfleet_host_memory_total_bytes":            2048000 * 1024,
		"pressfleet_host_memory_available_bytes":        1024000 * 1024,
		"pressfleet_host_filesystem_size_bytes{/data}":  1000,
		"pressfleet_host_filesystem_avail_bytes{/data}": 250,
		"pressfleet_host_disk_read_bytes_total":         (2000 + 80) * 512,
		"pressfleet_host_disk_written_bytes_total":      (4000 + 160) * 512,
	}
	if len(got) != len(want) {
		t.Errorf("got %d series, want %d: %v", len(got), len(want), got)
	}
	for k, v := range want {
		if g, ok := got[k]; !ok || g != v {
			t.Errorf("%s = %v (present %v), want %v", k, g, ok, v)
		}
	}
}

func TestHostCollectorPartialFailure(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "meminfo"), []byte(meminfoFixture), 0o600); err != nil {
		t.Fatal(err)
	}
	c := HostCollector{
		ProcDir:     dir,
		Mountpoints: []string{"/gone"},
		Statfs:      func(string) (uint64, uint64, error) { return 0, 0, errors.New("no such file") },
	}
	samples, err := c.Collect(context.Background(), nil)
	if err == nil {
		t.Fatal("want an error for the missing stat, diskstats, and mountpoint")
	}
	if got := values(samples); len(got) != 2 {
		t.Errorf("samples = %v, want only the memory ones", got)
	}
}

func TestParseHostErrors(t *testing.T) {
	if _, err := parseCPUStat([]byte("intr 1\n")); err == nil {
		t.Error("parseCPUStat without a cpu line: no error")
	}
	if _, err := parseCPUStat([]byte("cpu 1 x 3\n")); err == nil {
		t.Error("parseCPUStat with a bad number: no error")
	}
	if _, err := parseMeminfo([]byte("MemTotal: 1 kB\n")); err == nil {
		t.Error("parseMeminfo without MemAvailable: no error")
	}
	s, err := parseDiskstats([]byte("7 0 loop0 1 0 1 0 1 0 1 0 0 0 0\n"))
	if err != nil || len(s) != 2 || s[0].Value != 0 || s[1].Value != 0 {
		t.Errorf("parseDiskstats without physical disks = %v, %v; want two zero counters", s, err)
	}
}

func TestStatfsBytes(t *testing.T) {
	size, avail, err := statfsBytes(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if size == 0 || avail > size {
		t.Errorf("size = %d, avail = %d", size, avail)
	}
}
