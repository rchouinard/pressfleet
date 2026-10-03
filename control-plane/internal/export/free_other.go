//go:build !unix

package export

import "errors"

// freeSpace isn't supported here; the space check is skipped.
func freeSpace(string) (uint64, error) { return 0, errors.ErrUnsupported }
