package api

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
)

func TestWithClient(t *testing.T) {
	trusted := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}
	tests := []struct {
		name, remote, xff, proto string
		wantIP                   string
		wantHTTPS                bool
	}{
		{"direct", "203.0.113.5:1234", "", "", "203.0.113.5", false},
		{"untrusted remote ignores headers", "203.0.113.5:1234", "198.51.100.1", "https", "203.0.113.5", false},
		{"trusted proxy", "10.0.0.2:1234", "198.51.100.1", "https", "198.51.100.1", true},
		{"forged leftmost entries are skipped", "10.0.0.2:1234", "1.2.3.4, 198.51.100.1", "http", "198.51.100.1", false},
		{"chain of trusted proxies", "10.0.0.2:1234", "198.51.100.1, 10.0.0.9", "https", "198.51.100.1", true},
		{"garbage stops the walk", "10.0.0.2:1234", "198.51.100.1, junk", "", "10.0.0.2", false},
		{"IPv6", "[2001:db8::1]:443", "", "", "2001:db8::1", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got clientInfo
			h := withClient(trusted)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				got = clientFromContext(r.Context())
			}))
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			r.RemoteAddr = tt.remote
			if tt.xff != "" {
				r.Header.Set("X-Forwarded-For", tt.xff)
			}
			if tt.proto != "" {
				r.Header.Set("X-Forwarded-Proto", tt.proto)
			}
			h.ServeHTTP(httptest.NewRecorder(), r)
			if got.IP != tt.wantIP || got.HTTPS != tt.wantHTTPS {
				t.Errorf("got %+v, want IP %s HTTPS %v", got, tt.wantIP, tt.wantHTTPS)
			}
		})
	}
}
