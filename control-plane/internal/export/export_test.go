package export_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/backup"
	"github.com/rchouinard/pressfleet/control-plane/internal/export"
	"github.com/rchouinard/pressfleet/control-plane/internal/id"
	"github.com/rchouinard/pressfleet/control-plane/internal/jobs"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner/provisionertest"
	"github.com/rchouinard/pressfleet/control-plane/internal/sitedb/sitedbtest"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// snapshots is a fake export.Snapshots holding snapshots in memory.
type snapshots struct {
	mu    sync.Mutex
	snaps map[backup.SnapshotID]snapshot
	// err, if set, fails the next dump.
	err error
}

type snapshot struct {
	backup.Snapshot
	files    []tarEntry
	database string
}

func (f *snapshots) get(siteID string, snap backup.SnapshotID) (snapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.err; err != nil {
		f.err = nil
		return snapshot{}, err
	}
	s, ok := f.snaps[snap]
	if !ok || s.SiteID != siteID {
		return snapshot{}, backup.ErrSnapshotNotFound
	}
	return s, nil
}

func (f *snapshots) DumpFiles(_ context.Context, siteID string, snap backup.SnapshotID, w io.Writer) (backup.Snapshot, error) {
	s, err := f.get(siteID, snap)
	if err != nil {
		return backup.Snapshot{}, err
	}
	_, err = w.Write(makeTar(s.files))
	return s.Snapshot, err
}

func (f *snapshots) DumpDatabase(_ context.Context, siteID string, snap backup.SnapshotID, w io.Writer) (backup.Snapshot, error) {
	s, err := f.get(siteID, snap)
	if err != nil {
		return backup.Snapshot{}, err
	}
	_, err = io.WriteString(w, s.database)
	return s.Snapshot, err
}

// sizes is a fake export.DatabaseSizer.
type sizes map[string]int64

func (s sizes) DatabaseSizes(context.Context) (map[string]int64, error) { return s, nil }

type env struct {
	db    *store.DB
	dbs   *sitedbtest.Fake
	prov  *provisionertest.Fake
	snaps *snapshots
	svc   *export.Service
	pool  *jobs.Pool
	dir   string
	clock *clock
	group string
	uids  int

	mu sync.Mutex
	// wpContent is what the tar task archives, by site.
	wpContent map[string][]tarEntry
	// free is the free space the service sees.
	free uint64
	// tarExit, if set, is the tar task's exit status after writing.
	tarExit int
}

const (
	ttl     = 10 * time.Minute
	siteTTL = 2 * time.Hour
)

