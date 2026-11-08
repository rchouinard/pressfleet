package kubernetes

import (
	"errors"
	"testing"
)

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
