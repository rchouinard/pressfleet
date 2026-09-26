package mariadb

import (
	"context"
	"fmt"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/secrets"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
)

// record is a site_databases row.
type record struct {
	SiteID    string
	Database  string
	Username  string
	SecretRef secrets.Ref
	CreatedAt time.Time
}

// getRecord returns the site's row, or store.ErrNotFound.
func getRecord(ctx context.Context, q store.Querier, siteID string) (record, error) {
	var (
		r   record
		ref string
	)
	err := q.QueryRowContext(ctx,
		"SELECT site_id, database_name, username, secret_ref, created_at FROM site_databases WHERE site_id = ?", siteID).
		Scan(&r.SiteID, &r.Database, &r.Username, &ref, &r.CreatedAt)
	if err != nil {
		return record{}, fmt.Errorf("reading site database record: %w", err)
	}
	r.SecretRef = secrets.Ref(ref)
	return r, nil
}

// insertRecord adds r. It returns store.ErrConflict if the site already has
// a row.
func insertRecord(ctx context.Context, q store.Querier, r record) error {
	_, err := q.ExecContext(ctx,
		"INSERT INTO site_databases (site_id, database_name, username, secret_ref, created_at) VALUES (?, ?, ?, ?, ?)",
		r.SiteID, r.Database, r.Username, string(r.SecretRef), r.CreatedAt)
	if err != nil {
		return fmt.Errorf("recording site database: %w", err)
	}
	return nil
}

// deleteRecord removes the site's row if it exists.
func deleteRecord(ctx context.Context, q store.Querier, siteID string) error {
	if _, err := q.ExecContext(ctx, "DELETE FROM site_databases WHERE site_id = ?", siteID); err != nil {
		return fmt.Errorf("deleting site database record: %w", err)
	}
	return nil
}
