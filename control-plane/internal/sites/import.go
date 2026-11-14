package sites

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/rchouinard/pressfleet/control-plane/internal/backup/sitebackup"
	"github.com/rchouinard/pressfleet/control-plane/internal/id"
	"github.com/rchouinard/pressfleet/control-plane/internal/jobs"
	"github.com/rchouinard/pressfleet/control-plane/internal/pluginupdates"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
	"github.com/rchouinard/pressfleet/control-plane/internal/secrets"
	"github.com/rchouinard/pressfleet/control-plane/internal/sitecron"
	"github.com/rchouinard/pressfleet/control-plane/internal/sitedb"
	"github.com/rchouinard/pressfleet/control-plane/internal/siteimport"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/uploads"
	"github.com/rchouinard/pressfleet/control-plane/internal/wpconfig"
)

// Importing an existing site from a wp-content archive and a SQL dump
// (#112, design doc §5.7).
//
// [Service.Import] checks the request's settings like Create does, picks
// the new site's ID, and in one transaction queues an [ImportJobType] job
// targeting that ID and claims the two uploads (package uploads) for it.
// The site itself doesn't exist yet: the archive and dump are checked
// first, and they can be gigabytes, far too much to read within an API
// request. So the job's first step does it, and only then is the site
// created; a rejected import leaves nothing behind but the failed job.
// The job's steps are:
//
//  1. validate: package siteimport reads the archive and the dump: the
//     archive must hold a wp-content directory, and the dump must be a
//     readable dump of one single-site WordPress database whose schema
//     version (db_version) the newest site image's core can run. It reads
//     the table prefix and the source URLs from the dump, and warns about
//     must-use plugins, which may belong to the previous host, and about
//     entries it skips. Its Undo runs last in a rollback (see below).
//  2. allocate: create the site as Create does (UID, slug, preview domain,
//     salts, cron and backup schedules), with status provisioning, the
//     dump's table prefix, and the newest core. Its name defaults to the
//     dump's site title. No admin password is generated: the site keeps
//     its own users.
//  3. create_database, create_object_cache, start_stack: as provisioning.
//  4. restore_content: write the archive's wp-content as a plain tar
//     without the host-specific drop-ins (siteimport.WriteTar), and extract
//     it into the site's emptied wp-content with a task running as the
//     site's user.
//  5. import_database: siteimport.SanitizeDump strips what only the
//     source's server accepts (DEFINER clauses, CREATE DATABASE and USE,
//     GTID statements), and sitedb.Manager.Import loads the result.
//  6. restart_site and upgrade_database, as a restore: `wp core
//     update-db` brings an older database up to the site's core.
//  7. rewrite_urls: `wp search-replace` from the source's siteurl and home
//     to the preview URL, http and https alike (the preview domain is
//     served over HTTPS), then set home and siteurl.
//  8. configure_plugins: the default plugin stack, as provisioning; the
//     object cache drop-in the archive's was replaced with comes from
//     here.
//  9. purge_object_cache and purge_page_cache, then finish: the site is
//     running on its preview domain, and goes live like any other.
//
// # Failure
//
// Every step is retried like provisioning's, except that rejected input
// and WP-CLI failing on the imported site are permanent. A job that fails
// for good rolls back like provisioning, removing the stack, the object
// cache user, and the database, and then deletes the site entirely:
// secrets, schedules, and the row, since nothing of an import is worth
// keeping (a site being deleted meanwhile is left to its deletion job).
// The job keeps the error. The uploads are removed when the job ends.

// ImportJobType imports a site.
const ImportJobType = "site.import"

// Step names of the site.import job, besides the provisioning, restore,
// and cache steps it shares. Keep them stable: a queued job resumes by
// step name.
const (
	stepValidateImport = "validate"
	stepAllocate       = "allocate"
	stepRestoreContent = "restore_content"
	stepImportDatabase = "import_database"
)

// dataImportSource is the job data key of what validate read.
const dataImportSource = "source"

