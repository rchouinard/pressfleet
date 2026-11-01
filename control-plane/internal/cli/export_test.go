package cli

import (
	"context"
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

// GuideRecoveryStepUnchecked renders step 2 of the recovery steps as
// docs/upgrading.md shows it for a host that couldn't look for the
// pre-migration backup.
func GuideRecoveryStepUnchecked() (string, error) {
	var b strings.Builder
	err := recoveryTemplate.Execute(&b, recoveryData{
		From:            "<previous version>",
		To:              "<new version>",
		Project:         "pressfleet",
		PlatformDir:     "/var/lib/pressfleet/platform",
		PreviousDir:     "/var/lib/pressfleet/upgrade/previous-platform",
		PreviousImage:   "<previous control plane image>",
		BackupUnchecked: true,
		BackupsDir:      "/var/lib/pressfleet/state/backups",
		Since:           "20260101T120000.000Z",
	})
	step, _, _ := strings.Cut(b.String(), "\n3. ")
	_, step, _ = strings.Cut(step, "\n2. ")
	return "2. " + step, err
}

// HostListDir replaces how the CLI lists the state backups on the host for
// the test's duration, e.g. to deny it as a non-root operator would be.
func HostListDir(t interface{ Cleanup(func()) }, list func(dir string) ([]string, error)) {
	old := hostListDir
	hostListDir = func(_ context.Context, dir string) ([]string, error) { return list(dir) }
	t.Cleanup(func() { hostListDir = old })
}

// FastUpgradeReadyChecks shortens the wait between readiness checks of an
// upgraded control plane for the test's duration.
func FastUpgradeReadyChecks(t interface{ Cleanup(func()) }) {
	old := upgradeReadyInterval
	upgradeReadyInterval = time.Millisecond
	t.Cleanup(func() { upgradeReadyInterval = old })
}
