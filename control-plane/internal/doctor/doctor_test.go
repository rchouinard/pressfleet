package doctor_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/netip"
	"strings"
	"testing"

	"github.com/rchouinard/pressfleet/control-plane/internal/doctor"
	"github.com/rchouinard/pressfleet/control-plane/internal/doctor/doctortest"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner/compose"
)

const gib = 1 << 30

var (
	hostIP   = netip.MustParseAddr("203.0.113.10")
	publicIP = netip.MustParseAddr("198.51.100.20")
	otherIP  = netip.MustParseAddr("192.0.2.99")
)

// fixture is a healthy setup that a test case breaks in one way.
type fixture struct {
	host   *doctortest.Host
	docker *doctortest.Docker
	opts   doctor.Options
}

func newFixture() *fixture {
	return &fixture{
		host:   doctortest.Healthy(),
		docker: doctortest.HealthyDocker(),
		opts:   doctor.Options{DataDir: "/var/lib/pressfleet"},
	}
}

func (f *fixture) run(t *testing.T, id string) doctor.Result {
	t.Helper()
	f.opts.Host, f.opts.Docker = f.host, f.docker
	rep := doctor.Run(context.Background(), f.opts, doctor.ScopeInstall, doctor.ScopeRuntime)
	for _, r := range rep.Results {
		if r.ID == id {
			return r
		}
	}
	t.Fatalf("no result for check %q", id)
	return doctor.Result{}
}

type checkCase struct {
	name        string
	setup       func(*fixture)
	wantStatus  doctor.Status
	wantMessage string // substring
	wantFix     string // substring
}

func runCases(t *testing.T, id string, cases []checkCase) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture()
			if tc.setup != nil {
				tc.setup(f)
			}
			r := f.run(t, id)
			if r.Status != tc.wantStatus {
				t.Errorf("status = %s, want %s (message %q)", r.Status, tc.wantStatus, r.Message)
			}
			if !strings.Contains(r.Message, tc.wantMessage) {
				t.Errorf("message = %q, want it to contain %q", r.Message, tc.wantMessage)
			}
			if !strings.Contains(r.Fix, tc.wantFix) {
				t.Errorf("fix = %q, want it to contain %q", r.Fix, tc.wantFix)
			}
			// Acceptance criterion: every warning and failure says how to fix it.
			if (r.Status == doctor.StatusFail || r.Status == doctor.StatusWarn) && r.Fix == "" {
				t.Errorf("%s result has no fix hint", r.Status)
			}
		})
	}
}

func TestPlatform(t *testing.T) {
	runCases(t, "platform", []checkCase{
		{name: "linux amd64", wantStatus: doctor.StatusPass, wantMessage: "linux/amd64"},
		{name: "arm64", setup: func(f *fixture) { f.host.GOARCH = "arm64" }, wantStatus: doctor.StatusFail, wantMessage: "linux/arm64", wantFix: "amd64"},
		{name: "macOS", setup: func(f *fixture) { f.host.GOOS = "darwin" }, wantStatus: doctor.StatusFail, wantMessage: "darwin"},
	})
}

func TestDocker(t *testing.T) {
	runCases(t, "docker", []checkCase{
		{name: "reachable", wantStatus: doctor.StatusPass, wantMessage: "27.3.1"},
		{name: "unreachable", setup: func(f *fixture) { f.docker.InfoErr = errors.New("permission denied") },
			wantStatus: doctor.StatusFail, wantMessage: "permission denied", wantFix: "docker group"},
		{name: "windows containers", setup: func(f *fixture) { f.docker.Info.OSType = "windows" },
			wantStatus: doctor.StatusFail, wantMessage: "windows", wantFix: "Linux containers"},
	})
}

func TestCompose(t *testing.T) {
	runCases(t, "compose", []checkCase{
		{name: "v2", wantStatus: doctor.StatusPass, wantMessage: "2.29.1"},
		{name: "v5", setup: func(f *fixture) { f.docker.Compose = "5.5.1" }, wantStatus: doctor.StatusPass},
		{name: "missing", setup: func(f *fixture) { f.docker.ComposeErr = errors.New("'compose' is not a docker command") },
			wantStatus: doctor.StatusFail, wantMessage: "not available", wantFix: "docker-compose-plugin"},
		{name: "v1", setup: func(f *fixture) { f.docker.Compose = "1.29.2" },
			wantStatus: doctor.StatusFail, wantMessage: "too old", wantFix: "v2"},
		{name: "garbage version", setup: func(f *fixture) { f.docker.Compose = "dev" }, wantStatus: doctor.StatusFail},
	})
}

