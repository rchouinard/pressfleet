package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
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
	// PreviewProtection turns on HTTP basic auth on the preview domain
	// (§5.1) with PreviewUsername and the password that
	// PreviewPasswordSecretRef references. PreviewPasswordHash is the
	// password's bcrypt hash, which is all Traefik gets. The credentials
	// are kept while protection is off.
	PreviewProtection        bool
	PreviewUsername          string
	PreviewPasswordHash      string
	PreviewPasswordSecretRef string
	CreatedAt                time.Time
	UpdatedAt                time.Time
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
	"uid, wp_salts_secret_ref, admin_username, admin_email, admin_password_secret_ref, " +
	"preview_protection, preview_username, preview_password_hash, preview_password_secret_ref, created_at, updated_at"

func scanSite(s scanner) (Site, error) {
	var (
		site    Site
		primary sql.NullString
	)
	err := s.Scan(&site.ID, &site.ClientGroupID, &site.Name, &site.Slug, &site.Status, &site.StatusMessage,
		&site.PreviewDomain, &primary, &site.PHPVersion, &site.WPCoreVersion, &site.WPCorePinned, &site.EnableCache,
		&site.CPULimit, &site.MemoryReservationMB, &site.MemoryLimitMB, &site.UID, &site.WPSaltsSecretRef,
		&site.AdminUsername, &site.AdminEmail, &site.AdminPasswordSecretRef,
		&site.PreviewProtection, &site.PreviewUsername, &site.PreviewPasswordHash, &site.PreviewPasswordSecretRef,
		&site.CreatedAt, &site.UpdatedAt)
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
		"("+placeholders(26)+")",
		created.ID, created.ClientGroupID, created.Name, created.Slug, string(created.Status), created.StatusMessage,
		created.PreviewDomain, nullString(created.PrimaryDomain), created.PHPVersion, created.WPCoreVersion,
		created.WPCorePinned, created.EnableCache, created.CPULimit, created.MemoryReservationMB, created.MemoryLimitMB,
		created.UID, created.WPSaltsSecretRef, created.AdminUsername, created.AdminEmail, created.AdminPasswordSecretRef,
		created.PreviewProtection, created.PreviewUsername, created.PreviewPasswordHash, created.PreviewPasswordSecretRef,
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

// SiteFilter selects sites for List. Zero fields don't filter.
type SiteFilter struct {
	ClientGroupID string
	Status        SiteStatus
	// Before is a cursor: only sites with a smaller ID (older ones) are
	// returned. Pass the previous page's NextCursor.
	Before string
	// Limit is the page size: default 50, at most 200.
	Limit int
}

// SitePage is one page of List results, newest first.
type SitePage struct {
	Sites []Site
	// NextCursor is the Before value for the next page, or empty on the
	// last page.
	NextCursor string
}

// List returns the sites matching f, newest first, one page at a time. IDs
// are ULIDs, so ID order is creation order and a cursor stays valid while
// sites are added or deleted.
func (r Sites) List(ctx context.Context, f SiteFilter) (SitePage, error) {
	limit := f.Limit
	switch {
	case limit <= 0:
		limit = 50
	case limit > 200:
		limit = 200
	}
	var (
		where []string
		args  []any
	)
	if f.ClientGroupID != "" {
		where = append(where, "client_group_id = ?")
		args = append(args, f.ClientGroupID)
	}
	if f.Status != "" {
		where = append(where, "status = ?")
		args = append(args, string(f.Status))
	}
	if f.Before != "" {
		where = append(where, "id < ?")
		args = append(args, f.Before)
	}
	query := "SELECT " + siteColumns + " FROM sites"
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	// Fetch one extra row to learn whether there is another page.
	query += " ORDER BY id DESC LIMIT ?"
	args = append(args, limit+1)
	rows, err := r.q.QueryContext(ctx, query, args...)
	if err != nil {
		return SitePage{}, fmt.Errorf("list sites: %w", err)
	}
	defer func() { _ = rows.Close() }()
	page := SitePage{Sites: []Site{}}
	for rows.Next() {
		s, err := scanSite(rows)
		if err != nil {
			return SitePage{}, fmt.Errorf("list sites: %w", err)
		}
		page.Sites = append(page.Sites, s)
	}
	if err := rows.Err(); err != nil {
		return SitePage{}, fmt.Errorf("list sites: %w", err)
	}
	if len(page.Sites) > limit {
		page.Sites = page.Sites[:limit]
		page.NextCursor = page.Sites[limit-1].ID
	}
	return page, nil
}

// AliasesForSites returns the alias domains of several sites, sorted and
// keyed by site ID, for listings. Sites without aliases have no entry.
func (r Sites) AliasesForSites(ctx context.Context, siteIDs []string) (map[string][]string, error) {
	out := map[string][]string{}
	if len(siteIDs) == 0 {
		return out, nil
	}
	args := make([]any, len(siteIDs))
	for i, siteID := range siteIDs {
		args[i] = siteID
	}
	rows, err := r.q.QueryContext(ctx, "SELECT site_id, domain FROM site_aliases WHERE site_id IN ("+
		placeholders(len(siteIDs))+") ORDER BY site_id, domain", args...)
	if err != nil {
		return nil, fmt.Errorf("aliases of sites: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var siteID, domain string
		if err := rows.Scan(&siteID, &domain); err != nil {
			return nil, fmt.Errorf("aliases of sites: %w", err)
		}
		out[siteID] = append(out[siteID], domain)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("aliases of sites: %w", err)
	}
	return out, nil
}

// SiteSettings are the settings of a site that users change after
// creation: its PHP version, resource limits, and whether its preview
// domain is protected, with which username. The preview password isn't
// among them: it is regenerated rather than set (SetPreviewPassword).
type SiteSettings struct {
	PHPVersion          string `json:"php_version"`
	CPULimit            string `json:"cpu_limit"`
	MemoryReservationMB int    `json:"memory_reservation_mb"`
	MemoryLimitMB       int    `json:"memory_limit_mb"`
	PreviewProtection   bool   `json:"preview_protection"`
	PreviewUsername     string `json:"preview_username"`
}

// Settings returns the site's current settings.
func (s Site) Settings() SiteSettings {
	return SiteSettings{
		PHPVersion:          s.PHPVersion,
		CPULimit:            s.CPULimit,
		MemoryReservationMB: s.MemoryReservationMB,
		MemoryLimitMB:       s.MemoryLimitMB,
		PreviewProtection:   s.PreviewProtection,
		PreviewUsername:     s.PreviewUsername,
	}
}

// WithSettings returns a copy of the site with the given settings.
func (s Site) WithSettings(set SiteSettings) Site {
	s.PHPVersion, s.CPULimit = set.PHPVersion, set.CPULimit
	s.MemoryReservationMB, s.MemoryLimitMB = set.MemoryReservationMB, set.MemoryLimitMB
	s.PreviewProtection, s.PreviewUsername = set.PreviewProtection, set.PreviewUsername
	return s
}

// SetSettings saves the site's settings. It reports whether the site
// exists; a missing site is not an error.
func (r Sites) SetSettings(ctx context.Context, siteID string, s SiteSettings) (bool, error) {
	return r.exec(ctx, "set settings of site "+siteID,
		"UPDATE sites SET php_version = ?, cpu_limit = ?, memory_reservation_mb = ?, memory_limit_mb = ?, "+
			"preview_protection = ?, preview_username = ?, updated_at = ? WHERE id = ?",
		s.PHPVersion, s.CPULimit, s.MemoryReservationMB, s.MemoryLimitMB,
		s.PreviewProtection, s.PreviewUsername, now(), siteID)
}

// SetPreviewPassword records the site's preview password: the reference to
// the secret holding it and its bcrypt hash. It reports whether the site
// exists; a missing site is not an error.
func (r Sites) SetPreviewPassword(ctx context.Context, siteID, secretRef, hash string) (bool, error) {
	if secretRef == "" || hash == "" {
		return false, errors.New("set preview password: reference and hash are required")
	}
	return r.exec(ctx, "set preview password of site "+siteID,
		"UPDATE sites SET preview_password_secret_ref = ?, preview_password_hash = ?, updated_at = ? WHERE id = ?",
		secretRef, hash, now(), siteID)
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

// ClearSecretRefs forgets the site's salts, admin password, and preview
// password references, after their secrets have been deleted, and turns
// preview protection off, since it has no password any more. A missing
// site is not an error.
func (r Sites) ClearSecretRefs(ctx context.Context, siteID string) error {
	_, err := r.exec(ctx, "clear secret references of site "+siteID,
		"UPDATE sites SET wp_salts_secret_ref = '', admin_password_secret_ref = '', preview_password_secret_ref = '', "+
			"preview_password_hash = '', preview_protection = ?, updated_at = ? WHERE id = ?", false, now(), siteID)
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
