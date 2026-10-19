package doctor

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Capacity plan figures from design doc §4.1.
const (
	mib = 1 << 20
	gib = 1 << 30

	// planMemory is the host size the capacity plan targets (50 sites).
	planMemory = 8 * gib
	// memTolerance allows for memory the kernel and firmware reserve: an
	// 8 GiB machine reports less than 8 GiB in /proc/meminfo.
	memTolerance = gib / 2
	// platformMemory is the budget for MariaDB, Redis, VictoriaMetrics,
	// Traefik, the control plane, and OS headroom.
	platformMemory = 1792 * mib
	// siteReservation is each site's guaranteed memory baseline.
	siteReservation = 128 * mib

	// minFreeDisk is the free space below which the disk check warns. It
	// covers images, a few dozen small sites, local backups, and metrics.
	minFreeDisk = 20 * gib

	// minNetworks and wantNetworks bound the number of networks the address
	// pools must provide: one per site plus platform networks.
	minNetworks  = 64
	wantNetworks = 256
	// maxPoolSize is the longest usable pool prefix: a site network holds
	// the gateway, two site containers, and three shared services.
	maxPoolSize = 28

	dnsTimeout = 5 * time.Second
)

// recommendedDaemonJSON is the daemon.json setting from §4.1.
const recommendedDaemonJSON = `{ "default-address-pools": [ { "base": "10.200.0.0/16", "size": 24 } ] }`

const restartDocker = "Then restart Docker (sudo systemctl restart docker)."

var (
	both        = []Scope{ScopeInstall, ScopeRuntime}
	installOnly = []Scope{ScopeInstall}
	runtimeOnly = []Scope{ScopeRuntime}
)

// Checks returns every preflight check in the order they run.
func Checks() []Check {
	return []Check{
		{ID: "platform", Name: "Linux on amd64", Severity: Required, Scopes: both, run: checkPlatform},
		{ID: "docker", HostOnly: true, Name: "Docker Engine", Severity: Required, Scopes: both, run: checkDocker},
		{ID: "compose", HostOnly: true, Name: "Docker Compose v2", Severity: Required, Scopes: both, run: checkCompose},
		{ID: "cgroup", HostOnly: true, Name: "cgroup v2", Severity: Required, Scopes: both, run: checkCgroup},
		{ID: "address-pools", HostOnly: true, Name: "Docker address pools", Severity: Required, Scopes: both, run: checkAddressPools},
		{ID: "ports", HostOnly: true, Name: "Ports 80 and 443 free", Severity: Required, Scopes: installOnly, run: checkPorts},
		{ID: "sftp-port", HostOnly: true, Name: "SFTP gateway port free", Severity: Required, Scopes: installOnly, run: checkSFTPPort},
		{ID: "dns-dashboard", Name: "Dashboard DNS", Severity: Required, Scopes: installOnly, run: checkDashboardDNS},
		{ID: "dns-preview", Name: "Wildcard preview DNS", Severity: Required, Scopes: installOnly, run: checkPreviewDNS},
		{ID: "log-rotation", HostOnly: true, Name: "Container log rotation", Severity: Recommended, Scopes: installOnly, run: checkLogRotation},
		{ID: "disk", Name: "Free disk space", Severity: Recommended, Scopes: both, run: checkDisk},
		{ID: "memory", Name: "Host memory", Severity: Recommended, Scopes: both, run: checkMemory},
		{ID: "site-reservations", Name: "Site memory reservations", Severity: Recommended, Scopes: runtimeOnly, run: checkReservations},
		{ID: "webauthn-relying-party", Name: "Security keys and passkeys", Severity: Recommended, Scopes: runtimeOnly, run: checkWebAuthn},
	}
}

func pass(format string, args ...any) outcome {
	return outcome{status: StatusPass, message: fmt.Sprintf(format, args...)}
}

func skip(format string, args ...any) outcome {
	return outcome{status: StatusSkip, message: fmt.Sprintf(format, args...)}
}

