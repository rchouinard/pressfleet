package kubernetes

import (
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
	e := siteEgress(services, 25, pods)
	if !slices.Equal(e.PlatformPorts, []int{3306, 6379}) {
		t.Errorf("platform ports %v", e.PlatformPorts)
	}
	if len(e.Hosts) != 1 || e.Hosts[0] != (egressHost{CIDR: "10.1.2.3/32", Port: 3307}) {
		t.Errorf("hosts %v: want the external MariaDB by address only", e.Hosts)
	}
	if e.SMTPPort != 25 {
		t.Errorf("SMTP port %d", e.SMTPPort)
	}
	// The private pod range is covered already; the public ones are left
	// out explicitly. The metadata address is in 169.254.0.0/16.
	if !slices.Contains(e.ExceptV4, "169.254.0.0/16") || !slices.Contains(e.ExceptV4, "203.0.113.0/24") || slices.Contains(e.ExceptV4, "10.244.0.0/16") {
		t.Errorf("IPv4 exceptions %v", e.ExceptV4)
	}
	if !slices.Contains(e.ExceptV6, "fc00::/7") || !slices.Contains(e.ExceptV6, "2001:db8:42::/56") {
		t.Errorf("IPv6 exceptions %v", e.ExceptV6)
	}
	if e := siteEgress(nil, 0, nil); e.SMTPPort != 0 || e.PlatformPorts != nil || len(e.ExceptV4) == 0 {
		t.Errorf("egress without services: %+v", e)
	}
}