// Sentinel errors of imports.
var (
	// ErrNoImports means the service has no uploads, task runner, or
	// temporary directory, so it can't import; a wiring bug.
	ErrNoImports = errors.New("sites: imports need an upload service, a task runner, and a temporary directory")
	// ErrUploadNotReady means an upload an import names isn't complete or
	// is in use by another job.
	ErrUploadNotReady = errors.New("upload not ready")
)

// Import is a request to import a site.
type Import struct {
	ClientGroupID string
	// ArchiveUploadID and DumpUploadID are complete uploads for a site
	// import into the client group, made by CreatedByUserID.
	ArchiveUploadID string
	DumpUploadID    string
	// Name defaults to the imported site's title.
	Name string
	// PHPVersion and the resources are as for NewSite.
	PHPVersion          string
	CPU                 *string
	MemoryReservationMB *int
	MemoryLimitMB       *int
	// CreatedByUserID is the user starting the job.
	CreatedByUserID string
}

// importPayload is the payload of a site.import job: the request with its
// defaults resolved.
type importPayload struct {
	Name                string `json:"name,omitempty"`
	PHPVersion          string `json:"php_version"`
	CPU                 string `json:"cpu"`
	MemoryReservationMB int    `json:"memory_reservation_mb"`
	MemoryLimitMB       int    `json:"memory_limit_mb"`
	ArchiveUploadID     string `json:"archive_upload_id"`
	DumpUploadID        string `json:"dump_upload_id"`
}

// importSource is what the validate step read, for the later steps.
type importSource struct {
	Archive siteimport.ArchiveInfo `json:"archive"`
	Dump    siteimport.DumpInfo    `json:"dump"`
}

func (s *Service) imports() bool { return s.uploads != nil && s.tasks != nil && s.tmpDir != "" }

// Import checks the request and queues a [ImportJobType] job that creates
// the site and imports the uploads into it. It returns the ID the site will
// have and the job; the site appears once the job has checked the uploads.
// Invalid settings, an unknown client group, and uploads that don't exist,
// aren't the caller's, or were made for something else are a
// *ValidationError; uploads that aren't complete or are taken by another
// job are ErrUploadNotReady. Nothing changes then.
func (s *Service) Import(ctx context.Context, req Import) (string, store.Job, error) {
	if !s.imports() {
		return "", store.Job{}, ErrNoImports
	}
	if s.previewBase == "" {
		return "", store.Job{}, ErrNoPreviewDomain
	}
	name := strings.TrimSpace(req.Name)
	probe := NewSite{
		Name: name, PHPVersion: req.PHPVersion,
		CPU: req.CPU, MemoryReservationMB: req.MemoryReservationMB, MemoryLimitMB: req.MemoryLimitMB,
	}
	if probe.Name == "" {
		probe.Name = "imported" // the job names it after the imported site
	}
	site, err := s.validate(probe)
	var fields []FieldError
	var ve *ValidationError
	if errors.As(err, &ve) {
		fields = ve.Fields
	} else if err != nil {
		return "", store.Job{}, err
	}
	if req.ArchiveUploadID != "" && req.ArchiveUploadID == req.DumpUploadID {
		fields = append(fields, FieldError{"/dump_upload_id", "must be another upload than archive_upload_id"})
	}
	if len(fields) > 0 {
		return "", store.Job{}, &ValidationError{Fields: fields}
	}

	siteID := id.New()
	var job store.Job
	err = s.db.InTx(ctx, func(q store.Querier) error {
		if _, err := store.NewClientGroups(q).Get(ctx, req.ClientGroupID); errors.Is(err, store.ErrNotFound) {
			return &ValidationError{Fields: []FieldError{{"/client_group_id", "client group does not exist"}}}
		} else if err != nil {
			return err
		}
		var err error
		job, err = jobs.Enqueue(ctx, q, jobs.Spec{
			Type:       ImportJobType,
			TargetType: store.JobTargetSite,
			TargetID:   siteID,
			Payload: importPayload{
				Name: name, PHPVersion: site.PHPVersion, CPU: site.CPULimit,
				MemoryReservationMB: site.MemoryReservationMB, MemoryLimitMB: site.MemoryLimitMB,
				ArchiveUploadID: req.ArchiveUploadID, DumpUploadID: req.DumpUploadID,
			},
			LockKey:         store.SiteLockKey(siteID),
			CreatedByUserID: req.CreatedByUserID,
		})
		if err != nil {
			return err
		}
		use := uploads.Use{Purpose: store.UploadSiteImport, ClientGroupID: req.ClientGroupID, UserID: req.CreatedByUserID}
		for _, u := range []struct{ field, id string }{
			{"/archive_upload_id", req.ArchiveUploadID}, {"/dump_upload_id", req.DumpUploadID},
		} {
			if _, err := s.uploads.Claim(ctx, q, u.id, use, job.ID); err != nil {
				return uploadError(u.field, err)
			}
		}
		return nil
	})
	if err != nil {
		return "", store.Job{}, err
	}
	s.log.InfoContext(ctx, "site import queued", "site_id", siteID, "client_group_id", req.ClientGroupID, "job_id", job.ID)
	return siteID, job, nil
}

