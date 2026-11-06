package provisioner

import (
	"strings"
	"testing"
)

func TestSplitResources(t *testing.T) {
	tests := []struct {
		name                 string
		cpu                  string
		reservation, limit   int
		nginx, php           ContainerResources
		phpMemoryMB, workers int
	}{
		{
			name: "platform default", cpu: "1.0", reservation: 128, limit: 512,
			nginx: ContainerResources{"0.25", 64, 16}, php: ContainerResources{"0.75", 448, 112},
			phpMemoryMB: 256, workers: 6,
		},
		{
			name: "smallest site", cpu: "0.25", reservation: 32, limit: 256,
			nginx: ContainerResources{"0.1", 32, 8}, php: ContainerResources{"0.15", 224, 24},
			phpMemoryMB: 160, workers: 4,
		},
		{
			name: "reservation equals limit", cpu: "2", reservation: 512, limit: 512,
			nginx: ContainerResources{"0.5", 64, 64}, php: ContainerResources{"1.5", 448, 448},
			phpMemoryMB: 256, workers: 6,
		},
		{
			name: "1 GiB", cpu: "1.5", reservation: 256, limit: 1024,
			nginx: ContainerResources{"0.37", 128, 32}, php: ContainerResources{"1.13", 896, 224},
			phpMemoryMB: 256, workers: 13,
		},
		{
			name: "large site hits the caps", cpu: "4", reservation: 1024, limit: 4096,
			nginx: ContainerResources{"0.5", 128, 32}, php: ContainerResources{"3.5", 3968, 992},
			phpMemoryMB: 256, workers: 32,
		},
		{
			name: "0.75 CPUs", cpu: "0.75", reservation: 192, limit: 640,
			nginx: ContainerResources{"0.18", 80, 24}, php: ContainerResources{"0.57", 560, 168},
			phpMemoryMB: 256, workers: 7,
		},
		{
			name: "odd sizes", cpu: "0.333", reservation: 100, limit: 300,
			nginx: ContainerResources{"0.1", 37, 12}, php: ContainerResources{"0.233", 263, 88},
			phpMemoryMB: 192, workers: 4,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, err := SplitResources(ResourceLimits{CPU: tt.cpu, MemoryReservationMB: tt.reservation, MemoryLimitMB: tt.limit})
			if err != nil {
				t.Fatal(err)
			}
			if s.Nginx != tt.nginx || s.PHP != tt.php {
				t.Errorf("nginx = %+v, php = %+v; want %+v, %+v", s.Nginx, s.PHP, tt.nginx, tt.php)
			}
			if s.PHPMemoryLimitMB != tt.phpMemoryMB || s.PHPMaxChildren != tt.workers {
				t.Errorf("PHP_MEMORY_LIMIT = %d, max_children = %d; want %d, %d",
					s.PHPMemoryLimitMB, s.PHPMaxChildren, tt.phpMemoryMB, tt.workers)
			}
			checkSplitInvariants(t, s, tt.reservation, tt.limit)
		})
	}
}

// TestSplitResourcesInvariants sweeps valid inputs: the shares add up, every
// reservation fits its limit, and one worker at the full memory_limit fits
// next to the overhead.
func TestSplitResourcesInvariants(t *testing.T) {
	for limit := MinMemoryLimitMB; limit <= 8192; limit += 37 {
		for reservation := MinMemoryReservationMB; reservation <= limit; reservation += 29 {
			s, err := SplitResources(ResourceLimits{CPU: "1", MemoryReservationMB: reservation, MemoryLimitMB: limit})
			if err != nil {
				t.Fatalf("%d/%d: %v", reservation, limit, err)
			}
			checkSplitInvariants(t, s, reservation, limit)
		}
	}
	for cpu := MinCPUMillis; cpu <= 16000; cpu += 13 {
		s, err := SplitResources(ResourceLimits{CPU: FormatCPUMillis(cpu), MemoryReservationMB: 128, MemoryLimitMB: 512})
		if err != nil {
			t.Fatalf("cpu %d: %v", cpu, err)
		}
		n, _ := ParseCPUMillis(s.Nginx.CPUs)
		p, _ := ParseCPUMillis(s.PHP.CPUs)
		// Systemd rounds each container's limit up to a multiple of 10 ms:
		// together they must not get more than the site's total rounded up.
		if up := func(m int) int { return (m + 9) / 10 * 10 }; up(n)+up(p) != up(cpu) {
			t.Errorf("cpu %d: nginx %s + php %s are rounded up to more than the site's limit", cpu, s.Nginx.CPUs, s.PHP.CPUs)
		}
		if n+p != cpu || n < 100 || p < n/2 {
			t.Errorf("cpu %d: nginx %s + php %s", cpu, s.Nginx.CPUs, s.PHP.CPUs)
		}
	}
}

