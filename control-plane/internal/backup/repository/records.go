package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/config"
	"github.com/rchouinard/pressfleet/control-plane/internal/secrets"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
)

// getRecord returns the group's backup_repositories row, or
// store.ErrNotFound.
func getRecord(ctx context.Context, q store.Querier, groupID string) (Repository, error) {
	var (
		r        Repository
		ref      string
		verified sql.NullTime
	)
	err := q.QueryRowContext(ctx,
		"SELECT client_group_id, backend, location, secret_ref, created_at, password_verified_at FROM backup_repositories WHERE client_group_id = ?", groupID).
		Scan(&r.ClientGroupID, &r.Backend, &r.Location, &ref, &r.CreatedAt, &verified)
	if err != nil {
		return Repository{}, fmt.Errorf("reading backup repository record: %w", err)
	}
	r.SecretRef = secrets.Ref(ref)
	if verified.Valid {
		r.PasswordVerifiedAt = verified.Time
	}
	return r, nil
}

// markVerified records that rec's password opens its repository. It
// changes nothing if the record has changed since rec was read, so a
// location or password other than the one that opened can't be marked.
func markVerified(ctx context.Context, q store.Querier, rec Repository, at time.Time) error {
	_, err := q.ExecContext(ctx,
		"UPDATE backup_repositories SET password_verified_at = ? "+
			"WHERE client_group_id = ? AND location = ? AND secret_ref = ? AND password_verified_at IS NULL",
		at, rec.ClientGroupID, rec.Location, string(rec.SecretRef))
	if err != nil {
		return fmt.Errorf("marking backup repository password verified: %w", err)
	}
	return nil
}

// moveRecord points rec's record at location and reports whether it did:
// false means the record has changed since rec was read, or its password
// has been verified since, and was left alone.
func moveRecord(ctx context.Context, q store.Querier, rec Repository, location string) (bool, error) {
	res, err := q.ExecContext(ctx,
		"UPDATE backup_repositories SET location = ? "+
			"WHERE client_group_id = ? AND location = ? AND secret_ref = ? AND password_verified_at IS NULL",
		location, rec.ClientGroupID, rec.Location, string(rec.SecretRef))
	if err != nil {
		return false, fmt.Errorf("moving backup repository record: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("moving backup repository record: %w", err)
	}
	return n == 1, nil
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

// markDeleting records that the group's repository is being deleted, in
// one transaction with the check that the group has no sites, so a site
// can't be created in between: store.Sites.Create takes the same group
// lock and refuses a group whose repository is marked (#351). It returns
// ErrGroupHasSites if the group has sites. A group that no longer exists
// has no sites.
func markDeleting(ctx context.Context, db *store.DB, groupID string, at time.Time) error {
	return db.InTx(ctx, func(q store.Querier) error {
		groups := store.NewClientGroups(q)
		switch err := groups.Lock(ctx, groupID); {
		case errors.Is(err, store.ErrNotFound):
		case err != nil:
			return err
		default:
			g, err := groups.Get(ctx, groupID)
			if err != nil {
				return err
			}
			if g.SiteCount > 0 {
				return fmt.Errorf("%w (%d)", ErrGroupHasSites, g.SiteCount)
			}
		}
		if _, err := q.ExecContext(ctx, "UPDATE backup_repositories SET deleting_at = ? WHERE client_group_id = ?", at, groupID); err != nil {
			return fmt.Errorf("marking backup repository as being deleted: %w", err)
		}
		return nil
	})
}

// clearDeleting lets sites be created in the group again after a failed
// deletion.
func clearDeleting(ctx context.Context, q store.Querier, groupID string) error {
	if _, err := q.ExecContext(ctx, "UPDATE backup_repositories SET deleting_at = NULL WHERE client_group_id = ?", groupID); err != nil {
		return fmt.Errorf("clearing the deletion mark of the backup repository: %w", err)
	}
	return nil
}

// ClearDeleting clears every deletion mark and returns how many it
// cleared. A deletion that was running when the control plane stopped
// leaves its mark behind, which would refuse new sites in the group until
// the deletion is retried; the control plane calls it at startup, before
// it serves requests. The deletion can simply be run again.
func (m *Manager) ClearDeleting(ctx context.Context) (int, error) {
	res, err := m.db.ExecContext(ctx, "UPDATE backup_repositories SET deleting_at = NULL WHERE deleting_at IS NOT NULL")
	if err != nil {
		return 0, fmt.Errorf("clearing the deletion marks of backup repositories: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("clearing the deletion marks of backup repositories: %w", err)
	}
	return int(n), nil
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

// RelocateLocal points every local repository record at layout's
// repositories directory, and returns how many it changed. A local
// repository is recorded by absolute path, which a state backup restored on
// a host with another data directory carries over from the old one; the
// repositories themselves are under the new directory, copied there with
// the rest of <data_dir>/backups. It is for state recovery, in the
// restoring transaction, and works on any dialect: the location of a local
// repository is a function of the data directory and the group, so nothing
// else is needed to rewrite it. S3 records are left alone.
func RelocateLocal(ctx context.Context, q store.Querier, layout config.Layout) (int, error) {
	rows, err := q.QueryContext(ctx, "SELECT client_group_id, location FROM backup_repositories WHERE backend = ?", BackendLocal)
	if err != nil {
		return 0, fmt.Errorf("listing local backup repositories: %w", err)
	}
	moves := map[string]string{}
	for rows.Next() {
		var groupID, location string
		if err := rows.Scan(&groupID, &location); err != nil {
			_ = rows.Close()
			return 0, fmt.Errorf("listing local backup repositories: %w", err)
		}
		want, err := layout.BackupRepositoryDir(groupID)
		if err != nil {
			_ = rows.Close()
			return 0, err
		}
		if location != want {
			moves[groupID] = want
		}
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return 0, fmt.Errorf("listing local backup repositories: %w", err)
	}
	for groupID, location := range moves {
		if _, err := q.ExecContext(ctx, "UPDATE backup_repositories SET location = ? WHERE client_group_id = ?", location, groupID); err != nil {
			return 0, fmt.Errorf("relocating backup repository of client group %s: %w", groupID, err)
		}
		// The mirror for display; not a change of the group, so updated_at stays.
		if _, err := q.ExecContext(ctx, "UPDATE client_groups SET backup_repository = ? WHERE id = ?", location, groupID); err != nil {
			return 0, fmt.Errorf("relocating backup repository of client group %s: %w", groupID, err)
		}
	}
	return len(moves), nil
}
