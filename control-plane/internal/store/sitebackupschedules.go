package store

import (
	"context"
	"database/sql"
	"fmt"
)

// DefaultBackupIntervalHours is the interval of a site's backups unless it
// has chosen another: daily.
const DefaultBackupIntervalHours = 24

// SiteBackupSchedule is a site's choice of when it is backed up (#53, design
// doc §5.3). Package backup/sitebackup turns it into a Schedule. New sites
// get DefaultSiteBackupSchedule.
type SiteBackupSchedule struct {
	// Enabled is false for a site without scheduled backups.
	Enabled bool
	// IntervalHours is the time between backups.
	IntervalHours int
	// Hour is the UTC hour, 0 to 23, that backups start in; nil means
	// within the platform's backup window.
	Hour *int
}

// DefaultSiteBackupSchedule is the schedule of a new site: daily, in the
// platform's backup window.
func DefaultSiteBackupSchedule() SiteBackupSchedule {
	return SiteBackupSchedule{Enabled: true, IntervalHours: DefaultBackupIntervalHours}
}

// BackupSchedule returns the site's backup schedule, or ErrNotFound.
func (r Sites) BackupSchedule(ctx context.Context, siteID string) (SiteBackupSchedule, error) {
	var (
		s    SiteBackupSchedule
		hour sql.NullInt64
	)
	err := r.q.QueryRowContext(ctx,
		"SELECT backup_schedule_enabled, backup_interval_hours, backup_hour FROM sites WHERE id = ?", siteID,
	).Scan(&s.Enabled, &s.IntervalHours, &hour)
	if err != nil {
		return SiteBackupSchedule{}, fmt.Errorf("get backup schedule of site %s: %w", siteID, err)
	}
	if hour.Valid {
		h := int(hour.Int64)
		s.Hour = &h
	}
	return s, nil
}

// SetBackupSchedule saves the site's backup schedule. It reports whether
// the site exists; a missing site is not an error. The caller validates s.
func (r Sites) SetBackupSchedule(ctx context.Context, siteID string, s SiteBackupSchedule) (bool, error) {
	var hour any
	if s.Hour != nil {
		hour = int64(*s.Hour)
	}
	return r.exec(ctx, "set backup schedule of site "+siteID,
		"UPDATE sites SET backup_schedule_enabled = ?, backup_interval_hours = ?, backup_hour = ?, updated_at = ? WHERE id = ?",
		s.Enabled, s.IntervalHours, hour, now(), siteID)
}
