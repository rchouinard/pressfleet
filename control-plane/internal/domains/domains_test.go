package domains_test

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/rchouinard/pressfleet/control-plane/internal/domains"
)

func TestNormalize(t *testing.T) {
	for _, tc := range []struct {
		in, want string
	}{
		{"www.acme.example", "www.acme.example"},
		{"  WWW.Acme.Example. ", "www.acme.example"},
		{"xn--bcher-kva.example", "xn--bcher-kva.example"},
		{"a-b.c1.example", "a-b.c1.example"},
		{"", ""},
		{"example", ""},
		{"*.acme.example", ""},
		{"https://acme.example", ""},
		{"acme.example:443", ""},
		{"acme.example/path", ""},
		{"192.0.2.1", ""},
		{"2001:db8::1", ""},
		{"1.2.3.4.5", ""},
		{"bücher.example", ""},
		{"-acme.example", ""},
		{"acme_site.example", ""},
		{"acme..example", ""},
		{"a`b.example", ""},
		{strings.Repeat("a", 64) + ".example", ""},
	} {
		got, err := domains.Normalize(tc.in)
		if tc.want == "" {
			if err == nil || !errors.Is(err, domains.ErrInvalid) {
				t.Errorf("Normalize(%q) = %q, %v; want ErrInvalid", tc.in, got, err)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("Normalize(%q) = %q, %v; want %q", tc.in, got, err, tc.want)
		}
	}
}

// fakeResolver answers from a map that tests change, as DNS records
// propagating would. Names not in it don't exist.
type fakeResolver struct {
	mu      sync.Mutex
	records map[string][]string
	errs    map[string]error
	lookups []string
}

func (f *fakeResolver) set(name string, addrs ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.records[name] = addrs
}

func (f *fakeResolver) LookupNetIP(_ context.Context, network, host string) ([]netip.Addr, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if network != "ip" {
		return nil, errors.New("unexpected network " + network)
	}
	f.lookups = append(f.lookups, host)
	if err := f.errs[host]; err != nil {
		return nil, err
	}
	addrs, ok := f.records[host]
	if !ok {
		return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
	}
	var out []netip.Addr
	for _, a := range addrs {
		out = append(out, netip.MustParseAddr(a))
	}
	return out, nil
}

func newResolver() *fakeResolver {
	return &fakeResolver{records: map[string][]string{}, errs: map[string]error{}}
}

func TestCheck(t *testing.T) {
	ctx := context.Background()
	r := newResolver()
	r.set("fleet.agency.example", "203.0.113.7", "2001:db8::7")
	r.set("ok.example", "203.0.113.7")
	r.set("ok6.example", "203.0.113.7", "2001:db8::7")
	r.set("mapped.example", "::ffff:203.0.113.7")
	r.set("public.example", "198.51.100.9")
	r.set("elsewhere.example", "192.0.2.1")
	r.set("stale6.example", "203.0.113.7", "2001:db8::99")
	r.set("empty.example")
	r.errs["slow.example"] = &net.DNSError{Err: "i/o timeout", Name: "slow.example", IsTimeout: true}
	r.errs["broken.example"] = errors.New("server misbehaving")
	c := domains.NewChecker(domains.Options{
		Resolver:        r,
		PublicIP:        netip.MustParseAddr("198.51.100.9"),
		DashboardDomain: "fleet.agency.example",
	})

	host, err := c.HostAddrs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := []netip.Addr{netip.MustParseAddr("198.51.100.9"), netip.MustParseAddr("203.0.113.7"), netip.MustParseAddr("2001:db8::7")}
	if !slices.Equal(host, want) {
		t.Errorf("host addresses = %v, want %v", host, want)
	}

	names := []string{
		"ok.example", "ok6.example", "mapped.example", "public.example",
		"missing.example", "elsewhere.example", "stale6.example", "empty.example", "slow.example", "broken.example",
	}
	problems, err := c.Check(ctx, names)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	var order []string
	for _, p := range problems {
		got[p.Domain] = p.Detail
		order = append(order, p.Domain)
	}
	wantOrder := []string{"missing.example", "elsewhere.example", "stale6.example", "empty.example", "slow.example", "broken.example"}
	if !slices.Equal(order, wantOrder) {
		t.Errorf("problems for %q, want %q", order, wantOrder)
	}
	for domain, substr := range map[string]string{
		"missing.example":   "does not resolve",
		"elsewhere.example": "resolves to 192.0.2.1, which is not this host",
		"stale6.example":    "resolves to 2001:db8::99",
		"empty.example":     "no A or AAAA record",
		"slow.example":      "timed out",
		"broken.example":    "server misbehaving",
	} {
		if !strings.Contains(got[domain], substr) {
			t.Errorf("%s: %q lacks %q", domain, got[domain], substr)
		}
		if !strings.Contains(got[domain], "Point its A (and AAAA) records at 198.51.100.9, 203.0.113.7, 2001:db8::7.") {
			t.Errorf("%s: %q doesn't say where to point it", domain, got[domain])
		}
	}

	if problems, err := c.Check(ctx, nil); err != nil || problems != nil {
		t.Errorf("no domains: %v, %v", problems, err)
	}
}

// TestCheckRetryAfterPropagation is the go-live acceptance criterion
// "retry after DNS failure succeeds" at the level of the check: a failed
// check keeps no state, so checking again once the record exists passes.
func TestCheckRetryAfterPropagation(t *testing.T) {
	ctx := context.Background()
	r := newResolver()
	c := domains.NewChecker(domains.Options{Resolver: r, PublicIP: netip.MustParseAddr("203.0.113.7")})
	r.set("www.acme.example", "192.0.2.1") // still at the old host
	if problems, err := c.Check(ctx, []string{"www.acme.example"}); err != nil || len(problems) != 1 {
		t.Fatalf("before the change: %v, %v", problems, err)
	}
	r.set("www.acme.example", "203.0.113.7")
	if problems, err := c.Check(ctx, []string{"www.acme.example"}); err != nil || len(problems) != 0 {
		t.Errorf("after the change: %v, %v", problems, err)
	}
}

func TestHostAddrs(t *testing.T) {
	ctx := context.Background()
	r := newResolver()
	r.set("fleet.agency.example", "203.0.113.7")
	for _, tc := range []struct {
		name string
		opts domains.Options
		want string // "" means ErrHostUnknown
	}{
		{"public ip only", domains.Options{PublicIP: netip.MustParseAddr("198.51.100.9")}, "198.51.100.9"},
		{"dashboard only", domains.Options{DashboardDomain: "fleet.agency.example"}, "203.0.113.7"},
		{"dashboard doesn't resolve, public ip", domains.Options{DashboardDomain: "gone.example", PublicIP: netip.MustParseAddr("198.51.100.9")}, "198.51.100.9"},
		{"nothing", domains.Options{}, ""},
		{"dashboard doesn't resolve", domains.Options{DashboardDomain: "gone.example"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.opts.Resolver = r
			addrs, err := domains.NewChecker(tc.opts).HostAddrs(ctx)
			if tc.want == "" {
				if !errors.Is(err, domains.ErrHostUnknown) {
					t.Errorf("err = %v, want ErrHostUnknown", err)
				}
				if _, err := domains.NewChecker(tc.opts).Check(ctx, []string{"x.example"}); !errors.Is(err, domains.ErrHostUnknown) {
					t.Errorf("Check: err = %v, want ErrHostUnknown", err)
				}
				return
			}
			if err != nil || len(addrs) != 1 || addrs[0].String() != tc.want {
				t.Errorf("= %v, %v; want %s", addrs, err, tc.want)
			}
		})
	}
}
