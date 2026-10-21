package repository

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Names inside an export directory.
const (
	ExportRepositoryDir = "repository"
	ExportPasswordFile  = "password.txt"
	ExportReadmeFile    = "README.txt"
)

// Export is a self-contained copy of a client group's repository, usable
// with plain restic and nothing from PressFleet.
type Export struct {
	// Dir is the export directory.
	Dir string
	// RepositoryDir is the copied repository (Dir/repository).
	RepositoryDir string
	// PasswordFile holds the repository password (Dir/password.txt, mode
	// 0600).
	PasswordFile string
}

// Export copies the group's repository into dest, for handing over to a
// client when offboarding (§5.3):
//
//	dest/repository/    the repository, as a local restic repository
//	dest/password.txt   its password (mode 0600)
//	dest/README.txt     how to use them with plain restic
//
// S3 repositories are downloaded, so the export is always a local
// repository. dest must be an absolute path that doesn't exist or is an
// empty directory. The copy is checked with `restic check`, opened with the
// exported password file, before Export returns.
//
// The repository isn't locked while it is copied: callers must not run it
// concurrently with prune, and a backup that is running meanwhile may be
// left out. Stale locks are not copied.
func (m *Manager) Export(ctx context.Context, clientGroupID, dest string) (Export, error) {
	rec, err := m.Get(ctx, clientGroupID)
	if err != nil {
		return Export{}, err
	}
	if err := prepareDest(dest); err != nil {
		return Export{}, err
	}
	password, err := m.password(ctx, rec)
	if err != nil {
		return Export{}, err
	}
	e := Export{
		Dir:           dest,
		RepositoryDir: filepath.Join(dest, ExportRepositoryDir),
		PasswordFile:  filepath.Join(dest, ExportPasswordFile),
	}
	if err := m.copyRepository(ctx, rec, e.RepositoryDir); err != nil {
		return Export{}, fmt.Errorf("export backup repository of client group %s: %w", clientGroupID, err)
	}
	if err := writeNew(e.PasswordFile, []byte(password.Reveal()+"\n"), 0o600); err != nil {
		return Export{}, fmt.Errorf("export backup repository of client group %s: %w", clientGroupID, err)
	}
	if err := writeNew(filepath.Join(dest, ExportReadmeFile), []byte(readme(clientGroupID)), 0o644); err != nil {
		return Export{}, fmt.Errorf("export backup repository of client group %s: %w", clientGroupID, err)
	}
	// Proves the export opens with the exported password alone.
	err = m.exec(ctx, map[string]string{
		"RESTIC_REPOSITORY":    e.RepositoryDir,
		"RESTIC_PASSWORD_FILE": e.PasswordFile,
	}, nil, nil, io.Discard, "check")
	if err != nil {
		return Export{}, fmt.Errorf("export backup repository of client group %s: checking the copy: %w", clientGroupID, err)
	}
	m.log.Info("backup repository exported", "client_group_id", clientGroupID, "dir", dest)
	return e, nil
}

// prepareDest creates dest, or accepts it if it is an empty directory.
func prepareDest(dest string) error {
	if !filepath.IsAbs(dest) {
		return fmt.Errorf("%w: %q is not an absolute path", ErrExportDestination, dest)
	}
	entries, err := os.ReadDir(dest)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if err := os.MkdirAll(dest, 0o700); err != nil {
			return fmt.Errorf("creating export directory: %w", err)
		}
		return nil
	case err != nil:
		return fmt.Errorf("%w: %w", ErrExportDestination, err)
	case len(entries) > 0:
		return fmt.Errorf("%w: %s is not empty", ErrExportDestination, dest)
	}
	return nil
}

// skipExport reports whether a repository-relative path (slash-separated)
// is left out of an export: locks, which a copy would carry over as stale
// locks the client would have to remove.
func skipExport(rel string) bool {
	return rel == "locks" || strings.HasPrefix(rel, "locks/")
}

