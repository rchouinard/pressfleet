// Package doctortest provides fake host and Docker probes for preflight
// tests.
package doctortest

import (
	"context"
	"fmt"
	"net/netip"
	"strings"

	"github.com/rchouinard/pressfleet/control-plane/internal/doctor"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner/compose"
)

// Host is a fake doctor.HostProbe. Healthy returns one that passes every
// check; tests change individual fields.
type Host struct {
	GOOS, GOARCH string
	Mem          uint64
	MemErr       error
	Free         uint64
	FreeErr      error
	// Ports lists ports in use.
	Ports   map[int]bool
	PortErr error
	// DNS maps names to addresses. A key starting with "*." matches any
	// name under that domain.
	DNS      map[string][]netip.Addr
	Addrs    []netip.Addr
	AddrsErr error
}

var _ doctor.HostProbe = (*Host)(nil)

// Healthy returns a Host that satisfies every check: linux/amd64, 16 GiB of
// memory, 100 GiB free, free ports, and interface address 203.0.113.10.
func Healthy() *Host {
	return &Host{
		GOOS: "linux", GOARCH: "amd64",
		Mem:   16 << 30,
		Free:  100 << 30,
		Addrs: []netip.Addr{netip.MustParseAddr("127.0.0.1"), netip.MustParseAddr("203.0.113.10")},
		DNS:   map[string][]netip.Addr{},
	}
}

// Platform implements doctor.HostProbe.
func (h *Host) Platform() (goos, goarch string) { return h.GOOS, h.GOARCH }

// MemTotal implements doctor.HostProbe.
func (h *Host) MemTotal() (uint64, error) { return h.Mem, h.MemErr }

// FreeDisk implements doctor.HostProbe.
func (h *Host) FreeDisk(string) (uint64, error) { return h.Free, h.FreeErr }

// PortInUse implements doctor.HostProbe.
func (h *Host) PortInUse(port int) (bool, error) { return h.Ports[port], h.PortErr }

// LookupHost implements doctor.HostProbe.
func (h *Host) LookupHost(_ context.Context, host string) ([]netip.Addr, error) {
	if a, ok := h.DNS[host]; ok {
		return a, nil
	}
	for k, a := range h.DNS {
		if suffix, ok := strings.CutPrefix(k, "*"); ok && strings.HasSuffix(host, suffix) {
			return a, nil
		}
	}
	return nil, fmt.Errorf("lookup %s: no such host", host)
}

// InterfaceAddrs implements doctor.HostProbe.
func (h *Host) InterfaceAddrs() ([]netip.Addr, error) { return h.Addrs, h.AddrsErr }

// Docker is a fake doctor.DockerProbe.
type Docker struct {
	Info       compose.EngineInfo
	InfoErr    error
	Compose    string
	ComposeErr error
	// EngineCalls counts Engine calls.
	EngineCalls int
}

var _ doctor.DockerProbe = (*Docker)(nil)

// HealthyDocker returns a Docker that satisfies every check: Engine 27 with
// Compose 2.29, cgroup v2, recommended address pools, and log rotation.
func HealthyDocker() *Docker {
	return &Docker{
		Info: compose.EngineInfo{
			ServerVersion: "28.5.1", OSType: "linux", Architecture: "x86_64",
			CgroupVersion: "2",
			DefaultAddressPools: []compose.AddressPool{
				{Base: netip.MustParsePrefix("10.200.0.0/16"), Size: 24},
			},
			LoggingDriver:     "json-file",
			DaemonConfigPath:  compose.DefaultDaemonConfig,
			DaemonConfigFound: true,
			LogOpts:           map[string]string{"max-size": "10m", "max-file": "3"},
		},
		Compose: "2.29.1",
	}
}

// Engine implements doctor.DockerProbe.
func (d *Docker) Engine(context.Context) (compose.EngineInfo, error) {
	d.EngineCalls++
	return d.Info, d.InfoErr
}

// ComposeVersion implements doctor.DockerProbe.
func (d *Docker) ComposeVersion(context.Context) (string, error) { return d.Compose, d.ComposeErr }

// Reservations is a fake doctor.ReservationProvider.
type Reservations struct {
	Total uint64
	Sites int
	Err   error
}

// MemoryReservations implements doctor.ReservationProvider.
func (r Reservations) MemoryReservations(context.Context) (totalBytes uint64, sites int, err error) {
	return r.Total, r.Sites, r.Err
}

// WebAuthn is a fake doctor.WebAuthnProvider.
type WebAuthn struct {
	RPID        string
	Credentials int
	Users       int
	Err         error
}

// StaleWebAuthnCredentials implements doctor.WebAuthnProvider.
func (w WebAuthn) StaleWebAuthnCredentials(context.Context) (rpID string, credentials, users int, err error) {
	return w.RPID, w.Credentials, w.Users, w.Err
}
