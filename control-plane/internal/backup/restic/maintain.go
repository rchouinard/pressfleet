package restic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/backup"
	"github.com/rchouinard/pressfleet/control-plane/internal/backup/repository"
	"github.com/rchouinard/pressfleet/control-plane/internal/id"
)

// damagedMessage is how restic check ends when it found errors.
const damagedMessage = "repository contains errors"

// Prune implements backup.Engine; see the package documentation.
func (e *Engine) Prune(ctx context.Context, clientGroupID string) (backup.PruneResult, error) {
	if !id.Valid(clientGroupID) {
		return backup.PruneResult{}, fmt.Errorf("prune repository of client group %q: invalid client group ID", clientGroupID)
	}
	if e.keep.KeepsEverything() {
		return backup.PruneResult{Forgotten: []backup.SnapshotID{}}, nil
	}
	start := time.Now()
	if err := e.unlock(ctx, clientGroupID); err != nil {
		return backup.PruneResult{}, fmt.Errorf("prune repository of client group %s: %w", clientGroupID, err)
	}
	out, err := e.repos.Restic(ctx, clientGroupID, forgetArgs(e.keep)...)
	if err != nil {
		return backup.PruneResult{}, fmt.Errorf("prune repository of client group %s: %w", clientGroupID, groupError(err))
	}
	forgotten, err := parseForget(out)
	if err != nil {
		return backup.PruneResult{}, fmt.Errorf("prune repository of client group %s: %w", clientGroupID, err)
	}
	// Prune even if nothing was forgotten now: an earlier attempt may have
	// forgotten snapshots and failed before pruning their data.
	if _, err := e.repos.Restic(ctx, clientGroupID, "prune", "--retry-lock", LockWait.String()); err != nil {
		return backup.PruneResult{}, fmt.Errorf("prune repository of client group %s: %w", clientGroupID, groupError(err))
	}
	e.log.InfoContext(ctx, "backup repository pruned", "client_group_id", clientGroupID,
		"forgotten", len(forgotten), "duration", time.Since(start))
	return backup.PruneResult{Forgotten: forgotten}, nil
}

// forgetArgs is restic forget with the retention policy, applied to each
// site's snapshots on their own: they are grouped by host, which is the
// site's (Host). restic's default grouping adds the paths, which would split
// a site's snapshots if their paths ever changed.
func forgetArgs(keep backup.Retention) []string {
	args := []string{"forget", "--json", "--group-by", "host", "--retry-lock", LockWait.String()}
	for _, rule := range []struct {
		flag string
		n    int
	}{
		{"--keep-last", keep.KeepLast}, {"--keep-hourly", keep.KeepHourly}, {"--keep-daily", keep.KeepDaily},
		{"--keep-weekly", keep.KeepWeekly}, {"--keep-monthly", keep.KeepMonthly}, {"--keep-yearly", keep.KeepYearly},
	} {
		if rule.n > 0 {
			args = append(args, rule.flag, strconv.Itoa(rule.n))
		}
	}
	return args
}

// forgetGroup is one group of snapshots in `restic forget --json` output.
type forgetGroup struct {
	Remove []struct {
		ID string `json:"id"`
	} `json:"remove"`
}

// parseForget returns the IDs of the snapshots restic forget removed.
func parseForget(out []byte) ([]backup.SnapshotID, error) {
	var groups []forgetGroup
	if err := json.Unmarshal(out, &groups); err != nil {
		return nil, fmt.Errorf("reading restic forget's output: %w", err)
	}
	removed := []backup.SnapshotID{}
	for _, g := range groups {
		for _, s := range g.Remove {
			if s.ID == "" {
				return nil, errors.New("reading restic forget's output: a snapshot without an ID")
			}
			removed = append(removed, backup.SnapshotID(s.ID))
		}
	}
	return removed, nil
}

// Check implements backup.Engine; see the package documentation.
func (e *Engine) Check(ctx context.Context, clientGroupID string) error {
	if !id.Valid(clientGroupID) {
		return fmt.Errorf("check repository of client group %q: invalid client group ID", clientGroupID)
	}
	start := time.Now()
	if err := e.unlock(ctx, clientGroupID); err != nil {
		return fmt.Errorf("check repository of client group %s: %w", clientGroupID, err)
	}
	args := []string{"check", "--retry-lock", LockWait.String()}
	if e.readPct > 0 {
		args = append(args, "--read-data-subset="+strconv.Itoa(e.readPct)+"%")
	}
	if _, err := e.repos.Restic(ctx, clientGroupID, args...); err != nil {
		err = groupError(err)
		if strings.Contains(err.Error(), damagedMessage) {
			e.log.ErrorContext(ctx, "backup repository is damaged", "client_group_id", clientGroupID, "error", err)
			err = fmt.Errorf("%w: %w", backup.ErrRepositoryDamaged, err)
		}
		return fmt.Errorf("check repository of client group %s: %w", clientGroupID, err)
	}
	e.log.InfoContext(ctx, "backup repository checked", "client_group_id", clientGroupID,
		"read_data_percent", e.readPct, "duration", time.Since(start))
	return nil
}

// unlock removes stale locks: those of restic processes that died, such
// as a backup task whose container was killed, which would otherwise keep
// prune and check from taking their exclusive lock. restic unlock leaves
// live locks alone.
func (e *Engine) unlock(ctx context.Context, groupID string) error {
	if _, err := e.repos.Restic(ctx, groupID, "unlock"); err != nil {
		return fmt.Errorf("removing stale locks: %w", groupError(err))
	}
	return nil
}

// GroupSnapshots implements backup.Engine.
func (e *Engine) GroupSnapshots(ctx context.Context, clientGroupID string) ([]backup.Snapshot, error) {
	if !id.Valid(clientGroupID) {
		return nil, fmt.Errorf("list snapshots of client group %q: invalid client group ID", clientGroupID)
	}
	out, err := e.repos.Restic(ctx, clientGroupID, "snapshots", "--json", "--no-lock")
	if errors.Is(err, repository.ErrNoRepository) {
		return []backup.Snapshot{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list snapshots of client group %s: %w", clientGroupID, err)
	}
	snaps, err := parseSnapshots(out)
	if err != nil {
		return nil, fmt.Errorf("list snapshots of client group %s: %w", clientGroupID, err)
	}
	return snaps, nil
}

// groupError adds backup.ErrNoRepository to an error for a group without a
// repository.
func groupError(err error) error {
	if errors.Is(err, repository.ErrNoRepository) && !errors.Is(err, backup.ErrNoRepository) {
		return fmt.Errorf("%w: %w", backup.ErrNoRepository, err)
	}
	return err
}
