-- Platform image update rollout (#58, design doc §5.5). Keep in sync with
-- migrations/postgres/0022_site_image_pins.sql.
--
-- The build of each image a site runs: image is the tag its PHP and
-- WordPress versions select (site-php:php8.4-wp7.1.2, site-nginx:wp7.1.2),
-- and pinned the reference of the exact build the site's containers are
-- rendered with (repository@sha256:..., or a local image ID). Rebuilds move
-- a tag to a new build; the image rollout moves each site's pin to it, and
-- a failed update moves the pin back. Pins of tags the site no longer uses
-- are kept, so moving back to one (a reverted core update) finds the build
-- the site ran before.
CREATE TABLE site_image_pins (
    site_id    TEXT NOT NULL REFERENCES sites (id) ON DELETE CASCADE,
    image      TEXT NOT NULL,
    pinned     TEXT NOT NULL,
    updated_at TIMESTAMP NOT NULL,
    PRIMARY KEY (site_id, image)
);