func fail(fix, format string, args ...any) outcome {
	return outcome{status: StatusFail, message: fmt.Sprintf(format, args...), fix: fix}
}

func warn(fix, format string, args ...any) outcome {
	return outcome{status: StatusWarn, message: fmt.Sprintf(format, args...), fix: fix}
}

func dockerUnreachable() outcome {
	return skip("Docker Engine is not reachable (see the docker check)")
}

func checkPlatform(_ context.Context, e *env) outcome {
	goos, goarch := e.Host.Platform()
	if goos != "linux" || goarch != "amd64" {
		return fail("Run PressFleet on a 64-bit x86 (amd64) Linux host.",
			"host is %s/%s; PressFleet supports only linux/amd64", goos, goarch)
	}
	return pass("%s/%s", goos, goarch)
}

// MinDockerEngineMajor is the oldest Docker Engine major version PressFleet
// supports. Engine 28 (API 1.48) added network gateway priorities, which the
// Compose driver needs to attach platform services to site networks without
// moving Traefik's published ports off the platform network.
const MinDockerEngineMajor = 28

func checkDocker(ctx context.Context, e *env) outcome {
	info, err := e.engineInfo(ctx)
	if err != nil {
		return fail("Install Docker Engine (https://docs.docker.com/engine/install/) and start it. "+
			"Run as root or as a user in the docker group.",
			"cannot reach Docker Engine: %v", err)
	}
	if info.OSType != "linux" {
		return fail("Switch Docker to Linux containers.",
			"Docker Engine %s runs %s containers; PressFleet needs Linux containers", info.ServerVersion, info.OSType)
	}
	major, _, _ := strings.Cut(info.ServerVersion, ".")
	if n, err := strconv.Atoi(major); err != nil || n < MinDockerEngineMajor {
		return fail(fmt.Sprintf("Upgrade Docker Engine to %d or later (https://docs.docker.com/engine/install/).", MinDockerEngineMajor),
			"Docker Engine %s is too old; PressFleet needs %d or later", info.ServerVersion, MinDockerEngineMajor)
	}
	return pass("Docker Engine %s (%s/%s)", info.ServerVersion, info.OSType, info.Architecture)
}

func checkCompose(ctx context.Context, e *env) outcome {
	const fix = "Install the Docker Compose v2 plugin, e.g. `sudo apt-get install docker-compose-plugin` " +
		"(https://docs.docker.com/compose/install/linux/). The standalone Python docker-compose v1 is not supported."
	v, err := e.Docker.ComposeVersion(ctx)
	if err != nil {
		return fail(fix, "`docker compose` is not available: %v", err)
	}
	major, _, _ := strings.Cut(v, ".")
	if n, err := strconv.Atoi(major); err != nil || n < 2 {
		return fail(fix, "Docker Compose %s is too old; PressFleet needs v2 or later", v)
	}
	return pass("Docker Compose %s", v)
}

func checkCgroup(ctx context.Context, e *env) outcome {
	info, err := e.engineInfo(ctx)
	if err != nil {
		return dockerUnreachable()
	}
	if info.CgroupVersion != "2" {
		return fail("Boot the host with the unified cgroup hierarchy: add systemd.unified_cgroup_hierarchy=1 "+
			"to the kernel command line and reboot, or use a distribution that defaults to cgroup v2 "+
			"(Ubuntu 21.10+, Debian 11+, RHEL 9+).",
			"Docker uses cgroup v%s; site memory reservations and limits need cgroup v2", info.CgroupVersion)
	}
	return pass("Docker uses cgroup v2")
}