func newEnv(t *testing.T, db *store.DB) *env {
	t.Helper()
	e := &env{
		db: db, dbs: sitedbtest.New(), prov: provisionertest.New(), snaps: &snapshots{snaps: map[backup.SnapshotID]snapshot{}},
		dir: filepath.Join(t.TempDir(), "exports"), clock: &clock{t: time.Now()}, wpContent: map[string][]tarEntry{}, free: 1 << 40,
	}
	e.prov.RunTaskFunc = e.runTask
	var err error
	e.svc, err = export.New(export.Options{
		DB: db, Databases: e.dbs, DatabaseSizes: sizes{}, Tasks: e.prov, Snapshots: e.snaps, Dir: e.dir,
		TTL: ttl, SiteTTL: siteTTL, Now: e.clock.now, Logger: storetest.Logger(),
		FreeSpace: func(string) (uint64, error) {
			e.mu.Lock()
			defer e.mu.Unlock()
			return e.free, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	e.pool = jobs.NewPool(db, jobs.Options{Logger: storetest.Logger(), Backoff: func(int) time.Duration { return 0 }})
	for _, h := range e.svc.Handlers() {
		if err := e.pool.Register(h); err != nil {
			t.Fatal(err)
		}
	}
	g := &store.ClientGroup{Name: "Acme"}
	if err := store.NewClientGroups(db).Create(context.Background(), g); err != nil {
		t.Fatal(err)
	}
	e.group = g.ID
	return e
}

// runTask runs the site tasks an export runs: du, and tar, which writes
// the site's wp-content as GNU tar does, ./ first.
func (e *env) runTask(siteID string, task provisioner.Task) (*provisioner.TaskResult, error) {
	e.mu.Lock()
	entries, exit := e.wpContent[siteID], e.tarExit
	e.mu.Unlock()
	if task.WPContent != provisioner.MountReadOnly {
		return nil, errors.New("wp-content must be mounted read-only")
	}
	switch task.Command[0] {
	case "du":
		return &provisioner.TaskResult{Stdout: "2048\t/site/wp-content\n"}, nil
	case "tar":
		if task.Stdout == nil {
			return nil, errors.New("tar's output must be streamed")
		}
		gnu := append([]tarEntry{{name: "./", dir: true}}, prefixed("./", entries)...)
		archive := makeTar(gnu)
		// GNU tar pads the archive to a multiple of 10 KiB.
		archive = append(archive, make([]byte, 10240-len(archive)%10240)...)
		if _, err := task.Stdout.Write(archive); err != nil {
			return nil, err
		}
		if exit != 0 {
			return nil, &provisioner.ExecError{ExitCode: exit, Stderr: "tar: ./uploads/x.jpg: file changed as we read it\n"}
		}
		return &provisioner.TaskResult{}, nil
	}
	return nil, errors.New("unexpected task " + task.Command[0])
}

// site creates a site with the given status; provisioned says whether it
// finished provisioning (has its secrets), and data is its database.
func (e *env) site(t *testing.T, status store.SiteStatus, provisioned bool, data []byte) store.Site {
	t.Helper()
	ctx := context.Background()
	s := &store.Site{
		ClientGroupID: e.group, Name: "Site", Slug: "site-" + strings.ToLower(id.New()[20:]), Status: status,
		PHPVersion: "8.4", WPCoreVersion: "6.9.1", PreviewDomain: strings.ToLower(id.New()[20:]) + ".preview.example",
		MemoryReservationMB: 256, MemoryLimitMB: 512, CPULimit: "1", TablePrefix: "acme_",
		UID: 30000 + e.uids,
	}
	e.uids++
	if provisioned {
		s.WPSaltsSecretRef = "salts"
	}
	if err := store.NewSites(e.db).Create(ctx, s); err != nil {
		t.Fatal(err)
	}
	if _, err := e.prov.CreateSite(ctx, &provisioner.SiteConfig{
		ID: s.ID, ClientGroupID: s.ClientGroupID, PreviewDomain: s.PreviewDomain, PHPVersion: s.PHPVersion,
		WPCoreVersion: s.WPCoreVersion, DBSecretRef: "db", WPSaltsSecretRef: "salts", UID: s.UID,
		Resources: provisioner.DefaultResourceLimits(),
	}); err != nil {
		t.Fatal(err)
	}
	if data != nil {
		e.dbs.SetData(s.ID, data)
	}
	return *s
}

func (e *env) runJobs(t *testing.T) {
	t.Helper()
	for range 100 {
		ran, err := e.pool.RunNext(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if !ran {
			return
		}
	}
	t.Fatal("jobs never finished")
}

func (e *env) job(t *testing.T, jobID string) store.Job {
	t.Helper()
	j, err := store.NewJobs(e.db).Get(context.Background(), jobID)
	if err != nil {
		t.Fatal(err)
	}
	return j
}

// run runs the queued job and returns it, finished.
func (e *env) run(t *testing.T, job store.Job, want store.JobState) store.Job {
	t.Helper()
	e.runJobs(t)
	done := e.job(t, job.ID)
	if done.State != want {
		t.Fatalf("job = %s, want %s: %s", done.State, want, done.Error)
	}
	return done
}

// exportID returns the ID of the export the job created.
func exportID(t *testing.T, job store.Job) string {
	t.Helper()
	var data struct {
		ExportID string `json:"export_id"`
	}
	if err := json.Unmarshal(job.Data, &data); err != nil || data.ExportID == "" {
		t.Fatalf("job data %s: %v", job.Data, err)
	}
	return data.ExportID
}

func (e *env) files(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(e.dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, en := range entries {
		names = append(names, en.Name())
	}
	return names
}

func gunzip(t *testing.T, r io.Reader) []byte {
	t.Helper()
	zr, err := gzip.NewReader(r)
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// tarEntry is an archive entry for makeTar: a file, a directory, or a
// symbolic link.
type tarEntry struct {
	name    string
	content string
	dir     bool
	link    string
}

func prefixed(prefix string, entries []tarEntry) []tarEntry {
	out := make([]tarEntry, len(entries))
	for i, en := range entries {
		en.name = prefix + en.name
		out[i] = en
	}
	return out
}

func makeTar(entries []tarEntry) []byte {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, en := range entries {
		h := &tar.Header{Name: en.name, Mode: 0o640, Uid: 70001, Gid: 70001, ModTime: time.Unix(1_700_000_000, 0)}
		switch {
		case en.dir:
			h.Typeflag, h.Mode = tar.TypeDir, 0o750
		case en.link != "":
			h.Typeflag, h.Linkname = tar.TypeSymlink, en.link
		default:
			h.Typeflag, h.Size = tar.TypeReg, int64(len(en.content))
		}
		if err := tw.WriteHeader(h); err != nil {
			panic(err)
		}
		if h.Typeflag == tar.TypeReg {
			_, _ = io.WriteString(tw, en.content)
		}
	}
	_ = tw.Close()
	return buf.Bytes()
}

// readTar reads an archive's entries: files with their contents, and
// directories with a trailing slash.
func readTar(t *testing.T, b []byte) map[string]string {
	t.Helper()
	out := map[string]string{}
	tr := tar.NewReader(bytes.NewReader(b))
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		if h.Uid != 0 || h.Gid != 0 || h.Uname != "" {
			t.Errorf("%s has an owner: %d:%d %q", h.Name, h.Uid, h.Gid, h.Uname)
		}
		switch h.Typeflag {
		case tar.TypeDir:
			out[h.Name] = ""
		case tar.TypeReg:
			c, err := io.ReadAll(tr)
			if err != nil {
				t.Fatal(err)
			}
			out[h.Name] = string(c)
		default:
			t.Errorf("unexpected entry %s of type %c", h.Name, h.Typeflag)
		}
	}
}

// wpDump is a WordPress dump with the table prefix acme_.
const wpDump = "CREATE TABLE `acme_posts` (x int);\nCREATE TABLE `acme_users` (x int);\nCREATE TABLE `acme_usermeta` (x int);\n" +
	"CREATE TABLE `acme_options` (x int);\n" +
	"INSERT INTO `acme_options` VALUES (1,'siteurl','https://acme.example','yes'),(2,'home','https://acme.example','yes'),(3,'db_version','60421','yes');\n"

// siteContent is a site's wp-content, as the tar task finds it.
var siteContent = []tarEntry{
	{name: "plugins/", dir: true},
	{name: "plugins/hello/", dir: true},
	{name: "plugins/hello/hello.php", content: "<?php // hello"},
	// Only the top-level drop-ins are the platform's.
	{name: "plugins/hello/db.php", content: "<?php // the plugin's own"},
	{name: "uploads/", dir: true},
	{name: "uploads/photo.jpg", content: "jpeg"},
	{name: "uploads/latest.jpg", link: "photo.jpg"},
	{name: "object-cache.php", content: "<?php // the platform's"},
	{name: "advanced-cache.php", content: "<?php // the platform's"},
	{name: "db.php", content: "<?php // the platform's"},
	{name: "index.php", content: "<?php // Silence is golden."},
}

// wantArchive is what an export's archive holds of siteContent.
var wantArchive = map[string]string{
	"wp-content/":                        "",
	"wp-content/plugins/":                "",
	"wp-content/plugins/hello/":          "",
	"wp-content/plugins/hello/hello.php": "<?php // hello",
	"wp-content/plugins/hello/db.php":    "<?php // the plugin's own",
	"wp-content/uploads/":                "",
	"wp-content/uploads/photo.jpg":       "jpeg",
	"wp-content/index.php":               "<?php // Silence is golden.",
}

// exportResult is a finished export job's result.
type exportResult struct {
	SiteID      string    `json:"site_id"`
	ExportID    string    `json:"export_id"`
	ExportURL   string    `json:"export_url"`
	DownloadURL string    `json:"download_url"`
	BackupID    string    `json:"backup_id"`
	SizeBytes   int64     `json:"size_bytes"`
	ExpiresAt   time.Time `json:"expires_at"`
	Estimate    *int64    `json:"estimated_size_bytes"`
	Files       []struct {
		Name        string `json:"name"`
		SizeBytes   int64  `json:"size_bytes"`
		DownloadURL string `json:"download_url"`
	} `json:"files"`
}

func result(t *testing.T, job store.Job) exportResult {
	t.Helper()
	var res exportResult
	if err := json.Unmarshal(job.Result, &res); err != nil {
		t.Fatal(err)
	}
	return res
}

// download reads an export's file.
func (e *env) download(t *testing.T, exportID, name string) (export.Download, []byte) {
	t.Helper()
	d, err := e.svc.Open(context.Background(), exportID, name)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.File.Close() }()
	return d, gunzip(t, d.File)
}

func TestExportDatabase(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		data := bytes.Repeat([]byte("INSERT INTO `wp_options` VALUES (1,'siteurl','https://example.com','yes');\n"), 100)
		site := e.site(t, store.SiteStopped, true, data)

		job, err := e.svc.ExportDatabase(ctx, site.ID, "")
		if err != nil {
			t.Fatal(err)
		}
		if job.Type != export.JobType || job.TargetType != "site" || job.TargetID != site.ID || job.LockKey != "site:"+site.ID {
			t.Errorf("job = %+v", job)
		}
		done := e.run(t, job, store.JobSucceeded)
		res := result(t, done)
		want := e.clock.now().Add(ttl).Truncate(time.Second)
		if res.SiteID != site.ID || !id.Valid(res.ExportID) || res.DownloadURL != export.DownloadPath(site.ID, res.ExportID) ||
			res.SizeBytes == 0 || !res.ExpiresAt.Equal(want) || len(res.Files) != 1 || res.Files[0].Name != export.FileDatabase ||
			res.Files[0].DownloadURL != export.FilePath(res.ExportID, export.FileDatabase) || res.Estimate != nil {
			t.Errorf("result = %s, want expiry %s", done.Result, want)
		}
		// Only the compressed dump is left, smaller than the dump.
		if files := e.files(t); !slices.Equal(files, []string{res.ExportID + ".database.sql.gz"}) {
			t.Errorf("files = %q", files)
		}
		rec, err := store.NewExports(db).Get(ctx, res.ExportID)
		if err != nil || rec.Kind != store.ExportDatabase || rec.Status != store.ExportReady || rec.SizeBytes != res.SizeBytes ||
			rec.JobID != job.ID || rec.SizeBytes >= int64(len(data)) || rec.SiteSlug != site.Slug || rec.ClientGroupID != e.group {
			t.Errorf("record = %+v, %v", rec, err)
		}

		d, err := e.svc.OpenDatabase(ctx, site.ID, res.ExportID)
		if err != nil {
			t.Fatal(err)
		}
		if d.Name != site.Slug+"-"+res.ExportID+".sql.gz" || d.Size != res.SizeBytes || d.Export.ID != res.ExportID {
			t.Errorf("download = %+v", d)
		}
		if got := gunzip(t, d.File); !bytes.Equal(got, data) {
			t.Errorf("dump = %q", got)
		}
		_ = d.File.Close()

		// Not someone else's, and only until it expires, when its file is
		// deleted on the spot.
		if _, err := e.svc.OpenDatabase(ctx, id.New(), res.ExportID); !errors.Is(err, export.ErrExportNotFound) {
			t.Errorf("OpenDatabase for another site: %v", err)
		}
		if _, err := e.svc.OpenDatabase(ctx, site.ID, "../etc"); !errors.Is(err, export.ErrExportNotFound) {
			t.Errorf("OpenDatabase with a bad ID: %v", err)
		}
		if _, err := e.svc.Open(ctx, res.ExportID, export.FileWPContent); !errors.Is(err, export.ErrExportNotFound) {
			t.Errorf("Open of a file the export doesn't have: %v", err)
		}
		e.clock.add(ttl - time.Second)
		if d, err := e.svc.OpenDatabase(ctx, site.ID, res.ExportID); err != nil {
			t.Errorf("OpenDatabase just before the expiry: %v", err)
		} else {
			_ = d.File.Close()
		}
		e.clock.add(time.Second)
		if _, err := e.svc.OpenDatabase(ctx, site.ID, res.ExportID); !errors.Is(err, export.ErrExportNotFound) {
			t.Errorf("OpenDatabase at the expiry: %v", err)
		}
		if files := e.files(t); len(files) != 0 {
			t.Errorf("files after expiry = %q", files)
		}
		if rec, err := store.NewExports(db).Get(ctx, res.ExportID); err != nil || rec.Status != store.ExportExpired {
			t.Errorf("record after expiry = %+v, %v", rec, err)
		}
	})
}

func TestExportRefused(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		for name, start := range map[string]func(context.Context, string, string) (store.Job, error){
			"database": e.svc.ExportDatabase,
			"site":     e.svc.ExportSite,
		} {
			if _, err := start(ctx, id.New(), ""); !errors.Is(err, export.ErrSiteNotFound) {
				t.Errorf("%s export of a missing site: %v", name, err)
			}
			for _, s := range []store.Site{
				e.site(t, store.SiteProvisioning, false, nil),
				e.site(t, store.SiteDeleting, true, nil),
				e.site(t, store.SiteFailed, false, nil),
			} {
				if _, err := start(ctx, s.ID, ""); !errors.Is(err, export.ErrNotExportable) {
					t.Errorf("%s export of a %s site (provisioned %v): %v", name, s.Status, s.WPSaltsSecretRef != "", err)
				}
			}
		}
	})
}

