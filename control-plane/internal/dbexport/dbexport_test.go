package dbexport_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/dbexport"
	"github.com/rchouinard/pressfleet/control-plane/internal/id"
	"github.com/rchouinard/pressfleet/control-plane/internal/jobs"
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

type env struct {
	db    *store.DB
	dbs   *sitedbtest.Fake
	svc   *dbexport.Service
	pool  *jobs.Pool
	dir   string
	clock *clock
	group string
	uids  int
}

const ttl = 10 * time.Minute

func newEnv(t *testing.T, db *store.DB) *env {
	t.Helper()
	e := &env{db: db, dbs: sitedbtest.New(), dir: filepath.Join(t.TempDir(), "exports"), clock: &clock{t: time.Now()}}
	var err error
	e.svc, err = dbexport.New(dbexport.Options{DB: db, Databases: e.dbs, Dir: e.dir, TTL: ttl, Now: e.clock.now, Logger: storetest.Logger()})
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

// site creates a site with the given status; provisioned says whether it
// finished provisioning (has its secrets), and data is its database.
func (e *env) site(t *testing.T, status store.SiteStatus, provisioned bool, data []byte) store.Site {
	t.Helper()
	s := &store.Site{
		ClientGroupID: e.group, Name: "Site", Slug: "site-" + id.New()[20:], Status: status,
		PHPVersion: "8.4", WPCoreVersion: "6.9.1", PreviewDomain: id.New()[20:] + ".preview.example",
		MemoryReservationMB: 256, MemoryLimitMB: 512, CPULimit: "1",
		UID: 70000 + e.uids,
	}
	e.uids++
	if provisioned {
		s.WPSaltsSecretRef = "salts"
	}
	if err := store.NewSites(e.db).Create(context.Background(), s); err != nil {
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

func TestExport(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		data := bytes.Repeat([]byte("INSERT INTO `wp_options` VALUES (1,'siteurl','https://example.com','yes');\n"), 100)
		site := e.site(t, store.SiteStopped, true, data)

		job, err := e.svc.Export(ctx, site.ID, "")
		if err != nil {
			t.Fatal(err)
		}
		if job.Type != dbexport.JobType || job.TargetType != "site" || job.TargetID != site.ID || job.LockKey != "site:"+site.ID {
			t.Errorf("job = %+v", job)
		}
		e.runJobs(t)
		done := e.job(t, job.ID)
		if done.State != store.JobSucceeded {
			t.Fatalf("job = %s: %s", done.State, done.Error)
		}
		var res struct {
			SiteID      string    `json:"site_id"`
			ExportID    string    `json:"export_id"`
			DownloadURL string    `json:"download_url"`
			SizeBytes   int64     `json:"size_bytes"`
			ExpiresAt   time.Time `json:"expires_at"`
		}
		if err := json.Unmarshal(done.Result, &res); err != nil {
			t.Fatal(err)
		}
		want := e.clock.now().Add(ttl).Truncate(time.Second)
		if res.SiteID != site.ID || !id.Valid(res.ExportID) || res.DownloadURL != dbexport.DownloadPath(site.ID, res.ExportID) ||
			res.SizeBytes == 0 || !res.ExpiresAt.Equal(want) {
			t.Errorf("result = %s, want expiry %s", done.Result, want)
		}
		// Only the compressed dump is left, smaller than the dump.
		if files := e.files(t); !slices.Equal(files, []string{dbexport.FileName(res.ExportID)}) {
			t.Errorf("files = %q", files)
		}
		rec, err := store.NewDatabaseExports(db).Get(ctx, res.ExportID)
		if err != nil || rec.Status != store.DatabaseExportReady || rec.SizeBytes != res.SizeBytes || rec.JobID != job.ID ||
			rec.SizeBytes >= int64(len(data)) {
			t.Errorf("record = %+v, %v", rec, err)
		}

		d, err := e.svc.Open(ctx, site.ID, res.ExportID)
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
		if _, err := e.svc.Open(ctx, id.New(), res.ExportID); !errors.Is(err, dbexport.ErrExportNotFound) {
			t.Errorf("Open for another site: %v", err)
		}
		if _, err := e.svc.Open(ctx, site.ID, "../etc"); !errors.Is(err, dbexport.ErrExportNotFound) {
			t.Errorf("Open with a bad ID: %v", err)
		}
		e.clock.add(ttl - time.Second)
		if d, err := e.svc.Open(ctx, site.ID, res.ExportID); err != nil {
			t.Errorf("Open just before the expiry: %v", err)
		} else {
			_ = d.File.Close()
		}
		e.clock.add(time.Second)
		if _, err := e.svc.Open(ctx, site.ID, res.ExportID); !errors.Is(err, dbexport.ErrExportNotFound) {
			t.Errorf("Open at the expiry: %v", err)
		}
		if files := e.files(t); len(files) != 0 {
			t.Errorf("files after expiry = %q", files)
		}
		if rec, err := store.NewDatabaseExports(db).Get(ctx, res.ExportID); err != nil || rec.Status != store.DatabaseExportExpired {
			t.Errorf("record after expiry = %+v, %v", rec, err)
		}
	})
}

func TestExportRefused(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		if _, err := e.svc.Export(ctx, id.New(), ""); !errors.Is(err, dbexport.ErrSiteNotFound) {
			t.Errorf("missing site: %v", err)
		}
		for _, s := range []store.Site{
			e.site(t, store.SiteProvisioning, false, nil),
			e.site(t, store.SiteDeleting, true, nil),
			e.site(t, store.SiteFailed, false, nil),
		} {
			if _, err := e.svc.Export(ctx, s.ID, ""); !errors.Is(err, dbexport.ErrNotExportable) {
				t.Errorf("%s site (provisioned %v): %v", s.Status, s.WPSaltsSecretRef != "", err)
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
		job, err := e.svc.Export(ctx, site.ID, "")
		if err != nil {
			t.Fatal(err)
		}
		e.runJobs(t)
		if done := e.job(t, job.ID); done.State != store.JobSucceeded || done.Attempts != 2 {
			t.Fatalf("job = %s after %d attempts: %s", done.State, done.Attempts, done.Error)
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
		job, err = e.svc.Export(ctx, gone.ID, "")
		if err != nil {
			t.Fatal(err)
		}
		e.runJobs(t)
		done := e.job(t, job.ID)
		if done.State != store.JobFailed {
			t.Fatalf("job = %s", done.State)
		}
		var exportID string
		if err := json.Unmarshal(done.Data, &struct {
			ExportID *string `json:"export_id"`
		}{&exportID}); err != nil {
			t.Fatal(err)
		}
		rec, err := store.NewDatabaseExports(db).Get(ctx, exportID)
		if err != nil || rec.Status != store.DatabaseExportFailed || rec.Message == "" {
			t.Errorf("failed export = %+v, %v", rec, err)
		}
		if files := e.files(t); len(files) != 1 {
			t.Errorf("files after the failed export = %q", files)
		}
		if _, err := e.svc.Open(ctx, gone.ID, exportID); !errors.Is(err, dbexport.ErrExportNotFound) {
			t.Errorf("Open of a failed export: %v", err)
		}
	})
}

func TestSweep(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		site := e.site(t, store.SiteRunning, true, []byte("SELECT 1;\n"))
		export := func() string {
			t.Helper()
			job, err := e.svc.Export(ctx, site.ID, "")
			if err != nil {
				t.Fatal(err)
			}
			e.runJobs(t)
			var res struct {
				ExportID string `json:"export_id"`
			}
			if err := json.Unmarshal(e.job(t, job.ID).Result, &res); err != nil {
				t.Fatal(err)
			}
			return res.ExportID
		}
		old := export()
		e.clock.add(ttl / 2)
		young := export()

		// Files that belong to no running or ready export: a failed
		// export's leftover, one of an export that doesn't exist, and a
		// temporary file. Files that aren't an export's are left alone.
		failed := &store.DatabaseExport{SiteID: site.ID}
		if err := store.NewDatabaseExports(db).Create(ctx, failed); err != nil {
			t.Fatal(err)
		}
		running := &store.DatabaseExport{SiteID: site.ID}
		if err := store.NewDatabaseExports(db).Create(ctx, running); err != nil {
			t.Fatal(err)
		}
		if _, err := store.NewDatabaseExports(db).Fail(ctx, failed.ID, "x"); err != nil {
			t.Fatal(err)
		}
		stray := id.New()
		for _, name := range []string{
			failed.ID + ".sql", stray + ".sql.gz", "." + stray + ".sql.gz.123.tmp", running.ID + ".sql", "README",
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
		want := []string{"README", dbexport.FileName(young), running.ID + ".sql"}
		slices.Sort(want)
		if files := e.files(t); !slices.Equal(files, want) {
			t.Errorf("files = %q, want %q", files, want)
		}
		for exportID, status := range map[string]store.DatabaseExportStatus{old: store.DatabaseExportExpired, young: store.DatabaseExportReady} {
			if rec, err := store.NewDatabaseExports(db).Get(ctx, exportID); err != nil || rec.Status != status {
				t.Errorf("export %s = %+v, %v; want %s", exportID, rec, err, status)
			}
		}

		// The cleanup job does the same; running it again changes nothing.
		e.clock.add(ttl)
		if err := dbexport.EnsureCleanupSchedule(ctx, db); err != nil {
			t.Fatal(err)
		}
		sch, err := store.NewSchedules(db).Get(ctx, dbexport.CleanupScheduleName)
		if err != nil {
			t.Fatal(err)
		}
		if sch.JobType != dbexport.CleanupJobType || sch.Interval != 5*time.Minute {
			t.Errorf("schedule = %+v", sch)
		}
		for range 2 {
			job, err := jobs.Enqueue(ctx, db, jobs.Spec{Type: sch.JobType, TargetType: sch.TargetType, TargetID: sch.TargetID})
			if err != nil {
				t.Fatal(err)
			}
			e.runJobs(t)
			if done := e.job(t, job.ID); done.State != store.JobSucceeded {
				t.Fatalf("cleanup job = %s: %s", done.State, done.Error)
			}
		}
		if files := e.files(t); !slices.Equal(files, []string{running.ID + ".sql", "README"}) {
			t.Errorf("files after cleanup = %q", files)
		}
	})
}