// uploadError explains why an upload can't be claimed.
func uploadError(field string, err error) error {
	switch {
	case errors.Is(err, uploads.ErrNotFound):
		return &ValidationError{Fields: []FieldError{{field, "no upload of yours has this ID"}}}
	case errors.Is(err, uploads.ErrWrongUpload):
		return &ValidationError{Fields: []FieldError{{field, err.Error()}}}
	case errors.Is(err, uploads.ErrNotComplete), errors.Is(err, uploads.ErrInUse):
		return fmt.Errorf("%w: %s: %w", ErrUploadNotReady, strings.TrimPrefix(field, "/"), err)
	}
	return err
}

func (s *Service) importHandler() jobs.Handler {
	steps := []jobs.Step{
		{Name: stepValidateImport, Do: s.validateImport, Undo: s.failImport},
		{Name: stepAllocate, Do: s.allocateImport},
		{Name: stepCreateDatabase, Do: s.createDatabase, Undo: s.dropDatabase},
		{Name: stepCreateCache, Do: s.createObjectCache, Undo: s.deleteObjectCache},
		{Name: stepStartStack, Do: s.startStack, Undo: s.deleteStack},
		{Name: stepRestoreContent, Do: s.importContent},
		{Name: stepImportDatabase, Do: s.importDatabase},
		{Name: stepRestartSite, Do: s.restartRestoredSite},
		{Name: stepUpgradeDatabase, Do: s.upgradeRestoredDatabase},
		{Name: stepRewriteURLs, Do: s.rewriteImportedURLs},
		{Name: stepConfigurePlugins, Do: s.configureCreatedSitePlugins},
	}
	steps = append(steps, s.cacheSteps()...)
	steps = append(steps, jobs.Step{Name: stepFinish, Do: s.finishImport})
	return jobs.Handler{Type: ImportJobType, Steps: steps}
}

func importJobPayload(r *jobs.Run) (importPayload, error) {
	var p importPayload
	if err := r.Payload(&p); err != nil {
		return importPayload{}, err
	}
	if !id.Valid(p.ArchiveUploadID) || !id.Valid(p.DumpUploadID) || !id.Valid(r.Job().TargetID) {
		return importPayload{}, jobs.Permanent(errors.New("the import job's payload names no uploads or site"))
	}
	return p, nil
}

// openUpload opens an upload the job claimed. A missing or unclaimed
// upload is permanent: it won't come back.
func (s *Service) openUpload(ctx context.Context, r *jobs.Run, uploadID, what string) (*os.File, error) {
	f, _, err := s.uploads.Open(ctx, uploadID, r.Job().ID)
	if errors.Is(err, uploads.ErrNotFound) || errors.Is(err, uploads.ErrNotClaimed) {
		return nil, jobs.Permanent(fmt.Errorf("the %s upload %s: %w", what, uploadID, err))
	}
	if err != nil {
		return nil, fmt.Errorf("opening the %s: %w", what, err)
	}
	return f, nil
}