func TestExportRetriesAndRollsBack(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		site := e.site(t, store.SiteRunning, true, []byte("SELECT 1;\n"))

		// A dump that fails once is retried, into the same export.
		fails := 1
		e.dbs.Err = func(op, _ string) error {
			if op == "Dump" && fails > 0 {
				fails--
				return errors.New("server has gone away")
			}
			return nil
		}
		job, err := e.svc.ExportDatabase(ctx, site.ID, "")
		if err != nil {
			t.Fatal(err)
		}
		if done := e.run(t, job, store.JobSucceeded); done.Attempts != 2 {
			t.Errorf("job took %d attempts", done.Attempts)
		}
		var dumps int
		for _, c := range e.dbs.Calls() {
			if c.Op == "Dump" {
				dumps++
			}
		}
		if dumps != 2 || len(e.files(t)) != 1 {
			t.Errorf("%d dumps, files %q", dumps, e.files(t))
		}

		// A site whose database is gone fails for good: the export is
		// marked failed and leaves no files.
		gone := e.site(t, store.SiteRunning, true, nil)
		job, err = e.svc.ExportDatabase(ctx, gone.ID, "")
		if err != nil {
			t.Fatal(err)
		}
		id := exportID(t, e.run(t, job, store.JobFailed))
		rec, err := store.NewExports(db).Get(ctx, id)
		if err != nil || rec.Status != store.ExportFailed || rec.Message == "" {
			t.Errorf("failed export = %+v, %v", rec, err)
		}
		if files := e.files(t); len(files) != 1 {
			t.Errorf("files after the failed export = %q", files)
		}
		if _, err := e.svc.Open(ctx, id, export.FileDatabase); !errors.Is(err, export.ErrExportNotFound) {
			t.Errorf("Open of a failed export: %v", err)
		}
	})
}