// copyRepository copies the repository into dir, which must not exist.
func (m *Manager) copyRepository(ctx context.Context, rec Repository, dir string) error {
	// restic creates locks/ itself on the local backend's Create, not Open;
	// an empty one keeps the copy a complete repository.
	if err := os.MkdirAll(filepath.Join(dir, "locks"), 0o700); err != nil {
		return fmt.Errorf("creating export repository: %w", err)
	}
	switch rec.Backend {
	case BackendLocal:
		src, err := m.localDir(rec)
		if err != nil {
			return err
		}
		return copyTree(ctx, src, dir)
	case BackendS3:
		obj, err := parseS3Location(rec.Location)
		if err != nil {
			return err
		}
		c, err := m.s3Client(obj)
		if err != nil {
			return err
		}
		keys, err := c.list(ctx, obj.key+"/")
		if err != nil {
			return err
		}
		for _, k := range keys {
			rel := strings.TrimPrefix(k, obj.key+"/")
			if skipExport(rel) || !fs.ValidPath(rel) || strings.HasSuffix(k, "/") {
				continue
			}
			if err := downloadFile(ctx, c, k, filepath.Join(dir, filepath.FromSlash(rel))); err != nil {
				return err
			}
		}
		return nil
	default:
		return fmt.Errorf("unknown backend %q", rec.Backend)
	}
}

// copyTree copies the regular files and directories under src into dst.
func copyTree(ctx context.Context, src, dst string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		if skipExport(filepath.ToSlash(rel)) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		target := filepath.Join(dst, rel)
		switch {
		case d.IsDir():
			return os.MkdirAll(target, 0o700)
		case d.Type().IsRegular():
			return copyFile(p, target)
		default:
			return fmt.Errorf("unexpected file %s in repository", p)
		}
	})
}

func copyFile(src, dst string) (err error) {
	in, err := os.Open(src) //nolint:gosec // G304: a file inside the group's repository
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	return writeFrom(dst, func(w io.Writer) error {
		_, err := io.Copy(w, in)
		return err
	})
}

func downloadFile(ctx context.Context, c *s3Client, key, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	return writeFrom(dst, func(w io.Writer) error { return c.get(ctx, key, w) })
}

// writeFrom creates dst (mode 0600, which restic uses too) and fills it
// with fill.
func writeFrom(dst string, fill func(io.Writer) error) (err error) {
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // G304: a path inside the export directory
	if err != nil {
		return err
	}
	defer func() {
		if cerr := out.Close(); err == nil {
			err = cerr
		}
	}()
	return fill(out)
}

// writeNew creates a file that must not exist.
func writeNew(p string, content []byte, mode os.FileMode) error {
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode) //nolint:gosec // G304: a path inside the export directory
	if err != nil {
		return err
	}
	_, err = f.Write(content)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// readme explains the export to whoever receives it.
func readme(clientGroupID string) string {
	return strings.ReplaceAll(`This directory holds the backups of PressFleet client group CLIENT_GROUP_ID
as a Restic repository (https://restic.net). It needs nothing from
PressFleet: restic 0.17 or later and the password file are enough.

  repository/     the Restic repository
  password.txt    its password; keep it safe, the backups can't be read
                  without it

Each snapshot holds one site's wp-content and database dump:

  /site/wp-content/        the site's wp-content directory
  /transfer/database.sql   its database, as SQL from mariadb-dump

Snapshots are tagged site:<site ID>, wp-core:<WordPress core version>, and
kind:<manual, scheduled, or pre_restore>; their host name is
site-<site ID>. WordPress core and wp-config.php are not in the backups:
install the WordPress version from the wp-core tag and write a new
wp-config.php for the restored database.

List the snapshots, or one site's:

  restic -r repository --password-file password.txt snapshots
  restic -r repository --password-file password.txt snapshots --tag site:<site ID>

Restore one (use an ID from the list, or "latest" with --tag site:<site ID>):

  restic -r repository --password-file password.txt restore <snapshot> --target restored

The repository can be moved to another location or storage service with
"restic copy", or its password changed with "restic key passwd".
`, "CLIENT_GROUP_ID", clientGroupID)
}
