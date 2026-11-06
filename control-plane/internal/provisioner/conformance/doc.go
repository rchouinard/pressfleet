// Package conformance is the test suite every [provisioner.Driver] has to
// pass. The Compose driver's Docker tests used to hold the driver contract
// (lifecycle, idempotency, maintenance mode, WP-CLI, tasks, sessions, logs,
// stats) through Compose types; this package states it once, against the
// interfaces, so that a second driver (Kubernetes, #228) is held to the
// same behavior.
//
// A driver's test calls [Run] with an [Env] that says how to build the
// driver, which images and platform services its sites use, and how to
// look inside a site from outside the driver:
//
//	func TestConformance(t *testing.T) {
//		conformance.Run(t, conformance.Env{ /* ... */ })
//	}
//
// The suite needs a real orchestrator, so the driver's test is skipped
// unless PRESSFLEET_DOCKER_TESTS=1 (docs/testing.md). The package itself
// names no driver: it imports no orchestrator SDK, and archtest keeps it so.
package conformance