func TestExportSite(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		site := e.site(t, store.SiteRunning, true, []byte(wpDump))
		e.wpContent[site.ID] = siteContent

		job, err := e.svc.ExportSite(ctx, site.ID, "")
		if err != nil {
			t.Fatal(err)
		}
		if job.Type != export.SiteJobType || job.TargetType != "site" || job.TargetID != site.ID || job.LockKey != "site:"+site.ID {
			t.Errorf("job = %+v", job)
		}
		start := e.clock.now()
		done := e.run(t, job, store.JobSucceeded)
		res := result(t, done)
		if res.SiteID != site.ID || res.ExportURL != export.Path(res.ExportID) || res.DownloadURL != "" ||
			!res.ExpiresAt.Equal(start.Add(siteTTL).Truncate(time.Second)) || len(res.Files) != 2 ||
			res.Estimate == nil || *res.Estimate != 2048*1024 {
			t.Errorf("result = %s", done.Result)
		}

		// Both files, under the names site import takes.
		rec, err := store.NewExports(db).Get(ctx, res.ExportID)
		if err != nil {
			t.Fatal(err)
		}
		if rec.Kind != store.ExportSite || rec.Status != store.ExportReady || len(rec.Files) != 2 ||
			rec.Source != (store.ExportSource{
				SiteURL: "https://" + site.PreviewDomain, TablePrefix: "acme_", WPCoreVersion: "6.9.1", PHPVersion: "8.4",
				TakenAt: start.UTC().Truncate(time.Microsecond),
			}) {
			t.Errorf("record = %+v", rec)
		}
		d, archive := e.download(t, res.ExportID, export.FileWPContent)
		if d.Name != site.Slug+"-wp-content.tar.gz" {
			t.Errorf("archive's name = %q", d.Name)
		}
		if got := readTar(t, archive); !maps.Equal(got, wantArchive) {
			t.Errorf("archive = %v\nwant %v", got, wantArchive)
		}
		d, dump := e.download(t, res.ExportID, export.FileDatabase)
		if d.Name != site.Slug+"-database.sql.gz" || string(dump) != wpDump {
			t.Errorf("dump %q = %q", d.Name, dump)
		}
		want := []string{res.ExportID + ".database.sql.gz", res.ExportID + ".wp-content.tar.gz"}
		if files := e.files(t); !slices.Equal(files, want) {
			t.Errorf("files = %q, want %q", files, want)
		}

		// The site's tasks only read: wp-content is mounted read-only.
		for _, c := range e.prov.Calls() {
			if c.Op == "RunTask" && c.Task.WPContent != provisioner.MountReadOnly {
				t.Errorf("task %q mounts wp-content %v", c.Task.Command, c.Task.WPContent)
			}
		}

		// Site exports expire like database exports, later.
		e.clock.add(siteTTL)
		if _, err := e.svc.Open(ctx, res.ExportID, export.FileWPContent); !errors.Is(err, export.ErrExportNotFound) {
			t.Errorf("Open after the expiry: %v", err)
		}
		if files := e.files(t); len(files) != 0 {
			t.Errorf("files after expiry = %q", files)
		}
	})
}

