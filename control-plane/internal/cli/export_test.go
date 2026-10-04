package cli

import (
	"strings"
	"time"
)

// MasterKeyWarning exposes the §4.1 warning to the external tests.
const MasterKeyWarning = masterKeyWarning

// GuideRecoverySteps renders the recovery steps of a failed upgrade as
// docs/upgrading.md shows them.
func GuideRecoverySteps() (string, error) {
	var b strings.Builder
	err := recoveryTemplate.Execute(&b, recoveryData{
		From:          "<previous version>",
		To:            "<new version>",
		Project:       "pressfleet",
		PlatformDir:   "/var/lib/pressfleet/platform",
		PreviousDir:   "/var/lib/pressfleet/upgrade/previous-platform",
		PreviousImage: "<previous control plane image>",
		Backup:        "/var/lib/pressfleet/state/backups/pressfleet-<timestamp>-schema<NNNN>.db",
	})
	return b.String(), err
}

// FastUpgradeReadyChecks shortens the wait between readiness checks of an
// upgraded control plane for the test's duration.
func FastUpgradeReadyChecks(t interface{ Cleanup(func()) }) {
	old := upgradeReadyInterval
	upgradeReadyInterval = time.Millisecond
	t.Cleanup(func() { upgradeReadyInterval = old })
}
