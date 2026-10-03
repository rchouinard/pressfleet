<?php
/**
 * Platform module: one-click administrator login from the dashboard
 * (design doc §5.9).
 *
 * Loaded by pressfleet.php when wp-config.php defines PRESSFLEET_SITE_ID and
 * PRESSFLEET_LOGIN_PUBLIC_KEY. It is not a security set toggle: a site
 * without the constants has no way to verify a token and the endpoint does
 * not exist.
 *
 * The control plane signs a short-lived token with the platform's Ed25519
 * key, and the browser POSTs it as the `token` field to
 * wp-login.php?action=pressfleet-login, so it never appears in a URL, access
 * log, Referer, or browser history. Token format (v1):
 *
 *     v1.<payload>.<signature>
 *
 * Both parts are base64url without padding. The payload is JSON
 * {"site", "user", "actor", "jti", "iat", "exp"}: the site ID, the WordPress
 * user's login, the platform user's ID, a random 128-bit ID (hex), and Unix
 * times at most 60 seconds apart. The signature is Ed25519 over the ASCII
 * bytes "v1.<payload>".
 *
 * PRESSFLEET_LOGIN_PUBLIC_KEY is the 32-byte public key as base64url
 * (padding optional), standard base64, or 64 hex characters.
 *
 * Every failure shows the same error; the reason goes to the error log, never
 * the token. The session is marked with `pressfleet_login` (actor and jti),
 * which the two-factor module uses to exempt it from enrollment.
 */

defined( 'ABSPATH' ) || exit;

/** Longest accepted token; real ones are about 400 bytes. */
const PRESSFLEET_LOGIN_MAX_TOKEN_BYTES = 2048;

/** Longest lifetime a token may claim, in seconds. */
const PRESSFLEET_LOGIN_MAX_LIFETIME = 60;

/** How far in the future `iat` may be, to tolerate clock drift between the platform and the site. */
const PRESSFLEET_LOGIN_CLOCK_SKEW = 10;

/** Prefix of the option rows that record used token IDs. */
const PRESSFLEET_LOGIN_JTI_PREFIX = 'pressfleet_login_jti_';

/**
 * Decodes base64url, with or without padding. Strict: any other character
 * fails.
 *
 * @param string $data Encoded data.
 * @return string|false
 */
function pressfleet_login_base64url_decode( $data ) {
	if ( ! is_string( $data ) || '' === $data || ! preg_match( '/^[A-Za-z0-9_-]+={0,2}$/', $data ) ) {
		return false;
	}
	return base64_decode( strtr( $data, '-_', '+/' ), true );
}

/**
 * Returns the configured public key as 32 raw bytes, or false if the
 * constant is malformed.
 *
 * @return string|false
 */
function pressfleet_login_public_key() {
	$configured = trim( (string) PRESSFLEET_LOGIN_PUBLIC_KEY );
	if ( 64 === strlen( $configured ) && ctype_xdigit( $configured ) ) {
		$key = hex2bin( $configured );
	} else {
		$key = pressfleet_login_base64url_decode( $configured );
		if ( false === $key ) {
			$key = base64_decode( $configured, true );
		}
	}
	return ( is_string( $key ) && SODIUM_CRYPTO_SIGN_PUBLICKEYBYTES === strlen( $key ) ) ? $key : false;
}

/**
 * Verifies a login token's version, signature, and claims.
 *
 * @param string $token The token.
 * @param int    $now   Current Unix time.
 * @return array|string The validated claims, or the reason it was refused.
 */
function pressfleet_login_verify( $token, $now ) {
	if ( '' === $token || strlen( $token ) > PRESSFLEET_LOGIN_MAX_TOKEN_BYTES ) {
		return 'missing or oversized token';
	}
	$parts = explode( '.', $token );
	if ( 3 !== count( $parts ) ) {
		return 'malformed token';
	}
	list( $version, $encoded_payload, $encoded_signature ) = $parts;
	if ( 'v1' !== $version ) {
		return 'unsupported token version';
	}

	$signature = pressfleet_login_base64url_decode( $encoded_signature );
	if ( false === $signature || SODIUM_CRYPTO_SIGN_BYTES !== strlen( $signature ) ) {
		return 'malformed signature';
	}
	$key = pressfleet_login_public_key();
	if ( false === $key ) {
		return 'PRESSFLEET_LOGIN_PUBLIC_KEY is not a valid Ed25519 public key';
	}
	if ( ! sodium_crypto_sign_verify_detached( $signature, $version . '.' . $encoded_payload, $key ) ) {
		return 'signature does not verify';
	}

	// Past this point the platform signed the payload; the checks guard
	// against a token meant for another site, an old one, or a bug.
	$json = pressfleet_login_base64url_decode( $encoded_payload );
	$data = false === $json ? null : json_decode( $json, true );
	if ( ! is_array( $data ) ) {
		return 'malformed payload';
	}
	foreach ( array( 'site', 'user', 'actor', 'jti' ) as $field ) {
		if ( ! isset( $data[ $field ] ) || ! is_string( $data[ $field ] ) || '' === $data[ $field ] ) {
			return "payload field $field is missing or not a string";
		}
	}
	foreach ( array( 'iat', 'exp' ) as $field ) {
		if ( ! isset( $data[ $field ] ) || ! is_int( $data[ $field ] ) ) {
			return "payload field $field is missing or not an integer";
		}
	}
	if ( ! preg_match( '/^[0-9a-f]{32}$/', $data['jti'] ) ) {
		return 'jti is not 128 bits of hex';
	}
	if ( ! hash_equals( (string) PRESSFLEET_SITE_ID, $data['site'] ) ) {
		return 'token is for another site';
	}
	if ( $data['exp'] <= $now ) {
		return 'token expired';
	}
	if ( $data['iat'] > $now + PRESSFLEET_LOGIN_CLOCK_SKEW ) {
		return 'token issued in the future';
	}
	if ( $data['exp'] - $data['iat'] > PRESSFLEET_LOGIN_MAX_LIFETIME ) {
		return 'token lifetime too long';
	}
	return $data;
}

