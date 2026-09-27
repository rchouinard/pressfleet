<?php
/**
 * Plugin Name: PressFleet Security
 * Description: The platform's WordPress security set. Each module runs only when the platform turns it on for this installation.
 * Author: PressFleet
 * License: MIT
 *
 * The PHP half of the install-wide WordPress security set (design doc §6.1).
 * The control plane switches modules on with PRESSFLEET_* constants in the
 * generated wp-config.php; a module whose constant is missing or false is
 * never loaded. The nginx half (login rate limit, security headers) lives in
 * the site-nginx image. See images/README.md.
 */

defined( 'ABSPATH' ) || exit;

foreach (
	array(
		'PRESSFLEET_BLOCK_USER_ENUMERATION'        => 'user-enumeration.php',
		'PRESSFLEET_PASSWORD_POLICY_MIN_LENGTH'    => 'password-policy.php',
		'PRESSFLEET_PASSWORD_HASHING'              => 'password-hashing.php',
		'PRESSFLEET_DISABLE_APPLICATION_PASSWORDS' => 'application-passwords.php',
		'PRESSFLEET_REQUIRE_TWO_FACTOR'            => 'two-factor.php',
	) as $pressfleet_constant => $pressfleet_module
) {
	if ( defined( $pressfleet_constant ) && constant( $pressfleet_constant ) ) {
		require __DIR__ . '/pressfleet-security/' . $pressfleet_module;
	}
}
unset( $pressfleet_constant, $pressfleet_module );
