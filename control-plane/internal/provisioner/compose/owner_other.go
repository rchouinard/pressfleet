//go:build !unix

package compose

import "io/fs"

// fileOwner returns -1: file ownership isn't kept here.
func fileOwner(fs.FileInfo) int { return -1 }
