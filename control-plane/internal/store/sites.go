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
	// ParentSiteID is the production site a staging site was cloned from
	// (§5.11); empty for a production site.
	ParentSiteID string
	Name         string
	// Slug is unique and forms the preview domain.
	Slug          string
	Status        SiteStatus
	StatusMessage string
	PreviewDomain string
	PrimaryDomain string // empty until go-live
	PHPVersion    string
	WPCoreVersion string
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
	// Maintenance means the site's visitors get the platform's maintenance
	// page (§5.4).
	Maintenance bool
	// URLRewriteFrom are the host names whose URLs go-live still has to
	// rewrite to the primary domain's in the site's WordPress database
	// (§5.2), until a go-live job has.
	URLRewriteFrom []string
	// TablePrefix is the WordPress table prefix of the site's database;
	// Create fills in DefaultTablePrefix when it is empty. An imported site
	// keeps its dump's prefix (§5.7).
	TablePrefix string
	// TemplateFingerprint identifies the driver templates the site was last
	// rendered with (provisioner.TemplateFingerprinter); empty if unknown,
	// such as for a site rendered before fingerprints were recorded.
	TemplateFingerprint string
	// MailDisabled means the site sends no outbound mail (§5.11). Only
	// staging sites turn it off.
	MailDisabled bool
	// StagingRefreshedAt is when a staging site was last cloned or
	// refreshed from production, StagingSnapshotID the production snapshot
	// it came from, and StagingPushedAt when it was last pushed to
	// production. The times are zero until then.
	StagingRefreshedAt time.Time
	StagingSnapshotID  string
	StagingPushedAt    time.Time
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

// IsStaging reports whether the site is a staging site: a clone of a
// production site.
func (s Site) IsStaging() bool { return s.ParentSiteID != "" }

// Sites reads and writes sites.
type Sites struct {
	q Querier
}

// NewSites returns a Sites that runs queries on q.
func NewSites(q Querier) Sites {
	return Sites{q: q}
}

// siteColumns lists columns in the order scanSite expects.
const siteColumns = "id, client_group_id, parent_site_id, name, slug, status, status_message, preview_domain, primary_domain, " +
	"php_version, wp_core_version, enable_cache, cpu_limit, memory_reservation_mb, memory_limit_mb, " +
	"uid, wp_salts_secret_ref, admin_username, admin_email, admin_password_secret_ref, " +
	"preview_protection, preview_username, preview_password_hash, preview_password_secret_ref, maintenance, url_rewrite_from, " +
	"table_prefix, template_fingerprint, mail_disabled, staging_refreshed_at, staging_snapshot_id, staging_pushed_at, " +
	"created_at, updated_at"

// siteColumnCount is the number of siteColumns, so the INSERT's
// placeholders follow the column list.
var siteColumnCount = strings.Count(siteColumns, ",") + 1

func scanSite(s scanner) (Site, error) {
	var (
		site    Site
		primary sql.NullString
		parent  sql.NullString
		snap    sql.NullString
		rewrite string
		refresh sql.NullTime
		pushed  sql.NullTime
	)
	err := s.Scan(&site.ID, &site.ClientGroupID, &parent, &site.Name, &site.Slug, &site.Status, &site.StatusMessage,
		&site.PreviewDomain, &primary, &site.PHPVersion, &site.WPCoreVersion, &site.EnableCache,
		&site.CPULimit, &site.MemoryReservationMB, &site.MemoryLimitMB, &site.UID, &site.WPSaltsSecretRef,
		&site.AdminUsername, &site.AdminEmail, &site.AdminPasswordSecretRef,
		&site.PreviewProtection, &site.PreviewUsername, &site.PreviewPasswordHash, &site.PreviewPasswordSecretRef,
		&site.Maintenance, &rewrite, &site.TablePrefix, &site.TemplateFingerprint,
		&site.MailDisabled, &refresh, &snap, &pushed, &site.CreatedAt, &site.UpdatedAt)
	site.PrimaryDomain, site.ParentSiteID, site.StagingSnapshotID = primary.String, parent.String, snap.String
	if refresh.Valid {
		site.StagingRefreshedAt = refresh.Time
	}
	if pushed.Valid {
		site.StagingPushedAt = pushed.Time
	}
	if rewrite != "" {
		site.URLRewriteFrom = strings.Fields(rewrite)
	}
	return site, err
}