func TestCgroup(t *testing.T) {
	runCases(t, "cgroup", []checkCase{
		{name: "v2", wantStatus: doctor.StatusPass},
		{name: "v1", setup: func(f *fixture) { f.docker.Info.CgroupVersion = "1" },
			wantStatus: doctor.StatusFail, wantMessage: "cgroup v1", wantFix: "systemd.unified_cgroup_hierarchy=1"},
		{name: "docker unreachable", setup: func(f *fixture) { f.docker.InfoErr = errors.New("down") },
			wantStatus: doctor.StatusSkip, wantMessage: "not reachable"},
	})
}

func pool(base string, size int) compose.AddressPool {
	return compose.AddressPool{Base: netip.MustParsePrefix(base), Size: size}
}

func TestAddressPools(t *testing.T) {
	setPools := func(p ...compose.AddressPool) func(*fixture) {
		return func(f *fixture) { f.docker.Info.DefaultAddressPools = p }
	}
	runCases(t, "address-pools", []checkCase{
		{name: "recommended", wantStatus: doctor.StatusPass, wantMessage: "256 networks"},
		{name: "several pools", setup: setPools(pool("10.200.0.0/17", 24), pool("10.201.0.0/17", 24)),
			wantStatus: doctor.StatusPass, wantMessage: "256 networks"},
		{name: "Docker defaults", setup: setPools(),
			wantStatus: doctor.StatusFail, wantMessage: "default address pools", wantFix: `"default-address-pools"`},
		{name: "Docker defaults written out", setup: setPools(pool("172.17.0.0/16", 16), pool("172.20.0.0/14", 16), pool("192.168.0.0/16", 20)),
			wantStatus: doctor.StatusFail, wantMessage: "21 networks", wantFix: "10.200.0.0/16"},
		{name: "too few", setup: setPools(pool("10.200.0.0/19", 24)),
			wantStatus: doctor.StatusFail, wantMessage: "32 networks", wantFix: "daemon.json"},
		{name: "minimum", setup: setPools(pool("10.200.0.0/18", 24)),
			wantStatus: doctor.StatusWarn, wantMessage: "64 networks"},
		{name: "marginal", setup: setPools(pool("10.200.0.0/17", 24)),
			wantStatus: doctor.StatusWarn, wantMessage: "128 networks", wantFix: "daemon.json"},
		{name: "huge IPv6 pool", setup: setPools(pool("fd00::/48", 64)),
			wantStatus: doctor.StatusPass, wantMessage: "65536 networks"},
		{name: "IPv6 subnets too small", setup: setPools(pool("fd00::/48", 126)),
			wantStatus: doctor.StatusFail, wantMessage: "/126"},
		{name: "subnets too small", setup: setPools(pool("10.200.0.0/16", 30)),
			wantStatus: doctor.StatusFail, wantMessage: "/30", wantFix: "daemon.json"},
		{name: "size shorter than base", setup: setPools(pool("10.200.0.0/24", 16)),
			wantStatus: doctor.StatusFail, wantMessage: "/16"},
		{name: "docker unreachable", setup: func(f *fixture) { f.docker.InfoErr = errors.New("down") },
			wantStatus: doctor.StatusSkip},
	})
}

func TestPorts(t *testing.T) {
	runCases(t, "ports", []checkCase{
		{name: "free", wantStatus: doctor.StatusPass},
		{name: "80 busy", setup: func(f *fixture) { f.host.Ports = map[int]bool{80: true} },
			wantStatus: doctor.StatusFail, wantMessage: "port 80 in use", wantFix: "ss -ltnp"},
		{name: "both busy", setup: func(f *fixture) { f.host.Ports = map[int]bool{80: true, 443: true} },
			wantStatus: doctor.StatusFail, wantMessage: "80 and 443"},
		{name: "cannot check", setup: func(f *fixture) { f.host.PortErr = errors.New("no /proc") },
			wantStatus: doctor.StatusWarn, wantMessage: "could not check", wantFix: "ss -ltn"},
	})
}

