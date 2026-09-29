// Package doctor implements host preflight checks (design doc §4.1).
//
// Checks run in two scopes. Install-scope checks run on the host before the
// platform is up (`pressfleet init`, `pressfleet doctor`); some of them, such
// as "ports 80/443 are free", only make sense there because Traefik holds
// those ports once installed. Runtime-scope checks run when the control plane
// starts, possibly inside its container.
//
// Checks never touch the host directly: they go through HostProbe and
// DockerProbe so every check can be unit tested with fakes (see doctortest).
package doctor

import (
	"context"
	"net/netip"
	"slices"

	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner/compose"
)

// Scope says when a check runs.
type Scope string

// Check scopes.
const (
	// ScopeInstall checks run on the host before the platform is running.
	ScopeInstall Scope = "install"
	// ScopeRuntime checks run when the control plane starts.
	ScopeRuntime Scope = "runtime"
)

// Severity says whether PressFleet can run when a check doesn't pass.
type Severity string

// Check severities.
const (
	// Required checks block installation and control plane startup.
	Required Severity = "required"
	// Recommended checks only warn.
	Recommended Severity = "recommended"
)

// Status is the outcome of one check.
type Status string

// Check statuses.
const (
	StatusPass Status = "pass"
	// StatusWarn means the check found a likely problem or couldn't verify
	// the requirement.
	StatusWarn Status = "warn"
	StatusFail Status = "fail"
	// StatusSkip means the check doesn't apply, e.g. DNS checks when no
	// domain is configured yet.
	StatusSkip Status = "skip"
)

// HostProbe reads facts about the host that aren't Docker's.
type HostProbe interface {
	// Platform returns the OS and CPU architecture in GOOS/GOARCH terms.
	Platform() (goos, goarch string)
	// MemTotal returns total host memory in bytes.
	MemTotal() (uint64, error)
	// FreeDisk returns the bytes available to unprivileged users on the
	// filesystem holding path, or its nearest existing parent.
	FreeDisk(path string) (uint64, error)
	// PortInUse reports whether something listens on the TCP port.
	PortInUse(port int) (bool, error)
	// LookupHost resolves a name to its IP addresses.
	LookupHost(ctx context.Context, host string) ([]netip.Addr, error)
	// InterfaceAddrs returns the addresses of the host's network interfaces.
	InterfaceAddrs() ([]netip.Addr, error)
}

// DockerProbe reads Docker facts. compose.HostInspector implements it; Docker
// access is confined to the Compose driver (§4.2).
type DockerProbe interface {
	Engine(ctx context.Context) (compose.EngineInfo, error)
	ComposeVersion(ctx context.Context) (string, error)
}

// ReservationProvider reports the memory reserved by all sites. It needs the
// state database, so only the control plane can provide it (§4.7).
type ReservationProvider interface {
	MemoryReservations(ctx context.Context) (totalBytes uint64, sites int, err error)
}

// Options are the inputs to a preflight run.
type Options struct {
	Host   HostProbe
	Docker DockerProbe
	// Reservations feeds the site reservations check; nil skips it.
	Reservations ReservationProvider
	// DataDir is where sites, backups, and metrics are stored.
	DataDir string
	// DashboardDomain and PreviewBaseDomain are checked for DNS records
	// when set.
	DashboardDomain   string
	PreviewBaseDomain string
	// PublicIP, if valid, is the address DNS records must point at. It lets
	// hosts behind NAT verify DNS.
	PublicIP netip.Addr
	// SFTPPort is the host port the SFTP gateway will be published on
	// (sftp.port); zero skips its check.
	SFTPPort int
	// RandomLabel returns a DNS label for probing the wildcard preview
	// record; nil uses a random one.
	RandomLabel func() string
}

// Check is one preflight check.
type Check struct {
	ID       string
	Name     string
	Severity Severity
	Scopes   []Scope
	run      func(ctx context.Context, e *env) outcome
}

// outcome is what a check's run function reports.
type outcome struct {
	status  Status
	message string
	fix     string
}

// Result is the outcome of running one check.
type Result struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	Severity Severity `json:"severity"`
	Status   Status   `json:"status"`
	Message  string   `json:"message"`
	// Fix tells the operator how to resolve a warning or failure.
	Fix string `json:"fix,omitempty"`
}

// Report is the result of a preflight run.
type Report struct {
	Results []Result `json:"results"`
}

// Outcome summarizes a report.
type Outcome int

// Report outcomes, from best to worst.
const (
	// OK means every check passed or was skipped.
	OK Outcome = iota
	// Warnings means no required check failed, but a check warned or a
	// recommended check failed.
	Warnings
	// Failed means at least one required check failed.
	Failed
)

// Outcome returns the worst outcome among the results.
func (r Report) Outcome() Outcome {
	o := OK
	for _, res := range r.Results {
		switch {
		case res.Status == StatusFail && res.Severity == Required:
			return Failed
		case res.Status == StatusFail, res.Status == StatusWarn:
			o = Warnings
		}
	}
	return o
}

// Failures returns the results of required checks that failed.
func (r Report) Failures() []Result {
	var out []Result
	for _, res := range r.Results {
		if res.Status == StatusFail && res.Severity == Required {
			out = append(out, res)
		}
	}
	return out
}

// Run runs every check that applies to any of scopes, in a stable order.
func Run(ctx context.Context, opts Options, scopes ...Scope) Report {
	e := &env{Options: opts}
	var rep Report
	for _, c := range Checks() {
		if !slices.ContainsFunc(c.Scopes, func(s Scope) bool { return slices.Contains(scopes, s) }) {
			continue
		}
		o := c.run(ctx, e)
		rep.Results = append(rep.Results, Result{
			ID: c.ID, Name: c.Name, Severity: c.Severity,
			Status: o.status, Message: o.message, Fix: o.fix,
		})
	}
	return rep
}

// env gives checks access to the options and caches Docker facts, so one run
// asks the daemon only once.
type env struct {
	Options
	engine     *compose.EngineInfo
	engineErr  error
	engineDone bool
}

func (e *env) engineInfo(ctx context.Context) (compose.EngineInfo, error) {
	if !e.engineDone {
		info, err := e.Docker.Engine(ctx)
		e.engine, e.engineErr, e.engineDone = &info, err, true
	}
	return *e.engine, e.engineErr
}
