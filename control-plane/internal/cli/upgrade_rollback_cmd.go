package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/authz"
	"github.com/rchouinard/pressfleet/control-plane/internal/changes"
	"github.com/rchouinard/pressfleet/control-plane/internal/platform"
	"github.com/rchouinard/pressfleet/control-plane/internal/sites"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
)

// `pressfleet upgrade --rollback` (design doc §4.7, #250) returns the
// platform to the release before the last upgrade, for an upgrade that
// succeeded but is regretted, with the sequence a failed upgrade rolls back
// with (upgrader.rollBack). Restoring the state database undoes what the
// control plane recorded since the upgrade, but not what it did to sites,
// so before anything changes the CLI asks the running control plane what
// changed (`pressfleet admin changes`, package changes) and refuses the
// changes a restore would leave sites broken or orphaned by, unless
// --force is given. Only one platform project is saved, so only the last
// upgrade can be rolled back, and only once. After a recovery by hand, it
// only marks the upgrade rolled back (upgrade_recovered.go, #310).

// errNoRollback means there is no upgrade --rollback can roll back.
var errNoRollback = errors.New("nothing to roll back")

// errBlockingChanges means the control plane made changes since the
// upgrade that restoring the state database would leave behind.
var errBlockingChanges = errors.New("changes since the upgrade would be left behind")

// errChangesUnknown means --rollback couldn't find out what the control
// plane changed since a succeeded upgrade.
var errChangesUnknown = errors.New("can't tell what changed since the upgrade")

// rollbackCause is the cause an on-demand rollback records.
const rollbackCause = "rolled back on demand (pressfleet upgrade --rollback)"