func TestDashboardDNS(t *testing.T) {
	withDomain := func(addrs ...netip.Addr) func(*fixture) {
		return func(f *fixture) {
			f.opts.DashboardDomain = "fleet.example.com"
			if addrs != nil {
				f.host.DNS["fleet.example.com"] = addrs
			}
		}
	}
	runCases(t, "dns-dashboard", []checkCase{
		{name: "unset", wantStatus: doctor.StatusSkip, wantMessage: "dashboard_domain is not set"},
		{name: "does not resolve", setup: withDomain(),
			wantStatus: doctor.StatusFail, wantMessage: "does not resolve", wantFix: "A (or AAAA) record for fleet.example.com"},
		{name: "resolves to interface", setup: withDomain(otherIP, hostIP),
			wantStatus: doctor.StatusPass, wantMessage: "203.0.113.10 (a local interface address)"},
		{name: "resolves elsewhere, no public_ip", setup: withDomain(otherIP),
			wantStatus: doctor.StatusWarn, wantMessage: "could not verify", wantFix: "public_ip"},
		{name: "interface addresses unavailable", setup: func(f *fixture) {
			withDomain(hostIP)(f)
			f.host.AddrsErr = errors.New("boom")
		}, wantStatus: doctor.StatusWarn},
		{name: "loopback", setup: withDomain(netip.MustParseAddr("127.0.0.1")),
			wantStatus: doctor.StatusFail, wantMessage: "loopback"},
		{name: "matches public_ip", setup: func(f *fixture) {
			withDomain(publicIP)(f)
			f.opts.PublicIP = publicIP
		}, wantStatus: doctor.StatusPass, wantMessage: "(public_ip)"},
		{name: "IPv4-mapped answer matches public_ip", setup: func(f *fixture) {
			withDomain(netip.AddrFrom16(publicIP.As16()))(f)
			f.opts.PublicIP = publicIP
		}, wantStatus: doctor.StatusPass},
		{name: "mismatches public_ip", setup: func(f *fixture) {
			withDomain(hostIP)(f)
			f.opts.PublicIP = publicIP
		}, wantStatus: doctor.StatusFail, wantMessage: "not this host's public_ip 198.51.100.20", wantFix: "correct public_ip"},
	})
}

func TestPreviewDNS(t *testing.T) {
	runCases(t, "dns-preview", []checkCase{
		{name: "unset", wantStatus: doctor.StatusSkip, wantMessage: "preview_base_domain is not set"},
		{name: "wildcard resolves", setup: func(f *fixture) {
			f.opts.PreviewBaseDomain = "preview.example.com"
			f.host.DNS["*.preview.example.com"] = []netip.Addr{hostIP}
		}, wantStatus: doctor.StatusPass, wantMessage: "*.preview.example.com resolves to 203.0.113.10"},
		{name: "only the base resolves", setup: func(f *fixture) {
			f.opts.PreviewBaseDomain = "preview.example.com"
			f.host.DNS["preview.example.com"] = []netip.Addr{hostIP}
		}, wantStatus: doctor.StatusFail, wantMessage: "*.preview.example.com does not resolve", wantFix: "wildcard A"},
	})

	t.Run("probes a random label", func(t *testing.T) {
		f := newFixture()
		f.opts.PreviewBaseDomain = "preview.example.com"
		f.opts.RandomLabel = func() string { return "probe" }
		f.host.DNS["probe.preview.example.com"] = []netip.Addr{hostIP}
		if r := f.run(t, "dns-preview"); r.Status != doctor.StatusPass {
			t.Errorf("status = %s (%s)", r.Status, r.Message)
		}
	})
}

func TestLogRotation(t *testing.T) {
	runCases(t, "log-rotation", []checkCase{
		{name: "max-size set", wantStatus: doctor.StatusPass, wantMessage: "10m (3 files)"},
		{name: "local driver", setup: func(f *fixture) {
			f.docker.Info.LoggingDriver = "local"
			f.docker.Info.LogOpts = nil
		}, wantStatus: doctor.StatusPass, wantMessage: "local log driver"},
		{name: "journald", setup: func(f *fixture) { f.docker.Info.LoggingDriver = "journald" },
			wantStatus: doctor.StatusPass, wantMessage: "journald"},
		{name: "no daemon.json", setup: func(f *fixture) {
			f.docker.Info.DaemonConfigFound = false
			f.docker.Info.LogOpts = nil
		}, wantStatus: doctor.StatusWarn, wantMessage: "does not exist", wantFix: `"max-size": "10m"`},
		{name: "daemon.json without log-opts", setup: func(f *fixture) { f.docker.Info.LogOpts = map[string]string{} },
			wantStatus: doctor.StatusWarn, wantMessage: "does not set log-opts", wantFix: "max-size"},
		{name: "daemon.json unreadable", setup: func(f *fixture) {
			f.docker.Info.LogOpts = nil
			f.docker.Info.DaemonConfigErr = errors.New("permission denied")
		}, wantStatus: doctor.StatusWarn, wantMessage: "permission denied", wantFix: "root"},
		{name: "docker unreachable", setup: func(f *fixture) { f.docker.InfoErr = errors.New("down") },
			wantStatus: doctor.StatusSkip},
	})
}

