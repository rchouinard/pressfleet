package alertchannels

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"syscall"
	"time"
)

// ErrBlockedTarget means a channel tried to reach an address it isn't
// allowed to: loopback, link-local (which includes the cloud metadata
// address 169.254.169.254), private, shared (carrier-grade NAT), unspecified,
// multicast, or another reserved range. Webhook URLs and SMTP hosts are
// typed in by users, and every service beside the control plane (the
// metrics store, the databases, the Kubernetes API) sits on a private
// address, so without this check a channel would let an Operator reach
// them. The alerts.webhook_allow_local_targets setting turns the check off.
var ErrBlockedTarget = errors.New("address is not allowed (loopback, link-local, private, or otherwise reserved)")

// reservedRanges are the ranges that netip's predicates don't cover. IPv4
// private ranges and IPv6 unique local addresses (which include AWS's
// fd00:ec2::254 metadata address) are caught by IsPrivate. This list is
// related to, but deliberately separate from, the Kubernetes driver's
// nonInternet: that one decides what a pod may reach, this one what the
// control plane may dial.
var reservedRanges = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),       // "this network"
	netip.MustParsePrefix("100.64.0.0/10"),   // shared address space (CGNAT); Alibaba's metadata service is 100.100.100.200
	netip.MustParsePrefix("192.0.0.0/24"),    // IETF protocol assignments
	netip.MustParsePrefix("192.0.2.0/24"),    // documentation (TEST-NET-1)
	netip.MustParsePrefix("198.18.0.0/15"),   // benchmarking
	netip.MustParsePrefix("198.51.100.0/24"), // documentation (TEST-NET-2)
	netip.MustParsePrefix("203.0.113.0/24"),  // documentation (TEST-NET-3)
	netip.MustParsePrefix("240.0.0.0/4"),     // reserved, includes the broadcast address
	netip.MustParsePrefix("64:ff9b::/96"),    // NAT64, which can reach any IPv4 address, private ones included
	netip.MustParsePrefix("64:ff9b:1::/48"),  // local-use NAT64
	netip.MustParsePrefix("2001:db8::/32"),   // documentation
}

// blockedAddr reports whether a channel may not connect to ip.
func blockedAddr(ip netip.Addr) bool {
	ip = ip.Unmap()
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() {
		return true
	}
	for _, p := range reservedRanges {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

// guardedDialer returns a dialer that refuses blocked addresses. It checks
// the address actually being connected to, after name resolution, so a DNS
// name that resolves to (or is later rebound to) a local address, or a
// redirect to one, is refused as well. With allowLocal there is no check.
func guardedDialer(timeout time.Duration, allowLocal bool) *net.Dialer {
	d := &net.Dialer{Timeout: timeout}
	if allowLocal {
		return d
	}
	d.Control = func(_, address string, _ syscall.RawConn) error {
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return fmt.Errorf("dial %q: %w", address, err)
		}
		ip, err := netip.ParseAddr(host)
		if err != nil {
			return fmt.Errorf("dial %q: %w", address, err)
		}
		if blockedAddr(ip) {
			return fmt.Errorf("%w: %s", ErrBlockedTarget, ip)
		}
		return nil
	}
	return d
}

// checkLiteralHost rejects a host that is a blocked IP address, so an
// obviously bad URL fails when the channel is saved instead of when it is
// first used. Names are checked at connection time.
func checkLiteralHost(host string, allowLocal bool) error {
	if allowLocal {
		return nil
	}
	if ip, err := netip.ParseAddr(host); err == nil && blockedAddr(ip) {
		return fmt.Errorf("%w: %s", ErrBlockedTarget, ip)
	}
	return nil
}
