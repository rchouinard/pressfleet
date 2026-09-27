<?php
/**
 * Plugin Name: PressFleet SMTP
 * Description: Sends site email through the platform's SMTP relay, configured by PressFleet in wp-config.php.
 * Author: PressFleet
 * License: MIT
 *
 * The site images have no local mail transfer agent, so PHP's mail() can't
 * deliver anything; the platform's SMTP relay (design doc §5.6) is the way
 * out. The control plane writes the relay settings into the generated,
 * read-only wp-config.php as PRESSFLEET_SMTP_* constants (package wpconfig).
 * Without PRESSFLEET_SMTP_HOST this plugin does nothing.
 *
 * It hooks in early, so a site that installs its own mail plugin (a
 * transactional provider's, say) still has the last word.
 */

defined( 'ABSPATH' ) || exit;

if ( ! defined( 'PRESSFLEET_SMTP_HOST' ) || '' === PRESSFLEET_SMTP_HOST ) {
	return;
}

/**
 * Returns a PRESSFLEET_SMTP_* constant, or $fallback if it is not defined.
 *
 * @param string $name     Constant name without the PRESSFLEET_SMTP_ prefix.
 * @param mixed  $fallback Value when the constant is missing.
 * @return mixed
 */
function pressfleet_smtp_setting( $name, $fallback = '' ) {
	$constant = 'PRESSFLEET_SMTP_' . $name;
	return defined( $constant ) ? constant( $constant ) : $fallback;
}

add_action(
	'phpmailer_init',
	static function ( $phpmailer ) {
		$phpmailer->isSMTP();
		$phpmailer->Host = PRESSFLEET_SMTP_HOST;
		$phpmailer->Port = (int) pressfleet_smtp_setting( 'PORT', 587 );

		// "tls" is STARTTLS and "ssl" is TLS from the start, as PHPMailer
		// names them. With neither, PHPMailer would still upgrade to TLS
		// whenever the server offers it and then fail on a certificate the
		// operator deliberately doesn't verify (a relay on the Docker
		// network, say), so plaintext means plaintext.
		$encryption              = (string) pressfleet_smtp_setting( 'ENCRYPTION' );
		$phpmailer->SMTPSecure  = $encryption;
		$phpmailer->SMTPAutoTLS = '' !== $encryption;

		$username = (string) pressfleet_smtp_setting( 'USERNAME' );
		if ( '' !== $username ) {
			$phpmailer->SMTPAuth = true;
			$phpmailer->Username = $username;
			$phpmailer->Password = (string) pressfleet_smtp_setting( 'PASSWORD' );
		} else {
			$phpmailer->SMTPAuth = false;
		}

		// PHPMailer waits 300 s by default; a request sending mail shouldn't
		// hang on an unreachable relay for that long.
		$phpmailer->Timeout = 15;

		// The envelope sender (Return-Path) is always the platform's address,
		// so bounces go to it and SPF checks the relay's domain.
		$from = (string) pressfleet_smtp_setting( 'FROM_EMAIL' );
		if ( '' !== $from ) {
			$phpmailer->Sender = $from;
		}
	},
	1
);

// Replace WordPress's default sender, wordpress@<site domain>, which relays
// usually refuse, but keep a From address that a plugin or the site chose.
add_filter(
	'wp_mail_from',
	static function ( $from_email ) {
		$platform = (string) pressfleet_smtp_setting( 'FROM_EMAIL' );
		if ( '' !== $platform && 0 === strpos( strtolower( (string) $from_email ), 'wordpress@' ) ) {
			return $platform;
		}
		return $from_email;
	},
	1
);

add_filter(
	'wp_mail_from_name',
	static function ( $from_name ) {
		$platform = (string) pressfleet_smtp_setting( 'FROM_NAME' );
		if ( '' !== $platform && 'WordPress' === $from_name ) {
			return $platform;
		}
		return $from_name;
	},
	1
);

// PHP's error log is the container's stderr, where operators look first.
add_action(
	'wp_mail_failed',
	static function ( $error ) {
		error_log( 'pressfleet: sending email through the SMTP relay failed: ' . $error->get_error_message() ); // phpcs:ignore WordPress.PHP.DevelopmentFunctions.error_log_error_log
	}
);