func TestExportSiteFilesChanged(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		site := e.site(t, store.SiteRunning, true, []byte(wpDump))
		e.wpContent[site.ID] = siteContent
		e.tarExit = 1 // files changed while tar read them
		job, err := e.svc.ExportSite(ctx, site.ID, "")
		if err != nil {
			t.Fatal(err)
		}
		e.run(t, job, store.JobSucceeded)
		logs, err := store.NewJobs(db).Logs(ctx, job.ID, 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.ContainsFunc(logs, func(l store.JobLog) bool { return strings.Contains(l.Message, "file changed as we read it") }) {
			t.Errorf("no warning about changed files in %+v", logs)
		}

		// Any other failure of tar fails the attempt, and is retried.
		e.tarExit = 2
		job, err = e.svc.ExportSite(ctx, site.ID, "")
		if err != nil {
			t.Fatal(err)
		}
		done := e.run(t, job, store.JobFailed)
		if rec, err := store.NewExports(db).Get(ctx, exportID(t, done)); err != nil || rec.Status != store.ExportFailed {
			t.Errorf("export = %+v, %v", rec, err)
		}
	})
}

func TestExportSiteSpace(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		site := e.site(t, store.SiteRunning, true, []byte(wpDump))
		e.wpContent[site.ID] = siteContent
		e.svc, _ = export.New(export.Options{
			DB: db, Databases: e.dbs, DatabaseSizes: sizes{site.ID: 10 << 20}, Tasks: e.prov, Dir: e.dir,
			Now: e.clock.now, Logger: storetest.Logger(),
			FreeSpace: func(string) (uint64, error) { return 10<<20 + 2047*1024, nil },
		})
		pool := jobs.NewPool(db, jobs.Options{Logger: storetest.Logger(), Backoff: func(int) time.Duration { return 0 }})
		for _, h := range e.svc.Handlers() {
			if err := pool.Register(h); err != nil {
				t.Fatal(err)
			}
		}
		e.pool = pool

		// The database's 10 MiB and wp-content's 2 MiB don't fit: the
		// export fails for good before writing anything, and says why.
		job, err := e.svc.ExportSite(ctx, site.ID, "")
		if err != nil {
			t.Fatal(err)
		}
		done := e.run(t, job, store.JobFailed)
		if done.Attempts != 1 || !strings.Contains(done.Error, export.ErrInsufficientSpace.Error()) ||
			!strings.Contains(done.Error, "12.0 MiB") {
			t.Errorf("job failed after %d attempts: %s", done.Attempts, done.Error)
		}
		if res := result(t, done); res.Estimate == nil || *res.Estimate != 12<<20 {
			t.Errorf("result = %s", done.Result)
		}
		if files := e.files(t); len(files) != 0 {
			t.Errorf("files = %q", files)
		}
		if rec, err := store.NewExports(db).Get(ctx, exportID(t, done)); err != nil || rec.Status != store.ExportFailed {
			t.Errorf("export = %+v, %v", rec, err)
		}
	})
}

