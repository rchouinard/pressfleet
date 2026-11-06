package kubernetes

import (
	"errors"
	"testing"

	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
)

// Every operation a later change implements says so, rather than doing
// nothing.
func TestNotImplemented(t *testing.T) {
	p := testProvisioner(t)
	ctx := t.Context()
	cfg := testSite()
	ops := map[string]func() error{
		"CreateSite": func() error { _, err := p.CreateSite(ctx, cfg); return err },
		"UpdateSite": func() error { _, err := p.UpdateSite(ctx, cfg); return err },
		"GetSite":    func() error { _, err := p.GetSite(ctx, cfg.ID); return err },
		"DeleteSite": func() error { return p.DeleteSite(ctx, cfg.ID) },
		"StartSite":  func() error { return p.StartSite(ctx, cfg.ID) },
		"StopSite":   func() error { return p.StopSite(ctx, cfg.ID) },
		"SetMaintenance": func() error {
			return p.SetMaintenance(ctx, cfg.ID, true)
		},
		"ExecWPCLI":  func() error { _, err := p.ExecWPCLI(ctx, cfg.ID, []string{"option", "get", "home"}); return err },
		"PurgeCache": func() error { return p.PurgeCache(ctx, cfg.ID) },
		"ReloadCode": func() error { return p.ReloadCode(ctx, cfg.ID) },
		"Logs": func() error {
			for _, err := range p.Logs(ctx, cfg.ID, provisioner.LogOptions{}) {
				return err
			}
			return nil
		},
		"Stats":   func() error { _, err := p.Stats(ctx, cfg.ID); return err },
		"RunTask": func() error { _, err := p.RunTask(ctx, cfg.ID, provisioner.Task{}); return err },
		"RunRepositoryTask": func() error {
			_, err := p.RunRepositoryTask(ctx, provisioner.StateRepository, provisioner.Task{})
			return err
		},
		"RunSession": func() error { return p.RunSession(ctx, cfg.ID, provisioner.Session{}) },
		"ProbeHTTP": func() error {
			_, err := p.ProbeHTTP(ctx, cfg.ID, provisioner.ProbeRequest{Host: cfg.PreviewDomain, Path: "/"})
			return err
		},
		"PinImage":  func() error { _, err := p.PinImage(ctx, "ghcr.io/rchouinard/pressfleet/site-php:8.4"); return err },
		"PullImage": func() error { return p.PullImage(ctx, "ghcr.io/rchouinard/pressfleet/site-php:8.4") },
	}
	for name, op := range ops {
		if err := op(); !errors.Is(err, ErrNotImplemented) {
			t.Errorf("%s = %v, want ErrNotImplemented", name, err)
		}
	}
}

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