// rejected makes siteimport's rejections permanent.
func rejected(err error) error {
	if errors.Is(err, siteimport.ErrInvalidArchive) || errors.Is(err, siteimport.ErrInvalidDump) {
		return jobs.Permanent(err)
	}
	return err
}

// validateImport checks the archive and the dump before anything exists.
func (s *Service) validateImport(ctx context.Context, r *jobs.Run) error {
	p, err := importJobPayload(r)
	if err != nil {
		return err
	}
	archive, err := s.openUpload(ctx, r, p.ArchiveUploadID, "wp-content archive")
	if err != nil {
		return err
	}
	ai, err := siteimport.InspectArchive(archive)
	_ = archive.Close()
	if err != nil {
		return rejected(err)
	}
	dump, err := s.openUpload(ctx, r, p.DumpUploadID, "SQL dump")
	if err != nil {
		return err
	}
	di, err := siteimport.InspectDump(dump)
	_ = dump.Close()
	if err != nil {
		return rejected(err)
	}
	latest := s.catalog.LatestWordPress()
	switch {
	case latest.DBVersion == 0:
		r.Warnf("The image catalog doesn't know WordPress %s's database version, so the dump's (%d) wasn't checked.", latest.Version, di.DBVersion)
	case di.DBVersion > latest.DBVersion:
		return jobs.Permanent(fmt.Errorf("%w: its database is from a WordPress core newer than the newest site image's, "+
			"WordPress %s (database version %d, which WordPress %s has as %d); import it once site images of that core are available",
			siteimport.ErrInvalidDump, latest.Version, di.DBVersion, latest.Version, latest.DBVersion))
	}

	r.Logf("The archive (%s) holds wp-content at %s: %d files, %s.", ai.Format, rootName(ai.Root), ai.Files, sizeName(ai.Bytes))
	r.Logf("The dump is of %s (table prefix %s, %d tables, database version %d).", di.Home, di.TablePrefix, di.Tables, di.DBVersion)
	for _, m := range ai.MUPlugins {
		if host := hostOf(m); host != "" {
			r.Warnf("Must-use plugin %q is %s's; it was kept, but likely needs that host. Remove it from wp-content/mu-plugins if the site misbehaves.", m, host)
		} else {
			r.Warnf("Must-use plugin %q was kept; check that it doesn't depend on the site's previous host.", m)
		}
	}
	if ai.SkippedCount > 0 {
		r.Warnf("Skipped %d archive entries that aren't regular files or directories (symbolic links and the like): %s.",
			ai.SkippedCount, strings.Join(ai.Skipped, ", "))
	}
	if di.Stylesheet != "" && !slices.Contains(ai.Themes, di.Stylesheet) {
		r.Warnf("The active theme %q isn't in the archive's wp-content/themes; WordPress will fall back to another theme.", di.Stylesheet)
	}
	return r.Set(dataImportSource, importSource{Archive: ai, Dump: di})
}

func rootName(root string) string {
	if root == "" {
		return "its top"
	}
	return root + "/"
}

func sizeName(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	}
	return fmt.Sprintf("%d KiB", (n+1023)/1024)
}

// hostMUPlugins name the hosts whose must-use plugins an import commonly
// brings along, by the plugin's file or directory name prefix.
var hostMUPlugins = []struct{ prefix, host string }{
	{"wpengine", "WP Engine"}, {"wpe-", "WP Engine"}, {"mu-plugin.php", "WP Engine"}, {"slt-force-strong-passwords", "WP Engine"},
	{"kinsta", "Kinsta"},
	{"pantheon", "Pantheon"},
	{"gd-system-plugin", "GoDaddy"}, {"wpaas", "GoDaddy"},
	{"endurance-", "Bluehost"}, {"sso.php", "Bluehost"},
	{"wpcomsh", "WordPress.com"}, {"jetpack-mu-wpcom", "WordPress.com"},
	{"pressable", "Pressable"},
	{"flywheel", "Flywheel"},
	{"liquidweb", "Liquid Web"}, {"mwp-", "Liquid Web"},
	{"sg-", "SiteGround"},
	{"wp-stack-cache", "Pagely"}, {"pagely", "Pagely"},
}

