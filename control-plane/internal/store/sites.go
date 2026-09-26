package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/id"
)

// SiteStatus is a site's lifecycle status, as the API reports it.
type SiteStatus string

// Site statuses. Provisioning, Updating, and Deleting mean a job is working
// on the site.
const (
	SiteProvisioning SiteStatus = "provisioning"
	SiteRunning      SiteStatus = "running"
	SiteStopped      SiteStatus = "stopped"
	SiteUpdating     SiteStatus = "updating"
	SiteDeleting     SiteStatus = "deleting"
	SiteFailed       SiteStatus = "failed"
)

// Valid reports whether s is a known status.
func (s SiteStatus) Valid() bool {
	switch s {
	case SiteProvisioning, SiteRunning, SiteStopped, SiteUpdating, SiteDeleting, SiteFailed:
		return true
	}
	return false
}

// Site is a WordPress site (design doc §4.3).
type Site struct {
	ID            string
	ClientGroupID string
	Name          string
	// Slug is unique and forms the preview domain.
	Slug          string
	Status        SiteStatus
	StatusMessage string
	PreviewDomain string
	PrimaryDomain string // empty until go-live
	PHPVersion    string
	WPCoreVersion string
	WPCorePinned  bool
	EnableCache   bool
	// CPULimit, MemoryReservationMB, and MemoryLimitMB are the site's
	// resource limits (provisioner.ResourceLimits).
	CPULimit            string
	MemoryReservationMB int
	MemoryLimitMB       int
	// UID is the site's UID and GID (AllocateSiteUID).
	UID int
	// WPSaltsSecretRef references the site's WordPress keys and salts.
	WPSaltsSecretRef string
	// AdminUsername and AdminEmail are the initial WordPress administrator.
	AdminUsername string
	AdminEmail    string
	// AdminPasswordSecretRef references the administrator's generated
	// password until the API has handed it out (TakeAdminPassword).
	AdminPasswordSecretRef string
	CreatedAt              time.Time
	UpdatedAt              time.Time
}

// Sites reads and writes sites.
type Sites struct {
	q Querier
}

// NewSites returns a Sites that runs queries on q.
func NewSites(q Querier) Sites {
	return Sites{q: q}
}

// siteColumns lists columns in the order scanSite expects.
const siteColumns = "id, client_group_id, name, slug, status, status_message, preview_domain, primary_domain, " +
	"php_version, wp_core_version, wp_core_pinned, enable_cache, cpu_limit, memory_reservation_mb, memory_limit_mb, " +
	"uid, wp_salts_secret_ref, admin_username, admin_email, admin_password_secret_ref, created_at, updated_at"

func scanSite(s scanner) (Site, error) {
	var (
		site    Site
		primary sql.NullString
	)
	err := s.Scan(&site.ID, &site.ClientGroupID, &site.Name, &site.Slug, &site.Status, &site.StatusMessage,
		&site.PreviewDomain, &primary, &site.PHPVersion, &site.WPCoreVersion, &site.WPCorePinned, &site.EnableCache,
		&site.CPULimit, &site.MemoryReservationMB, &site.MemoryLimitMB, &site.UID, &site.WPSaltsSecretRef,
		&site.AdminUsername, &site.AdminEmail, &site.AdminPasswordSecretRef, &site.CreatedAt, &site.UpdatedAt)
	site.PrimaryDomain = primary.String
	return site, err
}

// Create inserts s, filling in its ID and timestamps. It returns ErrConflict
// if the slug, preview domain, or UID is taken and ErrReference if the
// client group doesn't exist.
func (r Sites) Create(ctx context.Context, s *Site) error {
	if !s.Status.Valid() {
		return fmt.Errorf("create site: invalid status %q", s.Status)
	}
	created := *s
	created.ID = id.New()
	created.CreatedAt = now()
	created.UpdatedAt = created.CreatedAt
	_, err := r.q.ExecContext(ctx, "INSERT INTO sites ("+siteColumns+") VALUES "+
		"(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		created.ID, created.ClientGroupID, created.Name, created.Slug, string(created.Status), created.StatusMessage,
		created.PreviewDomain, nullString(created.PrimaryDomain), created.PHPVersion, created.WPCoreVersion,
		created.WPCorePinned, created.EnableCache, created.CPULimit, created.MemoryReservationMB, created.MemoryLimitMB,
		created.UID, created.WPSaltsSecretRef, created.AdminUsername, created.AdminEmail, created.AdminPasswordSecretRef,
		created.CreatedAt, created.UpdatedAt)
	if err != nil {
		return fmt.Errorf("create site: %w", err)
	}
	*s = created
	return nil
}

