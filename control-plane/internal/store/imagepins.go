package store

import (
	"context"
	"fmt"
)

// ImagePins records the build of each image a site runs (#58, design doc
// §5.5): for an image tag the site's versions select, the reference of the
// exact build its containers are rendered with (provisioner.SiteConfig
// PinnedImages). A site keeps the pins of tags it no longer uses, so moving
// back to one finds the build it ran before.
type ImagePins struct {
	q Querier
}

// NewImagePins returns an ImagePins that runs queries on q.
func NewImagePins(q Querier) ImagePins {
	return ImagePins{q: q}
}

// Site returns the site's pins, keyed by image tag. It doesn't check that
// the site exists.
func (r ImagePins) Site(ctx context.Context, siteID string) (map[string]string, error) {
	all, err := r.list(ctx, "SELECT site_id, image, pinned FROM site_image_pins WHERE site_id = ?", siteID)
	if err != nil {
		return nil, fmt.Errorf("image pins of site %s: %w", siteID, err)
	}
	if all[siteID] == nil {
		return map[string]string{}, nil
	}
	return all[siteID], nil
}

// All returns every site's pins, keyed by site ID and then image tag.
func (r ImagePins) All(ctx context.Context) (map[string]map[string]string, error) {
	all, err := r.list(ctx, "SELECT site_id, image, pinned FROM site_image_pins")
	if err != nil {
		return nil, fmt.Errorf("list image pins: %w", err)
	}
	return all, nil
}

func (r ImagePins) list(ctx context.Context, query string, args ...any) (map[string]map[string]string, error) {
	rows, err := r.q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string]map[string]string{}
	for rows.Next() {
		var siteID, image, pinned string
		if err := rows.Scan(&siteID, &image, &pinned); err != nil {
			return nil, err
		}
		if out[siteID] == nil {
			out[siteID] = map[string]string{}
		}
		out[siteID][image] = pinned
	}
	return out, rows.Err()
}

// Set pins the site's image tag to a build. A missing site is
// ErrReference.
func (r ImagePins) Set(ctx context.Context, siteID, image, pinned string) error {
	_, err := r.q.ExecContext(ctx, "INSERT INTO site_image_pins (site_id, image, pinned, updated_at) VALUES (?, ?, ?, ?) "+
		"ON CONFLICT (site_id, image) DO UPDATE SET pinned = excluded.pinned, updated_at = excluded.updated_at",
		siteID, image, pinned, now())
	if err != nil {
		return fmt.Errorf("pin image %s of site %s: %w", image, siteID, err)
	}
	return nil
}
