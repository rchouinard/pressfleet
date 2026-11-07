package kubernetes

import (
	"errors"
	"testing"

	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
)

// Every operation a later change implements says so, rather than doing
// nothing.
func TestNotImplemented(t *testing.T) {
	p := testProvisioner(t)
	ctx := t.Context()
	cfg := testSite()
	ops := map[string]func() error{
		"RunSession": func() error { return p.RunSession(ctx, cfg.ID, provisioner.Session{}) },
	}
	for name, op := range ops {
		if err := op(); !errors.Is(err, ErrNotImplemented) {
			t.Errorf("%s = %v, want ErrNotImplemented", name, err)
		}
	}
}

func TestSiteNamespace(t *testing.T) {
	p := testProvisioner(t)
	if got := p.SiteNamespace(testSiteID); got != "pf-"+testSiteID {
		t.Errorf("SiteNamespace = %q", got)
	}
	for id, ok := range map[string]bool{
		testSiteID: true,
		"site1":    true,
		"Site1":    false,
		"site1-":   false,
		"":         false,
	} {
		if err := p.validSiteNamespace(id); (err == nil) != ok || (err != nil && !errors.Is(err, ErrInvalidSite)) {
			t.Errorf("validSiteNamespace(%q) = %v, want ok %v", id, err, ok)
		}
	}
}
