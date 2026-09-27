// Package domains checks the custom domains a site goes live on (design doc
// §5.2): that a name is a host name the edge router can route and get a
// certificate for ([Normalize]), and that its DNS records point at this
// host ([Checker]).
//
// # The DNS check
//
// Traefik gets a certificate for each of a site's domains with the ACME
// HTTP-01 challenge, which only succeeds if the certificate authority
// reaches this host on port 80 at every address the domain resolves to
// (Let's Encrypt prefers IPv6, so a stale AAAA record alone breaks it). The
// check therefore resolves each domain's A and AAAA records and accepts it
// only if it has at least one and all of them are addresses of this host.
//
// The control plane runs in a container behind NAT and can't see the
// host's public addresses on its own interfaces, so the host's addresses
// are what the operator says they are:
//
//   - public_ip (config.Config.PublicIP), if set, and
//   - whatever the dashboard domain resolves to: `pressfleet doctor` and
//     the dashboard's own certificate already require it to point at this
//     host, so its records are the host's addresses, IPv4 and IPv6, even
//     when public_ip is unset or holds one address family only.
//
// If neither gives an address, the check can't tell right from wrong and
// fails with [ErrHostUnknown]. Hosts behind a CDN or load balancer, where
// domains legitimately resolve elsewhere, skip the check per request
// instead.
//
// The check uses the control plane's resolver, which answers from caches;
// a record that was just created may take up to its negative-caching TTL to
// be seen. Retrying later is always safe: a failed check changes nothing.
package domains

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/config"
)

// ErrInvalid means a string is not a domain a site can use.
var ErrInvalid = errors.New("not a valid domain name")

// ErrHostUnknown means the check has no address of this host to compare
// DNS records with: public_ip is unset and the dashboard domain doesn't
// resolve.
var ErrHostUnknown = errors.New("this host's public addresses are unknown")

// Normalize returns the canonical form of a domain a site can use: lower
// case, without a trailing dot. It rejects IP addresses, wildcards,
// single-label names, internationalized names not in their ASCII (xn--)
// form, and anything else that can't be a host name in a certificate.
func Normalize(s string) (string, error) {
	d := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(s)), ".")
	switch {
	case d == "":
		return "", fmt.Errorf("%w: it is empty", ErrInvalid)
	case strings.HasPrefix(d, "*."):
		return "", fmt.Errorf("%w: wildcards can't be used, list each host name", ErrInvalid)
	case strings.Contains(d, "://") || strings.ContainsAny(d, "/:"):
		return "", fmt.Errorf("%w: give the host name alone, without a scheme, port, or path", ErrInvalid)
	}
	if _, err := netip.ParseAddr(d); err == nil {
		return "", fmt.Errorf("%w: IP addresses can't be used", ErrInvalid)
	}
	for _, r := range d {
		if r > 0x7f {
			return "", fmt.Errorf("%w: write internationalized names in their ASCII (xn--) form", ErrInvalid)
		}
	}
	if !config.ValidDomain(d) {
		return "", fmt.Errorf("%w: use letters, digits, and hyphens, with at least two labels", ErrInvalid)
	}
	// A numeric top-level label is an IPv4 address in disguise, not a
	// domain (RFC 3696 §2).
	if tld := d[strings.LastIndexByte(d, '.')+1:]; strings.Trim(tld, "0123456789") == "" {
		return "", fmt.Errorf("%w: the last label can't be numeric", ErrInvalid)
	}
	return d, nil
}

// Resolver looks up a host's IP addresses; *net.Resolver implements it.
type Resolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

// DefaultTimeout bounds each DNS lookup of a check.
const DefaultTimeout = 5 * time.Second

// Options configures a Checker.
type Options struct {
	// Resolver defaults to net.DefaultResolver.
	Resolver Resolver
	// PublicIP is this host's public address (config.Config.PublicIP);
	// the zero value means unset.
	PublicIP netip.Addr
	// DashboardDomain is config.Config.DashboardDomain. Its addresses
	// count as this host's.
	DashboardDomain string
	// Timeout bounds each lookup; zero means DefaultTimeout.
	Timeout time.Duration
}

// Checker verifies that domains resolve to this host.
type Checker struct {
	resolver  Resolver
	publicIP  netip.Addr
	dashboard string
	timeout   time.Duration
}

