package cli

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner/compose"
	"github.com/rchouinard/pressfleet/control-plane/internal/skew"
)

// An upgrade recovered by hand (#310). When the automatic rollback can't
// finish, or with --no-rollback, the operator returns the platform to the
// previous release with the printed recovery steps. Those restore the
// platform but not the upgrade record, which keeps the control plane's
// rollouts held (#247). Their last step runs `pressfleet upgrade
// --rollback`, which finds the platform on the previous release and only
// marks the record rolled-back.

// errNeitherRelease means the control plane runs a release that is neither
// the one the last upgrade started from nor the one it went to.
var errNeitherRelease = errors.New("the platform runs neither release of the last upgrade")

// recoveredCause is the cause recorded for an upgrade the operator
// returned to the previous release by hand, when no rollback recorded one.
const recoveredCause = "returned to the previous release by hand, with the recovery steps"

// rollbackAction is what `upgrade --rollback` does, given the upgrade
// record and what the platform runs (decideRollback).
type rollbackAction int

const (
	// rollbackRun rolls the upgrade back, or resumes a rollback that
	// stopped.
	rollbackRun rollbackAction = iota
	// rollbackMarkRecovered marks an unconfirmed upgrade rolled back,
	// changing nothing else.
	rollbackMarkRecovered
	// rollbackRefuseOnPrevious refuses a succeeded upgrade whose platform
	// runs the previous release already.
	rollbackRefuseOnPrevious
	// rollbackRefuseNeither refuses an unconfirmed upgrade whose control
	// plane runs neither release.
	rollbackRefuseNeither
)

// platformState is what `upgrade --rollback` finds the platform running.
type platformState struct {
	// Ready says the control plane's admin interface answered ready, and
	// Version is the version it reported (empty if it isn't ready).
	Ready   bool
	Version string
	// PreviousImage says the control plane container runs the image it
	// ran before the upgrade (the record's previous_images), and
	// PreviousProject that every rendered file of the platform project
	// saved before the upgrade is back in the platform directory. Both are
	// looked at only when the control plane is ready at the record's From.
	PreviousImage   bool
	PreviousProject bool
}

// decideRollback decides what `upgrade --rollback` does for rec, whose
// status is one it can roll back from (not rolled-back), and the platform
// in state.
//
// The platform runs the previous release when the control plane answers
// ready at the record's From, from the control plane image it ran before
// the upgrade, with the platform project saved before the upgrade put back.
// Each signal alone can mislead: an upgrade to a development image
// (--control-plane-image) may report the same version as From; the project
// may be put back without the services being started on it; and readiness
// at From is what says the state database is one the previous release
// accepts, because its control plane refuses a schema the upgrade migrated
// and never becomes ready. That holds for PostgreSQL too, which the CLI
// can't restore or inspect (§4.7): the previous control plane starting on
// the database proves the operator restored their backup, if one was
// needed. So this mode never touches the state database, and doesn't run
// the changes check: what changed since was the operator's recovery.
//
// For an unconfirmed upgrade (in-progress, failed, rolling-back,
// rollback-failed), the platform on the previous release means the
// operator finished the rollback by hand, and --rollback only marks the
// record rolled-back, lifting the rollout hold. Running the rollback
// instead would restore the pre-migration backup a second time, losing
// what the previous control plane recorded since the recovery. A control
// plane ready at a release that is neither From nor To is refused, as
// neither marking nor rolling back is known to be right. That check needs
// To to be a release: a development CLI's version names no control plane.
//
// A succeeded upgrade holds nothing, so there is nothing to mark:
// --rollback rolls it back, unless the platform runs the previous release
// already, which is refused.
func decideRollback(rec upgradeRecord, state platformState) rollbackAction {
	onPrevious := state.Ready && rec.From != "" && state.Version == rec.From && state.PreviousImage && state.PreviousProject
	if rec.Status == upgradeSucceeded {
		if onPrevious {
			return rollbackRefuseOnPrevious
		}
		return rollbackRun
	}
	if onPrevious {
		return rollbackMarkRecovered
	}
	if _, err := skew.Parse(rec.To); err == nil && state.Ready && rec.From != "" &&
		state.Version != rec.From && state.Version != rec.To {
		return rollbackRefuseNeither
	}
	return rollbackRun
}

