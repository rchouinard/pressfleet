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
// address 169.254.169.254), unspecified, or multicast. Webhook URLs and SMTP
// hosts are typed in by users, so without this check a channel would let an
// Operator probe the services listening beside the control plane. The
// alerts.webhook_allow_local_targets setting turns the check off.
var ErrBlockedTarget = errors.New("address is not allowed (loopback, link-local, or otherwise local)")

// awsIMDSv6 is the IPv6 address of the EC2 metadata service, which is in
// a unique local range rather than a link-local one.
var awsIMDSv6 = netip.MustParseAddr("fd00:ec2::254")

// blockedAddr reports whether a channel may not connect to ip.
func blockedAddr(ip netip.Addr) bool {
	ip = ip.Unmap()
	return ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() || ip == awsIMDSv6
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
