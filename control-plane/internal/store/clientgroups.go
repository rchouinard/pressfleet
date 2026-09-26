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
	// SiteCount is the number of sites in the group. Get and List fill it
	// in; Create and Update leave it as it is.
	SiteCount int
	CreatedAt time.Time
	UpdatedAt time.Time
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

// clientGroupColumns lists the stored columns, in the order of the INSERT.
const clientGroupColumns = "id, name, notes, backup_repository, created_at, updated_at"

// clientGroupSelect selects what scanClientGroup expects: the stored columns
// and the site count.
const clientGroupSelect = "SELECT " + clientGroupColumns +
	", (SELECT COUNT(*) FROM sites WHERE sites.client_group_id = client_groups.id) FROM client_groups"

type scanner interface {
	Scan(dest ...any) error
}

func scanClientGroup(s scanner) (ClientGroup, error) {
	var g ClientGroup
	err := s.Scan(&g.ID, &g.Name, &g.Notes, &g.BackupRepository, &g.CreatedAt, &g.UpdatedAt, &g.SiteCount)
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
	g, err := scanClientGroup(r.q.QueryRowContext(ctx, clientGroupSelect+" WHERE id = ?", groupID))
	if err != nil {
		return ClientGroup{}, fmt.Errorf("get client group %s: %w", groupID, err)
	}
	return g, nil
}

// ClientGroupFilter pages through client groups in List's order.
type ClientGroupFilter struct {
	// AfterName and AfterID, if AfterID is set, only return groups that sort
	// after the group with this name and ID: the pagination cursor. The name
	// is part of the cursor so a page still works if that group was deleted.
	AfterName, AfterID string
	// Limit caps the number of groups returned; zero means no limit.
	Limit int
}

// List returns client groups ordered by name, then ID.
func (r ClientGroups) List(ctx context.Context, f ClientGroupFilter) ([]ClientGroup, error) {
	query, args := clientGroupSelect, []any{}
	if f.AfterID != "" {
		query += " WHERE name > ? OR (name = ? AND id > ?)"
		args = append(args, f.AfterName, f.AfterName, f.AfterID)
	}
	query += " ORDER BY name, id"
	if f.Limit > 0 {
		query += " LIMIT ?"
		args = append(args, f.Limit)
	}
	rows, err := r.q.QueryContext(ctx, query, args...)
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