// hostOf names the host a must-use plugin belongs to, or "".
func hostOf(muPlugin string) string {
	name := strings.ToLower(muPlugin)
	for _, h := range hostMUPlugins {
		if strings.HasPrefix(name, h.prefix) {
			return h.host
		}
	}
	return ""
}

// source returns what validate read.
func source(r *jobs.Run) (importSource, error) {
	var src importSource
	found, err := r.Get(dataImportSource, &src)
	if err != nil {
		return importSource{}, err
	}
	if !found || src.Dump.TablePrefix == "" {
		return importSource{}, jobs.Permanent(errors.New("the import job has no validated source"))
	}
	return src, nil
}

// maxImportedName bounds the site name taken from the imported site.
const maxImportedName = 100

// allocateImport creates the site, unless an earlier attempt did.
func (s *Service) allocateImport(ctx context.Context, r *jobs.Run) error {
	siteID := r.Job().TargetID
	if site, err := store.NewSites(s.db).Get(ctx, siteID); err == nil {
		r.Logf("The site %s exists already.", site.Slug)
		return nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return err
	}
	p, err := importJobPayload(r)
	if err != nil {
		return err
	}
	src, err := source(r)
	if err != nil {
		return err
	}
	name := p.Name
	if name == "" {
		name = strings.TrimSpace(src.Dump.BlogName)
	}
	if name == "" {
		if u, err := url.Parse(src.Dump.Home); err == nil {
			name = u.Host
		}
	}
	if runes := []rune(name); len(runes) > maxImportedName {
		name = string(runes[:maxImportedName])
	}
	site := store.Site{
		ID:                  siteID,
		Name:                name,
		Status:              store.SiteProvisioning,
		PHPVersion:          p.PHPVersion,
		WPCoreVersion:       s.catalog.LatestWordPress().Version,
		EnableCache:         true,
		CPULimit:            p.CPU,
		MemoryReservationMB: p.MemoryReservationMB,
		MemoryLimitMB:       p.MemoryLimitMB,
		TablePrefix:         src.Dump.TablePrefix,
	}
	group, err := s.importGroup(ctx, r)
	if err != nil {
		return err
	}
	site.ClientGroupID = group
	for attempt := 1; ; attempt++ {
		var created store.Site
		created, err = s.createImported(ctx, site)
		if errors.Is(err, store.ErrConflict) && attempt < createAttempts {
			continue
		}
		if errors.Is(err, errNoClientGroup) || errors.Is(err, store.ErrRepositoryDeleting) {
			return jobs.Permanent(err)
		}
		if err != nil {
			return err
		}
		r.Logf("Created site %s at %s (PHP %s, WordPress %s).", created.Slug, created.PreviewDomain, created.PHPVersion, created.WPCoreVersion)
		return nil
	}
}

// importGroup returns the client group the uploads were made for.
func (s *Service) importGroup(ctx context.Context, r *jobs.Run) (string, error) {
	p, err := importJobPayload(r)
	if err != nil {
		return "", err
	}
	u, err := s.uploads.Claimed(ctx, p.ArchiveUploadID, r.Job().ID)
	if errors.Is(err, uploads.ErrNotFound) || errors.Is(err, uploads.ErrNotClaimed) {
		return "", jobs.Permanent(fmt.Errorf("the wp-content archive upload %s: %w", p.ArchiveUploadID, err))
	}
	if err != nil {
		return "", err
	}
	return u.ClientGroupID, nil
}

