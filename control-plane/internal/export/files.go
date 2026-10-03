package export

import (
	"bufio"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/id"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
)

// Export file names. In the exports directory each file is named
// <export ID>.<file name>, after an optional dot for temporary files, so
// the sweep can tell whose it is.
const (
	// FileDatabase is the database dump, compressed with gzip.
	FileDatabase = "database.sql.gz"
	// FileWPContent is the wp-content archive, a tar compressed with gzip.
	FileWPContent = "wp-content.tar.gz"
)

// ContentType is the media type of every export file.
const ContentType = "application/gzip"

// sweepBatch is how many expired exports Sweep reads per query.
const sweepBatch = 100

// filePath is where the export's file is kept.
func (s *Service) filePath(exportID, name string) string {
	return filepath.Join(s.dir, exportID+"."+name)
}

// writeGzip writes the export's file through a temporary file, so the
// name only ever holds a complete file, compressing what write writes with
// gzip, and returns the file's size. A retry starts over, replacing what
// an earlier attempt left.
func (s *Service) writeGzip(exportID, name string, write func(w io.Writer) error) (size int64, err error) {
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return 0, fmt.Errorf("creating the exports directory: %w", err)
	}
	dest := s.filePath(exportID, name)
	out, err := os.CreateTemp(s.dir, "."+filepath.Base(dest)+".*.tmp")
	if err != nil {
		return 0, err
	}
	defer func() {
		if err != nil {
			_ = out.Close()
			_ = os.Remove(out.Name())
		}
	}()
	buf := bufio.NewWriterSize(out, 1<<20)
	zw := gzip.NewWriter(buf)
	zw.Name = strings.TrimSuffix(name, ".gz")
	if err := write(zw); err != nil {
		return 0, err
	}
	if err := zw.Close(); err != nil {
		return 0, err
	}
	if err := buf.Flush(); err != nil {
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

// writeDatabase dumps the site's database and compresses it into the
// export's FileDatabase, returning its size.
func (s *Service) writeDatabase(ctx context.Context, siteID, exportID string) (int64, error) {
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return 0, fmt.Errorf("creating the exports directory: %w", err)
	}
	dump := s.filePath(exportID, strings.TrimSuffix(FileDatabase, ".gz"))
	defer func() { _ = os.Remove(dump) }()
	if err := s.dbs.Dump(ctx, siteID, dump); err != nil {
		return 0, fmt.Errorf("dumping the database: %w", err)
	}
	size, err := s.writeGzip(exportID, FileDatabase, func(w io.Writer) error {
		in, err := os.Open(dump) //nolint:gosec // G304: a path this package built in its own directory
		if err != nil {
			return err
		}
		defer func() { _ = in.Close() }()
		_, err = io.Copy(w, in)
		return err
	})
	if err != nil {
		return 0, fmt.Errorf("compressing the dump: %w", err)
	}
	return size, nil
}

// writeArchive writes the export's FileWPContent from the tar archive of
// wp-content that produce writes, keeping what site import takes
// (filterTar), and returns what it kept and the file's size. produce runs
// alongside, so the archive never needs room of its own.
func (s *Service) writeArchive(exportID string, produce func(w io.Writer) error) (archiveStats, int64, error) {
	var stats archiveStats
	size, err := s.writeGzip(exportID, FileWPContent, func(w io.Writer) error {
		pr, pw := io.Pipe()
		produced := make(chan error, 1)
		go func() {
			err := produce(pw)
			_ = pw.CloseWithError(err) // nil closes it with io.EOF
			produced <- err
		}()
		var err error
		stats, err = filterTar(w, pr)
		if err == nil {
			// Whatever follows the archive's end, such as tar's padding,
			// must still be read, or the producer's writes fail.
			_, err = io.Copy(io.Discard, pr)
		}
		// Stops the producer if the archive couldn't be read.
		_ = pr.CloseWithError(errors.New("export: the archive was not read to its end"))
		perr := <-produced
		if err != nil {
			// The cause: a producer that failed first closed the pipe with
			// its error, which err wraps; otherwise the producer's error
			// only says that the pipe was closed.
			return err
		}
		return perr
	})
	return stats, size, err
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
	Export store.Export
	// File is the file; the caller closes it.
	File *os.File
	// Name is the file name to offer the user.
	Name string
	// Size is the file's size in bytes, and ModTime when it was written.
	Size    int64
	ModTime time.Time
}

