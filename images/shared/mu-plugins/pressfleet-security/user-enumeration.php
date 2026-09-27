<?php
/**
 * Security set module: block user enumeration (design doc §6.1).
 *
 * Loaded by pressfleet-security.php when PRESSFLEET_BLOCK_USER_ENUMERATION is
 * set. User names are half of a login; WordPress gives them away in several
 * places to anyone who asks. Logged-in users keep full access (the block
 * editor lists authors through the REST API).
 */

defined( 'ABSPATH' ) || exit;

// ?author=N redirects to /author/<user_nicename>/, and the nicename is
// derived from the login. Anonymous requests get a 404 instead, before
// redirect_canonical (priority 10) can redirect.
add_action(
	'template_redirect',
	static function () {
		// phpcs:ignore WordPress.Security.NonceVerification.Recommended -- read-only check of the query string
		if ( ! isset( $_GET['author'] ) || is_user_logged_in() ) {
			return;
		}
		global $wp_query;
		$wp_query->set_404();
		status_header( 404 );
		nocache_headers();
	},
	1
);

// /wp/v2/users lists every author with their slug. REST routes match
// case-insensitively, so the check does too; it also covers
// ?rest_route=/wp/v2/users and embedded author links.
add_filter(
	'rest_pre_dispatch',
	static function ( $result, $server, $request ) {
		if ( null !== $result || is_user_logged_in() ) {
			return $result;
		}
		if ( preg_match( '#^/wp/v2/users(?:/|$)#i', $request->get_route() ) ) {
			return new WP_Error(
				'rest_user_cannot_view',
				__( 'Sorry, you are not allowed to list users.' ),
				array( 'status' => rest_authorization_required_code() )
			);
		}
		return $result;
	},
	10,
	3
);

// oEmbed responses name the post's author and link to their archive.
add_filter(
	'oembed_response_data',
	static function ( $data ) {
		unset( $data['author_name'], $data['author_url'] );
		return $data;
	},
	20
);

// The users sitemap lists every author archive.
add_filter(
	'wp_sitemaps_add_provider',
	static function ( $provider, $name ) {
		return 'users' === $name ? false : $provider;
	},
	10,
	2
);

// Login errors say whether the user name or the password was wrong. Replace
// them with one message, after every authentication handler (priority 20)
// and the spam check (99) have run.
add_filter(
	'authenticate',
	static function ( $user, $username, $password ) {
		if ( ! is_wp_error( $user ) ) {
			return $user;
		}
		$codes = $user->get_error_codes();
		if ( ! array_intersect( $codes, array( 'invalid_username', 'invalid_email', 'incorrect_password' ) ) ) {
			return $user;
		}
		if ( ! in_array( 'incorrect_password', $codes, true ) && is_string( $password ) && '' !== $password ) {
			// An unknown user fails without checking a password hash, which
			// is measurably faster; spend the same effort.
			wp_hash_password( $password );
		}
		return new WP_Error(
			'pressfleet_invalid_credentials',
			__( '<strong>Error:</strong> The username, email address, or password is incorrect.' )
		);
	},
	100,
	3
);

add_filter(
	'shake_error_codes',
	static function ( $codes ) {
		$codes[] = 'pressfleet_invalid_credentials';
		return $codes;
	}
);

// The lost password form says when no account matches. Answer as if one
// did, the same way wp-login.php does after sending the email.
add_action(
	'lostpassword_post',
	static function ( $errors, $user_data ) {
		if ( $user_data || ! did_action( 'login_init' ) ) {
			return;
		}
		if ( array_diff( $errors->get_error_codes(), array( 'invalid_email' ) ) ) {
			return; // e.g. an empty form: nothing to hide
		}
		// phpcs:ignore WordPress.Security.NonceVerification.Recommended, WordPress.Security.ValidatedSanitizedInput -- wp-login.php does the same; wp_safe_redirect validates it
		$redirect_to = ! empty( $_REQUEST['redirect_to'] ) ? wp_unslash( $_REQUEST['redirect_to'] ) : 'wp-login.php?checkemail=confirm';
		wp_safe_redirect( $redirect_to );
		exit;
	},
	100,
	2
);