// createImported runs the transaction that creates an imported site once.
func (s *Service) createImported(ctx context.Context, site store.Site) (store.Site, error) {
	err := s.db.InTx(ctx, func(q store.Querier) error {
		if _, err := store.NewClientGroups(q).Get(ctx, site.ClientGroupID); errors.Is(err, store.ErrNotFound) {
			return errNoClientGroup
		} else if err != nil {
			return err
		}
		uid, err := store.AllocateSiteUID(ctx, q)
		if err != nil {
			return err
		}
		sites := store.NewSites(q)
		slug, err := uniqueSlug(ctx, sites, Slugify(site.Name), s.previewBase)
		if err != nil {
			return err
		}
		salts, err := wpconfig.MarshalSalts(wpconfig.GenerateSalts())
		if err != nil {
			return err
		}
		saltsRef, err := s.secrets.With(q).Put(ctx, secrets.KindSiteWPSalts, salts)
		if err != nil {
			return err
		}
		site.UID, site.Slug, site.PreviewDomain = uid, slug, previewDomain(slug, s.previewBase)
		site.WPSaltsSecretRef = string(saltsRef)
		if err := sites.Create(ctx, &site); errors.Is(err, store.ErrReference) {
			return errNoClientGroup
		} else if err != nil {
			return err
		}
		if err := sitecron.EnsureSchedule(ctx, q, site.ID); err != nil {
			return err
		}
		if err := pluginupdates.EnsureSchedule(ctx, q, site.ID); err != nil {
			return err
		}
		return sitebackup.EnsureSchedule(ctx, q, site.ID, s.backupWindow)
	})
	return site, err
}

// importTransferDir is the directory the archive's tar is handed to the
// extracting task in; named after the site, so a retry replaces what a
// crashed attempt left.
func (s *Service) importTransferDir(siteID string) string {
	return filepath.Join(s.tmpDir, "import-"+siteID)
}

// importTar is the tar's name in the transfer directory.
const importTar = "wp-content.tar"

// extractScript empties the site's wp-content (the image seeds a new site
// with WordPress's bundled themes) and extracts the imported one, as the
// site's user, so the files are the site's.
var extractScript = "set -eu\n" +
	"find " + provisioner.TaskWPContentPath + " -mindepth 1 -delete\n" +
	"tar -x -f " + provisioner.TaskTransferPath + "/" + importTar + " -C " + provisioner.TaskWPContentPath +
	" --no-same-owner --no-same-permissions\n"

// importContent replaces the site's wp-content with the archive's.
func (s *Service) importContent(ctx context.Context, r *jobs.Run) error {
	site, err := s.jobSite(ctx, r)
	if err != nil {
		return err
	}
	if err := abortIfDeleting(site); err != nil {
		return err
	}
	p, err := importJobPayload(r)
	if err != nil {
		return err
	}
	src, err := source(r)
	if err != nil {
		return err
	}
	dir := s.importTransferDir(site.ID)
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("removing an earlier attempt's transfer directory: %w", err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("creating the transfer directory: %w", err)
	}
	defer func() {
		if err := os.RemoveAll(dir); err != nil {
			s.log.ErrorContext(ctx, "removing an import transfer directory failed", "site_id", site.ID, "dir", dir, "error", err)
		}
	}()
	archive, err := s.openUpload(ctx, r, p.ArchiveUploadID, "wp-content archive")
	if err != nil {
		return err
	}
	err = writeFile(filepath.Join(dir, importTar), func(w io.Writer) error {
		return siteimport.WriteTar(w, archive, src.Archive)
	})
	_ = archive.Close()
	if err != nil {
		return rejected(fmt.Errorf("preparing wp-content: %w", err))
	}
	_, err = s.tasks.RunTask(ctx, site.ID, provisioner.Task{
		Command:     []string{"sh", "-c", extractScript},
		WPContent:   provisioner.MountReadWrite,
		TransferDir: dir,
	})
	if errors.Is(err, provisioner.ErrSiteNotFound) {
		return jobs.Permanent(err)
	}
	if err != nil {
		return fmt.Errorf("extracting wp-content: %w", err)
	}
	r.Logf("Imported wp-content: %d files, %s.", src.Archive.Files, sizeName(src.Archive.Bytes))
	if len(src.Archive.DropIns) > 0 {
		r.Logf("Left out the previous host's drop-ins (%s); the platform provides its own.", strings.Join(src.Archive.DropIns, ", "))
	}
	return nil
}