// Get returns the site with the given ID, or ErrNotFound.
func (r Sites) Get(ctx context.Context, siteID string) (Site, error) {
	s, err := scanSite(r.q.QueryRowContext(ctx, "SELECT "+siteColumns+" FROM sites WHERE id = ?", siteID))
	if err != nil {
		return Site{}, fmt.Errorf("get site %s: %w", siteID, err)
	}
	return s, nil
}

// Aliases returns the site's alias domains, sorted.
func (r Sites) Aliases(ctx context.Context, siteID string) ([]string, error) {
	rows, err := r.q.QueryContext(ctx, "SELECT domain FROM site_aliases WHERE site_id = ? ORDER BY domain", siteID)
	if err != nil {
		return nil, fmt.Errorf("aliases of site %s: %w", siteID, err)
	}
	defer func() { _ = rows.Close() }()
	out := []string{}
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			return nil, fmt.Errorf("aliases of site %s: %w", siteID, err)
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("aliases of site %s: %w", siteID, err)
	}
	return out, nil
}

// SlugTaken reports whether a site already uses the slug or the preview
// domain.
func (r Sites) SlugTaken(ctx context.Context, slug, previewDomain string) (bool, error) {
	var n int
	err := r.q.QueryRowContext(ctx, "SELECT COUNT(*) FROM sites WHERE slug = ? OR preview_domain = ?", slug, previewDomain).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("check slug %q: %w", slug, err)
	}
	return n > 0, nil
}

// ListIDs returns the IDs of the sites whose status is not one of except,
// in ID order.
func (r Sites) ListIDs(ctx context.Context, except ...SiteStatus) ([]string, error) {
	query, args := "SELECT id FROM sites", []any{}
	if len(except) > 0 {
		query += " WHERE status NOT IN (" + placeholders(len(except)) + ")"
		for _, s := range except {
			args = append(args, string(s))
		}
	}
	rows, err := r.q.QueryContext(ctx, query+" ORDER BY id", args...)
	if err != nil {
		return nil, fmt.Errorf("list sites: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var ids []string
	for rows.Next() {
		var siteID string
		if err := rows.Scan(&siteID); err != nil {
			return nil, fmt.Errorf("list sites: %w", err)
		}
		ids = append(ids, siteID)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list sites: %w", err)
	}
	return ids, nil
}

// SetStatus sets the site's status and status message. If from is given,
// only a site whose current status is one of them changes, so a job can't
// overwrite a status another request has set since. It reports whether the
// site changed; a missing site is not an error.
func (r Sites) SetStatus(ctx context.Context, siteID string, status SiteStatus, message string, from ...SiteStatus) (bool, error) {
	if !status.Valid() {
		return false, fmt.Errorf("set status of site %s: invalid status %q", siteID, status)
	}
	query := "UPDATE sites SET status = ?, status_message = ?, updated_at = ? WHERE id = ?"
	args := []any{string(status), message, now(), siteID}
	if len(from) > 0 {
		query += " AND status IN (" + placeholders(len(from)) + ")"
		for _, s := range from {
			args = append(args, string(s))
		}
	}
	return r.exec(ctx, "set status of site "+siteID, query, args...)
}

// ClearSecretRefs forgets the site's salts and admin password references,
// after their secrets have been deleted. A missing site is not an error.
func (r Sites) ClearSecretRefs(ctx context.Context, siteID string) error {
	_, err := r.exec(ctx, "clear secret references of site "+siteID,
		"UPDATE sites SET wp_salts_secret_ref = '', admin_password_secret_ref = '', updated_at = ? WHERE id = ?", now(), siteID)
	return err
}

// TakeAdminPassword forgets the admin password reference if it is still
// ref, reporting whether it was. Of concurrent callers with the same ref,
// only one gets true: the update is conditional on the old value, so the
// password is handed out once.
func (r Sites) TakeAdminPassword(ctx context.Context, siteID, ref string) (bool, error) {
	if ref == "" {
		return false, errors.New("take admin password: reference is empty")
	}
	return r.exec(ctx, "take admin password of site "+siteID,
		"UPDATE sites SET admin_password_secret_ref = '', updated_at = ? WHERE id = ? AND admin_password_secret_ref = ?",
		now(), siteID, ref)
}

// Delete removes a site and its aliases, reporting whether it existed.
func (r Sites) Delete(ctx context.Context, siteID string) (bool, error) {
	return r.exec(ctx, "delete site "+siteID, "DELETE FROM sites WHERE id = ?", siteID)
}

// exec runs a statement and reports whether it changed a row.
func (r Sites) exec(ctx context.Context, what, query string, args ...any) (bool, error) {
	res, err := r.q.ExecContext(ctx, query, args...)
	if err != nil {
		return false, fmt.Errorf("%s: %w", what, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("%s: %w", what, err)
	}
	return n > 0, nil
}
