//go:build e2emigration

package store

// A no-op migration for the upgrade rollback's end-to-end tests (#251),
// which build the control plane with this tag: an upgrade to such a binary
// migrates the state database, backing it up first, so the rollback has to
// restore it.
func init() {
	testMigration = "SELECT 1"
}