// writeFile writes a file with fn, removing it if fn fails.
func writeFile(path string, fn func(io.Writer) error) (err error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600) //nolint:gosec // G304: a path under the data directory
	if err != nil {
		return err
	}
	defer func() {
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			_ = os.Remove(path)
		}
	}()
	return fn(f)
}

// importDatabase loads the sanitized dump into the site's database.
func (s *Service) importDatabase(ctx context.Context, r *jobs.Run) error {
	site, err := s.jobSite(ctx, r)
	if err != nil {
		return err
	}
	if err := abortIfDeleting(site); err != nil {
		return err
	}
	p, err := importJobPayload(r)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(s.tmpDir, 0o700); err != nil {
		return fmt.Errorf("creating the temporary directory: %w", err)
	}
	path := filepath.Join(s.tmpDir, "import-"+site.ID+".sql")
	defer func() {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			s.log.ErrorContext(ctx, "removing an import dump failed", "site_id", site.ID, "path", path, "error", err)
		}
	}()
	dump, err := s.openUpload(ctx, r, p.DumpUploadID, "SQL dump")
	if err != nil {
		return err
	}
	var stats siteimport.SanitizeStats
	err = writeFile(path, func(w io.Writer) error {
		var err error
		stats, err = siteimport.SanitizeDump(w, dump)
		return err
	})
	_ = dump.Close()
	if err != nil {
		return rejected(fmt.Errorf("preparing the dump: %w", err))
	}
	if stats.Definers > 0 || stats.Statements > 0 {
		r.Logf("Removed %d DEFINER clauses and %d statements that only the source's database server accepts.", stats.Definers, stats.Statements)
	}
	if err := s.databases.Import(ctx, site.ID, path); err != nil {
		if errors.Is(err, sitedb.ErrDatabaseNotFound) {
			return jobs.Permanent(err)
		}
		return fmt.Errorf("importing the dump: %w", err)
	}
	r.Logf("Imported the database.")
	return nil
}

// urlReplacements are the search-replace pairs that move a source URL to
// target (https://<host>): both schemes to https, then protocol-relative
// ones, each also in its JSON-escaped form (\/\/), which WordPress stores
// in block attributes and some plugins' settings.
func urlReplacements(from, target string) [][2]string {
	u, err := url.Parse(from)
	if err != nil || u.Host == "" {
		return nil
	}
	hostPath := strings.ToLower(u.Host) + strings.TrimRight(u.EscapedPath(), "/")
	targetHost := strings.TrimPrefix(target, "https://")
	if hostPath == targetHost {
		return nil
	}
	var pairs [][2]string
	for _, esc := range []func(string) string{
		func(s string) string { return s },
		func(s string) string { return strings.ReplaceAll(s, "/", `\/`) },
	} {
		pairs = append(pairs,
			[2]string{esc("https://" + hostPath), esc(target)},
			[2]string{esc("http://" + hostPath), esc(target)},
			[2]string{esc("//" + hostPath), esc("//" + targetHost)},
		)
	}
	return pairs
}

// rewriteImportedURLs moves the imported database's URLs to the site's
// preview URL.
func (s *Service) rewriteImportedURLs(ctx context.Context, r *jobs.Run) error {
	site, err := s.managedSite(ctx, r)
	if err != nil {
		return err
	}
	src, err := source(r)
	if err != nil {
		return err
	}
	target := URL(site)
	sources := []string{src.Dump.SiteURL, src.Dump.Home}
	// The longer URL first: a siteurl in a subdirectory of home must be
	// replaced before home would turn it into target/<dir>.
	slices.SortStableFunc(sources, func(a, b string) int { return len(b) - len(a) })
	if err := s.caches.FlushCache(ctx, site.ID); err != nil {
		return fmt.Errorf("flushing the object cache: %w", err)
	}
	var done [][2]string
	for _, from := range sources {
		for _, pair := range urlReplacements(from, target) {
			if slices.Contains(done, pair) {
				continue
			}
			out, err := s.wpcliRestored(ctx, site.ID, "search-replace", pair[0], pair[1],
				"--all-tables-with-prefix", "--skip-columns=guid", "--format=count")
			if err != nil {
				return fmt.Errorf("replacing %s with %s: %w", pair[0], pair[1], err)
			}
			done = append(done, pair)
			r.Logf("Replaced %s with %s: %s.", pair[0], pair[1], countOf(out))
		}
	}
	if err := s.caches.FlushCache(ctx, site.ID); err != nil {
		return fmt.Errorf("flushing the object cache: %w", err)
	}
	for _, option := range []string{"home", "siteurl"} {
		if _, err := s.wpcliRestored(ctx, site.ID, "option", "update", option, target); err != nil {
			return fmt.Errorf("setting %s: %w", option, err)
		}
	}
	r.Logf("WordPress's address is %s.", target)
	return nil
}

