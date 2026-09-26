//go:build !linux

package doctor

import "errors"

// PressFleet only runs on Linux; this keeps the package building elsewhere
// so the platform check can report the problem.
func freeDisk(string) (uint64, error) {
	return 0, errors.New("free disk space is only checked on Linux")
}
