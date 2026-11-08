package cli

import (
	"github.com/rchouinard/pressfleet/control-plane/internal/config"
	"github.com/rchouinard/pressfleet/control-plane/internal/releasecheck"
)

// releaseChecker returns the check for newer PressFleet releases (#140), or
// nil when updates.release_check is off: then nothing asks for releases.
func (a *app) releaseChecker() (*releasecheck.Checker, error) {
	if !a.cfg.Updates.ReleaseCheck {
		return nil, nil
	}
	src := a.opts.ReleaseSource
	if src == nil {
		src = releasecheck.GitHub{Version: a.opts.Version}
	}
	return releasecheck.New(releasecheck.Options{
		Source:     src,
		Version:    a.opts.Version,
		Kubernetes: a.cfg.Driver == config.SiteDriverKubernetes,
		File:       a.cfg.Layout().ReleaseCheckFile(),
		Logger:     a.log.With("component", "releasecheck"),
	})
}