// finishImport marks the site running, records the import in the job's
// result, and removes the uploads.
func (s *Service) finishImport(ctx context.Context, r *jobs.Run) error {
	site, err := s.jobSite(ctx, r)
	if err != nil {
		return err
	}
	src, err := source(r)
	if err != nil {
		return err
	}
	changed, err := store.NewSites(s.db).SetStatus(ctx, site.ID, store.SiteRunning, "", store.SiteProvisioning)
	if err != nil {
		return err
	}
	for k, v := range map[string]string{"site_id": site.ID, "source_url": src.Dump.Home, "table_prefix": src.Dump.TablePrefix} {
		if err := r.SetResult(k, v); err != nil {
			return err
		}
	}
	s.releaseUploads(ctx, r)
	if !changed {
		r.Warnf("The site is %s now; leaving its status alone.", site.Status)
		return nil
	}
	r.Logf("The imported site is ready at %s.", URL(site))
	return nil
}

// releaseUploads removes the import's uploads once the job is done with
// them. It is best effort: the uploads cleanup job removes them anyway
// when the job ends.
func (s *Service) releaseUploads(ctx context.Context, r *jobs.Run) {
	p, err := importJobPayload(r)
	if err != nil {
		return
	}
	for _, u := range []string{p.ArchiveUploadID, p.DumpUploadID} {
		if err := s.uploads.Release(ctx, u, r.Job().ID); err != nil {
			s.log.WarnContext(ctx, "removing an import's upload failed", "upload_id", u, "job_id", r.Job().ID, "error", err)
		}
	}
}

// failImport is the last Undo of a failed site.import job: the steps after
// it removed the stack, the object cache user, and the database. It
// deletes what is left of the site: its secrets, schedules, and row. A site
// that a deletion is waiting for is left to it.
func (s *Service) failImport(ctx context.Context, r *jobs.Run) error {
	siteID := r.Job().TargetID
	for _, p := range []string{s.importTransferDir(siteID), filepath.Join(s.tmpDir, "import-"+siteID+".sql")} {
		if err := os.RemoveAll(p); err != nil {
			return err
		}
	}
	deleted, deleting := false, false
	err := s.db.InTx(ctx, func(q store.Querier) error {
		sites := store.NewSites(q)
		site, err := sites.Get(ctx, siteID)
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if site.Status != store.SiteProvisioning {
			deleting = true
			return nil
		}
		if err := s.deleteSiteSecrets(ctx, q, site); err != nil {
			return err
		}
		if err := sitecron.RemoveSchedule(ctx, q, siteID); err != nil {
			return err
		}
		if err := pluginupdates.RemoveSchedule(ctx, q, siteID); err != nil {
			return err
		}
		if err := sitebackup.RemoveSchedule(ctx, q, siteID); err != nil {
			return err
		}
		if _, err := jobs.DeleteTargetSchedules(ctx, q, "site", siteID); err != nil {
			return err
		}
		deleted, err = sites.Delete(ctx, siteID)
		return err
	})
	if err != nil {
		return err
	}
	s.releaseUploads(ctx, r)
	switch {
	case deleted:
		r.Logf("Deleted the imported site; nothing of it is left.")
	case deleting:
		r.Logf("The site is being deleted; its deletion removes the rest.")
	default:
		r.Logf("No site was created.")
	}
	return nil
}
