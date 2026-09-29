//go:build !linux

package deploy

import (
	"io/fs"
	"os"
)

// exchange is only available on Linux, where site tasks run.
func exchange(*os.File, string, string) error { return errNoExchange }

// inode is only available on Linux, where site tasks run; 0 makes the
// deployment operations fail.
func inode(fs.FileInfo) uint64 { return 0 }