/**
 * Records a token ID as used. The insert fails on a duplicate option name
 * (a unique key), so of two concurrent requests with the same token only one
 * succeeds. It goes to the database, not the object cache, which can evict
 * entries. Expired records are pruned first.
 *
 * @param string $jti The token ID.
 * @param int    $exp The token's expiry.
 * @param int    $now Current Unix time.
 * @return bool True if this is the first use.
 */
function pressfleet_login_consume( $jti, $exp, $now ) {
	global $wpdb;

	// A record can go once its token is expired beyond doubt.
	$wpdb->query(
		$wpdb->prepare(
			"DELETE FROM {$wpdb->options} WHERE option_name LIKE %s AND CAST( option_value AS UNSIGNED ) < %d",
			$wpdb->esc_like( PRESSFLEET_LOGIN_JTI_PREFIX ) . '%',
			$now - PRESSFLEET_LOGIN_MAX_LIFETIME
		)
	);

	$suppress = $wpdb->suppress_errors( true );
	$inserted = $wpdb->insert(
		$wpdb->options,
		array(
			'option_name'  => PRESSFLEET_LOGIN_JTI_PREFIX . $jti,
			'option_value' => (string) $exp,
			'autoload'     => 'no',
		),
		array( '%s', '%s', '%s' )
	);
	$wpdb->suppress_errors( $suppress );

	return 1 === $inserted;
}

/**
 * Ends a login request with the one generic error, after logging why.
 *
 * @param string $reason What went wrong (never the token).
 * @param int    $status HTTP status.
 */
function pressfleet_login_refuse( $reason, $status = 403 ) {
	error_log( 'pressfleet-login: refused: ' . $reason );
	status_header( $status );
	if ( 405 === $status ) {
		header( 'Allow: POST' );
	}
	login_header(
		__( 'Log In' ),
		'',
		new WP_Error( 'pressfleet_login_failed', __( 'The login link is not valid. Go back to the dashboard and try again.' ) )
	);
	login_footer();
	exit;
}

// Marks the session wp_set_auth_cookie() creates. The filter has no request
// context, so the login handler passes the marker through this global.
add_filter(
	'attach_session_information',
	static function ( $session ) {
		$login = $GLOBALS['pressfleet_login_session'] ?? null;
		if ( is_array( $login ) ) {
			$session['pressfleet_login'] = $login;
		}
		return $session;
	}
);

add_action(
	'login_form_pressfleet-login',
	static function () {
		// Never cache or store any response of this endpoint.
		nocache_headers();
		header( 'Cache-Control: no-store' );

		$method = isset( $_SERVER['REQUEST_METHOD'] ) ? strtoupper( (string) $_SERVER['REQUEST_METHOD'] ) : '';
		if ( 'POST' !== $method ) {
			pressfleet_login_refuse( 'method ' . preg_replace( '/[^A-Z]/', '', $method ) . ' not allowed', 405 );
		}

		$token = isset( $_POST['token'] ) && is_string( $_POST['token'] ) ? trim( wp_unslash( $_POST['token'] ) ) : '';
		$now   = time();
		$claim = pressfleet_login_verify( $token, $now );
		if ( is_string( $claim ) ) {
			pressfleet_login_refuse( $claim );
		}

		if ( ! pressfleet_login_consume( $claim['jti'], $claim['exp'], $now ) ) {
			pressfleet_login_refuse( 'token ' . $claim['jti'] . ' already used' );
		}

		$user = get_user_by( 'login', $claim['user'] );
		if ( ! $user instanceof WP_User ) {
			pressfleet_login_refuse( 'token ' . $claim['jti'] . ': user does not exist' );
		}
		if ( ! user_can( $user, 'manage_options' ) ) {
			pressfleet_login_refuse( 'token ' . $claim['jti'] . ': user is not an administrator' );
		}

		// wp_login is deliberately not fired: Two Factor intercepts it to ask
		// for a second factor, and the platform login stands in for one.
		$GLOBALS['pressfleet_login_session'] = array(
			'actor' => $claim['actor'],
			'jti'   => $claim['jti'],
		);
		wp_set_auth_cookie( $user->ID, false );
		unset( $GLOBALS['pressfleet_login_session'] );
		wp_set_current_user( $user->ID );

		/**
		 * Fires after a dashboard login sets the session.
		 *
		 * @param WP_User $user  The administrator logged in as.
		 * @param string  $actor The platform user's ID.
		 * @param string  $jti   The token ID.
		 */
		do_action( 'pressfleet_admin_login', $user, $claim['actor'], $claim['jti'] );

		wp_safe_redirect( admin_url() );
		exit;
	}
);
