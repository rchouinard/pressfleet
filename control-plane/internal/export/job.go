package export

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/backup"
	"github.com/rchouinard/pressfleet/control-plane/internal/id"
	"github.com/rchouinard/pressfleet/control-plane/internal/jobs"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
	"github.com/rchouinard/pressfleet/control-plane/internal/sitedb"
	"github.com/rchouinard/pressfleet/control-plane/internal/siteimport"
	"github.com/rchouinard/pressfleet/control-plane/internal/sites"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
)

// maxMessage bounds an export's failure message.
const maxMessage = 2000

// dataSource is the job data key under which dump_database keeps what it
// learned from a snapshot's dump, for mark_ready.
const dataSource = "source"

// source is what dump_database learned about a snapshot export.
type source struct {
	SiteURL     string    `json:"site_url,omitempty"`
	TablePrefix string    `json:"table_prefix,omitempty"`
	TakenAt     time.Time `json:"taken_at"`
}

// jobExportID returns the ID of the export the job's steps created, and
// whether they have created one yet.
func jobExportID(r *jobs.Run) (string, bool, error) {
	var exportID string
	found, err := r.Get(DataExportID, &exportID)
	return exportID, found, err
}

// newExportID returns the job's export ID, recording a new one before the
// export exists, so a retry creates the same one.
func newExportID(r *jobs.Run) (string, error) {
	exportID, found, err := jobExportID(r)
	if err != nil || found {
		return exportID, err
	}
	exportID = id.New()
	return exportID, r.Set(DataExportID, exportID)
}

// jobExport returns the job's export, and whether the step has nothing to
// do because the export is already ready (or expired since). A failed
// export fails the step for good.
func (s *Service) jobExport(ctx context.Context, r *jobs.Run) (store.Export, bool, error) {
	exportID, found, err := jobExportID(r)
	if err != nil {
		return store.Export{}, false, err
	}
	if !found {
		return store.Export{}, false, jobs.Permanent(errors.New("the job has no export"))
	}
	e, err := store.NewExports(s.db).Get(ctx, exportID)
	if err != nil {
		return store.Export{}, false, err
	}
	switch e.Status {
	case store.ExportReady, store.ExportExpired:
		return e, true, nil
	case store.ExportFailed:
		return store.Export{}, false, jobs.Permanent(fmt.Errorf("export %s already failed", exportID))
	}
	return e, false, nil
}

// create creates the export's record, once.
func (s *Service) create(ctx context.Context, r *jobs.Run, e store.Export) error {
	err := store.NewExports(s.db).Create(ctx, &e)
	if errors.Is(err, store.ErrConflict) {
		return nil // an earlier attempt created it
	}
	if err != nil {
		return err
	}
	switch e.Kind {
	case store.ExportDatabase:
		r.Logf("Exporting the site's database (export %s).", e.ID)
	case store.ExportSite:
		r.Logf("Exporting the site's wp-content and database (export %s).", e.ID)
	case store.ExportSnapshot:
		r.Logf("Exporting the site's wp-content and database from snapshot %s of backup %s (export %s).",
			shortSnapshot(e.SnapshotID), e.BackupID, e.ID)
	}
	return nil
}

// recordLive returns the record step of database and site exports: check
// the site and create the export's record, describing the site as it is
// now.
func (s *Service) recordLive(kind store.ExportKind) func(context.Context, *jobs.Run) error {
	return func(ctx context.Context, r *jobs.Run) error {
		siteID := r.Job().TargetID
		site, err := exportableSite(ctx, s.db, siteID)
		if errors.Is(err, ErrSiteNotFound) || errors.Is(err, ErrNotExportable) {
			return jobs.Permanent(err)
		} else if err != nil {
			return err
		}
		exportID, err := newExportID(r)
		if err != nil {
			return err
		}
		return s.create(ctx, r, store.Export{
			ID: exportID, Kind: kind, SiteID: siteID, SiteSlug: site.Slug, ClientGroupID: site.ClientGroupID, JobID: r.Job().ID,
			Source: store.ExportSource{
				SiteURL: sites.URL(site), TablePrefix: site.TablePrefix, WPCoreVersion: site.WPCoreVersion,
				PHPVersion: site.PHPVersion, TakenAt: s.now(),
			},
		})
	}
}