// runRollback runs `pressfleet upgrade --rollback`. Like upgrade, it is
// exempt from the version skew check (§4.7): the CLI is the release
// upgraded to, and the control plane goes back to an older one. It exits
// with 0 when the platform was rolled back, as asked, and 1 when it wasn't;
// exitRolledBack is only for an upgrade that failed (decided 2026-11-06).
func (u *upgrader) runRollback(ctx context.Context) error {
	if u.f.noRollback || u.f.skipPreflight || u.f.controlPlaneImage != "" {
		return errors.New("--rollback can't be combined with --no-rollback, --skip-preflight, or --control-plane-image")
	}
	u.name = u.a.platformProjectName()
	u.layout = u.a.cfg.Layout()
	u.platformDir = u.layout.PlatformDir()
	u.upgradeDir = u.layout.UpgradeDir()
	u.to = u.a.opts.Version
	u.project = u.a.upgradeProject(u.name, u.platformDir, nil)

	rec, err := readUpgradeRecord(u.upgradeDir)
	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("%w: there is no upgrade record in %s, so the platform hasn't been upgraded since it was installed",
			errNoRollback, u.upgradeDir)
	}
	if err != nil {
		return err
	}
	u.record = rec
	from := rec.From
	if from == "" {
		from = "the previous release"
	}
	resuming := rec.Rollback != nil && (rec.Status == upgradeRollingBack || rec.Status == upgradeRollbackFailed)
	switch rec.Status {
	case upgradeRolledBack:
		at := ""
		if rec.Rollback != nil && rec.Rollback.FinishedAt != nil {
			at = " at " + rec.Rollback.FinishedAt.Format(time.RFC3339)
		}
		return fmt.Errorf("%w: the last upgrade, from %s to %s, was rolled back already%s. Only the most recent upgrade "+
			"can be rolled back, once", errNoRollback, from, rec.To, at)
	case upgradeSucceeded, upgradeFailed, upgradeInProgress, upgradeRollingBack, upgradeRollbackFailed:
	default:
		return fmt.Errorf("the last upgrade's record %s has an unknown status %q", filepath.Join(u.upgradeDir, upgradeRecordFile), rec.Status)
	}
	if !exists(filepath.Join(u.upgradeDir, previousPlatform, platform.ComposeFile)) {
		return fmt.Errorf("%w: the platform project from before the last upgrade isn't in %s",
			errNoRollback, filepath.Join(u.upgradeDir, previousPlatform))
	}

	cp, err := u.a.controlPlaneStatus(ctx)
	if err != nil {
		return err
	}
	u.from = cp.Version
	state, err := u.platformState(ctx, cp)
	if err != nil {
		return err
	}
	switch decideRollback(rec, state) {
	case rollbackMarkRecovered:
		return u.markRecovered()
	case rollbackRefuseOnPrevious:
		return fmt.Errorf("%w: the upgrade from %s to %s succeeded, but the platform runs %s already: its control plane "+
			"runs the image from before the upgrade and is ready, and the platform project from before the upgrade was put "+
			"back. The record says the upgrade succeeded, so the rollouts aren't held; to upgrade again, run pressfleet upgrade",
			errNoRollback, from, rec.To, rec.From)
	case rollbackRefuseNeither:
		return fmt.Errorf("%w: the control plane runs %s, while the last upgrade went from %s to %s. --rollback can't tell "+
			"what the platform runs, so it changes nothing. Return it to %s with the steps in \"Recovering from a failed "+
			"upgrade\" in docs/upgrading.md, which end by running pressfleet upgrade --rollback again, or upgrade it with "+
			"pressfleet upgrade", errNeitherRelease, state.Version, from, rec.To, from)
	case rollbackRun:
	}

	u.printf("Rolling back the upgrade from %s to %s, which started at %s.\n", from, rec.To, rec.StartedAt.Format(time.RFC3339))
	if rec.Status == upgradeSucceeded && cp.Version != "" && rec.To != "" && cp.Version != rec.To {
		u.a.warnf("the control plane runs %s, not %s, the release the last upgrade moved it to", cp.Version, rec.To)
	}

	postgres := u.a.cfg.Database.Driver == string(store.Postgres)
	mig, known, err := u.checkRollback(ctx, postgres)
	if errors.Is(err, errPostgresMigrated) {
		return u.rollbackStopped(context.WithoutCancel(ctx), ctx, &rollbackError{err: err, mig: mig, migKnown: known})
	}
	if err != nil {
		return err
	}
	restore := mig.Migrated() && !u.stateRestored()

	switch {
	case resuming:
		u.printf("The rollback that started at %s didn't finish; resuming it. What changed since the upgrade was "+
			"checked when it started.\n", rec.Rollback.StartedAt.Format(time.RFC3339))
	case restore:
		if err := u.checkChanges(ctx, cp, mig); err != nil {
			return err
		}
	default:
		u.printf("The upgrade didn't migrate the state database, so the rollback keeps it as it is, with everything " +
			"the control plane recorded since the upgrade; there are no changes to check.\n")
	}
	if err := u.confirmRollback(restore, mig, resuming); err != nil {
		return err
	}

	u.printf("\nRolling back to %s. Sites keep serving.\n", from)
	res, err := u.rollBack(ctx, rollbackCause)
	if err != nil {
		return u.rollbackStopped(context.WithoutCancel(ctx), ctx, err)
	}
	u.printf("\nThe upgrade to %s was rolled back: the platform is back on %s and healthy.\n", rec.To, from)
	if res.RestoredFrom != "" {
		u.printf("  The state database was restored from %s, the backup from before the upgrade migrated it. "+
			"The database it replaced was saved beside it.\n", res.RestoredFrom)
	} else {
		u.printf("  The state database was kept as it was: nothing recorded is lost.\n")
	}
	u.printf("  The platform project from before the upgrade was put back, and the previous release's services started.\n")
	u.printf("  To upgrade to %s again, run pressfleet upgrade with its CLI.\n", rec.To)
	if note := u.skewNote(u.record.From); note != "" {
		u.printf("\n%s\n", note)
	}
	return nil
}

