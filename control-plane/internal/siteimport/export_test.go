package siteimport

// SetLimits lowers the limits for a test and returns a function that
// restores them.
func SetLimits(entries int, unpacked int64) (restore func()) {
	e, u := maxEntries, maxUnpackedBytes
	maxEntries, maxUnpackedBytes = entries, unpacked
	return func() { maxEntries, maxUnpackedBytes = e, u }
}
