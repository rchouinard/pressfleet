<?php
/**
 * Security set module: require two-factor authentication for administrators
 * (design doc §6.1).
 *
 * Loaded by pressfleet-security.php when PRESSFLEET_REQUIRE_TWO_FACTOR is set.
 * The control plane installs the WordPress.org Two Factor plugin; this module
 * makes a second factor mandatory for administrators (users who can
 * manage_options), including the one the platform creates with a site:
 *
 * - Two Factor stays active: it counts as active whenever it is installed,
 *   and its Deactivate link is replaced by a note.
 * - An administrator without a second factor is enrolled at their next login.
 *   Logging in lands on their profile, every other admin screen sends them
 *   back there, and a notice says why, until they set one up. The platform's
 *   initial admin password is shown once, to the person creating the site,
 *   so that first login is theirs.
 * - A session started by a one-click login from the dashboard (design doc
 *   §5.9) is exempt from that enrollment redirect and notice: the platform
 *   login stands in for a second factor, and a shared administrator account
 *   isn't bound to one person's device. Password logins are unaffected.
 * - Until then they can't use application passwords, which would sign in to
 *   the REST API without a second factor.
 *
 * Without the Two Factor plugin (deleted, or not installed yet) nothing is
 * enforced and administrators see a warning: locking every administrator out
 * of the site would be worse. The control plane reinstalls the plugin when it
 * next reconciles the site.
 */

defined( 'ABSPATH' ) || exit;

/**
 * Whether the user is an administrator who must still set up a second
 * factor. False while the Two Factor plugin isn't loaded.
 *
 * @param WP_User|false|null $user The user.
 * @return bool
 */
function pressfleet_two_factor_setup_required( $user ) {
	return $user instanceof WP_User && $user->exists()
		&& class_exists( 'Two_Factor_Core' )
		&& user_can( $user, 'manage_options' )
		&& ! Two_Factor_Core::is_user_using_two_factor( $user->ID );
}

/**
 * Whether the current session was started by a dashboard login (the
 * `pressfleet_login` session information set by the login module).
 *
 * @return bool
 */
function pressfleet_is_platform_login_session() {
	if ( ! is_user_logged_in() ) {
		return false;
	}
	$session = WP_Session_Tokens::get_instance( get_current_user_id() )->get( wp_get_session_token() );
	return is_array( $session ) && ! empty( $session['pressfleet_login'] );
}

/**
 * Whether to send the user to enrollment: they must set up a second factor
 * and aren't in a dashboard login session. Application passwords use
 * pressfleet_two_factor_setup_required() instead, so a dashboard login
 * doesn't open a way around the second factor.
 *
 * @param WP_User|false|null $user The user.
 * @return bool
 */
function pressfleet_two_factor_enrollment_required( $user ) {
	return pressfleet_two_factor_setup_required( $user )
		&& ! ( get_current_user_id() === $user->ID && pressfleet_is_platform_login_session() );
}

// Two Factor counts as active whenever it is installed, so deactivating it
// in wp-admin or with WP-CLI has no effect. WordPress still checks that the
// file exists before loading it.
add_filter(
	'option_active_plugins',
	static function ( $plugins ) {
		$plugins = is_array( $plugins ) ? $plugins : array();
		$plugin  = 'two-factor/two-factor.php';
		if ( ! in_array( $plugin, $plugins, true ) && file_exists( WP_PLUGIN_DIR . '/' . $plugin ) ) {
			$plugins[] = $plugin;
		}
		return $plugins;
	}
);

add_filter(
	'plugin_action_links_two-factor/two-factor.php',
	static function ( $actions ) {
		unset( $actions['deactivate'] );
		$actions['pressfleet'] = esc_html__( 'Required for administrators by the platform' );
		return $actions;
	}
);

// Logging in lands on the profile's two-factor settings.
add_filter(
	'login_redirect',
	static function ( $redirect_to, $requested, $user ) {
		if ( pressfleet_two_factor_setup_required( $user ) ) {
			return admin_url( 'profile.php#two-factor-options' );
		}
		return $redirect_to;
	},
	100,
	3
);

// Every other admin screen sends them back there. AJAX and the REST API are
// left alone: setting up a second factor uses them.
add_action(
	'admin_init',
	static function () {
		if ( wp_doing_ajax() || ( defined( 'WP_CLI' ) && WP_CLI ) ) {
			return;
		}
		if ( ! pressfleet_two_factor_enrollment_required( wp_get_current_user() ) ) {
			return;
		}
		global $pagenow;
		if ( 'profile.php' === $pagenow ) {
			return;
		}
		wp_safe_redirect( admin_url( 'profile.php#two-factor-options' ) );
		exit;
	},
	1
);

add_action(
	'admin_notices',
	static function () {
		if ( pressfleet_two_factor_enrollment_required( wp_get_current_user() ) ) {
			echo '<div class="notice notice-warning"><p>';
			echo esc_html__( 'This site requires two-factor authentication for administrators. Set up a second factor under Two-Factor Options below to continue to the rest of the dashboard.' );
			echo '</p></div>';
		} elseif ( ! class_exists( 'Two_Factor_Core' ) && current_user_can( 'manage_options' ) ) {
			echo '<div class="notice notice-error"><p>';
			echo esc_html__( 'This site requires two-factor authentication for administrators, but the Two Factor plugin is missing, so it is not enforced. The platform installs it again the next time it checks the site.' );
			echo '</p></div>';
		}
	}
);

add_filter(
	'wp_is_application_passwords_available_for_user',
	static function ( $available, $user ) {
		return $available && ! pressfleet_two_factor_setup_required( $user );
	},
	10,
	2
);
