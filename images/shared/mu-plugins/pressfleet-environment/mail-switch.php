<?php
/**
 * Outbound email switch (PRESSFLEET_DISABLE_MAIL).
 *
 * Loaded by pressfleet.php when the constant is true. A staging site is a
 * copy of production, users included, so the mail it sends would reach real
 * customers. This module stops every message that goes through wp_mail().
 * Plugins that talk to a mail API directly aren't covered.
 *
 * It lives apart from pressfleet-smtp.php on purpose: that plugin returns
 * early when no relay is configured, and the switch must work then too. The
 * SMTP settings can't outrank it, because pre_wp_mail short-circuits
 * wp_mail() before PHPMailer is built, so phpmailer_init never fires.
 */

defined( 'ABSPATH' ) || exit;

add_filter(
	'pre_wp_mail',
	static function ( $return, $atts ) {
		// Another filter at an earlier priority already decided.
		if ( null !== $return ) {
			return $return;
		}

		$to = isset( $atts['to'] ) ? $atts['to'] : array();
		if ( ! is_array( $to ) ) {
			$to = explode( ',', (string) $to );
		}
		$recipients = count( array_filter( array_map( 'trim', $to ), 'strlen' ) );

		// Only the recipient count: addresses and subjects are personal data,
		// and logs travel. PHP's error log is the container's stderr, where
		// operators look first.
		error_log( sprintf( 'pressfleet: outbound email is disabled (PRESSFLEET_DISABLE_MAIL); suppressed a message to %d recipient(s)', $recipients ) ); // phpcs:ignore WordPress.PHP.DevelopmentFunctions.error_log_error_log

		// True makes wp_mail() report success, so callers carry on as if the
		// message was sent.
		return true;
	},
	PHP_INT_MIN,
	2
);
