package cli_test

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestHealthcheck runs against an httptest server standing in for the
// control plane's /healthz endpoint (#12).
func TestHealthcheck(t *testing.T) {
	status := http.StatusOK
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(status)
	}))
	defer srv.Close()
	_, port, err := net.SplitHostPort(srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name   string
		listen string
		status int
		code   int
	}{
		{"healthy wildcard listen", ":" + port, http.StatusOK, 0},
		{"healthy unspecified IP", "0.0.0.0:" + port, http.StatusNoContent, 0},
		{"healthy explicit host", "127.0.0.1:" + port, http.StatusOK, 0},
		{"unhealthy status", ":" + port, http.StatusServiceUnavailable, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status, gotPath = tt.status, ""
			r := run(t, context.Background(), []string{"healthcheck"},
				map[string]string{"PRESSFLEET_LISTEN": tt.listen, "PRESSFLEET_DATA_DIR": t.TempDir()})
			if r.code != tt.code {
				t.Errorf("exit %d, want %d; stderr: %s", r.code, tt.code, r.stderr)
			}
			if gotPath != "/healthz" {
				t.Errorf("requested path %q", gotPath)
			}
		})
	}
}

func TestHealthcheckUnreachable(t *testing.T) {
	// Reserve a port, then close it so nothing is listening.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()

	r := run(t, context.Background(), []string{"healthcheck"}, map[string]string{"PRESSFLEET_LISTEN": addr})
	if r.code != 1 || !strings.Contains(r.stderr, "unhealthy") {
		t.Errorf("exit %d, stderr: %s", r.code, r.stderr)
	}
}

func TestHealthcheckBadListen(t *testing.T) {
	r := run(t, context.Background(), []string{"healthcheck"}, map[string]string{"PRESSFLEET_LISTEN": "no-port"})
	if r.code != 1 || !strings.Contains(r.stderr, "listen address") {
		t.Errorf("exit %d, stderr: %s", r.code, r.stderr)
	}
}
