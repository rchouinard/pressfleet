package kubernetes

import (
	"net"
	"net/netip"
	"slices"
	"testing"
)

func TestResolvePlatformAddress(t *testing.T) {
	tests := []struct {
		addr      string
		hostPort  string
		inCluster bool
		ip        string
	}{
		{"mariadb:3306", "mariadb.pressfleet.svc:3306", true, ""},
		{"mariadb.pressfleet.svc:3306", "mariadb.pressfleet.svc:3306", true, ""},
		{"valkey.pressfleet.svc.cluster.local:6379", "valkey.pressfleet.svc.cluster.local:6379", true, ""},
		{"db.example.com:3306", "db.example.com:3306", false, ""},
		{"mariadb.other.svc:3306", "mariadb.other.svc:3306", false, ""},
		{"192.168.1.20:3306", "192.168.1.20:3306", false, "192.168.1.20"},
		{"[fd00::20]:3306", "[fd00::20]:3306", false, "fd00::20"},
	}
	for _, tt := range tests {
		a, err := resolvePlatformAddress(tt.addr, "pressfleet")
		if err != nil {
			t.Errorf("%s: %v", tt.addr, err)
			continue
		}
		if a.HostPort != tt.hostPort || a.InCluster != tt.inCluster || (tt.ip != "" && a.IP.String() != tt.ip) || (tt.ip == "" && a.IP.IsValid()) {
			t.Errorf("resolvePlatformAddress(%s) = %+v", tt.addr, a)
		}
	}
	for _, bad := range []string{"mariadb", "mariadb:0", "mariadb:http", ":3306x"} {
		if _, err := resolvePlatformAddress(bad, "pressfleet"); err == nil {
			t.Errorf("resolvePlatformAddress(%s) succeeded", bad)
		}
	}
}

func TestSiteEgress(t *testing.T) {
	var services []platformAddress
	for _, addr := range []string{"mariadb:3306", "valkey:6379", "10.1.2.3:3307", "db.example.com:3308", "redis:6379"} {
		a, err := resolvePlatformAddress(addr, "pressfleet")
		if err != nil {
			t.Fatal(err)
		}
		services = append(services, a)
	}
	pods := []netip.Prefix{netip.MustParsePrefix("10.244.0.0/16"), netip.MustParsePrefix("203.0.113.0/24"), netip.MustParsePrefix("2001:db8:42::/56")}
	e := siteEgress(services, nil, pods)
	if !slices.Equal(e.PlatformPorts, []int{3306, 6379}) {
		t.Errorf("platform ports %v", e.PlatformPorts)
	}
	if len(e.Hosts) != 1 || e.Hosts[0] != (egressHost{CIDR: "10.1.2.3/32", Port: 3307}) {
		t.Errorf("hosts %v: want the external MariaDB by address only", e.Hosts)
	}
	if e.SMTPPort != 0 || e.SMTPNamespace != "" {
		t.Errorf("SMTP rule %q:%d without a relay", e.SMTPNamespace, e.SMTPPort)
	}
	// The private pod range is covered already; the public ones are left
	// out explicitly. The metadata address is in 169.254.0.0/16.
	if !slices.Contains(e.ExceptV4, "169.254.0.0/16") || !slices.Contains(e.ExceptV4, "203.0.113.0/24") || slices.Contains(e.ExceptV4, "10.244.0.0/16") {
		t.Errorf("IPv4 exceptions %v", e.ExceptV4)
	}
	if !slices.Contains(e.ExceptV6, "fc00::/7") || !slices.Contains(e.ExceptV6, "2001:db8:42::/56") {
		t.Errorf("IPv6 exceptions %v", e.ExceptV6)
	}
	if e := siteEgress(nil, nil, nil); e.SMTPPort != 0 || e.PlatformPorts != nil || len(e.ExceptV4) == 0 {
		t.Errorf("egress without services: %+v", e)
	}
}

// The SMTP relay's port is open only to the relay when it can be named, and
// otherwise to what the internet policy allows, never to every address.
func TestSiteEgressSMTP(t *testing.T) {
	tests := []struct {
		host      string
		wantNS    string
		wantHosts []egressHost
	}{
		{host: "192.0.2.25", wantHosts: []egressHost{{CIDR: "192.0.2.25/32", Port: 587}}},
		{host: "10.0.0.7", wantHosts: []egressHost{{CIDR: "10.0.0.7/32", Port: 587}}},
		{host: "2001:db8::25", wantHosts: []egressHost{{CIDR: "2001:db8::25/128", Port: 587}}},
		{host: "mailpit", wantNS: "pressfleet"},
		{host: "mailpit.pressfleet.svc", wantNS: "pressfleet"},
		{host: "mailpit.mail.svc", wantNS: "mail"},
		{host: "mailpit.mail.svc.cluster.local", wantNS: "mail"},
		{host: "mailpit.mail.svc.cluster.local.", wantNS: "mail"},
		{host: "smtp.example.com"},
		{host: "mail.svc.example.com"}, // a public name that only looks like one
	}
	for _, tt := range tests {
		a, err := resolvePlatformAddress(net.JoinHostPort(tt.host, "587"), "pressfleet")
		if err != nil {
			t.Fatal(err)
		}
		e := siteEgress(nil, &a, nil)
		wantPort := 0
		if tt.wantNS != "" {
			wantPort = 587
		}
		if e.SMTPNamespace != tt.wantNS || e.SMTPPort != wantPort || !slices.Equal(e.Hosts, tt.wantHosts) {
			t.Errorf("siteEgress for relay %s = namespace %q port %d hosts %v, want %q %d %v",
				tt.host, e.SMTPNamespace, e.SMTPPort, e.Hosts, tt.wantNS, wantPort, tt.wantHosts)
		}
	}
}

func TestInternetExceptions(t *testing.T) {
	pods := []netip.Prefix{netip.MustParsePrefix("10.42.0.0/16"), netip.MustParsePrefix("203.0.113.0/26")}
	tests := []struct {
		name   string
		host   string
		v4, v6 []string // the exceptions the host adds
	}{
		{name: "public node", host: "198.51.100.7", v4: []string{"198.51.100.7/32"}},
		{name: "public IPv6 node", host: "2001:db8::7", v6: []string{"2001:db8::7/128"}},
		{name: "IPv4-mapped", host: "::ffff:198.51.100.8", v4: []string{"198.51.100.8/32"}},
		{name: "private node", host: "10.1.2.3"},
		{name: "shared address space", host: "100.64.1.2"},
		{name: "unique local", host: "fd00::5"},
		{name: "in a public pod range", host: "203.0.113.9"},
	}
	baseV4, baseV6 := internetExceptions(pods, nil)
	if !slices.Contains(baseV4, "203.0.113.0/26") || slices.Contains(baseV4, "10.42.0.0/16") {
		t.Errorf("IPv4 exceptions without hosts %v", baseV4)
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Twice: an address is left out once.
			host := netip.MustParseAddr(tt.host)
			v4, v6 := internetExceptions(pods, []netip.Addr{host, host})
			if added := v4[len(baseV4):]; !slices.Equal(added, tt.v4) {
				t.Errorf("IPv4 exceptions added %v, want %v", added, tt.v4)
			}
			if added := v6[len(baseV6):]; !slices.Equal(added, tt.v6) {
				t.Errorf("IPv6 exceptions added %v, want %v", added, tt.v6)
			}
		})
	}
}