// recordBackup is the record step of snapshot exports: check the backup
// and create the export's record. The rest of what the snapshot holds is
// learned from its dump.
func (s *Service) recordBackup(ctx context.Context, r *jobs.Run) error {
	var p backupPayload
	if err := r.Payload(&p); err != nil {
		return jobs.Permanent(err)
	}
	rec, err := exportableBackup(ctx, s.db, p.BackupID)
	if errors.Is(err, ErrBackupNotFound) || errors.Is(err, ErrBackupNotExportable) {
		return jobs.Permanent(err)
	} else if err != nil {
		return err
	}
	slug := rec.SiteSlug
	if slug == "" {
		// A backup taken before backups kept the slug.
		if site, err := store.NewSites(s.db).Get(ctx, rec.SiteID); err == nil {
			slug = site.Slug
		}
	}
	exportID, err := newExportID(r)
	if err != nil {
		return err
	}
	return s.create(ctx, r, store.Export{
		ID: exportID, Kind: store.ExportSnapshot, SiteID: rec.SiteID, SiteSlug: slug, ClientGroupID: rec.ClientGroupID,
		JobID: r.Job().ID, BackupID: rec.ID, SnapshotID: rec.SnapshotID,
		Source: store.ExportSource{WPCoreVersion: rec.WPCoreVersion, TakenAt: rec.CreatedAt},
	})
}

// failExport marks the job's export failed and deletes its files, unless
// it is ready: then the caller may already have its link, and it expires
// as usual.
func (s *Service) failExport(ctx context.Context, r *jobs.Run) error {
	exportID, found, err := jobExportID(r)
	if err != nil || !found {
		return err
	}
	exports := store.NewExports(s.db)
	e, err := exports.Get(ctx, exportID)
	if errors.Is(err, store.ErrNotFound) {
		return s.removeFiles(exportID)
	}
	if err != nil {
		return err
	}
	if e.Status == store.ExportReady {
		return nil
	}
	msg := r.Job().Error
	if msg == "" {
		msg = "the export job failed"
	}
	if len(msg) > maxMessage {
		msg = msg[:maxMessage-3] + "..."
	}
	changed, err := exports.Fail(ctx, exportID, msg)
	if err != nil {
		return err
	}
	if changed {
		r.Warnf("Export %s failed.", exportID)
	}
	return s.removeFiles(exportID)
}

// checkSpace estimates the export's size and refuses to go on if the
// exports directory lacks room for it.
func (s *Service) checkSpace(ctx context.Context, r *jobs.Run) error {
	e, done, err := s.jobExport(ctx, r)
	if err != nil || done {
		return err
	}
	var estimate int64
	switch e.Kind {
	case store.ExportSite:
		if estimate, err = s.estimateSite(ctx, r, e.SiteID); err != nil {
			return err
		}
	case store.ExportSnapshot:
		rec, err := store.NewBackupRecords(s.db).Get(ctx, e.BackupID)
		if errors.Is(err, store.ErrNotFound) {
			return jobs.Permanent(fmt.Errorf("%w: backup %s is gone", ErrBackupNotExportable, e.BackupID))
		} else if err != nil {
			return err
		}
		estimate = rec.SizeBytes
	default:
		return nil
	}
	if err := r.SetResult("estimated_size_bytes", estimate); err != nil {
		return err
	}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return fmt.Errorf("creating the exports directory: %w", err)
	}
	avail, err := s.free(s.dir)
	if errors.Is(err, errors.ErrUnsupported) {
		r.Warnf("The export may need up to %s; free disk space can't be checked on this system.", formatBytes(estimate))
		return nil
	}
	if err != nil {
		return fmt.Errorf("checking free disk space: %w", err)
	}
	if estimate > 0 && uint64(estimate) > avail {
		return jobs.Permanent(fmt.Errorf("%w: it may need up to %s, and the data directory has %s free",
			ErrInsufficientSpace, formatBytes(estimate), formatBytes(int64(min(avail, 1<<62)))))
	}
	r.Logf("The export may need up to %s; the data directory has %s free.", formatBytes(estimate), formatBytes(int64(min(avail, 1<<62))))
	return nil
}

// estimateSite returns the size of the site's database and wp-content.
func (s *Service) estimateSite(ctx context.Context, r *jobs.Run, siteID string) (int64, error) {
	var dbSize int64
	if s.sizes != nil {
		sizes, err := s.sizes.DatabaseSizes(ctx)
		if err != nil {
			r.Warnf("The database's size is unknown, so it is left out of the estimate: %v", err)
		}
		dbSize = sizes[siteID]
	}
	res, err := s.tasks.RunTask(ctx, siteID, provisioner.Task{
		Command:   []string{"du", "-sk", provisioner.TaskWPContentPath},
		WPContent: provisioner.MountReadOnly,
		Timeout:   s.timeout,
	})
	if errors.Is(err, provisioner.ErrSiteNotFound) {
		return 0, jobs.Permanent(fmt.Errorf("%w: the site has no containers", ErrNotExportable))
	}
	if err != nil {
		return 0, fmt.Errorf("measuring wp-content: %w", err)
	}
	wpSize, err := parseDU(res.Stdout)
	if err != nil {
		return 0, fmt.Errorf("measuring wp-content: %w", err)
	}
	return dbSize + wpSize, nil
}