// NewChecker returns a Checker.
func NewChecker(opts Options) *Checker {
	c := &Checker{
		resolver:  opts.Resolver,
		publicIP:  opts.PublicIP.Unmap(),
		dashboard: opts.DashboardDomain,
		timeout:   opts.Timeout,
	}
	if c.resolver == nil {
		c.resolver = net.DefaultResolver
	}
	if c.timeout <= 0 {
		c.timeout = DefaultTimeout
	}
	return c
}

// Problem is a domain that failed the check, and why.
type Problem struct {
	Domain string
	Detail string
}

// HostAddrs returns this host's public addresses, sorted: public_ip and the
// dashboard domain's addresses. It returns ErrHostUnknown if there are
// none.
func (c *Checker) HostAddrs(ctx context.Context) ([]netip.Addr, error) {
	var addrs []netip.Addr
	if c.publicIP.IsValid() {
		addrs = append(addrs, c.publicIP)
	}
	var lookupErr error
	if c.dashboard != "" {
		found, err := c.lookup(ctx, c.dashboard)
		if err != nil {
			lookupErr = fmt.Errorf("the dashboard domain %s: %s", c.dashboard, describe(err))
		}
		for _, a := range found {
			if !a.IsUnspecified() {
				addrs = append(addrs, a)
			}
		}
	}
	if len(addrs) == 0 {
		why := "public_ip is not set and there is no dashboard domain"
		if lookupErr != nil {
			why = "public_ip is not set and " + lookupErr.Error()
		}
		return nil, fmt.Errorf("%w: %s", ErrHostUnknown, why)
	}
	slices.SortFunc(addrs, netip.Addr.Compare)
	return slices.Compact(addrs), nil
}

// Check resolves each domain and returns the ones that don't point at this
// host only, in the order given; none means every domain passed. It fails
// with ErrHostUnknown if it doesn't know this host's addresses. The lookups
// run concurrently.
func (c *Checker) Check(ctx context.Context, domains []string) ([]Problem, error) {
	if len(domains) == 0 {
		return nil, nil
	}
	host, err := c.HostAddrs(ctx)
	if err != nil {
		return nil, err
	}
	details := make([]string, len(domains))
	var wg sync.WaitGroup
	for i, d := range domains {
		wg.Go(func() { details[i] = c.checkOne(ctx, d, host) })
	}
	wg.Wait()
	var problems []Problem
	for i, d := range domains {
		if details[i] != "" {
			problems = append(problems, Problem{Domain: d, Detail: details[i]})
		}
	}
	return problems, nil
}

// checkOne returns why the domain fails the check, or "" if it passes.
func (c *Checker) checkOne(ctx context.Context, domain string, host []netip.Addr) string {
	want := "Point its A (and AAAA) records at " + joinAddrs(host) + "."
	addrs, err := c.lookup(ctx, domain)
	if err != nil {
		return describe(err) + ". " + want
	}
	if len(addrs) == 0 {
		return "it has no A or AAAA record. " + want
	}
	var stray []netip.Addr
	for _, a := range addrs {
		if !slices.Contains(host, a) {
			stray = append(stray, a)
		}
	}
	if len(stray) > 0 {
		return fmt.Sprintf("it resolves to %s, which is not this host (%s). %s Remove records that point elsewhere: "+
			"the certificate authority may use any of them.", joinAddrs(stray), joinAddrs(host), want)
	}
	return ""
}

// lookup resolves the name's A and AAAA records within the timeout, as
// sorted, unmapped addresses.
func (c *Checker) lookup(ctx context.Context, name string) ([]netip.Addr, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	addrs, err := c.resolver.LookupNetIP(ctx, "ip", name)
	if err != nil {
		return nil, err
	}
	out := make([]netip.Addr, 0, len(addrs))
	for _, a := range addrs {
		out = append(out, a.Unmap())
	}
	slices.SortFunc(out, netip.Addr.Compare)
	return slices.Compact(out), nil
}

// describe explains a lookup error to the operator.
func describe(err error) string {
	var dnsErr *net.DNSError
	switch {
	case errors.As(err, &dnsErr) && dnsErr.IsNotFound:
		return "it does not resolve (no such domain, or no A or AAAA record)"
	case errors.As(err, &dnsErr) && dnsErr.IsTimeout, errors.Is(err, context.DeadlineExceeded):
		return "the DNS lookup timed out"
	default:
		return "the DNS lookup failed: " + err.Error()
	}
}

func joinAddrs(addrs []netip.Addr) string {
	s := make([]string, len(addrs))
	for i, a := range addrs {
		s[i] = a.String()
	}
	return strings.Join(s, ", ")
}
