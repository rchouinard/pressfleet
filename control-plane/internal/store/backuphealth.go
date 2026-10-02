package store

import (
	"context"
	"fmt"
)

// LatestFinished returns, for each site that has one, its most recent
// backup record that is no longer running, keyed by site ID. Metrics use it
// to tell whether a site's latest backup succeeded.
func (r BackupRecords) LatestFinished(ctx context.Context) (map[string]BackupRecord, error) {
	return r.latestPerSite(ctx, "status IN ('succeeded', 'failed')")
}

// LatestSucceeded returns, for each site that has one, its most recent
// successful backup record, keyed by site ID.
func (r BackupRecords) LatestSucceeded(ctx context.Context) (map[string]BackupRecord, error) {
	return r.latestPerSite(ctx, "status = 'succeeded'")
}

// latestPerSite picks the record with the latest finished_at per site among
// those matching cond (a constant, never user input). It orders by time
// rather than ID because IDs made in the same millisecond don't sort in
// creation order; a tie on the time keeps the larger ID.
func (r BackupRecords) latestPerSite(ctx context.Context, cond string) (map[string]BackupRecord, error) {
	rows, err := r.q.QueryContext(ctx,
		"SELECT "+backupRecordColumns+" FROM backup_records b WHERE "+cond+" AND finished_at = "+
			"(SELECT MAX(finished_at) FROM backup_records WHERE site_id = b.site_id AND "+cond+") ORDER BY id")
	if err != nil {
		return nil, fmt.Errorf("list latest backup records: %w", err)
	}
	defer rows.Close()
	out := map[string]BackupRecord{}
	for rows.Next() {
		b, err := scanBackupRecord(rows)
		if err != nil {
			return nil, fmt.Errorf("scan latest backup record: %w", err)
		}
		out[b.SiteID] = b // ORDER BY id: a later one wins a tie
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list latest backup records: %w", err)
	}
	return out, nil
}
