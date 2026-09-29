package dbexport

import (
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/rchouinard/pressfleet/control-plane/internal/id"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
)

// File names in the exports directory. Every file of an export starts with
// its ID, after an optional dot for temporary files, so the sweep can tell
// whose it is.
const (
	dumpSuffix = ".sql"
	fileSuffix = ".sql.gz"
)

// FileName is the name of an export's compressed dump in the exports
// directory.
func FileName(exportID string) string { return exportID + fileSuffix }

// ContentType is the media type of an export's file.
const ContentType = "application/gzip"

// sweepBatch is how many expired exports Sweep reads per query.
const sweepBatch = 100

// writeExport dumps the site's database and compresses it into the
// export's file, returning its size. A retry starts over, replacing what
// an earlier attempt left.
func (s *Service) writeExport(ctx context.Context, siteID, exportID string) (int64, error) {
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return 0, fmt.Errorf("creating the exports directory: %w", err)
	}
	dump := filepath.Join(s.dir, exportID+dumpSuffix)
	defer func() { _ = os.Remove(dump) }()
	if err := s.dbs.Dump(ctx, siteID, dump); err != nil {
		return 0, fmt.Errorf("dumping the database: %w", err)
	}
	size, err := compress(dump, filepath.Join(s.dir, FileName(exportID)))
	if err != nil {
		return 0, fmt.Errorf("compressing the dump: %w", err)
	}
	return size, nil
}

// compress gzips src into dest through a temporary file, so dest only ever
// holds a complete file, and returns dest's size.
func compress(src, dest string) (size int64, err error) {
	in, err := os.Open(src) //nolint:gosec // G304: a path this package built in its own directory
	if err != nil {
		return 0, err
	}
	defer func() { _ = in.Close() }()
	out, err := os.CreateTemp(filepath.Dir(dest), "."+filepath.Base(dest)+".*.tmp")
	if err != nil {
		return 0, err
	}
	defer func() {
		if err != nil {
			_ = out.Close()
			_ = os.Remove(out.Name())
		}
	}()
	zw := gzip.NewWriter(out)
	zw.Name = strings.TrimSuffix(filepath.Base(dest), ".gz")
	if _, err := io.Copy(zw, in); err != nil {
		return 0, err
	}
	if err := zw.Close(); err != nil {
		return 0, err
	}
	if err := out.Sync(); err != nil {
		return 0, err
	}
	info, err := out.Stat()
	if err != nil {
		return 0, err
	}
	if err := out.Close(); err != nil {
		return 0, err
	}
	if err := os.Rename(out.Name(), dest); err != nil {
		return 0, err
	}
	return info.Size(), nil
}