// changesSince is the time from which the control plane's changes are
// lost by restoring the state database: the first migration run since the
// upgrade started, by the control plane's own clock, which its records
// use; without markers, the upgrade's start, less the clock skew between
// the host and the container (backupClockSkew).
func (u *upgrader) changesSince(mig upgradeMigration) time.Time {
	for _, m := range mig.Markers {
		if !m.StartedAt.IsZero() {
			return m.StartedAt
		}
	}
	return u.record.StartedAt.Add(-backupClockSkew)
}

// checkChanges asks the control plane what changed since the upgrade,
// lists it, and refuses blocking changes unless --force is given. A control
// plane that doesn't answer is checked only after a succeeded upgrade:
// while an upgrade is unconfirmed, the control plane holds the rollouts
// (#247), so it changes sites only when asked.
func (u *upgrader) checkChanges(ctx context.Context, cp controlPlane, mig upgradeMigration) error {
	since := u.changesSince(mig)
	rep, err := u.askChanges(ctx, cp, since)
	if err != nil {
		switch {
		case u.record.Status != upgradeSucceeded:
			u.printf("The control plane doesn't answer (%v), so the rollback can't list what changed since the upgrade. "+
				"The upgrade was never confirmed, so the control plane held the WordPress core, image, and template "+
				"rollouts: it changed sites only when asked.\n", err)
			return nil
		case u.f.force:
			u.a.warnf("can't tell what changed since the upgrade: %v; rolling back anyway (--force)", err)
			return nil
		}
		return fmt.Errorf("%w: %w. Start the control plane and try again, or roll back without the check with --force",
			errChangesUnknown, err)
	}
	u.printf("\nWhat the control plane changed since %s:\n", since.UTC().Format(time.RFC3339))
	for _, g := range []struct {
		title   string
		changes []changes.Change
	}{
		{"Blocking: the rollback would leave these behind", rep.Blocking},
		{"Converged: the previous release's control plane moves the sites back to match the restored records", rep.Converged},
		{"Lost: their records go with the restored state database; backup snapshots and copied content stay", rep.Lost},
	} {
		u.printf("  %s:\n", g.title)
		if len(g.changes) == 0 {
			u.printf("    none\n")
		}
		for _, c := range g.changes {
			u.printf("    %s\n", describeChange(c))
		}
	}
	if len(rep.Blocking) == 0 {
		return nil
	}
	var left strings.Builder
	for _, c := range rep.Blocking {
		fmt.Fprintf(&left, "\n  - %s", leftBehind(c))
	}
	if u.f.force {
		u.printf("\nWARNING: rolling back anyway (--force) leaves behind:%s\n", left.String())
		return nil
	}
	return fmt.Errorf("%w: restoring the state database would leave behind:%s\nUndo these changes first, or roll back anyway with --force",
		errBlockingChanges, left.String())
}

// askChanges runs `pressfleet admin changes` in the control plane
// container. The CLI never reads the state database itself (§4.7).
func (u *upgrader) askChanges(ctx context.Context, cp controlPlane, since time.Time) (changes.Report, error) {
	if cp.Info == nil {
		if cp.InfoErr != nil {
			return changes.Report{}, cp.InfoErr
		}
		return changes.Report{}, errors.New("the control plane isn't running")
	}
	ctx, cancel := context.WithTimeout(ctx, controlPlaneProbeTimeout)
	defer cancel()
	out, err := u.a.controlPlaneProbe().ExecService(ctx, controlPlaneService,
		"pressfleet", "admin", "changes", "--since", since.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return changes.Report{}, fmt.Errorf("asking the control plane what changed: %w", err)
	}
	var rep changes.Report
	if err := json.Unmarshal(out, &rep); err != nil {
		return changes.Report{}, fmt.Errorf("parsing the control plane's changes: %w", err)
	}
	return rep, nil
}