// Open opens a file of a ready export for download. It returns
// ErrExportNotFound if there is no such export or file, or the export isn't
// ready or has expired, in which case it is expired at once: its files
// deleted and its record marked expired. The site may have been deleted
// since the export.
func (s *Service) Open(ctx context.Context, exportID, name string) (Download, error) {
	e, err := s.Get(ctx, exportID)
	if err != nil {
		return Download{}, err
	}
	if e.Status != store.ExportReady {
		return Download{}, fmt.Errorf("%w: export %s is %s", ErrExportNotFound, exportID, e.Status)
	}
	if _, ok := e.File(name); !ok {
		return Download{}, fmt.Errorf("%w: export %s has no file %q", ErrExportNotFound, exportID, name)
	}
	f, err := os.Open(s.filePath(e.ID, name))
	if errors.Is(err, fs.ErrNotExist) {
		return Download{}, fmt.Errorf("%w: the file %s of export %s is gone", ErrExportNotFound, name, exportID)
	}
	if err != nil {
		return Download{}, fmt.Errorf("opening export %s: %w", exportID, err)
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return Download{}, fmt.Errorf("opening export %s: %w", exportID, err)
	}
	return Download{Export: e, File: f, Name: downloadName(e, name), Size: info.Size(), ModTime: info.ModTime()}, nil
}

// OpenDatabase opens the file of a ready database export of the site, for
// the download endpoint of database exports. Besides Open's errors, it
// returns ErrExportNotFound for an export of another site or kind, or
// whose site is gone.
func (s *Service) OpenDatabase(ctx context.Context, siteID, exportID string) (Download, error) {
	e, err := s.Get(ctx, exportID)
	if err != nil {
		return Download{}, err
	}
	if e.SiteID != siteID || e.Kind != store.ExportDatabase {
		return Download{}, fmt.Errorf("%w: %s", ErrExportNotFound, exportID)
	}
	if _, err := store.NewSites(s.db).Get(ctx, siteID); errors.Is(err, store.ErrNotFound) {
		return Download{}, fmt.Errorf("%w: site %s is gone", ErrExportNotFound, siteID)
	} else if err != nil {
		return Download{}, err
	}
	return s.Open(ctx, exportID, FileDatabase)
}

// downloadName is the file name offered for an export's file: the site's
// slug says whose it is. A database export's names its ID too, as it
// always has; a site export's files have the names site import expects.
func downloadName(e store.Export, name string) string {
	slug := e.SiteSlug
	if slug == "" {
		slug = "site-" + e.SiteID
	}
	if e.Kind == store.ExportDatabase {
		return slug + "-" + e.ID + ".sql.gz"
	}
	return slug + "-" + name
}

// expire deletes the export's files and marks it expired. The files go
// first, so an expired record never has a file left behind; a download
// already streaming keeps its open file.
func (s *Service) expire(ctx context.Context, e store.Export) error {
	if err := s.removeFiles(e.ID); err != nil {
		return fmt.Errorf("expiring export %s: %w", e.ID, err)
	}
	changed, err := store.NewExports(s.db).Expire(ctx, e.ID)
	if err != nil {
		return err
	}
	if changed {
		s.log.InfoContext(ctx, "export expired", "kind", e.Kind, "site_id", e.SiteID, "export_id", e.ID)
	}
	return nil
}

// Sweep expires every export whose expiry has passed and deletes files
// that belong to no running or ready export, returning how many exports it
// expired. Running it again changes nothing.
func (s *Service) Sweep(ctx context.Context) (int, error) {
	exports := store.NewExports(s.db)
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
	exports := store.NewExports(s.db)
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
				k = e.Status == store.ExportRunning || e.Status == store.ExportReady
			}
			keep[exportID] = k
		}
		if k {
			continue
		}
		if err := os.Remove(filepath.Join(s.dir, entry.Name())); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("removing a stale export file: %w", err)
		}
		s.log.InfoContext(ctx, "stale export file removed", "export_id", exportID, "file", entry.Name())
	}
	return nil
}

// parseDU reads the size in bytes from the output of "du -sk PATH".
func parseDU(stdout string) (int64, error) {
	f := strings.Fields(stdout)
	if len(f) == 0 {
		return 0, errors.New("du printed nothing")
	}
	kib, err := strconv.ParseInt(f[0], 10, 64)
	if err != nil || kib < 0 {
		return 0, fmt.Errorf("parse du output %q: %w", strings.TrimSpace(stdout), err)
	}
	return kib * 1024, nil
}