// platformState finds what the platform runs, for decideRollback. It reads
// the platform project and the control plane container, nothing else.
func (u *upgrader) platformState(ctx context.Context, cp controlPlane) (platformState, error) {
	var s platformState
	if cp.Info == nil || !cp.Info.Ready {
		return s, nil
	}
	s.Ready, s.Version = true, cp.Info.Version
	if u.record.From == "" || s.Version != u.record.From {
		return s, nil
	}
	var err error
	if s.PreviousImage, err = u.runsPreviousControlPlane(ctx); err != nil {
		return s, err
	}
	if s.PreviousProject, err = projectRestored(filepath.Join(u.upgradeDir, previousPlatform), u.platformDir); err != nil {
		return s, fmt.Errorf("comparing the platform project with the one from before the upgrade: %w", err)
	}
	return s, nil
}

// runsPreviousControlPlane reports whether the control plane container
// runs the image the record says it ran before the upgrade: the same image
// ID, or the same reference, by tag or by one of its digests.
func (u *upgrader) runsPreviousControlPlane(ctx context.Context) (bool, error) {
	prev, ok := u.record.PreviousImages[controlPlaneService]
	if !ok {
		return false, nil
	}
	c, err := u.project.ServiceContainer(ctx, controlPlaneService)
	if errors.Is(err, compose.ErrNoContainer) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("finding the control plane's image: %w", err)
	}
	switch {
	case c.ImageID != "" && c.ImageID == prev.ImageID:
		return true, nil
	case c.Image == "":
		return false, nil
	}
	return c.Image == prev.Image || slices.Contains(prev.Digests, c.Image), nil
}

// projectRestored reports whether every rendered file of the platform
// project saved in src is in dst with the same content, as the recovery's
// `cp -a` leaves it. Files only dst has don't count: the copy doesn't
// remove the files the new release added, and the previous release's
// services don't use them. The control plane's runtime files in Traefik's
// dynamic directory (runtimeFile) aren't compared.
func projectRestored(src, dst string) (bool, error) {
	restored := true
	err := filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if d.IsDir() || runtimeFile(rel) {
			return nil
		}
		same, err := sameFile(path, filepath.Join(dst, rel))
		if err != nil {
			return err
		}
		if !same {
			restored = false
			return fs.SkipAll
		}
		return nil
	})
	return restored, err
}

// markRecovered marks an unconfirmed upgrade rolled back, after the
// operator returned the platform to the previous release by hand
// (decideRollback). It writes only the upgrade record, which lifts the
// control plane's rollout hold without a restart.
func (u *upgrader) markRecovered() error {
	rec := &u.record
	now := time.Now().UTC()
	if rec.Rollback == nil {
		rec.Rollback = &rollbackRecord{StartedAt: now}
	}
	rb := rec.Rollback
	if rb.Cause == "" {
		rb.Cause = recoveredCause
	}
	rb.RecoveredByHand = true
	rb.StoppedAt, rb.Error = "", ""
	rb.FinishedAt = &now
	prior := rec.Status
	rec.Status = upgradeRolledBack
	if err := u.writeRecord(); err != nil {
		return fmt.Errorf("marking the upgrade rolled back: %w", err)
	}
	if err := os.RemoveAll(filepath.Join(u.upgradeDir, stagedPlatform)); err != nil {
		u.a.warnf("removing the staged platform project: %v", err)
	}
	u.printf("The platform is back on %s already: its control plane runs the image from before the upgrade to %s "+
		"and is ready, and the platform project from before the upgrade was put back.\n", rec.From, rec.To)
	u.printf("Marked the upgrade rolled back (it was %s): the control plane runs its WordPress core, image, and "+
		"template rollouts again. Nothing else was changed.\n", prior)
	u.printf("  To upgrade to %s again, run pressfleet upgrade with its CLI.\n", rec.To)
	if note := u.skewNote(rec.From); note != "" {
		u.printf("\n%s\n", note)
	}
	return nil
}
