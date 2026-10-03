package restic

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/backup"
	"github.com/rchouinard/pressfleet/control-plane/internal/backup/repository"
	"github.com/rchouinard/pressfleet/control-plane/internal/id"
)

// forgetBatch bounds the snapshot IDs on one restic forget command line.
const forgetBatch = 100

// ForgetSite implements backup.Engine; see its documentation.
//
// The site's snapshots are found by their site tag and forgotten by ID, so
// no other site's snapshot can match: the group's repository is shared by
// all of its sites (design doc §5.3).
func (e *Engine) ForgetSite(ctx context.Context, siteID string) (backup.PruneResult, error) {
	if !id.Valid(siteID) {
		return backup.PruneResult{}, fmt.Errorf("forget snapshots of site %q: invalid site ID", siteID)
	}
	groupID, err := e.siteGroup(ctx, siteID)
	if err != nil {
		return backup.PruneResult{}, fmt.Errorf("forget snapshots of site %s: %w", siteID, err)
	}
	start := time.Now()
	snaps, err := e.snapshots(ctx, groupID, TagSite+siteID)
	if errors.Is(err, repository.ErrNoRepository) {
		return backup.PruneResult{Forgotten: []backup.SnapshotID{}}, nil
	}
	if err != nil {
		return backup.PruneResult{}, fmt.Errorf("forget snapshots of site %s: %w", siteID, err)
	}
	err = e.unlock(ctx, groupID)
	if errors.Is(err, repository.ErrNoRepository) {
		return backup.PruneResult{Forgotten: []backup.SnapshotID{}}, nil
	}
	if err != nil {
		return backup.PruneResult{}, fmt.Errorf("forget snapshots of site %s: %w", siteID, err)
	}
	forgotten := make([]backup.SnapshotID, 0, len(snaps))
	for batch := range slices.Chunk(snaps, forgetBatch) {
		args := []string{"forget", "--retry-lock", LockWait.String()}
		for _, s := range batch {
			args = append(args, string(s.ID))
		}
		_, err = e.repos.Restic(ctx, groupID, args...)
		if errors.Is(err, repository.ErrNoRepository) {
			// Deleted meanwhile, with every snapshot in it.
			return backup.PruneResult{Forgotten: forgotten}, nil
		}
		if err != nil {
			return backup.PruneResult{}, fmt.Errorf("forget snapshots of site %s: %w", siteID, err)
		}
		for _, s := range batch {
			forgotten = append(forgotten, s.ID)
		}
	}
	// Prune even if nothing was forgotten now: an earlier attempt may have
	// forgotten the snapshots and failed before pruning their data.
	_, err = e.repos.Restic(ctx, groupID, "prune", "--retry-lock", LockWait.String())
	if errors.Is(err, repository.ErrNoRepository) {
		return backup.PruneResult{Forgotten: forgotten}, nil
	}
	if err != nil {
		return backup.PruneResult{}, fmt.Errorf("forget snapshots of site %s: %w", siteID, err)
	}
	e.log.InfoContext(ctx, "site snapshots forgotten", "site_id", siteID, "client_group_id", groupID,
		"forgotten", len(forgotten), "duration", time.Since(start))
	return backup.PruneResult{Forgotten: forgotten}, nil
}