func checkSplitInvariants(t *testing.T, s ResourceSplit, reservation, limit int) {
	t.Helper()
	if s.Nginx.MemoryLimitMB+s.PHP.MemoryLimitMB != limit ||
		s.Nginx.MemoryReservationMB+s.PHP.MemoryReservationMB != reservation {
		t.Errorf("%d/%d: shares don't add up: %+v", reservation, limit, s)
	}
	for _, c := range []ContainerResources{s.Nginx, s.PHP} {
		if c.MemoryReservationMB <= 0 || c.MemoryReservationMB > c.MemoryLimitMB {
			t.Errorf("%d/%d: reservation outside limit: %+v", reservation, limit, c)
		}
	}
	if s.PHPMemoryLimitMB < 128 || s.PHPMemoryLimitMB+phpOverheadMB > s.PHP.MemoryLimitMB {
		t.Errorf("%d/%d: PHP_MEMORY_LIMIT %d doesn't fit php's %d MiB", reservation, limit, s.PHPMemoryLimitMB, s.PHP.MemoryLimitMB)
	}
	if s.PHPMaxChildren < minPHPMaxChildren || s.PHPMaxChildren > maxPHPMaxChildren {
		t.Errorf("%d/%d: max_children %d out of bounds", reservation, limit, s.PHPMaxChildren)
	}
}

func TestSplitResourcesInvalid(t *testing.T) {
	tests := []struct {
		name string
		r    ResourceLimits
		want string
	}{
		{"cpu not a number", ResourceLimits{CPU: "one", MemoryReservationMB: 128, MemoryLimitMB: 512}, "positive decimal"},
		{"cpu negative", ResourceLimits{CPU: "-1", MemoryReservationMB: 128, MemoryLimitMB: 512}, "positive decimal"},
		{"cpu NaN", ResourceLimits{CPU: "NaN", MemoryReservationMB: 128, MemoryLimitMB: 512}, "positive decimal"},
		{"cpu too small", ResourceLimits{CPU: "0.2", MemoryReservationMB: 128, MemoryLimitMB: 512}, "at least 0.25"},
		{"limit too small", ResourceLimits{CPU: "1", MemoryReservationMB: 128, MemoryLimitMB: 192}, "at least 256 MiB"},
		{"reservation too small", ResourceLimits{CPU: "1", MemoryReservationMB: 16, MemoryLimitMB: 512}, "at least 32 MiB"},
		{"reservation above limit", ResourceLimits{CPU: "1", MemoryReservationMB: 1024, MemoryLimitMB: 512}, "must not exceed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := SplitResources(tt.r)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want it to mention %q", err, tt.want)
			}
		})
	}
}

func TestFormatCPUMillis(t *testing.T) {
	for m, want := range map[int]string{1000: "1", 750: "0.75", 250: "0.25", 100: "0.1", 1125: "1.125", 16000: "16", 5: "0.005"} {
		if got := FormatCPUMillis(m); got != want {
			t.Errorf("FormatCPUMillis(%d) = %q, want %q", m, got, want)
		}
	}
}
