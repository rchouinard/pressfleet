<?php
/**
 * Security set module: password hashing algorithm (design doc §6.1).
 *
 * Loaded by pressfleet-security.php when PRESSFLEET_PASSWORD_HASHING is set.
 * Without it WordPress (6.8+) hashes new passwords with bcrypt. With
 * 'argon2id', new hashes use PHP's Argon2id at its default cost, which takes
 * about 64 MiB of memory per hash (outside PHP's memory_limit, inside the
 * container's): a login costs that much, which counts against the capacity
 * target (§4.1).
 *
 * Existing hashes of any kind still verify: wp_check_password() recognizes
 * them, and wp_password_needs_rehash() rehashes a password with the current
 * algorithm the next time its user logs in. Switching back works the same
 * way.
 */

defined( 'ABSPATH' ) || exit;

add_filter(
	'wp_hash_password_algorithm',
	static function ( $algorithm ) {
		if ( 'argon2id' === PRESSFLEET_PASSWORD_HASHING && defined( 'PASSWORD_ARGON2ID' ) ) {
			return PASSWORD_ARGON2ID;
		}
		return $algorithm;
	}
);
