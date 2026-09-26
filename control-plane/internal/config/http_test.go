package config_test

import (
	"net/netip"
	"slices"
	"strings"
	"testing"

	"github.com/rchouinard/pressfleet/control-plane/internal/config"
)

func TestHTTPSettings(t *testing.T) {
	cfg, err := config.Load("", env(noDefaultFile(t, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HTTP.SecureCookies != "" || len(cfg.HTTP.TrustedProxies) != 0 {
		t.Errorf("defaults: %+v", cfg.HTTP)
	}
	if p, err := cfg.HTTP.TrustedProxyPrefixes(); err != nil || p != nil {
		t.Errorf("default prefixes = %v, %v; want nil (use the API default)", p, err)
	}

	path := writeFile(t, "http:\n  secure_cookies: always\n  trusted_proxies: [10.1.0.0/16, 192.0.2.7]\n")
	cfg, err = config.Load(path, env(nil))
	if err != nil {
		t.Fatal(err)
	}
	got, err := cfg.HTTP.TrustedProxyPrefixes()
	want := []netip.Prefix{netip.MustParsePrefix("10.1.0.0/16"), netip.MustParsePrefix("192.0.2.7/32")}
	if err != nil || !slices.Equal(got, want) || cfg.HTTP.SecureCookies != config.SecureCookiesAlways || cfg.Validate() != nil {
		t.Errorf("from file: %+v, %v, %v", cfg.HTTP, got, err)
	}

	cfg, err = config.Load(path, env(map[string]string{
		"PRESSFLEET_HTTP_SECURE_COOKIES":  "auto",
		"PRESSFLEET_HTTP_TRUSTED_PROXIES": " 172.18.0.0/16 , fd00::/8,",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(cfg.HTTP.TrustedProxies, []string{"172.18.0.0/16", "fd00::/8"}) || cfg.HTTP.SecureCookies != "auto" {
		t.Errorf("from env: %+v", cfg.HTTP)
	}

	cfg.HTTP.SecureCookies, cfg.HTTP.TrustedProxies = "sometimes", []string{"traefik"}
	err = cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "http.secure_cookies") || !strings.Contains(err.Error(), `"traefik"`) {
		t.Errorf("Validate = %v", err)
	}
}
