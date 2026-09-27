<?php
/**
 * Security set module: disable application passwords (design doc §6.1).
 *
 * Loaded by pressfleet-security.php when
 * PRESSFLEET_DISABLE_APPLICATION_PASSWORDS is set. Application passwords let
 * integrations log in to the REST API and XML-RPC without the user's password
 * or a second factor. With this module they can't be created or used, and
 * the profile screen hides them.
 */

defined( 'ABSPATH' ) || exit;

add_filter( 'wp_is_application_passwords_available', '__return_false' );
