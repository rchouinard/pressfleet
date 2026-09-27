<?php
/**
 * Security set module: server-side password policy (design doc §6.1).
 *
 * Loaded by pressfleet-security.php when PRESSFLEET_PASSWORD_POLICY_MIN_LENGTH
 * is set. WordPress's strength meter only runs in the browser and can be
 * overridden with "Confirm use of weak password"; this checks every password
 * on the server when it is set: a minimum length, and a list of commonly used
 * passwords. Existing passwords keep working until they are changed.
 *
 * common-passwords.txt holds the NCSC's 100,000 most used passwords (from Have
 * I Been Pwned, via SecLists, MIT licensed) of at least 8 characters, the
 * smallest minimum the control plane allows, lowercased and sorted.
 */

defined( 'ABSPATH' ) || exit;

/**
 * Returns why a password breaks the policy, or '' if it is acceptable.
 *
 * @param string $password The new password, as entered.
 * @return string
 */
function pressfleet_password_policy_violation( $password ) {
	$min = max( 1, (int) PRESSFLEET_PASSWORD_POLICY_MIN_LENGTH );
	if ( mb_strlen( $password, 'UTF-8' ) < $min ) {
		/* translators: %d: minimum number of characters */
		return sprintf( __( 'Passwords must be at least %d characters long.' ), $min );
	}
	static $list = null;
	if ( null === $list ) {
		// One password per line; a leading newline lets every entry be
		// matched as "\n<password>\n". About 450 KB, read only when a
		// password is set.
		$list = "\n" . (string) file_get_contents( __DIR__ . '/common-passwords.txt' ); // phpcs:ignore WordPress.WP.AlternativeFunctions.file_get_contents_file_get_contents -- local file
	}
	if ( false === strpbrk( $password, "\r\n" ) && false !== strpos( $list, "\n" . strtolower( $password ) . "\n" ) ) {
		return __( 'This password is one of the most commonly used passwords. Please choose another one.' );
	}
	return '';
}

// Profile, user edit, and Users > Add New (edit_user() sets user_pass only
// when a new password was entered).
add_action(
	'user_profile_update_errors',
	static function ( $errors, $update, $user ) {
		if ( empty( $user->user_pass ) || ! is_string( $user->user_pass ) ) {
			return;
		}
		$violation = pressfleet_password_policy_violation( $user->user_pass );
		if ( '' !== $violation ) {
			$errors->add( 'pressfleet_password_policy', '<strong>' . __( 'Error:' ) . '</strong> ' . esc_html( $violation ), array( 'form-field' => 'pass1' ) );
		}
	},
	10,
	3
);

// The reset form on wp-login.php (?action=rp).
add_action(
	'validate_password_reset',
	static function ( $errors ) {
		// phpcs:ignore WordPress.Security.NonceVerification.Missing -- wp-login.php verified the reset key
		if ( empty( $_POST['pass1'] ) || ! is_string( $_POST['pass1'] ) ) {
			return;
		}
		$violation = pressfleet_password_policy_violation( wp_unslash( $_POST['pass1'] ) ); // phpcs:ignore WordPress.Security.ValidatedSanitizedInput.InputNotSanitized,WordPress.Security.NonceVerification.Missing -- passwords are not sanitized
		if ( '' !== $violation ) {
			$errors->add( 'pressfleet_password_policy', '<strong>' . __( 'Error:' ) . '</strong> ' . esc_html( $violation ) );
		}
	}
);

// The REST API: creating a user or changing a password. This runs after
// permission checks and before the users controller; the controller's own
// rest_pre_insert_user filter can't return an error.
add_filter(
	'rest_request_before_callbacks',
	static function ( $response, $handler, $request ) {
		if ( null !== $response || 'GET' === $request->get_method() || ! preg_match( '#^/wp/v2/users(?:/|$)#i', $request->get_route() ) ) {
			return $response;
		}
		$password = $request['password'];
		if ( ! is_string( $password ) || '' === $password ) {
			return $response;
		}
		$violation = pressfleet_password_policy_violation( $password );
		if ( '' !== $violation ) {
			return new WP_Error( 'rest_weak_password', $violation, array( 'status' => 400 ) );
		}
		return $response;
	},
	10,
	3
);

// WP-CLI (`wp user create`, `wp user update --user_pass`, `wp core install`)
// sets passwords without any form. New users reach wp_insert_user() with the
// plain password; password changes through wp_update_user() fire
// wp_set_password before anything is written. WP_CLI::error() ends the
// command there, so nothing is saved.
if ( defined( 'WP_CLI' ) && WP_CLI ) {
	add_filter(
		'wp_pre_insert_user_data',
		static function ( $data, $update, $user_id, $userdata ) {
			if ( ! $update && ! empty( $userdata['user_pass'] ) && is_string( $userdata['user_pass'] ) ) {
				$violation = pressfleet_password_policy_violation( $userdata['user_pass'] );
				if ( '' !== $violation ) {
					WP_CLI::error( $violation );
				}
			}
			return $data;
		},
		10,
		4
	);
	add_action(
		'wp_set_password',
		static function ( $password ) {
			// wp_update_user() fires this before saving. wp_insert_user()
			// (new users, checked above) and wp_set_password() fire it after
			// saving, where the error comes too late to stop the change but
			// still fails the command.
			if ( is_string( $password ) && '' !== $password ) {
				$violation = pressfleet_password_policy_violation( $password );
				if ( '' !== $violation ) {
					WP_CLI::error( $violation );
				}
			}
		}
	);
}