func TestDisk(t *testing.T) {
	runCases(t, "disk", []checkCase{
		{name: "plenty", wantStatus: doctor.StatusPass, wantMessage: "100.0 GiB free for /var/lib/pressfleet"},
		{name: "low", setup: func(f *fixture) { f.host.Free = 5 * gib },
			wantStatus: doctor.StatusWarn, wantMessage: "5.0 GiB free", wantFix: "20 GiB"},
		{name: "error", setup: func(f *fixture) { f.host.FreeErr = errors.New("statfs failed") },
			wantStatus: doctor.StatusWarn, wantMessage: "statfs failed", wantFix: "enough space"},
	})
}

func TestMemory(t *testing.T) {
	runCases(t, "memory", []checkCase{
		{name: "16 GiB", wantStatus: doctor.StatusPass, wantMessage: "fits about 114 sites"},
		{name: "8 GiB machine reporting less", setup: func(f *fixture) { f.host.Mem = 7_800 << 20 },
			wantStatus: doctor.StatusPass},
		{name: "4 GiB", setup: func(f *fixture) { f.host.Mem = 4 * gib },
			wantStatus: doctor.StatusWarn, wantMessage: "about 18 sites", wantFix: "8 GiB"},
		{name: "tiny", setup: func(f *fixture) { f.host.Mem = gib },
			wantStatus: doctor.StatusWarn, wantMessage: "about 0 sites"},
		{name: "error", setup: func(f *fixture) { f.host.MemErr = errors.New("no /proc") },
			wantStatus: doctor.StatusWarn, wantMessage: "no /proc", wantFix: "8 GiB"},
	})
}

func TestSiteReservations(t *testing.T) {
	runCases(t, "site-reservations", []checkCase{
		{name: "no provider", wantStatus: doctor.StatusSkip, wantMessage: "not available yet"},
		{name: "fits", setup: func(f *fixture) { f.opts.Reservations = doctortest.Reservations{Total: 50 * 128 << 20, Sites: 50} },
			wantStatus: doctor.StatusPass, wantMessage: "50 sites reserve 6.2 GiB of 16.0 GiB"},
		{name: "exceeds", setup: func(f *fixture) {
			f.host.Mem = 8 * gib
			f.opts.Reservations = doctortest.Reservations{Total: 60 * 128 << 20, Sites: 60}
		}, wantStatus: doctor.StatusWarn, wantMessage: "exceeds host memory", wantFix: "Add memory"},
		{name: "provider error", setup: func(f *fixture) { f.opts.Reservations = doctortest.Reservations{Err: errors.New("db down")} },
			wantStatus: doctor.StatusWarn, wantMessage: "db down"},
		{name: "memory error", setup: func(f *fixture) {
			f.opts.Reservations = doctortest.Reservations{Sites: 1}
			f.host.MemErr = errors.New("no /proc")
		}, wantStatus: doctor.StatusWarn, wantMessage: "no /proc"},
	})
}

func TestScopes(t *testing.T) {
	f := newFixture()
	opts := doctor.Options{Host: f.host, Docker: f.docker, DataDir: "/data"}
	ids := func(rep doctor.Report) map[string]bool {
		m := map[string]bool{}
		for _, r := range rep.Results {
			m[r.ID] = true
		}
		return m
	}
	runtime := ids(doctor.Run(context.Background(), opts, doctor.ScopeRuntime))
	install := ids(doctor.Run(context.Background(), opts, doctor.ScopeInstall))
	for _, id := range []string{"ports", "dns-dashboard", "dns-preview", "log-rotation"} {
		if runtime[id] || !install[id] {
			t.Errorf("%s: runtime=%v install=%v, want install only", id, runtime[id], install[id])
		}
	}
	if !runtime["site-reservations"] || install["site-reservations"] {
		t.Error("site-reservations should be runtime only")
	}
	for _, id := range []string{"platform", "docker", "compose", "cgroup", "address-pools", "disk", "memory"} {
		if !runtime[id] || !install[id] {
			t.Errorf("%s: runtime=%v install=%v, want both", id, runtime[id], install[id])
		}
	}
	all := doctor.Run(context.Background(), opts, doctor.ScopeInstall, doctor.ScopeRuntime)
	if len(all.Results) != len(doctor.Checks()) {
		t.Errorf("both scopes ran %d checks, want %d", len(all.Results), len(doctor.Checks()))
	}
}

