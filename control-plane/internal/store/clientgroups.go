package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/id"
)

// ClientGroup is an agency client; it owns sites and a backup repository
// (design doc §4.3).
type ClientGroup struct {
	ID    string
	Name  string
	Notes string
	// BackupRepository references the group's Restic repository; empty until
	// the repository is initialized.
	BackupRepository string
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

// ClientGroups reads and writes client groups. It is the example repository:
// wrap a Querier so the same code runs on a *DB or inside DB.InTx.
type ClientGroups struct {
	q Querier
}

// NewClientGroups returns a ClientGroups that runs queries on q.
func NewClientGroups(q Querier) ClientGroups {
	return ClientGroups{q: q}
}

// clientGroupColumns lists columns in the order scanClientGroup expects.
const clientGroupColumns = "id, name, notes, backup_repository, created_at, updated_at"

type scanner interface {
	Scan(dest ...any) error
}

func scanClientGroup(s scanner) (ClientGroup, error) {
	var g ClientGroup
	err := s.Scan(&g.ID, &g.Name, &g.Notes, &g.BackupRepository, &g.CreatedAt, &g.UpdatedAt)
	return g, err
}

// Create inserts g, filling in its ID and timestamps. It returns ErrConflict
// if the name is taken.
func (r ClientGroups) Create(ctx context.Context, g *ClientGroup) error {
	if g.Name == "" {
		return errors.New("create client group: name is required")
	}
	created := *g
	created.ID = id.New()
	created.CreatedAt = now()
	created.UpdatedAt = created.CreatedAt
	_, err := r.q.ExecContext(ctx,
		"INSERT INTO client_groups ("+clientGroupColumns+") VALUES (?, ?, ?, ?, ?, ?)",
		created.ID, created.Name, created.Notes, created.BackupRepository, created.CreatedAt, created.UpdatedAt)
	if err != nil {
		return fmt.Errorf("create client group: %w", err)
	}
	*g = created
	return nil
}

// Get returns the client group with the given ID, or ErrNotFound.
func (r ClientGroups) Get(ctx context.Context, groupID string) (ClientGroup, error) {
	g, err := scanClientGroup(r.q.QueryRowContext(ctx,
		"SELECT "+clientGroupColumns+" FROM client_groups WHERE id = ?", groupID))
	if err != nil {
		return ClientGroup{}, fmt.Errorf("get client group %s: %w", groupID, err)
	}
	return g, nil
}

// List returns all client groups ordered by name.
func (r ClientGroups) List(ctx context.Context) ([]ClientGroup, error) {
	rows, err := r.q.QueryContext(ctx, "SELECT "+clientGroupColumns+" FROM client_groups ORDER BY name, id")
	if err != nil {
		return nil, fmt.Errorf("list client groups: %w", err)
	}
	defer func() { _ = rows.Close() }()
	groups := []ClientGroup{}
	for rows.Next() {
		g, err := scanClientGroup(rows)
		if err != nil {
			return nil, fmt.Errorf("list client groups: %w", err)
		}
		groups = append(groups, g)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list client groups: %w", err)
	}
	return groups, nil
}

// Update saves g's name, notes, and backup repository and sets UpdatedAt.
// It returns ErrNotFound if g doesn't exist and ErrConflict if the new name
// is taken.
func (r ClientGroups) Update(ctx context.Context, g *ClientGroup) error {
	if g.Name == "" {
		return errors.New("update client group: name is required")
	}
	updated := now()
	var createdAt time.Time
	err := r.q.QueryRowContext(ctx,
		"UPDATE client_groups SET name = ?, notes = ?, backup_repository = ?, updated_at = ? WHERE id = ? RETURNING created_at",
		g.Name, g.Notes, g.BackupRepository, updated, g.ID).Scan(&createdAt)
	if err != nil {
		return fmt.Errorf("update client group %s: %w", g.ID, err)
	}
	g.CreatedAt, g.UpdatedAt = createdAt, updated
	return nil
}

// Delete removes a client group. It returns ErrNotFound if it doesn't exist
// and ErrReference if it still has sites.
func (r ClientGroups) Delete(ctx context.Context, groupID string) error {
	res, err := r.q.ExecContext(ctx, "DELETE FROM client_groups WHERE id = ?", groupID)
	if err != nil {
		return fmt.Errorf("delete client group %s: %w", groupID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete client group %s: %w", groupID, err)
	}
	if n == 0 {
		return fmt.Errorf("delete client group %s: %w", groupID, ErrNotFound)
	}
	return nil
}