func checkAddressPools(ctx context.Context, e *env) outcome {
	info, err := e.engineInfo(ctx)
	if err != nil {
		return dockerUnreachable()
	}
	fix := "Add this to /etc/docker/daemon.json (merge with any existing settings):\n" +
		recommendedDaemonJSON + "\n" + restartDocker +
		" Existing networks keep their subnets; the pools apply to new networks."
	pools := info.DefaultAddressPools
	if len(pools) == 0 {
		return fail(fix, "Docker uses its default address pools, which run out after about 30 networks; "+
			"PressFleet creates one network per site")
	}
	networks := 0
	for _, p := range pools {
		bits, maxSize := p.Base.Bits(), maxPoolSize
		if p.Base.Addr().Is6() {
			maxSize = 128 - (32 - maxPoolSize)
		}
		if p.Size < bits || p.Size > maxSize {
			return fail(fix, "address pool %s has subnet size /%d; use a size between /%d and /%d", p.Base, p.Size, bits, maxSize)
		}
		// Cap the shift: a huge IPv6 pool would overflow and doesn't need
		// counting past what we want.
		networks += 1 << min(p.Size-bits, 16)
	}
	switch {
	case networks < minNetworks:
		return fail(fix, "address pools provide %d networks; PressFleet needs at least %d", networks, minNetworks)
	case networks < wantNetworks:
		return warn(fix, "address pools provide %d networks; %d or more is recommended", networks, wantNetworks)
	}
	return pass("address pools provide %d networks", networks)
}

func checkPorts(_ context.Context, e *env) outcome {
	var busy, unknown []string
	for _, port := range []int{80, 443} {
		inUse, err := e.Host.PortInUse(port)
		switch {
		case err != nil:
			unknown = append(unknown, fmt.Sprintf("%d (%v)", port, err))
		case inUse:
			busy = append(busy, strconv.Itoa(port))
		}
	}
	if len(busy) > 0 {
		return fail("Stop the service using the port; find it with `sudo ss -ltnp 'sport = :80 or sport = :443'`. "+
			"If PressFleet is already installed, Traefik holds these ports and this is expected.",
			"port %s in use; Traefik needs ports 80 and 443", strings.Join(busy, " and "))
	}
	if len(unknown) > 0 {
		return warn("Check by hand with `sudo ss -ltn 'sport = :80 or sport = :443'`.",
			"could not check port %s", strings.Join(unknown, ", "))
	}
	return pass("ports 80 and 443 are free")
}

// checkSFTPPort checks the SFTP gateway's host port (§5.8). Like ports 80
// and 443, only before installing: the platform holds it afterwards.
func checkSFTPPort(_ context.Context, e *env) outcome {
	if e.SFTPPort == 0 {
		return skip("sftp.port is not set")
	}
	inUse, err := e.Host.PortInUse(e.SFTPPort)
	switch {
	case err != nil:
		return warn(fmt.Sprintf("Check by hand with `sudo ss -ltn 'sport = :%d'`.", e.SFTPPort),
			"could not check port %d (%v)", e.SFTPPort, err)
	case inUse:
		return fail(fmt.Sprintf("Stop the service using the port (find it with `sudo ss -ltnp 'sport = :%d'`), "+
			"or choose another port with sftp.port (PRESSFLEET_SFTP_PORT). "+
			"If PressFleet is already installed, its SFTP gateway holds this port and this is expected.", e.SFTPPort),
			"port %d in use; the SFTP gateway needs it", e.SFTPPort)
	}
	return pass("port %d is free", e.SFTPPort)
}

func checkDashboardDNS(ctx context.Context, e *env) outcome {
	if e.DashboardDomain == "" {
		return skip("dashboard_domain is not set (run `pressfleet init` or set PRESSFLEET_DASHBOARD_DOMAIN)")
	}
	return checkDNS(ctx, e, e.DashboardDomain, e.DashboardDomain,
		fmt.Sprintf("Create an A (or AAAA) record for %s pointing at this host's public IP address.", e.DashboardDomain))
}