func TestEngineQueriedOnce(t *testing.T) {
	f := newFixture()
	f.run(t, "platform")
	if f.docker.EngineCalls != 1 {
		t.Errorf("Engine called %d times, want 1", f.docker.EngineCalls)
	}
}

func TestOutcome(t *testing.T) {
	res := func(sev doctor.Severity, st doctor.Status) doctor.Result {
		return doctor.Result{ID: "x", Severity: sev, Status: st}
	}
	tests := []struct {
		name    string
		results []doctor.Result
		want    doctor.Outcome
	}{
		{"empty", nil, doctor.OK},
		{"pass and skip", []doctor.Result{res(doctor.Required, doctor.StatusPass), res(doctor.Required, doctor.StatusSkip)}, doctor.OK},
		{"recommended warn", []doctor.Result{res(doctor.Recommended, doctor.StatusWarn)}, doctor.Warnings},
		{"recommended fail", []doctor.Result{res(doctor.Recommended, doctor.StatusFail)}, doctor.Warnings},
		{"required warn", []doctor.Result{res(doctor.Required, doctor.StatusWarn)}, doctor.Warnings},
		{"required fail", []doctor.Result{res(doctor.Recommended, doctor.StatusWarn), res(doctor.Required, doctor.StatusFail)}, doctor.Failed},
	}
	for _, tt := range tests {
		rep := doctor.Report{Results: tt.results}
		if got := rep.Outcome(); got != tt.want {
			t.Errorf("%s: Outcome() = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestHealthyHostPasses(t *testing.T) {
	f := newFixture()
	f.opts.Host, f.opts.Docker = f.host, f.docker
	rep := doctor.Run(context.Background(), f.opts, doctor.ScopeInstall, doctor.ScopeRuntime)
	if rep.Outcome() != doctor.OK {
		t.Errorf("Outcome() = %v, results: %+v", rep.Outcome(), rep.Results)
	}
	if len(rep.Failures()) != 0 {
		t.Errorf("Failures() = %+v", rep.Failures())
	}
}

func TestWriteText(t *testing.T) {
	rep := doctor.Report{Results: []doctor.Result{
		{Name: "Short", Severity: doctor.Required, Status: doctor.StatusPass, Message: "ok"},
		{Name: "Longer name", Severity: doctor.Required, Status: doctor.StatusFail, Message: "broken", Fix: "line one\nline two"},
		{Name: "Memory", Severity: doctor.Recommended, Status: doctor.StatusSkip, Message: "n/a", Fix: "not shown"},
	}}
	var buf bytes.Buffer
	if err := rep.WriteText(&buf); err != nil {
		t.Fatal(err)
	}
	want := `PASS  Short         ok
FAIL  Longer name   broken
      fix: line one
           line two
SKIP  Memory*       n/a

* recommended
1 passed, 1 failed, 0 warned, 1 skipped
`
	if buf.String() != want {
		t.Errorf("WriteText() =\n%s\nwant:\n%s", buf.String(), want)
	}
}

func TestWriteJSON(t *testing.T) {
	rep := doctor.Report{Results: []doctor.Result{
		{ID: "ports", Name: "Ports", Severity: doctor.Required, Status: doctor.StatusFail, Message: "busy", Fix: "stop it"},
	}}
	var buf bytes.Buffer
	if err := rep.WriteJSON(&buf); err != nil {
		t.Fatal(err)
	}
	var got struct {
		Outcome string          `json:"outcome"`
		Results []doctor.Result `json:"results"`
	}
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Outcome != "failed" || len(got.Results) != 1 || got.Results[0] != rep.Results[0] {
		t.Errorf("round trip = %+v", got)
	}
}
