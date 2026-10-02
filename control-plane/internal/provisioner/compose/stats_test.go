package compose_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/config"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner/compose"
)

const cgroupOut = "usage_usec 2500000\nuser_usec 2000000\nsystem_usec 500000\n" +
	"memory.current 104857600\nmemory.max max\ninactive_file 4857600\n"

func statsProvisioner(t *testing.T, noSite bool, exec func(args []string) ([]byte, error)) *compose.Provisioner {
	t.Helper()
	layout := config.Layout{Root: t.TempDir()}
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	p, err := compose.NewProvisioner(compose.ProvisionerConfig{
		Layout: layout, Secrets: memSecrets{}, Images: imageMap{}, MariaDBContainer: testMariaDB,
		Docker: func(_ context.Context, _ io.Reader, args ...string) ([]byte, error) {
			if args[0] == "compose" && slices.Contains(args, "ps") {
				if noSite {
					return nil, nil
				}
				var out []byte
				for _, svc := range []string{"php", "nginx"} {
					b, _ := json.Marshal(compose.Container{Name: "pf-" + testSiteID + "-" + svc + "-1", Service: svc, State: "running"})
					out = append(append(out, b...), '\n')
				}
				return out, nil
			}
			return exec(args)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestStats(t *testing.T) {
	p := statsProvisioner(t, false, func(args []string) ([]byte, error) {
		switch args[1] {
		case "exec":
			return []byte(cgroupOut), nil
		case "inspect":
			if strings.HasSuffix(args[len(args)-1], "-php-1") {
				return []byte("3\n"), nil
			}
			return []byte("0\n"), nil
		}
		t.Errorf("unexpected docker call %q", args)
		return nil, nil
	})
	got, err := p.Stats(context.Background(), testSiteID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Component != provisioner.ComponentPHP {
		t.Fatalf("stats = %+v", got)
	}
	s := got[0]
	if s.CPUUsage != 2500*time.Millisecond || s.MemoryBytes != 100000000 || s.MemoryLimitBytes != 0 || s.Restarts != 3 || s.SampledAt.IsZero() {
		t.Errorf("php stats = %+v", s)
	}
	if got[1].Restarts != 0 {
		t.Errorf("nginx restarts = %d", got[1].Restarts)
	}
}

func TestStatsMemoryLimit(t *testing.T) {
	out := "usage_usec 1\nmemory.current 10\nmemory.max 512\ntotal_inactive_file 20\n"
	p := statsProvisioner(t, false, func(args []string) ([]byte, error) {
		if args[1] == "exec" {
			return []byte(out), nil
		}
		return []byte("1"), nil
	})
	got, err := p.Stats(context.Background(), testSiteID)
	if err != nil {
		t.Fatal(err)
	}
	// Inactive file larger than usage must not underflow.
	if got[0].MemoryLimitBytes != 512 || got[0].MemoryBytes != 10 {
		t.Errorf("stats = %+v", got[0])
	}
}

func TestStatsErrors(t *testing.T) {
	_, err := statsProvisioner(t, true, nil).Stats(context.Background(), testSiteID)
	if !errors.Is(err, provisioner.ErrSiteNotFound) {
		t.Errorf("missing site: %v", err)
	}
	p := statsProvisioner(t, false, func(args []string) ([]byte, error) {
		if args[1] == "exec" {
			return []byte("no cgroup here\n"), nil
		}
		return []byte("0"), nil
	})
	if _, err := p.Stats(context.Background(), testSiteID); err == nil {
		t.Error("unreadable cgroup: want error")
	}
}
