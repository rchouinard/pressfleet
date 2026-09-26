package doctortest_test

import (
	"context"
	"net/netip"
	"testing"

	"github.com/rchouinard/pressfleet/control-plane/internal/doctor/doctortest"
)

func TestHostLookupHost(t *testing.T) {
	h := doctortest.Healthy()
	ip := netip.MustParseAddr("192.0.2.1")
	h.DNS["exact.example.com"] = []netip.Addr{ip}
	h.DNS["*.wild.example.com"] = []netip.Addr{ip}

	for _, name := range []string{"exact.example.com", "any.wild.example.com"} {
		if got, err := h.LookupHost(context.Background(), name); err != nil || len(got) != 1 || got[0] != ip {
			t.Errorf("LookupHost(%q) = %v, %v", name, got, err)
		}
	}
	for _, name := range []string{"wild.example.com", "other.example.com"} {
		if _, err := h.LookupHost(context.Background(), name); err == nil {
			t.Errorf("LookupHost(%q) = nil error", name)
		}
	}
}
