package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
)

// ErrSiteUIDsExhausted means every UID in the site range has been allocated.
var ErrSiteUIDsExhausted = errors.New("no site UIDs left")

// AllocateSiteUID reserves the next site UID in [provisioner.MinSiteUID,
// provisioner.MaxSiteUID]. Call it in the transaction that creates the site
// ([DB.InTx]) so a rolled-back creation doesn't use up a UID.
//
// UIDs are never reused, even after their site is deleted: a counter that
// only grows hands them out (see migration 0005). Concurrent callers are
// serialized by the counter row's lock, so they always get distinct UIDs.
func AllocateSiteUID(ctx context.Context, q Querier) (int, error) {
	var uid int
	err := q.QueryRowContext(ctx,
		"UPDATE site_uid_counter SET last_uid = last_uid + 1 WHERE id = 1 AND last_uid < ? RETURNING last_uid",
		provisioner.MaxSiteUID).Scan(&uid)
	if errors.Is(err, ErrNotFound) {
		return 0, fmt.Errorf("allocate site UID: %w (range %d-%d)", ErrSiteUIDsExhausted, provisioner.MinSiteUID, provisioner.MaxSiteUID)
	}
	if err != nil {
		return 0, fmt.Errorf("allocate site UID: %w", err)
	}
	if uid < provisioner.MinSiteUID {
		// Only possible if the counter row was edited by hand.
		return 0, fmt.Errorf("allocate site UID: counter produced %d, below the site range", uid)
	}
	return uid, nil
}