// removeFiles deletes every file of the export.
func (s *Service) removeFiles(exportID string) error {
	entries, err := os.ReadDir(s.dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("listing the exports directory: %w", err)
	}
	var errs []error
	for _, e := range entries {
		if owner, ok := fileExport(e.Name()); ok && owner == exportID {
			if err := os.Remove(filepath.Join(s.dir, e.Name())); err != nil && !errors.Is(err, fs.ErrNotExist) {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

// fileExport returns the ID of the export a file in the exports directory
// belongs to.
func fileExport(name string) (string, bool) {
	exportID, _, _ := strings.Cut(strings.TrimPrefix(name, "."), ".")
	return exportID, id.Valid(exportID)
}

// Download is an export's file, opened for reading.
type Download struct {
	Export store.DatabaseExport
	// File is the compressed dump; the caller closes it.
	File *os.File
	// Name is the file name to offer the user.
	Name string
	// Size is the file's size in bytes.
	Size int64
}

// Open opens the file of a ready export of the site for download. It
// returns ErrExportNotFound if there is no such export, it isn't ready, the
// site is gone, or it has expired, in which case it is expired at once:
// its file deleted and its record marked expired.
func (s *Service) Open(ctx context.Context, siteID, exportID string) (Download, error) {
	if !id.Valid(exportID) {
		return Download{}, fmt.Errorf("%w: %s", ErrExportNotFound, exportID)
	}
	e, err := store.NewDatabaseExports(s.db).Get(ctx, exportID)
	if errors.Is(err, store.ErrNotFound) || (err == nil && e.SiteID != siteID) {
		return Download{}, fmt.Errorf("%w: %s", ErrExportNotFound, exportID)
	}
	if err != nil {
		return Download{}, err
	}
	if e.Status != store.DatabaseExportReady {
		return Download{}, fmt.Errorf("%w: export %s is %s", ErrExportNotFound, exportID, e.Status)
	}
	if !s.now().Before(e.ExpiresAt) {
		if err := s.expire(ctx, e); err != nil {
			return Download{}, err
		}
		return Download{}, fmt.Errorf("%w: export %s has expired", ErrExportNotFound, exportID)
	}
	site, err := store.NewSites(s.db).Get(ctx, siteID)
	if errors.Is(err, store.ErrNotFound) {
		return Download{}, fmt.Errorf("%w: site %s is gone", ErrExportNotFound, siteID)
	} else if err != nil {
		return Download{}, err
	}
	f, err := os.Open(filepath.Join(s.dir, FileName(exportID)))
	if errors.Is(err, fs.ErrNotExist) {
		return Download{}, fmt.Errorf("%w: the file of export %s is gone", ErrExportNotFound, exportID)
	}
	if err != nil {
		return Download{}, fmt.Errorf("opening database export %s: %w", exportID, err)
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return Download{}, fmt.Errorf("opening database export %s: %w", exportID, err)
	}
	// The slug says whose database it is; the ID tells exports apart.
	return Download{Export: e, File: f, Name: site.Slug + "-" + FileName(exportID), Size: info.Size()}, nil
}

// expire deletes the export's files and marks it expired. The files go
// first, so an expired record never has a file left behind; a download
// already streaming keeps its open file.
func (s *Service) expire(ctx context.Context, e store.DatabaseExport) error {
	if err := s.removeFiles(e.ID); err != nil {
		return fmt.Errorf("expiring database export %s: %w", e.ID, err)
	}
	changed, err := store.NewDatabaseExports(s.db).Expire(ctx, e.ID)
	if err != nil {
		return err
	}
	if changed {
		s.log.InfoContext(ctx, "database export expired", "site_id", e.SiteID, "export_id", e.ID)
	}
	return nil
}

// Sweep expires every export whose expiry has passed and deletes files
// that belong to no running or ready export, returning how many exports it
// expired. Running it again changes nothing.
func (s *Service) Sweep(ctx context.Context) (int, error) {
	exports := store.NewDatabaseExports(s.db)
	total := 0
	for {
		batch, err := exports.ListExpired(ctx, s.now(), sweepBatch)
		if err != nil {
			return total, err
		}
		for _, e := range batch {
			if err := s.expire(ctx, e); err != nil {
				return total, err
			}
			total++
		}
		if len(batch) < sweepBatch {
			break
		}
	}
	return total, s.removeOrphans(ctx)
}

// removeOrphans deletes files in the exports directory whose export isn't
// running or ready: failed and expired exports whose files a crash kept
// from being deleted, and files of exports that don't exist.
func (s *Service) removeOrphans(ctx context.Context) error {
	entries, err := os.ReadDir(s.dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("listing the exports directory: %w", err)
	}
	exports := store.NewDatabaseExports(s.db)
	keep := map[string]bool{}
	for _, entry := range entries {
		exportID, ok := fileExport(entry.Name())
		if !ok {
			continue // not ours
		}
		k, seen := keep[exportID]
		if !seen {
			e, err := exports.Get(ctx, exportID)
			switch {
			case errors.Is(err, store.ErrNotFound):
			case err != nil:
				return err
			default:
				k = e.Status == store.DatabaseExportRunning || e.Status == store.DatabaseExportReady
			}
			keep[exportID] = k
		}
		if k {
			continue
		}
		if err := os.Remove(filepath.Join(s.dir, entry.Name())); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("removing a stale database export file: %w", err)
		}
		s.log.InfoContext(ctx, "stale database export file removed", "export_id", exportID, "file", entry.Name())
	}
	return nil
}
