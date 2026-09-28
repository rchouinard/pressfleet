package repository

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// Modes of local repositories. The directory is setgid, so everything
// restic creates in it inherits the repository group.
const (
	repositoryDirMode = 0o770 | fs.ModeSetgid
	// groupBitsShift moves user permission bits to the group's.
	groupBitsShift = 3
)

// prepareLocalDir creates a local repository's directory, owned by the
// repository group with mode 2770, before restic init fills it. Its
// parents stay private to the control plane: tasks bind-mount the
// directory itself.
func (m *Manager) prepareLocalDir(dir string) error {
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		return fmt.Errorf("creating repositories directory: %w", err)
	}
	if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("creating repository directory: %w", err)
	}
	if err := m.chgrp(dir, func() error { return os.Lchown(dir, -1, m.gid) }); err != nil {
		return err
	}
	if err := os.Chmod(dir, repositoryDirMode); err != nil {
		return fmt.Errorf("setting repository directory mode: %w", err)
	}
	return nil
}

// shareLocal gives the repository group access to everything in a local
// repository: group ownership, setgid directories, and group permission
// bits matching the owner's. restic (0.14 and later) derives the modes of
// new files from the group bits of the repository's config file, so later
// writes, by the control plane or by tasks, stay shared.
func (m *Manager) shareLocal(dir string) error {
	// Scoped to the repository, so a symlink in it can't redirect a change.
	root, err := os.OpenRoot(dir)
	if err != nil {
		return fmt.Errorf("sharing repository with its group: %w", err)
	}
	defer func() { _ = root.Close() }()
	err = fs.WalkDir(root.FS(), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := m.chgrp(p, func() error { return root.Lchown(p, -1, m.gid) }); err != nil {
			return err
		}
		if d.IsDir() {
			return root.Chmod(p, repositoryDirMode)
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		perm := info.Mode().Perm()
		return root.Chmod(p, perm|(perm&0o600)>>groupBitsShift)
	})
	if err != nil {
		return fmt.Errorf("sharing repository with its group: %w", err)
	}
	return nil
}

// isShared reports whether a local repository's config file is readable by
// the group, i.e. shareLocal has run since restic init.
func isShared(dir string) bool {
	info, err := os.Stat(filepath.Join(dir, "config"))
	return err == nil && info.Mode().Perm()&0o040 != 0
}

// chgrp gives path to the repository group with lchown. Only root may give
// files to a group it isn't in; elsewhere (development machines, unit
// tests) a refused change is skipped, since tasks can't run there anyway.
func (m *Manager) chgrp(path string, lchown func() error) error {
	err := lchown()
	if err == nil {
		return nil
	}
	if errors.Is(err, fs.ErrPermission) && os.Geteuid() != 0 {
		if !m.warnedChgrp.Swap(true) {
			m.log.Debug("not root: repository group ownership skipped", "gid", m.gid)
		}
		return nil
	}
	return fmt.Errorf("giving %s to group %d: %w", path, m.gid, err)
}
