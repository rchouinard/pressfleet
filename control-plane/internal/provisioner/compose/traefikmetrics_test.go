package compose

import (
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
)

func TestTraefikMetricsArgv(t *testing.T) {
	argv := traefikMetricsArgv("pf-traefik")
	if want := []string{"container", "exec", "pf-traefik"}; !slices.Equal(argv[:3], want) {
		t.Errorf("argv = %q", argv)
	}
	// The endpoint is loopback only; any other address would be reachable
	// from site networks (security review F5).
	if last := argv[len(argv)-1]; last != "http://127.0.0.1:8082/metrics" {
		t.Errorf("URL = %q", last)
	}
}

func TestTraefikMetrics(t *testing.T) {
	t.Run("reads from the Traefik container", func(t *testing.T) {
		var got []string
		p := &Provisioner{traefik: "pf-traefik", docker: func(_ context.Context, _ io.Reader, args ...string) ([]byte, error) {
			got = args
			return []byte("traefik_up 1\n"), nil
		}}
		out, err := p.TraefikMetrics(context.Background())
		if err != nil || string(out) != "traefik_up 1\n" {
			t.Fatalf("out = %q, err = %v", out, err)
		}
		if !slices.Contains(got, "pf-traefik") || !strings.HasSuffix(got[len(got)-1], "/metrics") {
			t.Errorf("args = %q", got)
		}
	})
	t.Run("command error", func(t *testing.T) {
		p := &Provisioner{traefik: "pf-traefik", docker: func(context.Context, io.Reader, ...string) ([]byte, error) {
			return nil, &CommandError{Stderr: "wget: can't connect"}
		}}
		if _, err := p.TraefikMetrics(context.Background()); !errors.Is(err, ErrTraefikMetrics) {
			t.Errorf("err = %v, want ErrTraefikMetrics", err)
		}
	})
	t.Run("no container", func(t *testing.T) {
		if _, err := (&Provisioner{}).TraefikMetrics(context.Background()); !errors.Is(err, ErrTraefikMetrics) {
			t.Errorf("err = %v, want ErrTraefikMetrics", err)
		}
	})
}