func checkPreviewDNS(ctx context.Context, e *env) outcome {
	if e.PreviewBaseDomain == "" {
		return skip("preview_base_domain is not set (run `pressfleet init` or set PRESSFLEET_PREVIEW_BASE_DOMAIN)")
	}
	label := e.RandomLabel
	if label == nil {
		label = randomLabel
	}
	wildcard := "*." + e.PreviewBaseDomain
	return checkDNS(ctx, e, wildcard, label()+"."+e.PreviewBaseDomain,
		fmt.Sprintf("Create a wildcard A (or AAAA) record for %s pointing at this host's public IP address.", wildcard))
}

// checkDNS verifies that name (probed by resolving lookup) points at this
// host. Behind NAT the host's public address is on no interface, so unless
// PublicIP is configured a mismatch only warns.
func checkDNS(ctx context.Context, e *env, name, lookup, fix string) outcome {
	ctx, cancel := context.WithTimeout(ctx, dnsTimeout)
	defer cancel()
	addrs, err := e.Host.LookupHost(ctx, lookup)
	if err != nil || len(addrs) == 0 {
		if err == nil {
			err = fmt.Errorf("no addresses")
		}
		return fail(fix, "%s does not resolve: %v", name, err)
	}
	for i, a := range addrs {
		addrs[i] = a.Unmap()
	}
	if !slices.ContainsFunc(addrs, func(a netip.Addr) bool { return !a.IsLoopback() && !a.IsUnspecified() }) {
		return fail(fix, "%s resolves to %s, a loopback address", name, joinAddrs(addrs))
	}

	if e.PublicIP.IsValid() {
		if slices.Contains(addrs, e.PublicIP.Unmap()) {
			return pass("%s resolves to %s (public_ip)", name, e.PublicIP)
		}
		return fail(fix+" Or correct public_ip if it is wrong.",
			"%s resolves to %s, not this host's public_ip %s", name, joinAddrs(addrs), e.PublicIP)
	}

	local, err := e.Host.InterfaceAddrs()
	if err == nil {
		for _, a := range addrs {
			if !a.IsLoopback() && slices.Contains(local, a) {
				return pass("%s resolves to %s (a local interface address)", name, a)
			}
		}
	}
	return warn("If this host is behind NAT or a load balancer, set public_ip (PRESSFLEET_PUBLIC_IP) "+
		"to its public address so this check can verify the record. Otherwise: "+fix,
		"%s resolves to %s, which is not an address of this host's interfaces; could not verify it points here",
		name, joinAddrs(addrs))
}

func joinAddrs(addrs []netip.Addr) string {
	s := make([]string, len(addrs))
	for i, a := range addrs {
		s[i] = a.String()
	}
	return strings.Join(s, ", ")
}

// randomLabel returns a label unlikely to have its own DNS record, so
// resolving it exercises the wildcard.
func randomLabel() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b) // crypto/rand.Read never fails
	return "pf-doctor-" + hex.EncodeToString(b)
}

func checkLogRotation(ctx context.Context, e *env) outcome {
	info, err := e.engineInfo(ctx)
	if err != nil {
		return dockerUnreachable()
	}
	fix := `Add default log options to /etc/docker/daemon.json (merge with any existing settings):
{ "log-driver": "json-file", "log-opts": { "max-size": "10m", "max-file": "3" } }
` + restartDocker + " The options apply to containers created afterwards."
	switch info.LoggingDriver {
	case "json-file":
	case "local":
		return pass("the local log driver rotates logs by default")
	case "none":
		return pass("container logging is disabled")
	default:
		return pass("containers log to %s; rotation is handled there", info.LoggingDriver)
	}
	switch {
	case info.DaemonConfigErr != nil:
		return warn("Make sure "+info.DaemonConfigPath+" sets log-opts max-size, or run doctor as root so it can read the file.",
			"could not read Docker's daemon config: %v", info.DaemonConfigErr)
	case info.LogOpts["max-size"] != "":
		maxFile := info.LogOpts["max-file"]
		if maxFile == "" {
			maxFile = "1"
		}
		return pass("json-file logs rotate at %s (%s files)", info.LogOpts["max-size"], maxFile)
	case !info.DaemonConfigFound:
		return warn(fix, "json-file logs grow without limit: %s does not exist, so no log-opts are set", info.DaemonConfigPath)
	}
	return warn(fix, "json-file logs grow without limit: %s does not set log-opts max-size", info.DaemonConfigPath)
}

