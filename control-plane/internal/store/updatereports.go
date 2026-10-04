package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// Kinds of pending update.
const (
	UpdateKindPlugin = "plugin"
	UpdateKindTheme  = "theme"
)

// PendingUpdate is a plugin or theme with an update available (#125).
type PendingUpdate struct {
	// Kind is UpdateKindPlugin or UpdateKindTheme.
	Kind string `json:"kind"`
	// Name is the plugin's or theme's slug, Title its display name.
	Name  string `json:"name"`
	Title string `json:"title,omitempty"`
	// Version is the installed version, UpdateVersion the available one.
	Version       string `json:"version"`
	UpdateVersion string `json:"update_version"`
}

// UpdateCounts is how many plugins and themes of a site had an update
// available when it was last checked.
type UpdateCounts struct {
	Plugins   int
	Themes    int
	CheckedAt time.Time
}

// Total is the number of pending updates.
func (c UpdateCounts) Total() int { return c.Plugins + c.Themes }

// UpdateReport is the latest check of a site's plugins and themes for
// updates (package pluginupdates).
type UpdateReport struct {
	SiteID string
	UpdateCounts
	// Updates lists the pending updates, plugins first, each kind sorted
	// by name.
	Updates []PendingUpdate
}

// UpdateReports reads and writes the latest pending update report of each
// site.
type UpdateReports struct {
	q Querier
}

// NewUpdateReports returns an UpdateReports that runs queries on q.
func NewUpdateReports(q Querier) UpdateReports {
	return UpdateReports{q: q}
}

// Put replaces the site's report. The counts are taken from r.Updates. A
// missing site is ErrReference.
func (r UpdateReports) Put(ctx context.Context, rep UpdateReport) error {
	updates := rep.Updates
	if updates == nil {
		updates = []PendingUpdate{}
	}
	var plugins, themes int
	for _, u := range updates {
		switch u.Kind {
		case UpdateKindPlugin:
			plugins++
		case UpdateKindTheme:
			themes++
		default:
			return fmt.Errorf("update report of site %s: unknown update kind %q", rep.SiteID, u.Kind)
		}
	}
	data, err := json.Marshal(updates)
	if err != nil {
		return fmt.Errorf("update report of site %s: %w", rep.SiteID, err)
	}
	checked := rep.CheckedAt
	if checked.IsZero() {
		checked = now()
	}
	_, err = r.q.ExecContext(ctx, "INSERT INTO site_update_reports (site_id, checked_at, plugin_updates, theme_updates, updates) VALUES (?, ?, ?, ?, ?) "+
		"ON CONFLICT (site_id) DO UPDATE SET checked_at = excluded.checked_at, plugin_updates = excluded.plugin_updates, "+
		"theme_updates = excluded.theme_updates, updates = excluded.updates",
		rep.SiteID, utc(checked), plugins, themes, string(data))
	if err != nil {
		return fmt.Errorf("put update report of site %s: %w", rep.SiteID, err)
	}
	return nil
}

// Get returns the site's report, or ErrNotFound if the site has none (it
// hasn't been checked yet, or doesn't exist).
func (r UpdateReports) Get(ctx context.Context, siteID string) (UpdateReport, error) {
	rep := UpdateReport{SiteID: siteID}
	var data string
	err := r.q.QueryRowContext(ctx, "SELECT checked_at, plugin_updates, theme_updates, updates FROM site_update_reports WHERE site_id = ?", siteID).
		Scan(&rep.CheckedAt, &rep.Plugins, &rep.Themes, &data)
	if err != nil {
		return UpdateReport{}, fmt.Errorf("update report of site %s: %w", siteID, err)
	}
	rep.CheckedAt = utc(rep.CheckedAt)
	if err := json.Unmarshal([]byte(data), &rep.Updates); err != nil {
		return UpdateReport{}, fmt.Errorf("update report of site %s: %w", siteID, err)
	}
	return rep, nil
}

// Counts returns every checked site's counts, keyed by site ID.
func (r UpdateReports) Counts(ctx context.Context) (map[string]UpdateCounts, error) {
	rows, err := r.q.QueryContext(ctx, "SELECT site_id, checked_at, plugin_updates, theme_updates FROM site_update_reports")
	if err != nil {
		return nil, fmt.Errorf("list update reports: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]UpdateCounts{}
	for rows.Next() {
		var (
			siteID string
			c      UpdateCounts
		)
		if err := rows.Scan(&siteID, &c.CheckedAt, &c.Plugins, &c.Themes); err != nil {
			return nil, fmt.Errorf("list update reports: %w", err)
		}
		c.CheckedAt = utc(c.CheckedAt)
		out[siteID] = c
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list update reports: %w", err)
	}
	return out, nil
}
