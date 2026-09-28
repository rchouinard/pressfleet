package repository_test

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/rchouinard/pressfleet/control-plane/internal/backup/repository"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
)

// Local repositories are shared with the repository group, so tasks running
// as the different UIDs of a group's sites can all write to them, and what
// restic adds later stays shared.
func TestLocalRepositoryIsShared(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := storetest.Open(t, store.SQLite)
	// Not root: use a group this user may give files to.
	gid := os.Getgid()
	f := newFixture(t, db, repository.Options{RepositoryGID: gid})
	groupID := f.group(t, "Acme")
	access, err := f.m.TaskAccess(ctx, groupID)
	if err != nil {
		t.Fatal(err)
	}
	dir := access.RepositoryDir
	if access.BackupNetwork {
		t.Error("a local repository's tasks join the backup network")
	}
	if info, err := os.Stat(filepath.Dir(dir)); err != nil || info.Mode().Perm() != 0o700 {
		t.Errorf("repositories directory: %v, %v; want mode 0700", info, err)
	}
	backupFile(t, access, t.TempDir())

	check := func() {
		t.Helper()
		err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			info, err := d.Info()
			if err != nil {
				return err
			}
			mode := info.Mode()
			if d.IsDir() && (mode&os.ModeSetgid == 0 || mode.Perm() != 0o770) {
				t.Errorf("%s: mode %v, want setgid 0770", p, mode)
			}
			if !d.IsDir() && mode.Perm()&0o040 == 0 {
				t.Errorf("%s: mode %v, want group-readable", p, mode)
			}
			if st, ok := info.Sys().(*syscall.Stat_t); ok && int(st.Gid) != gid {
				t.Errorf("%s: group %d, want %d", p, st.Gid, gid)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	check()

	// An Ensure interrupted between restic init and sharing is finished by
	// the next one.
	if err := os.Chmod(filepath.Join(dir, "config"), 0o400); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(dir, "keys"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := f.m.Ensure(ctx, groupID); err != nil {
		t.Fatal(err)
	}
	check()
}
