package kubernetes

import (
	"fmt"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"strings"
)

// egress is what the site's egress NetworkPolicy allows besides DNS and
// the internet (network-policies.yaml.tmpl).
type egress struct {
	// PlatformPorts are the TCP ports of the platform services in the
	// platform namespace that sites use: MariaDB and Valkey.
	PlatformPorts []int
	// Hosts are platform services outside the cluster, by address.
	Hosts []egressHost
	// SMTPPort is the SMTP relay's port, 0 without a relay.
	SMTPPort int
	// ExceptV4 and ExceptV6 are what the internet rule leaves out.
	ExceptV4, ExceptV6 []string
}

// egressHost is one address and port a site may reach.
type egressHost struct {
	CIDR string
	Port int
}

// nonInternet are the ranges that the internet rule of a site's egress
// policy leaves out. The cluster's pod, service, and node addresses are in
// private ranges on every cluster the driver supports, so leaving those out
// keeps sites away from the rest of the cluster without knowing its ranges;
// a pod range outside them is left out as configured (kubernetes.pod_cidrs).
var nonInternet = []netip.Prefix{
	netip.MustParsePrefix("10.0.0.0/8"),     // private
	netip.MustParsePrefix("100.64.0.0/10"),  // shared address space (carrier-grade NAT, some CNIs)
	netip.MustParsePrefix("169.254.0.0/16"), // link-local, the cloud metadata address
	netip.MustParsePrefix("172.16.0.0/12"),  // private
	netip.MustParsePrefix("192.168.0.0/16"), // private
	netip.MustParsePrefix("fc00::/7"),       // unique local, AWS's metadata address
	netip.MustParsePrefix("fe80::/10"),      // link-local
}

// internetExceptions returns the ranges the internet rule leaves out: the
// non-internet ranges, and the pod ranges outside them.
func internetExceptions(pods []netip.Prefix) (v4, v6 []string) {
	except := slices.Clone(nonInternet)
	for _, p := range pods {
		if !slices.ContainsFunc(nonInternet, func(n netip.Prefix) bool { return n.Bits() <= p.Bits() && n.Contains(p.Addr()) }) {
			except = append(except, p)
		}
	}
	for _, p := range except {
		if p.Addr().Is4() {
			v4 = append(v4, p.String())
		} else {
			v6 = append(v6, p.String())
		}
	}
	return v4, v6
}

// platformAddress is a platform service's address as the control plane
// knows it (mariadb.address, redis.address), turned into what a site's
// pod uses. The control plane runs in the platform namespace, where a bare
// Service name resolves; a site's namespace is another, so the name is
// qualified with the platform namespace.
type platformAddress struct {
	// HostPort is the address for the site: host:port.
	HostPort string
	Host     string
	Port     int
	// InCluster is true for a Service of the platform namespace.
	InCluster bool
	// IP is set for an address given as an IP.
	IP netip.Addr
}

// resolvePlatformAddress parses addr (host:port) for sites.
func resolvePlatformAddress(addr, platformNamespace string) (platformAddress, error) {
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return platformAddress{}, fmt.Errorf("address %q must be host:port: %w", addr, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 1 || port > 65535 {
		return platformAddress{}, fmt.Errorf("address %q has an invalid port", addr)
	}
	a := platformAddress{Host: host, Port: port}
	switch ip, err := netip.ParseAddr(host); {
	case err == nil:
		a.IP = ip.Unmap()
	case !strings.Contains(host, "."):
		a.Host = host + "." + platformNamespace + ".svc"
		a.InCluster = true
	case strings.HasSuffix(host, "."+platformNamespace+".svc") ||
		strings.Contains(host, "."+platformNamespace+".svc."):
		a.InCluster = true
	}
	a.HostPort = net.JoinHostPort(a.Host, strconv.Itoa(port))
	return a, nil
}

// siteEgress builds the egress a site needs to reach the platform's
// services and SMTP relay (smtpPort, 0 for none).
func siteEgress(services []platformAddress, smtpPort int, pods []netip.Prefix) egress {
	var e egress
	for _, s := range services {
		switch {
		case s.InCluster:
			if !slices.Contains(e.PlatformPorts, s.Port) {
				e.PlatformPorts = append(e.PlatformPorts, s.Port)
			}
		case s.IP.IsValid():
			e.Hosts = append(e.Hosts, egressHost{CIDR: netip.PrefixFrom(s.IP, s.IP.BitLen()).String(), Port: s.Port})
		}
		// An external service by name is reached through the internet
		// rule: a NetworkPolicy can't name a host.
	}
	slices.Sort(e.PlatformPorts)
	e.SMTPPort = smtpPort
	e.ExceptV4, e.ExceptV6 = internetExceptions(pods)
	return e
}
