#!/bin/sh
# site-php entrypoint. Runs as the site UID, never root.
set -eu

wp_content=/var/www/wordpress/wp-content

# Seed an empty wp-content volume with the release's bundled content (default
# themes etc.) so a new site can be installed. A volume with any content, such
# as an existing or restored site, is never touched.
if [ "${PRESSFLEET_SEED_WP_CONTENT:-1}" = "1" ] && [ -w "$wp_content" ] \
	&& [ -z "$(ls -A "$wp_content")" ]; then
	echo "pressfleet: seeding empty wp-content from WordPress $(sed -n "s/^\$wp_version = '\(.*\)';/\1/p" /var/www/wordpress/wp-includes/version.php)" >&2
	cp -R /opt/pressfleet/wp-content-skel/. "$wp_content/"
fi

if [ "$(id -u)" = "0" ]; then
	echo "pressfleet: warning: running as root; site containers should run as the site UID" >&2
fi

if [ "${1#-}" != "$1" ]; then
	set -- php-fpm "$@"
fi
exec "$@"
