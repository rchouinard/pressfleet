package store

import (
	"context"
	"errors"
	"fmt"
)

// TargetClientGroup returns the ID of the client group a target belongs to,
// for authorization (design doc §4.6): a client group is its own group, a
// site belongs to its group, an upload to the group of the import or deploy
// it is for, a job belongs to its target's group, an alert rule to the group
// it is limited to, and an alert to the group of its site.
// Other target types (users, API tokens, the platform) belong to no group,
// and neither do targets that don't exist; both return "".
func TargetClientGroup(ctx context.Context, q Querier, targetType, targetID string) (string, error) {
	if targetID == "" {
		return "", nil
	}
	switch targetType {
	case "client_group":
		return targetID, nil
	case "site":
		var group string
		err := q.QueryRowContext(ctx, "SELECT client_group_id FROM sites WHERE id = ?", targetID).Scan(&group)
		if errors.Is(err, ErrNotFound) {
			return "", nil
		} else if err != nil {
			return "", fmt.Errorf("client group of site %s: %w", targetID, err)
		}
		return group, nil
	case "alert_rule", "alert":
		table := "alert_rules"
		if targetType == "alert" {
			table = "alerts"
		}
		var group *string
		err := q.QueryRowContext(ctx, "SELECT client_group_id FROM "+table+" WHERE id = ?", targetID).Scan(&group)
		if errors.Is(err, ErrNotFound) || (err == nil && group == nil) {
			return "", nil
		} else if err != nil {
			return "", fmt.Errorf("client group of %s %s: %w", targetType, targetID, err)
		}
		return *group, nil
	case "upload":
		// An upload belongs to the group its import or deploy is for.
		var group string
		err := q.QueryRowContext(ctx, "SELECT client_group_id FROM uploads WHERE id = ?", targetID).Scan(&group)
		if errors.Is(err, ErrNotFound) {
			return "", nil
		} else if err != nil {
			return "", fmt.Errorf("client group of upload %s: %w", targetID, err)
		}
		return group, nil
	case "job":
		var typ, id string
		err := q.QueryRowContext(ctx, "SELECT target_type, target_id FROM jobs WHERE id = ?", targetID).Scan(&typ, &id)
		if errors.Is(err, ErrNotFound) {
			return "", nil
		} else if err != nil {
			return "", fmt.Errorf("client group of job %s: %w", targetID, err)
		}
		if typ == "job" {
			// Jobs don't target jobs; refuse rather than recurse.
			return "", nil
		}
		return TargetClientGroup(ctx, q, typ, id)
	}
	return "", nil
}