// dump writes the export's database file.
func (s *Service) dump(ctx context.Context, r *jobs.Run) error {
	e, done, err := s.jobExport(ctx, r)
	if err != nil || done {
		return err
	}
	start := s.now()
	var size int64
	switch e.Kind {
	case store.ExportSnapshot:
		var snap backup.Snapshot
		size, err = s.writeGzip(e.ID, FileDatabase, func(w io.Writer) error {
			var err error
			snap, err = s.snaps.DumpDatabase(ctx, e.SiteID, backup.SnapshotID(e.SnapshotID), w)
			return err
		})
		if err == nil {
			err = s.inspectDump(r, e.ID, snap)
		}
	default:
		size, err = s.writeDatabase(ctx, e.SiteID, e.ID)
	}
	if errors.Is(err, sitedb.ErrDatabaseNotFound) || errors.Is(err, backup.ErrSnapshotNotFound) || errors.Is(err, backup.ErrSiteNotFound) {
		return jobs.Permanent(err)
	}
	if err != nil {
		return err
	}
	r.Logf("Dumped and compressed the database: %s.", formatBytes(size))
	s.log.InfoContext(ctx, "export database written", "export_id", e.ID, "kind", e.Kind, "site_id", e.SiteID,
		"size_bytes", size, "duration", s.now().Sub(start))
	return nil
}

// inspectDump reads the site's URL and table prefix from a snapshot
// export's dump, for whoever imports it, and keeps them for mark_ready. A
// dump it can't read is still exported.
func (s *Service) inspectDump(r *jobs.Run, exportID string, snap backup.Snapshot) error {
	src := source{TakenAt: snap.Time}
	f, err := os.Open(s.filePath(exportID, FileDatabase))
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	info, err := siteimport.InspectDump(f)
	if err != nil {
		r.Warnf("The snapshot's database dump doesn't look like a WordPress database: %v", err)
	} else {
		src.SiteURL, src.TablePrefix = info.Home, info.TablePrefix
	}
	return r.Set(dataSource, src)
}

// archive writes the export's wp-content archive.
func (s *Service) archive(ctx context.Context, r *jobs.Run) error {
	e, done, err := s.jobExport(ctx, r)
	if err != nil || done {
		return err
	}
	start := s.now()
	var produce func(w io.Writer) error
	switch e.Kind {
	case store.ExportSite:
		produce = func(w io.Writer) error {
			_, err := s.tasks.RunTask(ctx, e.SiteID, provisioner.Task{
				Command:   archiveCommand(),
				WPContent: provisioner.MountReadOnly,
				Timeout:   s.timeout,
				Stdout:    w,
			})
			if ee := (*provisioner.ExecError)(nil); errors.As(err, &ee) && ee.ExitCode == tarFilesChanged {
				// GNU tar's exit status 1: files changed while they were
				// read. They are archived as they were then, which is all
				// a live site can give.
				r.Warnf("Some files changed while they were archived: %s", strings.TrimSpace(ee.Stderr))
				return nil
			}
			return err
		}
	case store.ExportSnapshot:
		produce = func(w io.Writer) error {
			_, err := s.snaps.DumpFiles(ctx, e.SiteID, backup.SnapshotID(e.SnapshotID), w)
			return err
		}
	default:
		return nil
	}
	stats, size, err := s.writeArchive(e.ID, produce)
	if errors.Is(err, provisioner.ErrSiteNotFound) || errors.Is(err, backup.ErrSnapshotNotFound) || errors.Is(err, backup.ErrSiteNotFound) {
		return jobs.Permanent(err)
	}
	if err != nil {
		return err
	}
	r.Logf("Archived and compressed wp-content: %d files, %s.", stats.Files, formatBytes(size))
	if len(stats.DropIns) > 0 {
		r.Logf("Left out the platform's drop-ins: %s.", strings.Join(stats.DropIns, ", "))
	}
	if stats.SkippedCount > 0 {
		r.Warnf("Left out %d entries that aren't regular files or directories, which site import skips too: %s.",
			stats.SkippedCount, strings.Join(stats.Skipped, ", "))
	}
	s.log.InfoContext(ctx, "export archive written", "export_id", e.ID, "kind", e.Kind, "site_id", e.SiteID,
		"files", stats.Files, "size_bytes", size, "duration", s.now().Sub(start))
	return nil
}

