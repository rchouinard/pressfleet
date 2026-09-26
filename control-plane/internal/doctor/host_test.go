package doctor

import (
	"path/filepath"
	"runtime"
	"testing"
)

func TestParseMemTotal(t *testing.T) {
	got, err := parseMemTotal([]byte("MemTotal:       16303740 kB\nMemFree:         1234 kB\n"))
	if err != nil || got != 16303740*1024 {
		t.Errorf("parseMemTotal() = %d, %v", got, err)
	}
	for _, bad := range []string{"", "MemFree: 1 kB\n", "MemTotal: lots kB\n"} {
		if _, err := parseMemTotal([]byte(bad)); err == nil {
			t.Errorf("parseMemTotal(%q) = nil error", bad)
		}
	}
}

func TestListening(t *testing.T) {
	table := []byte(`  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 00000000:0050 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 1 1 0000000000000000 100 0 0 10 0
   1: 0100007F:01BB 0100007F:9C40 01 00000000:00000000 00:00000000 00000000  1000        0 2 1 0000000000000000 20 4 30 10 -1
`)
	if !listening(table, 80) {
		t.Error("port 80 should be listening")
	}
	// 443 appears only as an established connection, not a listener.
	if listening(table, 443) {
		t.Error("port 443 should not be listening")
	}
	if listening(table, 8080) {
		t.Error("port 8080 should not be listening")
	}
}

func TestKernelArch(t *testing.T) {
	for in, want := range map[string]string{"x86_64": "amd64", "aarch64": "arm64", "i686": "386", "riscv64": "riscv64"} {
		if got := kernelArch(in); got != want {
			t.Errorf("kernelArch(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestSystemFreeDisk walks up from a directory that doesn't exist yet, as
// the data directory doesn't before installation.
func TestSystemFreeDisk(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("free disk space is only checked on Linux")
	}
	free, err := System{}.FreeDisk(filepath.Join(t.TempDir(), "not", "yet"))
	if err != nil || free == 0 {
		t.Errorf("FreeDisk() = %d, %v", free, err)
	}
}