func checkDisk(_ context.Context, e *env) outcome {
	free, err := e.Host.FreeDisk(e.DataDir)
	if err != nil {
		return warn("Make sure the filesystem holding "+e.DataDir+" has enough space.",
			"could not check free space for %s: %v", e.DataDir, err)
	}
	if free < minFreeDisk {
		return warn(fmt.Sprintf("Free up space or grow the filesystem holding %s to at least %d GiB free.", e.DataDir, minFreeDisk/gib),
			"%s free for %s; sites, backups, and metrics need at least %d GiB", formatGiB(free), e.DataDir, minFreeDisk/gib)
	}
	return pass("%s free for %s", formatGiB(free), e.DataDir)
}

func checkMemory(_ context.Context, e *env) outcome {
	total, err := e.Host.MemTotal()
	if err != nil {
		return warn("Make sure the host has at least 8 GiB of memory.", "could not read host memory: %v", err)
	}
	if total+memTolerance < planMemory {
		return warn("Use a host with at least 8 GiB of memory to run 50 sites, or plan for fewer sites.",
			"%s of memory is below the capacity plan (8 GiB for 50 sites); plan for about %d sites",
			formatGiB(total), sitesFor(total))
	}
	return pass("%s of memory fits about %d sites at the 128 MiB baseline", formatGiB(total), sitesFor(total))
}

// sitesFor estimates how many site reservations fit beside the platform.
func sitesFor(total uint64) uint64 {
	if total <= platformMemory {
		return 0
	}
	return (total - platformMemory) / siteReservation
}

func checkReservations(ctx context.Context, e *env) outcome {
	if e.Reservations == nil {
		return skip("site reservations are not available yet")
	}
	reserved, sites, err := e.Reservations.MemoryReservations(ctx)
	if err != nil {
		return warn("Check the control plane logs.", "could not read site reservations: %v", err)
	}
	total, err := e.Host.MemTotal()
	if err != nil {
		return warn("Make sure the host has enough memory for all sites.", "could not read host memory: %v", err)
	}
	if reserved+platformMemory > total {
		return warn("Add memory to the host, or reduce site reservations or the number of sites.",
			"%d sites reserve %s; with the platform's %s that exceeds host memory (%s)",
			sites, formatGiB(reserved), formatGiB(platformMemory), formatGiB(total))
	}
	return pass("%d sites reserve %s of %s", sites, formatGiB(reserved), formatGiB(total))
}

func checkWebAuthn(ctx context.Context, e *env) outcome {
	if e.WebAuthn == nil {
		return outcome{status: StatusUnavailable,
			message: "needs the state database; the dashboard's platform overview (GET /api/v1/platform/doctor) shows this check"}
	}
	rpID, creds, users, err := e.WebAuthn.StaleWebAuthnCredentials(ctx)
	if err != nil {
		return warn("Check the control plane logs.", "could not count security keys and passkeys: %v", err)
	}
	if creds == 0 {
		return pass("every security key and passkey is registered for %s", rpID)
	}
	keys, whose := "security keys and passkeys", "users"
	if creds == 1 {
		keys = "security key or passkey"
	}
	if users == 1 {
		whose = "user"
	}
	return warn("Ask those users to register their security keys and passkeys again, have an Admin reset their second factors, "+
		"or change dashboard_domain back to the domain they were registered on.",
		"%d %s of %d %s can't be used: registered under another dashboard domain than %s, "+
			"so not counted as a second factor", creds, keys, users, whose, rpID)
}

func formatGiB(b uint64) string {
	return fmt.Sprintf("%.1f GiB", float64(b)/gib)
}