// describeChange is one line of the changes list.
func describeChange(c changes.Change) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s  %s", c.At.UTC().Format(time.RFC3339), c.Kind)
	switch {
	case c.Site != nil:
		fmt.Fprintf(&b, "  %s", siteName(c.Site))
	case c.ClientGroup != nil:
		fmt.Fprintf(&b, "  %s", groupName(c.ClientGroup))
	case c.TargetType != "":
		fmt.Fprintf(&b, "  %s %s", c.TargetType, c.TargetID)
	}
	if c.JobID != "" {
		fmt.Fprintf(&b, " (job %s, %s)", c.JobID, c.JobState)
	}
	return b.String()
}

// leftBehind says what rolling back past a blocking change leaves behind.
func leftBehind(c changes.Change) string {
	site := "a site"
	if c.Site != nil {
		site = siteName(c.Site)
	}
	group := "a client group"
	if c.ClientGroup != nil {
		group = groupName(c.ClientGroup)
	}
	switch c.Kind {
	case sites.CreateJobType, sites.ImportJobType, sites.CloneJobType:
		return fmt.Sprintf("%s was created after the upgrade: its containers, database, and object cache user would be "+
			"orphaned, with no record", site)
	case sites.DeleteJobType:
		return fmt.Sprintf("%s was deleted after the upgrade: its record would come back, and the previous release's "+
			"control plane would recreate it empty", site)
	case sites.CoreUpdateJobType:
		return fmt.Sprintf("%s had its WordPress core updated: it would go back to an older core than its database", site)
	case sites.GoLiveJobType:
		return fmt.Sprintf("%s went live: its database would keep the live URLs while its record routes the old domains", site)
	case string(authz.DeleteClientGroup), string(authz.OffboardClientGroup):
		return fmt.Sprintf("%s was deleted or offboarded: its record would come back without its backup repository, "+
			"which is gone", group)
	}
	return describeChange(c)
}

func siteName(s *changes.Site) string {
	name := s.Slug
	if name == "" {
		name = s.ID
	}
	if s.Staging {
		return "staging site " + name
	}
	return "site " + name
}

func groupName(g *changes.ClientGroup) string {
	if g.Name != "" {
		return "client group " + g.Name
	}
	return "client group " + g.ID
}

// confirmRollback says what the rollback does and asks to go ahead, unless
// --yes gives it.
func (u *upgrader) confirmRollback(restore bool, mig upgradeMigration, resuming bool) error {
	from := u.record.From
	if from == "" {
		from = "the previous release"
	}
	u.printf("\nThe rollback returns the platform to %s, the release before the upgrade to %s that started at %s.\n",
		from, u.record.To, u.record.StartedAt.Format(time.RFC3339))
	switch {
	case restore:
		u.printf("  The state database is restored from %s, the backup taken before the upgrade migrated it: "+
			"everything the control plane recorded since is lost. The database it replaces is saved beside it.\n", mig.Backup)
	case resuming && u.stateRestored():
		u.printf("  The state database was restored from %s already.\n", u.record.Rollback.StateRestoredFrom)
	default:
		u.printf("  The state database stays as it is: nothing it recorded since the upgrade is lost.\n")
	}
	if hit := slices.DeleteFunc(slices.Clone(u.record.Recreated), func(s string) bool { return !slices.Contains(interrupting, s) }); len(hit) > 0 {
		u.printf("  Recreating %s interrupts every site until %s back up, usually under a minute.\n",
			strings.Join(hit, ", "), plural(len(hit), "it is", "they are"))
	}
	u.printf("  The dashboard, API, and jobs are unavailable until the previous control plane is ready.\n")
	if u.f.yes {
		u.printf("Confirmed with --yes.\n")
		return nil
	}
	for {
		u.printf("Roll back to %s? [y/N]: ", from)
		line, err := u.in.ReadString('\n')
		if err != nil && (!errors.Is(err, io.EOF) || line == "") {
			u.printf("\n")
			return errors.New("the rollback needs confirmation: answer the prompt, or pass --yes")
		}
		switch strings.ToLower(strings.TrimSpace(line)) {
		case "y", "yes":
			return nil
		case "", "n", "no":
			return errors.New("rollback canceled; nothing was changed")
		}
	}
}
