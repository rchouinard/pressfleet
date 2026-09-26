package provisionertest_test

import (
	"context"
	"errors"
	"testing"

	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner/provisionertest"
)

func config(id string) *provisioner.SiteConfig {
	return &provisioner.SiteConfig{
		ID:            id,
		ClientGroupID: "group1",
		PreviewDomain: id + ".preview.example.com",
		PHPVersion:    "8.4",
		WPCoreVersion: "6.9.1",
		DBSecretRef:   "secret/db/" + id,
		Resources:     provisioner.DefaultResourceLimits(),
	}
}

func TestFakeLifecycle(t *testing.T) {
	ctx := context.Background()
	f := provisionertest.New()

	st, err := f.CreateSite(ctx, config("a"))
	if err != nil {
		t.Fatalf("CreateSite: %v", err)
	}
	if st.Phase != provisioner.SiteRunning || st.URL != "https://a.preview.example.com" {
		t.Errorf("CreateSite status = %+v", st)
	}
	// Create is idempotent.
	if _, err := f.CreateSite(ctx, config("a")); err != nil {
		t.Fatalf("second CreateSite: %v", err)
	}

	if err := f.StopSite(ctx, "a"); err != nil {
		t.Fatalf("StopSite: %v", err)
	}
	if st, _ := f.GetSite(ctx, "a"); st.Phase != provisioner.SiteStopped {
		t.Errorf("phase after stop = %q", st.Phase)
	}

	cfg := config("a")
	cfg.PrimaryDomain = "a.example.com"
	if st, err := f.UpdateSite(ctx, cfg); err != nil || st.URL != "https://a.example.com" {
		t.Errorf("UpdateSite = %+v, %v", st, err)
	}

	if err := f.DeleteSite(ctx, "a"); err != nil {
		t.Fatalf("DeleteSite: %v", err)
	}
	// Delete is idempotent.
	if err := f.DeleteSite(ctx, "a"); err != nil {
		t.Fatalf("second DeleteSite: %v", err)
	}
	if _, err := f.GetSite(ctx, "a"); !errors.Is(err, provisioner.ErrSiteNotFound) {
		t.Errorf("GetSite after delete err = %v, want ErrSiteNotFound", err)
	}
}

func TestFakeNotFound(t *testing.T) {
	ctx := context.Background()
	f := provisionertest.New()
	checks := map[string]error{
		"UpdateSite":     func() error { _, err := f.UpdateSite(ctx, config("x")); return err }(),
		"StartSite":      f.StartSite(ctx, "x"),
		"StopSite":       f.StopSite(ctx, "x"),
		"SetMaintenance": f.SetMaintenance(ctx, "x", true),
		"PurgeCache":     f.PurgeCache(ctx, "x"),
		"ExecWPCLI":      func() error { _, err := f.ExecWPCLI(ctx, "x", nil); return err }(),
	}
	for op, err := range checks {
		if !errors.Is(err, provisioner.ErrSiteNotFound) {
			t.Errorf("%s err = %v, want ErrSiteNotFound", op, err)
		}
	}
}

func TestFakeInvalidConfig(t *testing.T) {
	f := provisionertest.New()
	cfg := config("a")
	cfg.PHPVersion = ""
	if _, err := f.CreateSite(context.Background(), cfg); err == nil {
		t.Fatal("CreateSite with invalid config succeeded")
	}
	if _, ok := f.Site("a"); ok {
		t.Error("invalid config created a site")
	}
}

func TestFakeWPCLIAndFaults(t *testing.T) {
	ctx := context.Background()
	f := provisionertest.New()
	if _, err := f.CreateSite(ctx, config("a")); err != nil {
		t.Fatal(err)
	}

	f.WPCLI = func(_ string, args []string) (string, error) {
		if args[0] == "fail" {
			return "", &provisioner.ExecError{ExitCode: 1, Stderr: "boom"}
		}
		return "ok", nil
	}
	if out, err := f.ExecWPCLI(ctx, "a", []string{"core", "version"}); err != nil || out != "ok" {
		t.Errorf("ExecWPCLI = %q, %v", out, err)
	}
	var execErr *provisioner.ExecError
	if _, err := f.ExecWPCLI(ctx, "a", []string{"fail"}); !errors.As(err, &execErr) || execErr.ExitCode != 1 {
		t.Errorf("ExecWPCLI(fail) err = %v, want *ExecError", err)
	}

	injected := errors.New("injected")
	f.Err = func(op, _ string) error {
		if op == "PurgeCache" {
			return injected
		}
		return nil
	}
	if err := f.PurgeCache(ctx, "a"); !errors.Is(err, injected) {
		t.Errorf("PurgeCache err = %v, want injected", err)
	}
	if s, _ := f.Site("a"); s.CachePurges != 0 {
		t.Errorf("failed purge changed state: %d purges", s.CachePurges)
	}

	calls := f.Calls()
	if last := calls[len(calls)-1]; last.Op != "PurgeCache" || last.SiteID != "a" {
		t.Errorf("last call = %+v", last)
	}
}