// DefaultTablePrefix is WordPress's default table prefix, which
// provisioned sites use.
const DefaultTablePrefix = "wp_"

// Create inserts s, filling in its ID (unless it has one: a site import
// picks its ID before the site exists), timestamps, and default table
// prefix. It returns ErrConflict if the ID, slug, preview domain, or UID is
// taken and ErrReference if the client group doesn't exist.
func (r Sites) Create(ctx context.Context, s *Site) error {
	if !s.Status.Valid() {
		return fmt.Errorf("create site: invalid status %q", s.Status)
	}
	created := *s
	switch {
	case created.ID == "":
		created.ID = id.New()
	case !id.Valid(created.ID):
		return fmt.Errorf("create site: invalid ID %q", created.ID)
	}
	if created.TablePrefix == "" {
		created.TablePrefix = DefaultTablePrefix
	}
	created.CreatedAt = now()
	created.UpdatedAt = created.CreatedAt
	_, err := r.q.ExecContext(ctx, "INSERT INTO sites ("+siteColumns+") VALUES "+
		"("+placeholders(siteColumnCount)+")",
		created.ID, created.ClientGroupID, nullString(created.ParentSiteID), created.Name, created.Slug, string(created.Status), created.StatusMessage,
		created.PreviewDomain, nullString(created.PrimaryDomain), created.PHPVersion, created.WPCoreVersion,
		created.EnableCache, created.CPULimit, created.MemoryReservationMB, created.MemoryLimitMB,
		created.UID, created.WPSaltsSecretRef, created.AdminUsername, created.AdminEmail, created.AdminPasswordSecretRef,
		created.PreviewProtection, created.PreviewUsername, created.PreviewPasswordHash, created.PreviewPasswordSecretRef,
		created.Maintenance, strings.Join(created.URLRewriteFrom, " "), created.TablePrefix, created.TemplateFingerprint,
		created.MailDisabled, nullTime(utc(created.StagingRefreshedAt)), nullString(created.StagingSnapshotID),
		nullTime(utc(created.StagingPushedAt)), created.CreatedAt, created.UpdatedAt)
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

// Lock returns the site with the given ID, or ErrNotFound, and inside a
// transaction on PostgreSQL locks its row (SELECT ... FOR UPDATE) until the
// transaction ends. Creating a staging site and deleting its production
// site both lock the production site first, so neither decides on what it
// read before the other committed: the second waits, then reads the
// first's change (the deleting status, or the new staging site). SQLite
// needs no row lock, because the store's transactions take the database
// write lock when they begin, so they already run one at a time.
func (r Sites) Lock(ctx context.Context, siteID string) (Site, error) {
	query := "SELECT " + siteColumns + " FROM sites WHERE id = ?"
	if r.q.Dialect() == Postgres {
		query += " FOR UPDATE"
	}
	s, err := scanSite(r.q.QueryRowContext(ctx, query, siteID))
	if err != nil {
		return Site{}, fmt.Errorf("lock site %s: %w", siteID, err)
	}
	return s, nil
}

// GetBySlug returns the site with the given slug, or ErrNotFound. The SFTP
// gateway names sites by slug (design doc §5.8).
func (r Sites) GetBySlug(ctx context.Context, slug string) (Site, error) {
	s, err := scanSite(r.q.QueryRowContext(ctx, "SELECT "+siteColumns+" FROM sites WHERE slug = ?", slug))
	if err != nil {
		return Site{}, fmt.Errorf("get site by slug: %w", err)
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

// StagingIDs returns the IDs of all staging sites. The alert engine uses it
// to tell staging series from production ones with one query per
// evaluation instead of one per site.
func (r Sites) StagingIDs(ctx context.Context) (map[string]bool, error) {
	rows, err := r.q.QueryContext(ctx, "SELECT id FROM sites WHERE parent_site_id IS NOT NULL")
	if err != nil {
		return nil, fmt.Errorf("staging site IDs: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]bool{}
	for rows.Next() {
		var siteID string
		if err := rows.Scan(&siteID); err != nil {
			return nil, fmt.Errorf("staging site IDs: %w", err)
		}
		out[siteID] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("staging site IDs: %w", err)
	}
	return out, nil
}

// SiteFilter selects sites for List. Zero fields don't filter.
type SiteFilter struct {
	ClientGroupID string
	Status        SiteStatus
	// ParentSiteID selects the staging sites of one production site.
	ParentSiteID string
	// Environment selects production sites (no parent) or staging sites
	// (with one).
	Environment SiteEnvironment
	// Before is a cursor: only sites with a smaller ID (older ones) are
	// returned. Pass the previous page's NextCursor.
	Before string
	// Limit is the page size: default 50, at most 200.
	Limit int
}

// SiteEnvironment tells production sites from staging sites in a filter.
type SiteEnvironment string

// Site environments.
const (
	EnvProduction SiteEnvironment = "production"
	EnvStaging    SiteEnvironment = "staging"
)

// Valid reports whether e is a known environment.
func (e SiteEnvironment) Valid() bool { return e == EnvProduction || e == EnvStaging }

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
	if f.ParentSiteID != "" {
		where = append(where, "parent_site_id = ?")
		args = append(args, f.ParentSiteID)
	}
	switch f.Environment {
	case EnvProduction:
		where = append(where, "parent_site_id IS NULL")
	case EnvStaging:
		where = append(where, "parent_site_id IS NOT NULL")
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

// Children returns the staging sites cloned from the production site, oldest
// first. Deleting the parent has to wait for them, so this isn't paged.
func (r Sites) Children(ctx context.Context, parentID string) ([]Site, error) {
	rows, err := r.q.QueryContext(ctx, "SELECT "+siteColumns+" FROM sites WHERE parent_site_id = ? ORDER BY id", parentID)
	if err != nil {
		return nil, fmt.Errorf("children of site %s: %w", parentID, err)
	}
	defer func() { _ = rows.Close() }()
	out := []Site{}
	for rows.Next() {
		s, err := scanSite(rows)
		if err != nil {
			return nil, fmt.Errorf("children of site %s: %w", parentID, err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("children of site %s: %w", parentID, err)
	}
	return out, nil
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
	// MailDisabled is the outbound mail switch of a staging site; it
	// travels with the settings so a failed update puts it back.
	MailDisabled bool `json:"mail_disabled"`
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
		MailDisabled:        s.MailDisabled,
	}
}

// WithSettings returns a copy of the site with the given settings.
func (s Site) WithSettings(set SiteSettings) Site {
	s.PHPVersion, s.CPULimit = set.PHPVersion, set.CPULimit
	s.MemoryReservationMB, s.MemoryLimitMB = set.MemoryReservationMB, set.MemoryLimitMB
	s.PreviewProtection, s.PreviewUsername = set.PreviewProtection, set.PreviewUsername
	s.MailDisabled = set.MailDisabled
	return s
}

// SetSettings saves the site's settings. It reports whether the site
// exists; a missing site is not an error.
func (r Sites) SetSettings(ctx context.Context, siteID string, s SiteSettings) (bool, error) {
	return r.exec(ctx, "set settings of site "+siteID,
		"UPDATE sites SET php_version = ?, cpu_limit = ?, memory_reservation_mb = ?, memory_limit_mb = ?, "+
			"preview_protection = ?, preview_username = ?, mail_disabled = ?, updated_at = ? WHERE id = ?",
		s.PHPVersion, s.CPULimit, s.MemoryReservationMB, s.MemoryLimitMB,
		s.PreviewProtection, s.PreviewUsername, s.MailDisabled, now(), siteID)
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

// SetMaintenance records whether the site is in maintenance mode. If from
// is given, only a site whose flag is currently from changes. It reports
// whether the site changed; a missing site is not an error.
func (r Sites) SetMaintenance(ctx context.Context, siteID string, enabled bool, from ...bool) (bool, error) {
	query := "UPDATE sites SET maintenance = ?, updated_at = ? WHERE id = ?"
	args := []any{enabled, now(), siteID}
	if len(from) > 0 {
		query += " AND maintenance = ?"
		args = append(args, from[0])
	}
	return r.exec(ctx, "set maintenance of site "+siteID, query, args...)
}

// SetWPCoreVersion records the site's WordPress core version, which selects
// its images together with its PHP version. If from is given, only a site
// whose version is currently from changes. It reports whether the site
// changed; a missing site is not an error.
func (r Sites) SetWPCoreVersion(ctx context.Context, siteID, version string, from ...string) (bool, error) {
	if version == "" {
		return false, fmt.Errorf("set WordPress core version of site %s: version is empty", siteID)
	}
	query := "UPDATE sites SET wp_core_version = ?, updated_at = ? WHERE id = ?"
	args := []any{version, now(), siteID}
	if len(from) > 0 {
		query += " AND wp_core_version = ?"
		args = append(args, from[0])
	}
	return r.exec(ctx, "set WordPress core version of site "+siteID, query, args...)
}

// SetStagingRefreshed records that a staging site was cloned or refreshed
// from its production site's snapshot at the given time (§5.11). It
// reports whether the site changed; a missing site is not an error.
func (r Sites) SetStagingRefreshed(ctx context.Context, siteID, snapshotID string, at time.Time) (bool, error) {
	if snapshotID == "" || at.IsZero() {
		return false, fmt.Errorf("set staging refresh of site %s: snapshot and time are required", siteID)
	}
	return r.exec(ctx, "set staging refresh of site "+siteID,
		"UPDATE sites SET staging_snapshot_id = ?, staging_refreshed_at = ?, updated_at = ? WHERE id = ?",
		snapshotID, utc(at), now(), siteID)
}

// SetStagingPushed records that a staging site was pushed to its production
// site at the given time (§5.11). It reports whether the site changed; a
// missing site is not an error, since a staging site can be deleted while its
// push runs.
func (r Sites) SetStagingPushed(ctx context.Context, siteID string, at time.Time) (bool, error) {
	if at.IsZero() {
		return false, fmt.Errorf("set staging push of site %s: time is required", siteID)
	}
	return r.exec(ctx, "set staging push of site "+siteID,
		"UPDATE sites SET staging_pushed_at = ?, updated_at = ? WHERE id = ?", utc(at), now(), siteID)
}

// SetTemplateFingerprint records the fingerprint of the templates the site
// was just rendered with. It leaves updated_at alone: rendering doesn't
// change the site's settings, and every site is re-rendered at startup. It
// reports whether the fingerprint changed; a missing site is not an error.
func (r Sites) SetTemplateFingerprint(ctx context.Context, siteID, fingerprint string) (bool, error) {
	return r.exec(ctx, "set template fingerprint of site "+siteID,
		"UPDATE sites SET template_fingerprint = ? WHERE id = ? AND template_fingerprint <> ?", fingerprint, siteID, fingerprint)
}

// SiteDomains are a site's custom domains (§5.2): its primary domain, empty
// until go-live, the aliases that redirect to it, and the host names whose
// URLs are still to be rewritten to it (Site.URLRewriteFrom). Jobs record
// them in their payloads to put them back.
type SiteDomains struct {
	Primary        string   `json:"primary"`
	Aliases        []string `json:"aliases"`
	URLRewriteFrom []string `json:"url_rewrite_from"`
}

// SetDomains replaces the site's custom domains. Domains are unique across
// sites: a primary domain or alias another site has already, as the same
// kind, is ErrConflict (DomainOwner also checks across kinds). It reports
// whether the site exists; a missing site is not an error.
func (r Sites) SetDomains(ctx context.Context, siteID string, d SiteDomains) (bool, error) {
	what := "set domains of site " + siteID
	ok, err := r.exec(ctx, what, "UPDATE sites SET primary_domain = ?, url_rewrite_from = ?, updated_at = ? WHERE id = ?",
		nullString(d.Primary), strings.Join(d.URLRewriteFrom, " "), now(), siteID)
	if err != nil || !ok {
		return ok, err
	}
	if _, err := r.q.ExecContext(ctx, "DELETE FROM site_aliases WHERE site_id = ?", siteID); err != nil {
		return false, fmt.Errorf("%s: %w", what, err)
	}
	created := now()
	for _, alias := range d.Aliases {
		if _, err := r.q.ExecContext(ctx, "INSERT INTO site_aliases (domain, site_id, created_at) VALUES (?, ?, ?)",
			alias, siteID, created); err != nil {
			return false, fmt.Errorf("%s: alias %s: %w", what, alias, err)
		}
	}
	return true, nil
}

// Domains returns the site's custom domains.
func (r Sites) Domains(ctx context.Context, siteID string) (SiteDomains, error) {
	site, err := r.Get(ctx, siteID)
	if err != nil {
		return SiteDomains{}, err
	}
	aliases, err := r.Aliases(ctx, siteID)
	if err != nil {
		return SiteDomains{}, err
	}
	return SiteDomains{Primary: site.PrimaryDomain, Aliases: aliases, URLRewriteFrom: site.URLRewriteFrom}, nil
}

// DomainOwner returns the ID of the site that uses the domain as its
// preview domain, primary domain, or an alias, or "" if none does.
func (r Sites) DomainOwner(ctx context.Context, domain string) (string, error) {
	var siteID string
	err := r.q.QueryRowContext(ctx,
		"SELECT id FROM sites WHERE preview_domain = ? OR primary_domain = ? "+
			"UNION SELECT site_id FROM site_aliases WHERE domain = ?", domain, domain, domain).Scan(&siteID)
	if errors.Is(err, ErrNotFound) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("owner of domain %s: %w", domain, err)
	}
	return siteID, nil
}

// ClearURLRewrite forgets the host names whose URLs were to be rewritten,
// once they have been, if they are still from. It reports whether they
// were.
func (r Sites) ClearURLRewrite(ctx context.Context, siteID string, from []string) (bool, error) {
	return r.exec(ctx, "clear URL rewrite of site "+siteID,
		"UPDATE sites SET url_rewrite_from = '', updated_at = ? WHERE id = ? AND url_rewrite_from = ?",
		now(), siteID, strings.Join(from, " "))
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

// MemoryReservations returns the memory reserved, in MiB, by the sites whose
// status is not one of except, and how many sites that is. The sum is done
// in the database so callers need not page through every site.
func (r Sites) MemoryReservations(ctx context.Context, except ...SiteStatus) (totalMB int64, count int, err error) {
	query, args := "SELECT COALESCE(SUM(memory_reservation_mb), 0), COUNT(*) FROM sites", []any{}
	if len(except) > 0 {
		query += " WHERE status NOT IN (" + placeholders(len(except)) + ")"
		for _, s := range except {
			args = append(args, string(s))
		}
	}
	if err := r.q.QueryRowContext(ctx, query, args...).Scan(&totalMB, &count); err != nil {
		return 0, 0, fmt.Errorf("sum memory reservations: %w", err)
	}
	return totalMB, count, nil
}

// SetStatus sets the site's status and status message. If from is given,
// only a site whose current status is one of them changes, so a job can't
// overwrite a status another request has set since. It reports whether the
// site changed; a missing site is not an error.
//
// Setting updating also records the status it replaces, if that is running
// or stopped, for ResumeStatus; a site already updating keeps the one it
// has.
func (r Sites) SetStatus(ctx context.Context, siteID string, status SiteStatus, message string, from ...SiteStatus) (bool, error) {
	if !status.Valid() {
		return false, fmt.Errorf("set status of site %s: invalid status %q", siteID, status)
	}
	query := "UPDATE sites SET status = ?, status_message = ?, updated_at = ?"
	args := []any{string(status), message, now()}
	if status == SiteUpdating {
		// SET expressions see the row as it was, so status here is the
		// status being replaced.
		query += ", resume_status = CASE WHEN status IN (?, ?) THEN status WHEN status = ? THEN resume_status ELSE '' END"
		args = append(args, string(SiteRunning), string(SiteStopped), string(SiteUpdating))
	}
	query += " WHERE id = ?"
	args = append(args, siteID)
	if len(from) > 0 {
		query += " AND status IN (" + placeholders(len(from)) + ")"
		for _, s := range from {
			args = append(args, string(s))
		}
	}
	return r.exec(ctx, "set status of site "+siteID, query, args...)
}

// ResumeStatus returns the status the site had before it last went to
// updating: running or stopped, or empty if it was neither. Recovering
// the state database on a new host (#96) uses it for sites that were being
// updated, whose status the update would otherwise have read back from
// their containers. It returns ErrNotFound if the site doesn't exist.
func (r Sites) ResumeStatus(ctx context.Context, siteID string) (SiteStatus, error) {
	var status string
	err := r.q.QueryRowContext(ctx, "SELECT resume_status FROM sites WHERE id = ?", siteID).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("resume status of site %s: %w", siteID, ErrNotFound)
	}
	if err != nil {
		return "", fmt.Errorf("resume status of site %s: %w", siteID, err)
	}
	return SiteStatus(status), nil
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