// tarFilesChanged is GNU tar's exit status when files changed while it
// read them.
const tarFilesChanged = 1

// archiveCommand writes the site's wp-content to standard output as a tar
// archive with paths relative to wp-content. Hard links are stored as
// files, so every file has its content.
func archiveCommand() []string {
	return []string{
		"tar", "--create", "--file=-", "--directory=" + provisioner.TaskWPContentPath,
		"--hard-dereference", "--numeric-owner", "--warning=no-file-changed", ".",
	}
}

// exportFiles are the files of an export of the kind.
func exportFiles(kind store.ExportKind) []string {
	if kind == store.ExportDatabase {
		return []string{FileDatabase}
	}
	return []string{FileWPContent, FileDatabase}
}

// markReady marks the export ready with its files and their expiry.
func (s *Service) markReady(ctx context.Context, r *jobs.Run) error {
	e, done, err := s.jobExport(ctx, r)
	if err != nil || done {
		return err
	}
	var files []store.ExportFile
	for _, name := range exportFiles(e.Kind) {
		fi, err := os.Stat(s.filePath(e.ID, name))
		if errors.Is(err, os.ErrNotExist) {
			return jobs.Permanent(fmt.Errorf("export %s has no %s", e.ID, name))
		}
		if err != nil {
			return err
		}
		files = append(files, store.ExportFile{Name: name, SizeBytes: fi.Size()})
	}
	src := e.Source
	var learned source
	if found, err := r.Get(dataSource, &learned); err != nil {
		return err
	} else if found {
		src.SiteURL, src.TablePrefix = learned.SiteURL, learned.TablePrefix
		if !learned.TakenAt.IsZero() {
			src.TakenAt = learned.TakenAt
		}
	}
	expires := s.now().Add(s.TTL(e.Kind))
	err = s.db.InTx(ctx, func(q store.Querier) error {
		_, err := store.NewExports(q).Ready(ctx, e.ID, files, src, expires)
		return err
	})
	if err != nil {
		return err
	}
	var total int64
	for _, f := range files {
		total += f.SizeBytes
	}
	r.Logf("The export is ready: %s, downloadable until %s.", formatBytes(total), expires.UTC().Format(time.RFC3339))
	return nil
}

// resultFile is one file in a finished export job's result.
type resultFile struct {
	Name        string `json:"name"`
	SizeBytes   int64  `json:"size_bytes"`
	DownloadURL string `json:"download_url"`
}

// finish puts the export in the job's result.
func (s *Service) finish(ctx context.Context, r *jobs.Run) error {
	exportID, found, err := jobExportID(r)
	if err != nil {
		return err
	}
	if !found {
		return jobs.Permanent(errors.New("the job has no export"))
	}
	e, err := store.NewExports(s.db).Get(ctx, exportID)
	if err != nil {
		return err
	}
	if e.Status != store.ExportReady && e.Status != store.ExportExpired {
		return jobs.Permanent(fmt.Errorf("export %s is %s", exportID, e.Status))
	}
	files := make([]resultFile, len(e.Files))
	for i, f := range e.Files {
		files[i] = resultFile{Name: f.Name, SizeBytes: f.SizeBytes, DownloadURL: FilePath(e.ID, f.Name)}
	}
	result := map[string]any{
		"site_id":    e.SiteID,
		"export_id":  e.ID,
		"export_url": Path(e.ID),
		"files":      files,
		"size_bytes": e.SizeBytes,
		"expires_at": e.ExpiresAt.UTC().Format(time.RFC3339),
	}
	if e.Kind == store.ExportDatabase {
		result["download_url"] = DownloadPath(e.SiteID, e.ID)
	}
	if e.BackupID != "" {
		result["backup_id"] = e.BackupID
	}
	for k, v := range result {
		if err := r.SetResult(k, v); err != nil {
			return err
		}
	}
	return nil
}

// shortSnapshot is the short form restic shows snapshot IDs in.
func shortSnapshot(snap string) string {
	if len(snap) > 8 {
		return snap[:8]
	}
	return snap
}

// formatBytes formats a size in binary units, e.g. "1.5 MiB".
func formatBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
