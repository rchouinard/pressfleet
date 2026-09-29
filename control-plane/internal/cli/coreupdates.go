package cli

import (
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/catalog"
	"github.com/rchouinard/pressfleet/control-plane/internal/coreupdate"
	"github.com/rchouinard/pressfleet/control-plane/internal/sites"
)

// catalog loads the site image version manifest: the operator's
// (image_manifest_file) if set, else the one built in.
func (a *app) catalog() (*catalog.Catalog, error) {
	if a.cfg.ImageManifestFile != "" {
		a.log.Info("using the site image version manifest from image_manifest_file", "path", a.cfg.ImageManifestFile)
		return catalog.LoadFile(a.cfg.ImageManifestFile)
	}
	return catalog.Load()
}

// coreUpdates are the core update settings (config updates).
func (a *app) coreUpdates() sites.CoreUpdateOptions {
	u := a.cfg.Updates
	o := sites.CoreUpdateOptions{
		Policy: coreupdate.Settings{
			MajorAutoApproveAfter: time.Duration(u.Core.MajorAutoApproveDays) * 24 * time.Hour,
			HoldWarningAfter:      time.Duration(u.Core.HoldWarningDays) * 24 * time.Hour,
		},
		MaxFailures: u.MaxFailures,
	}
	if u.HasWindow() {
		o.Window = sites.UpdateWindow{Start: u.Start(), Length: u.Window()}
	}
	return o
}
