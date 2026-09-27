<?php
/**
 * Plugin Name: PressFleet Platform
 * Description: Platform integration for sites managed by PressFleet. WordPress core is managed by the platform, so core updates are disabled here.
 * Author: PressFleet
 * License: MIT
 *
 * Shipped in the site images at /opt/pressfleet/mu-plugins (outside
 * wp-content) and loaded through WPMU_PLUGIN_DIR, so it is versioned with the
 * image and cannot be removed from within WordPress. See images/README.md.
 */

defined( 'ABSPATH' ) || exit;

/** Marks a request as running under PressFleet; also used by the image smoke test. */
define( 'PRESSFLEET_PLATFORM', true );

/*
 * Core is baked into the image and read-only; the platform rolls out core
 * releases by replacing the image (design doc §5.5). WP_AUTO_UPDATE_CORE=false
 * comes from the generated wp-config.php; this plugin removes the rest of the
 * core update machinery and UI. Plugin and theme updates are untouched.
 */

// Nobody can update core from inside WordPress. This hides the core section
// of Dashboard > Updates, the "update now" links, and the footer version nag.
add_filter(
	'map_meta_cap',
	static function ( $caps, $cap ) {
		if ( 'update_core' === $cap ) {
			return array( 'do_not_allow' );
		}
		return $caps;
	},
	10,
	2
);

// Report core as current without calling api.wordpress.org, so no update
// notice is ever computed.
add_filter(
	'pre_site_transient_update_core',
	static function () {
		global $wp_version;
		return (object) array(
			'updates'         => array(),
			'version_checked' => $wp_version,
			'last_checked'    => time(),
		);
	}
);

add_filter( 'auto_update_core', '__return_false' );
add_filter( 'send_core_update_notification_email', '__return_false' );

// The wp_version_check cron event stays scheduled (core would reschedule it
// on the next request) but doesn't check for core releases.
remove_action( 'admin_init', '_maybe_update_core' );
remove_action( 'wp_version_check', 'wp_version_check' );

// WordPress starts background updates, plugin and theme auto-updates
// included, at the end of that check, so start them from the event here.
// Core stays out of them (auto_update_core above).
add_action(
	'wp_version_check',
	static function () {
		if ( wp_doing_cron() && ! doing_action( 'wp_maybe_auto_update' ) ) {
			do_action( 'wp_maybe_auto_update' );
		}
	}
);

add_action(
	'admin_init',
	static function () {
		remove_action( 'admin_notices', 'update_nag', 3 );
		remove_action( 'network_admin_notices', 'update_nag', 3 );
		remove_action( 'upgrader_process_complete', 'wp_version_check', 10 );
	}
);

// Site Health's background-update test would flag the intentional setup as a
// problem (read-only core, updates disabled).
add_filter(
	'site_status_tests',
	static function ( $tests ) {
		unset( $tests['async']['background_updates'] );
		unset( $tests['direct']['background_updates'] );
		return $tests;
	}
);
