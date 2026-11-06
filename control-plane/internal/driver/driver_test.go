package driver_test

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/rchouinard/pressfleet/control-plane/internal/catalog"
	"github.com/rchouinard/pressfleet/control-plane/internal/config"
	"github.com/rchouinard/pressfleet/control-plane/internal/driver"
	"github.com/rchouinard/pressfleet/control-plane/internal/redact"
	"github.com/rchouinard/pressfleet/control-plane/internal/secrets"
)

func testConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	return &cfg
}

type noSecrets struct{}

func (noSecrets) Get(context.Context, secrets.Ref) (redact.Secret, error) {
	return "", errors.New("no secrets")
}

func discard() *slog.Logger { return slog.New(slog.DiscardHandler) }

func TestNewCompose(t *testing.T) {
	rt, err := driver.New(t.Context(), driver.Options{Config: testConfig(t), Secrets: noSecrets{}, Images: &catalog.Catalog{}, Logger: discard()})
	if err != nil {
		t.Fatal(err)
	}
	if rt.Driver == nil || rt.FPMStatus == nil || rt.TraefikMetrics == nil ||
		rt.Templates == nil || rt.Snapshots == nil || rt.HostProbe == nil {
		t.Errorf("the Compose driver left a capability out: %+v", rt)
	}
	if rt.Templates.TemplateFingerprint() == "" {
		t.Error("empty template fingerprint")
	}
	if _, ok := rt.Driver.(driver.StartupHook); !ok {
		t.Error("the Compose driver has no startup hook")
	}
	// The optional interface that package sites looks for.
	if _, ok := rt.Driver.(interface {
		AttachSharedServices(ctx context.Context, siteID string) error
	}); !ok {
		t.Error("the Compose driver no longer attaches shared services")
	}
}

func TestStartupRemovesStaleTaskSecrets(t *testing.T) {
	cfg := testConfig(t)
	rt, err := driver.New(t.Context(), driver.Options{Config: cfg, Secrets: noSecrets{}, Images: &catalog.Catalog{}, Logger: discard()})
	if err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(cfg.Layout().TmpDir(), "task-secrets-1234")
	if err := os.MkdirAll(stale, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := rt.Driver.(driver.StartupHook).Startup(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the stale task secrets directory is still there: %v", err)
	}
}

func TestUnknownDriver(t *testing.T) {
	cfg := testConfig(t)
	cfg.Driver = "nomad"
	if _, err := driver.New(t.Context(), driver.Options{Config: cfg, Logger: discard()}); !errors.Is(err, driver.ErrUnknownDriver) {
		t.Errorf("New = %v, want ErrUnknownDriver", err)
	}
	if _, err := driver.NewRepositoryTasks(cfg, discard()); !errors.Is(err, driver.ErrUnknownDriver) {
		t.Errorf("NewRepositoryTasks = %v, want ErrUnknownDriver", err)
	}
	if _, err := driver.NewHostProbe(cfg); !errors.Is(err, driver.ErrUnknownDriver) {
		t.Errorf("NewHostProbe = %v, want ErrUnknownDriver", err)
	}
}

func TestRepositoryTasksAndHostProbe(t *testing.T) {
	cfg := testConfig(t)
	if r, err := driver.NewRepositoryTasks(cfg, discard()); err != nil || r == nil {
		t.Errorf("NewRepositoryTasks = %v, %v", r, err)
	}
	if p, err := driver.NewHostProbe(cfg); err != nil || p == nil {
		t.Errorf("NewHostProbe = %v, %v", p, err)
	}
}