// backupRecord records a succeeded backup of the site, of a snapshot the
// fake holds.
func (e *env) backupRecord(t *testing.T, siteID, slug string, files []tarEntry, database string) store.BackupRecord {
	t.Helper()
	ctx := context.Background()
	rec := store.BackupRecord{SiteID: siteID, SiteName: "Old Site", SiteSlug: slug, ClientGroupID: e.group, Kind: store.BackupManual, WPCoreVersion: "6.8.3"}
	records := store.NewBackupRecords(e.db)
	if err := records.Create(ctx, &rec); err != nil {
		t.Fatal(err)
	}
	snap := backup.SnapshotID(strings.Repeat("0", 32) + strings.ToLower(id.New())[:26] + "ffffff")
	if _, err := records.Succeed(ctx, rec.ID, store.BackupResult{SnapshotID: string(snap), SizeBytes: 4096}); err != nil {
		t.Fatal(err)
	}
	e.snaps.mu.Lock()
	e.snaps.snaps[snap] = snapshot{
		Snapshot: backup.Snapshot{ID: snap, SiteID: siteID, WPCoreVersion: "6.8.3", Time: time.Date(2026, 9, 30, 2, 0, 0, 0, time.UTC)},
		files:    files, database: database,
	}
	e.snaps.mu.Unlock()
	rec, err := records.Get(ctx, rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	return rec
}

func TestExportBackup(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		site := e.site(t, store.SiteRunning, true, []byte("-- the live database"))
		e.wpContent[site.ID] = []tarEntry{{name: "index.php", content: "<?php // the live site"}}
		// restic's archives have no ./ and no entry for wp-content itself.
		rec := e.backupRecord(t, site.ID, site.Slug, siteContent, wpDump)
		tasksBefore := len(e.prov.Calls())

		job, err := e.svc.ExportBackup(ctx, rec.ID, "")
		if err != nil {
			t.Fatal(err)
		}
		if job.Type != export.BackupJobType || job.TargetType != "site" || job.TargetID != site.ID || job.LockKey != "" {
			t.Errorf("job = %+v", job)
		}
		done := e.run(t, job, store.JobSucceeded)
		res := result(t, done)
		if res.BackupID != rec.ID || res.Estimate == nil || *res.Estimate != 4096 || len(res.Files) != 2 {
			t.Errorf("result = %s", done.Result)
		}
		got, err := store.NewExports(db).Get(ctx, res.ExportID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Kind != store.ExportSnapshot || got.BackupID != rec.ID || got.SnapshotID != rec.SnapshotID ||
			got.SiteSlug != site.Slug || got.Source != (store.ExportSource{
			SiteURL: "https://acme.example", TablePrefix: "acme_", WPCoreVersion: "6.8.3",
			TakenAt: time.Date(2026, 9, 30, 2, 0, 0, 0, time.UTC),
		}) {
			t.Errorf("export = %+v", got)
		}
		// The snapshot's content, not the live site's, and the site isn't
		// touched.
		if _, archive := e.download(t, res.ExportID, export.FileWPContent); !maps.Equal(readTar(t, archive), wantArchive) {
			t.Errorf("archive = %v", readTar(t, archive))
		}
		if _, dump := e.download(t, res.ExportID, export.FileDatabase); string(dump) != wpDump {
			t.Errorf("dump = %q", dump)
		}
		if calls := e.prov.Calls()[tasksBefore:]; len(calls) != 0 {
			t.Errorf("the export called the provisioner: %+v", calls)
		}

		// A deleted site's backup is exported under its client group, with
		// the slug its backup kept.
		gone := e.backupRecord(t, id.New(), "gone", siteContent, wpDump)
		job, err = e.svc.ExportBackup(ctx, gone.ID, "")
		if err != nil {
			t.Fatal(err)
		}
		if job.TargetType != "client_group" || job.TargetID != e.group {
			t.Errorf("job of a deleted site's backup = %+v", job)
		}
		res = result(t, e.run(t, job, store.JobSucceeded))
		if d, _ := e.download(t, res.ExportID, export.FileWPContent); d.Name != "gone-wp-content.tar.gz" {
			t.Errorf("download name = %q", d.Name)
		}

		// A snapshot whose dump isn't WordPress's is exported all the same,
		// with a warning.
		odd := e.backupRecord(t, site.ID, site.Slug, siteContent, "SELECT 1;\n")
		job, err = e.svc.ExportBackup(ctx, odd.ID, "")
		if err != nil {
			t.Fatal(err)
		}
		res = result(t, e.run(t, job, store.JobSucceeded))
		if got, err := store.NewExports(db).Get(ctx, res.ExportID); err != nil || got.Source.TablePrefix != "" || got.Source.WPCoreVersion != "6.8.3" {
			t.Errorf("export of an odd dump = %+v, %v", got, err)
		}

		// A snapshot that is gone from the repository fails for good.
		e.snaps.mu.Lock()
		delete(e.snaps.snaps, backup.SnapshotID(odd.SnapshotID))
		e.snaps.mu.Unlock()
		job, err = e.svc.ExportBackup(ctx, odd.ID, "")
		if err != nil {
			t.Fatal(err)
		}
		done = e.run(t, job, store.JobFailed)
		if done.Attempts != 1 {
			t.Errorf("job failed after %d attempts", done.Attempts)
		}

		// A failure partway is retried.
		e.snaps.err = errors.New("repository is busy")
		job, err = e.svc.ExportBackup(ctx, rec.ID, "")
		if err != nil {
			t.Fatal(err)
		}
		if done := e.run(t, job, store.JobSucceeded); done.Attempts != 2 {
			t.Errorf("job took %d attempts", done.Attempts)
		}
	})
}

