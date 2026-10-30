<?php
/**
 * Admin bar environment badge.
 *
 * Loaded by pressfleet.php. Names the environment a site runs in, and says
 * whether its outbound email is off, so nobody mistakes a staging site for
 * production. Nothing is shown in production. WordPress only builds the
 * admin bar for users who see it, so no capability check is needed.
 */

defined( 'ABSPATH' ) || exit;

add_action(
	'admin_bar_menu',
	static function ( $wp_admin_bar ) {
		$environment = wp_get_environment_type();
		if ( 'production' === $environment ) {
			return;
		}

		$mail_off = defined( 'PRESSFLEET_DISABLE_MAIL' ) && PRESSFLEET_DISABLE_MAIL;
		$label    = strtoupper( $environment ) . ( $mail_off ? ' | email off' : '' );

		$wp_admin_bar->add_node(
			array(
				'id'    => 'pressfleet-environment',
				// The node title is printed as HTML.
				'title' => '<span style="background:#dba617;color:#1d2327;padding:2px 8px;border-radius:3px;font-weight:600">' . esc_html( $label ) . '</span>',
				'meta'  => array(
					'title' => $mail_off
						? sprintf( 'This is a %s site. Outbound email is turned off.', $environment )
						: sprintf( 'This is a %s site.', $environment ),
				),
			)
		);
	},
	1000
);
