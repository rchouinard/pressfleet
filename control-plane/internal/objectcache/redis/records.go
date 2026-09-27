package redis

import (
	"context"
	"fmt"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/secrets"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
)

// record is a site_object_caches row.
type record struct {
	SiteID    string
	Username  string
	DB        int
	SecretRef secrets.Ref
	CreatedAt time.Time
}

const recordColumns = "site_id, username, db_index, secret_ref, created_at"

type scanner interface{ Scan(dest ...any) error }

func scanRecord(s scanner) (record, error) {
	var (
		r   record
		ref string
	)
	if err := s.Scan(&r.SiteID, &r.Username, &r.DB, &ref, &r.CreatedAt); err != nil {
		return record{}, err
	}
	r.SecretRef = secrets.Ref(ref)
	return r, nil
}

// getRecord returns the site's row, or store.ErrNotFound.
func getRecord(ctx context.Context, q store.Querier, siteID string) (record, error) {
	r, err := scanRecord(q.QueryRowContext(ctx,
		"SELECT "+recordColumns+" FROM site_object_caches WHERE site_id = ?", siteID))
	if err != nil {
		return record{}, fmt.Errorf("reading site object cache record: %w", err)
	}
	return r, nil
}

// listRecords returns every row, ordered by database.
func listRecords(ctx context.Context, q store.Querier) ([]record, error) {
	rows, err := q.QueryContext(ctx, "SELECT "+recordColumns+" FROM site_object_caches ORDER BY db_index")
	if err != nil {
		return nil, fmt.Errorf("listing site object cache records: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []record
	for rows.Next() {
		r, err := scanRecord(rows)
		if err != nil {
			return nil, fmt.Errorf("listing site object cache records: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing site object cache records: %w", err)
	}
	return out, nil
}

// usedDatabases returns the databases held by sites, in ascending order.
func usedDatabases(ctx context.Context, q store.Querier) ([]int, error) {
	rows, err := q.QueryContext(ctx, "SELECT db_index FROM site_object_caches ORDER BY db_index")
	if err != nil {
		return nil, fmt.Errorf("listing site object cache databases: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []int
	for rows.Next() {
		var db int
		if err := rows.Scan(&db); err != nil {
			return nil, fmt.Errorf("listing site object cache databases: %w", err)
		}
		out = append(out, db)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing site object cache databases: %w", err)
	}
	return out, nil
}

// insertRecord adds r. It returns store.ErrConflict if the site or the
// database already has a row.
func insertRecord(ctx context.Context, q store.Querier, r record) error {
	_, err := q.ExecContext(ctx,
		"INSERT INTO site_object_caches ("+recordColumns+") VALUES (?, ?, ?, ?, ?)",
		r.SiteID, r.Username, r.DB, string(r.SecretRef), r.CreatedAt)
	if err != nil {
		return fmt.Errorf("recording site object cache: %w", err)
	}
	return nil
}

// deleteRecord removes the site's row if it exists.
func deleteRecord(ctx context.Context, q store.Querier, siteID string) error {
	if _, err := q.ExecContext(ctx, "DELETE FROM site_object_caches WHERE site_id = ?", siteID); err != nil {
		return fmt.Errorf("deleting site object cache record: %w", err)
	}
	return nil
}