func TestExportBackupRefused(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		records := store.NewBackupRecords(db)
		if _, err := e.svc.ExportBackup(ctx, id.New(), ""); !errors.Is(err, export.ErrBackupNotFound) {
			t.Errorf("missing backup: %v", err)
		}
		if _, err := e.svc.ExportBackup(ctx, "../x", ""); !errors.Is(err, export.ErrBackupNotFound) {
			t.Errorf("invalid backup ID: %v", err)
		}
		running := store.BackupRecord{SiteID: id.New(), ClientGroupID: e.group, Kind: store.BackupManual}
		if err := records.Create(ctx, &running); err != nil {
			t.Fatal(err)
		}
		failed := store.BackupRecord{SiteID: id.New(), ClientGroupID: e.group, Kind: store.BackupManual}
		if err := records.Create(ctx, &failed); err != nil {
			t.Fatal(err)
		}
		if _, err := records.Fail(ctx, failed.ID, "x"); err != nil {
			t.Fatal(err)
		}
		expired := e.backupRecord(t, id.New(), "x", nil, "")
		if _, err := records.Expire(ctx, []string{expired.ID}); err != nil {
			t.Fatal(err)
		}
		for name, backupID := range map[string]string{"running": running.ID, "failed": failed.ID, "expired": expired.ID} {
			if _, err := e.svc.ExportBackup(ctx, backupID, ""); !errors.Is(err, export.ErrBackupNotExportable) {
				t.Errorf("%s backup: %v", name, err)
			}
		}
	})
}

