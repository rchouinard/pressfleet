package kubernetes

import (
	"fmt"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"strings"
)

// egress is what the site's egress NetworkPolicy allows besides DNS
// (network-policies.yaml.tmpl). The internet is in a policy of its own that
// the driver writes (internet.go), because it follows the cluster's nodes.
type egress struct {
	// PlatformPorts are the TCP ports of the platform services in the
	// platform namespace that sites use: MariaDB and Valkey.
	PlatformPorts []int
	// Hosts are platform services outside the cluster, by address.
	Hosts []egressHost
	// SMTPNamespace and SMTPPort are an SMTP relay that is a Service in the
	// cluster: any pod of that namespace on that port, as a NetworkPolicy
	// can't select a Service. Both are zero for a relay given by IP (that is
	// in Hosts), by an external name (the internet rule covers its public
	// addresses), or without a relay.
	SMTPNamespace string
	SMTPPort      int
	// ExceptV4 and ExceptV6 are what a repository task's internet rule
	// leaves out: the non-internet ranges and the pod ranges.
	ExceptV4, ExceptV6 []string
}

// egressHost is one address and port a site may reach.
type egressHost struct {
	CIDR string
	Port int
}

// nonInternet are the ranges that the internet rule of a site's egress
// leaves out. Pod and Service addresses are in them on every cluster the
// driver supports, and so are the nodes of most clusters, but not all: a
// node of k3s on a VPS has its public address as its InternalIP, and a
// managed cluster's API server may have a public endpoint (security review
// K4). So the rule also leaves out each node's and API server endpoint's
// own address, which the driver keeps current (internet.go), and a pod
// range outside these ranges as configured (kubernetes.pod_cidrs).
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
// non-internet ranges, then the pod ranges and the hosts (as /32 or /128)
// that those don't cover already. An exception must lie within the rule's
// range, 0.0.0.0/0 or ::/0, which each one does.
func internetExceptions(pods []netip.Prefix, hosts []netip.Addr) (v4, v6 []string) {
	except := slices.Clone(nonInternet)
	covered := func(p netip.Prefix) bool {
		return slices.ContainsFunc(except, func(n netip.Prefix) bool { return n.Bits() <= p.Bits() && n.Contains(p.Addr()) })
	}
	for _, p := range pods {
		if p = p.Masked(); !covered(p) {
			except = append(except, p)
		}
	}
	for _, h := range hosts {
		h = h.Unmap()
		if p := netip.PrefixFrom(h, h.BitLen()); h.IsValid() && !covered(p) {
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
	// ServiceNamespace is the namespace of an address that names a Service
	// (a bare name, or name.namespace.svc with any cluster domain), which
	// may not be the platform namespace. Empty otherwise.
	ServiceNamespace string
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
		a.ServiceNamespace = platformNamespace
	case strings.HasSuffix(host, "."+platformNamespace+".svc") ||
		strings.Contains(host, "."+platformNamespace+".svc."):
		a.InCluster = true
	}
	if a.ServiceNamespace == "" && !a.IP.IsValid() {
		// name.namespace.svc, with or without the cluster domain.
		if labels := strings.Split(strings.TrimSuffix(host, "."), "."); len(labels) >= 3 && labels[2] == "svc" {
			a.ServiceNamespace = labels[1]
		}
	}
	a.HostPort = net.JoinHostPort(a.Host, strconv.Itoa(port))
	return a, nil
}

// siteEgress builds the egress a site needs to reach the platform's
// services and SMTP relay (nil for none).
func siteEgress(services []platformAddress, smtp *platformAddress, pods []netip.Prefix) egress {
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
	if smtp != nil {
		switch {
		case smtp.IP.IsValid():
			e.Hosts = append(e.Hosts, egressHost{CIDR: netip.PrefixFrom(smtp.IP, smtp.IP.BitLen()).String(), Port: smtp.Port})
		case smtp.ServiceNamespace != "":
			e.SMTPNamespace, e.SMTPPort = smtp.ServiceNamespace, smtp.Port
		}
		// A relay named by an external host name is reached through the
		// internet rule, like an external MariaDB: a NetworkPolicy can't
		// name a host. One that resolves to a private address isn't
		// reachable by name; give its IP address instead.
	}
	e.ExceptV4, e.ExceptV6 = internetExceptions(pods, nil)
	return e
}
