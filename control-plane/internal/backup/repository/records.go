package repository

import (
	"context"
	"fmt"

	"github.com/rchouinard/pressfleet/control-plane/internal/secrets"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
)

// getRecord returns the group's backup_repositories row, or
// store.ErrNotFound.
func getRecord(ctx context.Context, q store.Querier, groupID string) (Repository, error) {
	var (
		r   Repository
		ref string
	)
	err := q.QueryRowContext(ctx,
		"SELECT client_group_id, backend, location, secret_ref, created_at FROM backup_repositories WHERE client_group_id = ?", groupID).
		Scan(&r.ClientGroupID, &r.Backend, &r.Location, &ref, &r.CreatedAt)
	if err != nil {
		return Repository{}, fmt.Errorf("reading backup repository record: %w", err)
	}
	r.SecretRef = secrets.Ref(ref)
	return r, nil
}

// insertRecord adds r. It returns store.ErrConflict if the group already
// has a row.
func insertRecord(ctx context.Context, q store.Querier, r Repository) error {
	_, err := q.ExecContext(ctx,
		"INSERT INTO backup_repositories (client_group_id, backend, location, secret_ref, created_at) VALUES (?, ?, ?, ?, ?)",
		r.ClientGroupID, r.Backend, r.Location, string(r.SecretRef), r.CreatedAt)
	if err != nil {
		return fmt.Errorf("recording backup repository: %w", err)
	}
	return nil
}

// deleteRecord removes the group's row if it exists.
func deleteRecord(ctx context.Context, q store.Querier, groupID string) error {
	if _, err := q.ExecContext(ctx, "DELETE FROM backup_repositories WHERE client_group_id = ?", groupID); err != nil {
		return fmt.Errorf("deleting backup repository record: %w", err)
	}
	return nil
}

// ClientGroupIDs returns the IDs of the client groups that have a
// repository record, in ID order.
func ClientGroupIDs(ctx context.Context, q store.Querier) ([]string, error) {
	rows, err := q.QueryContext(ctx, "SELECT client_group_id FROM backup_repositories ORDER BY client_group_id")
	if err != nil {
		return nil, fmt.Errorf("listing backup repositories: %w", err)
	}
	defer func() { _ = rows.Close() }()
	ids := []string{}
	for rows.Next() {
		var groupID string
		if err := rows.Scan(&groupID); err != nil {
			return nil, fmt.Errorf("listing backup repositories: %w", err)
		}
		ids = append(ids, groupID)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing backup repositories: %w", err)
	}
	return ids, nil
}