func TestGetAndList(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		site := e.site(t, store.SiteRunning, true, []byte(wpDump))
		e.wpContent[site.ID] = siteContent
		job, err := e.svc.ExportSite(ctx, site.ID, "")
		if err != nil {
			t.Fatal(err)
		}
		exportID := result(t, e.run(t, job, store.JobSucceeded)).ExportID

		got, err := e.svc.Get(ctx, exportID)
		if err != nil || got.Status != store.ExportReady || len(got.Files) != 2 {
			t.Errorf("Get = %+v, %v", got, err)
		}
		for _, bad := range []string{id.New(), "nope"} {
			if _, err := e.svc.Get(ctx, bad); !errors.Is(err, export.ErrExportNotFound) {
				t.Errorf("Get(%q): %v", bad, err)
			}
		}
		page, err := e.svc.List(ctx, store.ExportFilter{SiteID: site.ID})
		if err != nil || len(page.Exports) != 1 || page.Exports[0].Status != store.ExportReady {
			t.Errorf("List = %+v, %v", page, err)
		}

		// Past its expiry, an export is reported expired, and its files go.
		e.clock.add(siteTTL)
		page, err = e.svc.List(ctx, store.ExportFilter{SiteID: site.ID})
		if err != nil || len(page.Exports) != 1 || page.Exports[0].Status != store.ExportExpired {
			t.Errorf("List after the expiry = %+v, %v", page, err)
		}
		if got, err := e.svc.Get(ctx, exportID); err != nil || got.Status != store.ExportExpired {
			t.Errorf("Get after the expiry = %+v, %v", got, err)
		}
		if files := e.files(t); len(files) != 0 {
			t.Errorf("files after expiry = %q", files)
		}
	})
}

func TestSweep(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		site := e.site(t, store.SiteRunning, true, []byte("SELECT 1;\n"))
		exp := func() string {
			t.Helper()
			job, err := e.svc.ExportDatabase(ctx, site.ID, "")
			if err != nil {
				t.Fatal(err)
			}
			return result(t, e.run(t, job, store.JobSucceeded)).ExportID
		}
		old := exp()
		e.clock.add(ttl / 2)
		young := exp()

		// Files that belong to no running or ready export: a failed
		// export's leftover, one of an export that doesn't exist, and a
		// temporary file. Files that aren't an export's are left alone.
		exports := store.NewExports(db)
		failed := &store.Export{Kind: store.ExportSite, SiteID: site.ID}
		if err := exports.Create(ctx, failed); err != nil {
			t.Fatal(err)
		}
		running := &store.Export{Kind: store.ExportSite, SiteID: site.ID}
		if err := exports.Create(ctx, running); err != nil {
			t.Fatal(err)
		}
		if _, err := exports.Fail(ctx, failed.ID, "x"); err != nil {
			t.Fatal(err)
		}
		stray := id.New()
		for _, name := range []string{
			failed.ID + ".wp-content.tar.gz", stray + ".database.sql.gz", "." + stray + ".database.sql.gz.123.tmp",
			running.ID + ".database.sql", "README",
		} {
			if err := os.WriteFile(filepath.Join(e.dir, name), []byte("x"), 0o600); err != nil {
				t.Fatal(err)
			}
		}

		// The old export expires first; the young one is kept.
		e.clock.add(ttl / 2)
		n, err := e.svc.Sweep(ctx)
		if err != nil || n != 1 {
			t.Fatalf("Sweep = %d, %v", n, err)
		}
		want := []string{"README", young + ".database.sql.gz", running.ID + ".database.sql"}
		slices.Sort(want)
		if files := e.files(t); !slices.Equal(files, want) {
			t.Errorf("files = %q, want %q", files, want)
		}
		for exportID, status := range map[string]store.ExportStatus{old: store.ExportExpired, young: store.ExportReady} {
			if rec, err := exports.Get(ctx, exportID); err != nil || rec.Status != status {
				t.Errorf("export %s = %+v, %v; want %s", exportID, rec, err, status)
			}
		}

		// The cleanup job does the same; running it again changes nothing.
		e.clock.add(ttl)
		if err := export.EnsureCleanupSchedule(ctx, db); err != nil {
			t.Fatal(err)
		}
		sch, err := store.NewSchedules(db).Get(ctx, export.CleanupScheduleName)
		if err != nil {
			t.Fatal(err)
		}
		if sch.JobType != export.CleanupJobType || sch.Interval != 5*time.Minute {
			t.Errorf("schedule = %+v", sch)
		}
		for range 2 {
			job, err := jobs.Enqueue(ctx, db, jobs.Spec{Type: sch.JobType, TargetType: sch.TargetType, TargetID: sch.TargetID})
			if err != nil {
				t.Fatal(err)
			}
			e.run(t, job, store.JobSucceeded)
		}
		want = []string{"README", running.ID + ".database.sql"}
		slices.Sort(want)
		if files := e.files(t); !slices.Equal(files, want) {
			t.Errorf("files after cleanup = %q, want %q", files, want)
		}
	})
}
