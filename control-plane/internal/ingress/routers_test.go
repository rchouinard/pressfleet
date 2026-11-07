package ingress

import (
	"reflect"
	"testing"

	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
)

func TestSiteRouters(t *testing.T) {
	site := &provisioner.SiteConfig{
		ID:            "abc",
		PreviewDomain: "abc.preview.example.com",
		PreviewAuth:   &provisioner.BasicAuth{Username: "u", PasswordHash: "h"},
	}
	preview := Router{
		Name:         "pf-abc-preview",
		Rule:         "Host(`abc.preview.example.com`)",
		CertResolver: CertResolverPreview,
		Middlewares:  []string{"pf-abc-noindex", "pf-abc-preview-auth"},
	}
	if got := SiteRouters(site, true); !reflect.DeepEqual(got, []Router{preview}) {
		t.Errorf("preview only = %+v", got)
	}

	site.PrimaryDomain = "example.com"
	site.Aliases = []string{"www.example.com"}
	got := SiteRouters(site, true)
	if len(got) != 3 || !reflect.DeepEqual(got[0], preview) {
		t.Fatalf("live site = %+v", got)
	}
	if p := got[1]; p.Name != "pf-abc-primary" || !p.Domain || p.CertResolver != CertResolverHTTP ||
		!reflect.DeepEqual(p.Middlewares, []string{"pf-abc-hsts"}) {
		t.Errorf("primary = %+v", p)
	}
	if a := got[2]; a.Rule != "Host(`www.example.com`)" || !a.Domain ||
		!reflect.DeepEqual(a.Middlewares, []string{"pf-abc-hsts", "pf-abc-alias-redirect"}) {
		t.Errorf("alias = %+v", a)
	}
	if p := SiteRouters(site, false)[1]; len(p.Middlewares) != 0 {
		t.Errorf("primary without HSTS has middlewares %q", p.Middlewares)
	}
}

func TestHostRule(t *testing.T) {
	if got := HostRule("a.example.com", "b.example.com"); got != "Host(`a.example.com`) || Host(`b.example.com`)" {
		t.Errorf("HostRule = %q", got)
	}
}
