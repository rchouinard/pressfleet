//go:build unix

package export

import "syscall"

// freeSpace returns the bytes available to unprivileged users on the file
// system holding dir.
func freeSpace(dir string) (uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, err
	}
	return uint64(st.Bavail) * uint64(st.Bsize), nil //nolint:gosec,unconvert // G115: block counts and sizes are never negative; the types differ between systems
}
