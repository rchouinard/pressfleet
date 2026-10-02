package api_test

import (
	"net/http"
	"testing"

	"github.com/rchouinard/pressfleet/control-plane/internal/api"
	"github.com/rchouinard/pressfleet/control-plane/internal/api/gen"
)

func TestGetPlatform(t *testing.T) {
	h := newHandler(t, api.Options{Authenticator: &fakeAuth{}, SFTP: api.SFTPInfo{Enabled: true, Host: "fleet.example.com", Port: 2022}})
	w := do(t, h, request{authed: true, method: http.MethodGet, target: "/api/v1/platform"})
	got := decodeJSON[gen.Platform](t, w.Body.Bytes())
	if w.Code != http.StatusOK || !got.Sftp.Enabled || got.Sftp.Host != "fleet.example.com" || got.Sftp.Port != 2022 {
		t.Errorf("platform: %d %s", w.Code, w.Body)
	}
	if w := do(t, h, request{method: http.MethodGet, target: "/api/v1/platform"}); w.Code != http.StatusUnauthorized {
		t.Errorf("unauthenticated: %d", w.Code)
	}
}
