//go:build linux

package deploy

import (
	"errors"
	"io/fs"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// exchange atomically swaps the entries a and b of dir (renameat2 with
// RENAME_EXCHANGE): at every moment, each name is one version or the
// other. It returns errNoExchange where the filesystem can't do it.
func exchange(dir *os.File, a, b string) error {
	fd := int(dir.Fd())
	err := unix.Renameat2(fd, a, fd, b, unix.RENAME_EXCHANGE)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, unix.EINVAL), errors.Is(err, unix.ENOSYS), errors.Is(err, unix.EOPNOTSUPP):
		return errNoExchange
	}
	return &os.LinkError{Op: "exchange", Old: a, New: b, Err: err}
}

// inode returns the inode number of a file, which stays the same when the
// file is renamed.
func inode(fi fs.FileInfo) uint64 {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return st.Ino
	}
	return 0
}
