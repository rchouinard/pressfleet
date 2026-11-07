//go:build unix

package kubernetes

import (
	"io/fs"
	"syscall"
)

// fileOwner returns the UID that owns the file, or -1 if unknown.
func fileOwner(fi fs.FileInfo) int {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return int(st.Uid)
	}
	return -1
}
