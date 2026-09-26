package doctor

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// System is the HostProbe for the machine it runs on. It reads /proc rather
// than binding ports or shelling out, so it needs neither root nor external
// tools.
type System struct{}

var _ HostProbe = System{}

// Platform returns runtime.GOOS and the kernel's architecture. The binary is
// only built for amd64, but the kernel's view catches emulated runs.
func (System) Platform() (goos, goarch string) {
	goarch = runtime.GOARCH
	if b, err := os.ReadFile("/proc/sys/kernel/arch"); err == nil {
		goarch = kernelArch(strings.TrimSpace(string(b)))
	}
	return runtime.GOOS, goarch
}

// kernelArch maps uname machine names to GOARCH names.
func kernelArch(machine string) string {
	switch machine {
	case "x86_64":
		return "amd64"
	case "aarch64":
		return "arm64"
	case "i386", "i686":
		return "386"
	}
	return machine
}

// MemTotal reads MemTotal from /proc/meminfo.
func (System) MemTotal() (uint64, error) {
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, err
	}
	return parseMemTotal(b)
}

func parseMemTotal(meminfo []byte) (uint64, error) {
	sc := bufio.NewScanner(bytes.NewReader(meminfo))
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) == 3 && fields[0] == "MemTotal:" && fields[2] == "kB" {
			kb, err := strconv.ParseUint(fields[1], 10, 64)
			if err != nil {
				return 0, fmt.Errorf("parsing MemTotal: %w", err)
			}
			return kb * 1024, nil
		}
	}
	return 0, errors.New("MemTotal not found in /proc/meminfo")
}

// FreeDisk reports free space on the filesystem holding path. The data
// directory may not exist before installation, so it falls back to the
// nearest existing parent.
func (System) FreeDisk(path string) (uint64, error) {
	path = filepath.Clean(path)
	for {
		_, err := os.Stat(path)
		if err == nil {
			return freeDisk(path)
		}
		parent := filepath.Dir(path)
		if !errors.Is(err, fs.ErrNotExist) || parent == path {
			return 0, err
		}
		path = parent
	}
}

// PortInUse reports whether a TCP socket listens on port, from
// /proc/net/tcp and /proc/net/tcp6. Binding the port instead would need
// root for ports below 1024.
func (System) PortInUse(port int) (bool, error) {
	var errs []error
	for _, f := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		b, err := os.ReadFile(f) //nolint:gosec // G304: fixed /proc paths
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if listening(b, port) {
			return true, nil
		}
	}
	if len(errs) == 2 {
		return false, errors.Join(errs...)
	}
	return false, nil
}

// tcpListen is the socket state code for LISTEN in /proc/net/tcp.
const tcpListen = "0A"

// listening reports whether a /proc/net/tcp table has a listener on port.
func listening(table []byte, port int) bool {
	want := fmt.Sprintf(":%04X", port)
	sc := bufio.NewScanner(bytes.NewReader(table))
	for sc.Scan() {
		// sl local_address rem_address st ...
		fields := strings.Fields(sc.Text())
		if len(fields) > 3 && fields[3] == tcpListen && strings.HasSuffix(fields[1], want) {
			return true
		}
	}
	return false
}

// LookupHost resolves host with the system resolver.
func (System) LookupHost(ctx context.Context, host string) ([]netip.Addr, error) {
	return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
}

// InterfaceAddrs returns the addresses of all network interfaces.
func (System) InterfaceAddrs() ([]netip.Addr, error) {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil, err
	}
	var out []netip.Addr
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok {
			if ip, ok := netip.AddrFromSlice(n.IP); ok {
				out = append(out, ip.Unmap())
			}
		}
	}
	return out, nil
}
