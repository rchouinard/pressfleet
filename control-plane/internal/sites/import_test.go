package sites_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/id"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
	"github.com/rchouinard/pressfleet/control-plane/internal/sitedb"
	"github.com/rchouinard/pressfleet/control-plane/internal/sites"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
	"github.com/rchouinard/pressfleet/control-plane/internal/uploads"
)

// importEnv is an env that can import sites.
type importEnv struct {
	*env
	up     *uploads.Service
	tmpDir string
	user   string

	mu sync.Mutex
	// extracted maps a site to the tar entries its last extraction task
	// found in the transfer directory.
	extracted map[string]map[string]string
}

func newImportEnv(t *testing.T, db *store.DB) *importEnv {
	t.Helper()
	dir := t.TempDir()
	up, err := uploads.New(uploads.Options{
		DB: db, Dir: filepath.Join(dir, "uploads"), MaxSize: 1 << 30, Expiry: time.Hour, Logger: storetest.Logger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	ie := &importEnv{up: up, tmpDir: filepath.Join(dir, "tmp"), extracted: map[string]map[string]string{}}
	ie.env = newEnvWith(t, db, testPreviewBase, func(o *sites.Options) {
		o.Uploads, o.TmpDir = up, ie.tmpDir
	})
	ie.prov.RunTaskFunc = func(siteID string, task provisioner.Task) (*provisioner.TaskResult, error) {
		files, err := readTar(filepath.Join(task.TransferDir, "wp-content.tar"))
		if err != nil {
			return nil, err
		}
		ie.mu.Lock()
		ie.extracted[siteID] = files
		ie.mu.Unlock()
		return &provisioner.TaskResult{}, nil
	}
	u := store.User{Email: id.New() + "@example.com", PasswordHash: "x", Role: store.RoleOperator}
	if err := store.NewUsers(db).Create(context.Background(), &u); err != nil {
		t.Fatal(err)
	}
	ie.user = u.ID
	return ie
}

func readTar(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	out := map[string]string{}
	tr := tar.NewReader(f)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		b, err := io.ReadAll(tr)
		if err != nil {
			return nil, err
		}
		out[h.Name] = string(b)
	}
}

// upload stores data as a complete site import upload for the group.
func (ie *importEnv) upload(t *testing.T, group string, data []byte) string {
	t.Helper()
	ctx := context.Background()
	u, err := ie.up.Create(ctx, uploads.NewUpload{
		Purpose: store.UploadSiteImport, ClientGroupID: group, SizeBytes: int64(len(data)), CreatedByUserID: ie.user,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ie.up.Write(ctx, u.ID, 0, bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	if _, err := ie.up.Complete(ctx, u.ID, hex.EncodeToString(sum[:])); err != nil {
		t.Fatal(err)
	}
	return u.ID
}

// testArchive is a tar.gz of a wp-content from another host.
func testArchive(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for name, body := range map[string]string{
		"wp-content/themes/twentytwentyfive/style.css": "/* Theme Name: TT5 */",
		"wp-content/plugins/akismet/akismet.php":       "<?php",
		"wp-content/uploads/2026/09/photo.jpg":         "jpeg",
		"wp-content/mu-plugins/wpengine-common.php":    "<?php",
		"wp-content/object-cache.php":                  "<?php // host",
		"wp-content/advanced-cache.php":                "<?php die();",
	} {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// testDump is a mysqldump of a site at http://old.example with the table
// prefix wpx7_; extra adds statements.
func testDump(dbVersion, extra string) []byte {
	return []byte("-- MySQL dump\n" +
		"CREATE DATABASE /*!32312 IF NOT EXISTS*/ `old`;\nUSE `old`;\n" +
		"CREATE TABLE `wpx7_options` (x int);\nCREATE TABLE `wpx7_posts` (x int);\n" +
		"CREATE TABLE `wpx7_users` (x int);\nCREATE TABLE `wpx7_usermeta` (x int);\n" +
		"INSERT INTO `wpx7_options` VALUES (1,'siteurl','http://old.example/wp','yes'),(2,'home','http://old.example','yes')," +
		"(3,'blogname','Old Blog','yes'),(4,'db_version','" + dbVersion + "','yes'),(5,'stylesheet','twentytwentyfive','yes');\n" +
		"/*!50013 DEFINER=`root`@`localhost` SQL SECURITY DEFINER */\n" + extra)
}

func (ie *importEnv) importSite(t *testing.T, group string, archive, dump []byte) (string, store.Job) {
	t.Helper()
	siteID, job, err := ie.svc.Import(context.Background(), sites.Import{
		ClientGroupID: group, ArchiveUploadID: ie.upload(t, group, archive), DumpUploadID: ie.upload(t, group, dump),
		CreatedByUserID: ie.user,
	})
	if err != nil {
		t.Fatal(err)
	}
	return siteID, job
}

func TestImport(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		ie := newImportEnv(t, db)
		siteID, job := ie.importSite(t, ie.group, testArchive(t), testDump("57155", ""))
		if job.Type != sites.ImportJobType || job.TargetType != "site" || job.TargetID != siteID || job.LockKey != sites.LockKey(siteID) {
			t.Errorf("job = %+v", job)
		}
		// Nothing exists until the job has checked the uploads.
		if _, err := store.NewSites(db).Get(ctx, siteID); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("site before the job: err = %v", err)
		}
		ie.runJobs(t)
		d := ie.job(t, job.ID)
		if d.Job.State != store.JobSucceeded {
			t.Fatalf("job %s: %s\n%s", d.Job.State, d.Job.Error, jobLog(d))
		}
		site := ie.site(t, siteID)
		if site.Status != store.SiteRunning || site.Name != "Old Blog" || site.Slug != "old-blog" || site.TablePrefix != "wpx7_" ||
			site.WPCoreVersion != "7.1.2" || site.PHPVersion != "8.4" || site.ClientGroupID != ie.group ||
			site.AdminPasswordSecretRef != "" || site.WPSaltsSecretRef == "" {
			t.Errorf("site = %+v", site)
		}
		if st, ok := ie.prov.Site(siteID); !ok || st.Config.TablePrefix != "wpx7_" {
			t.Errorf("provisioned site = %+v, %v", st, ok)
		}

		// wp-content went to the task without the host's drop-ins.
		ie.mu.Lock()
		files := ie.extracted[siteID]
		ie.mu.Unlock()
		for _, name := range []string{"themes/twentytwentyfive/style.css", "uploads/2026/09/photo.jpg", "mu-plugins/wpengine-common.php"} {
			if _, ok := files[name]; !ok {
				t.Errorf("extracted wp-content lacks %s: %v", name, files)
			}
		}
		for _, name := range []string{"object-cache.php", "advanced-cache.php"} {
			if _, ok := files[name]; ok {
				t.Errorf("extracted wp-content has %s", name)
			}
		}
		var task *provisioner.Task
		for _, c := range ie.prov.Calls() {
			if c.Op == "RunTask" && c.SiteID == siteID {
				task = c.Task
			}
		}
		if task == nil || task.WPContent != provisioner.MountReadWrite || !strings.HasPrefix(task.TransferDir, ie.tmpDir) {
			t.Errorf("task = %+v", task)
		}

		// The sanitized dump was imported.
		data, ok := ie.dbs.Data(siteID)
		if !ok || !bytes.Contains(data, []byte("'Old Blog'")) || bytes.Contains(data, []byte("DEFINER=")) ||
			bytes.Contains(data, []byte("USE `old`")) || bytes.Contains(data, []byte("CREATE DATABASE")) {
			t.Errorf("imported database:\n%s", data)
		}

		// URLs moved to the preview domain, the subdirectory siteurl first.
		preview := "https://" + site.PreviewDomain
		calls := ie.wp.pluginCalls(siteID)
		var replaced []string
		for _, c := range calls {
			if strings.HasPrefix(c, "search-replace ") {
				f := strings.Fields(c)
				replaced = append(replaced, f[1]+" "+f[2])
			}
		}
		wantReplaced := []string{
			"https://old.example/wp " + preview, "http://old.example/wp " + preview, "//old.example/wp //" + site.PreviewDomain,
			`https:\/\/old.example\/wp ` + strings.ReplaceAll(preview, "/", `\/`),
			`http:\/\/old.example\/wp ` + strings.ReplaceAll(preview, "/", `\/`),
			`\/\/old.example\/wp \/\/` + site.PreviewDomain,
			"https://old.example " + preview, "http://old.example " + preview, "//old.example //" + site.PreviewDomain,
		}
		for i, w := range wantReplaced {
			if i >= len(replaced) || replaced[i] != w {
				t.Errorf("search-replace %d = %q, want %q (all: %q)", i, at(replaced, i), w, replaced)
			}
		}
		for _, want := range []string{"core update-db", "option update home " + preview, "option update siteurl " + preview, "redis enable"} {
			if !slices.Contains(calls, want) {
				t.Errorf("WP-CLI calls lack %q: %q", want, calls)
			}
		}
		log := jobLog(d)
		for _, want := range []string{`Must-use plugin "wpengine-common.php" is WP Engine's`, "advanced-cache.php, object-cache.php", "table prefix wpx7_"} {
			if !strings.Contains(log, want) {
				t.Errorf("job log lacks %q:\n%s", want, log)
			}
		}
		if !strings.Contains(string(d.Job.Result), `"source_url":"http://old.example"`) {
			t.Errorf("result = %s", d.Job.Result)
		}
		// The uploads and the temporary files are gone.
		if n := ie.count(t, "SELECT COUNT(*) FROM uploads"); n != 0 {
			t.Errorf("%d uploads left", n)
		}
		if left, _ := os.ReadDir(ie.tmpDir); len(left) != 0 {
			t.Errorf("left in tmp: %v", left)
		}
	})
}

func at(s []string, i int) string {
	if i < len(s) {
		return s[i]
	}
	return ""
}

// TestImportRejected checks that sources that can't be imported are
// refused before a site is created.
func TestImportRejected(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		ie := newImportEnv(t, db)
		for _, tt := range []struct {
			name          string
			archive, dump []byte
			want          string
		}{
			{"multisite", testArchive(t), testDump("57155", "CREATE TABLE `wpx7_blogs` (x int);\n"), "multisite"},
			{"unreadable dump", testArchive(t), []byte("\x00\x01binary"), "binary data"},
			{"not WordPress", testArchive(t), []byte("CREATE TABLE `orders` (x int);\n"), "no WordPress tables"},
			{"too new", testArchive(t), testDump("99999", ""), "newer than the newest site image's, WordPress 7.1.2"},
			{"not an archive", []byte("just text"), testDump("57155", ""), "isn't a tar or zip archive"},
		} {
			t.Run(tt.name, func(t *testing.T) {
				siteID, job := ie.importSite(t, ie.group, tt.archive, tt.dump)
				ie.runJobs(t)
				d := ie.job(t, job.ID)
				if d.Job.State != store.JobFailed || !strings.Contains(d.Job.Error, tt.want) {
					t.Errorf("job %s: %q, want failed with %q", d.Job.State, d.Job.Error, tt.want)
				}
				if d.Job.Attempts != 1 {
					t.Errorf("a rejection was retried: %d attempts", d.Job.Attempts)
				}
				if _, ok := ie.prov.Site(siteID); ok {
					t.Error("a stack was created")
				}
			})
		}
		for _, table := range []string{"sites", "secrets", "schedules", "uploads"} {
			if n := ie.count(t, "SELECT COUNT(*) FROM "+table); n != 0 {
				t.Errorf("%d rows in %s after rejected imports", n, table)
			}
		}
		if n := ie.count(t, "SELECT last_uid FROM site_uid_counter"); n != provisioner.MinSiteUID-1 {
			t.Errorf("rejected imports used up UIDs: counter at %d", n)
		}
		if len(ie.dbs.Calls()) != 0 || len(ie.cache.Calls()) != 0 {
			t.Errorf("rejected imports touched databases %v or caches %v", ie.dbs.Calls(), ie.cache.Calls())
		}
	})
}

// TestImportFailureDeletesSite checks that an import that fails after the
// site was created removes all of it.
func TestImportFailureDeletesSite(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		ie := newImportEnv(t, db)
		ie.dbs.Err = func(op, _ string) error {
			if op == "Import" {
				return sitedb.ErrDatabaseNotFound
			}
			return nil
		}
		siteID, job := ie.importSite(t, ie.group, testArchive(t), testDump("57155", ""))
		ie.runJobs(t)
		d := ie.job(t, job.ID)
		if d.Job.State != store.JobFailed {
			t.Fatalf("job %s: %s", d.Job.State, d.Job.Error)
		}
		if _, err := store.NewSites(db).Get(context.Background(), siteID); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("site after the failed import: err = %v", err)
		}
		if _, ok := ie.prov.Site(siteID); ok {
			t.Error("the stack is still there")
		}
		if _, ok := ie.dbs.Data(siteID); ok {
			t.Error("the database is still there")
		}
		for _, table := range []string{"sites", "secrets", "schedules"} {
			if n := ie.count(t, "SELECT COUNT(*) FROM "+table); n != 0 {
				t.Errorf("%d rows in %s after the failed import", n, table)
			}
		}
		if !strings.Contains(jobLog(d), "Deleted the imported site") {
			t.Errorf("job log:\n%s", jobLog(d))
		}
		if left, _ := os.ReadDir(ie.tmpDir); len(left) != 0 {
			t.Errorf("left in tmp: %v", left)
		}
	})
}

func TestImportRequest(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		ie := newImportEnv(t, db)
		archive := ie.upload(t, ie.group, []byte("a"))
		dump := ie.upload(t, ie.group, []byte("d"))
		other := &store.ClientGroup{Name: "Other " + id.New()}
		if err := store.NewClientGroups(db).Create(ctx, other); err != nil {
			t.Fatal(err)
		}
		elsewhere := ie.upload(t, other.ID, []byte("x"))
		incomplete, err := ie.up.Create(ctx, uploads.NewUpload{
			Purpose: store.UploadSiteImport, ClientGroupID: ie.group, SizeBytes: 10, CreatedByUserID: ie.user,
		})
		if err != nil {
			t.Fatal(err)
		}
		php := "7.4"
		for _, tt := range []struct {
			name   string
			req    sites.Import
			fields []string
			err    error
		}{
			{"unknown group", sites.Import{ClientGroupID: id.New(), ArchiveUploadID: archive, DumpUploadID: dump}, []string{"/client_group_id"}, nil},
			{"unknown upload", sites.Import{ArchiveUploadID: id.New(), DumpUploadID: dump}, []string{"/archive_upload_id"}, nil},
			{"same upload twice", sites.Import{ArchiveUploadID: archive, DumpUploadID: archive}, []string{"/dump_upload_id"}, nil},
			{"other group's upload", sites.Import{ArchiveUploadID: archive, DumpUploadID: elsewhere}, []string{"/dump_upload_id"}, nil},
			{"bad PHP", sites.Import{ArchiveUploadID: archive, DumpUploadID: dump, PHPVersion: php}, []string{"/php_version"}, nil},
			{"incomplete", sites.Import{ArchiveUploadID: archive, DumpUploadID: incomplete.ID}, nil, sites.ErrUploadNotReady},
		} {
			t.Run(tt.name, func(t *testing.T) {
				req := tt.req
				req.CreatedByUserID = ie.user
				if req.ClientGroupID == "" {
					req.ClientGroupID = ie.group
				}
				_, _, err := ie.svc.Import(ctx, req)
				if tt.err != nil {
					if !errors.Is(err, tt.err) {
						t.Errorf("err = %v, want %v", err, tt.err)
					}
					return
				}
				var ve *sites.ValidationError
				if !errors.As(err, &ve) {
					t.Fatalf("err = %v, want a ValidationError", err)
				}
				var got []string
				for _, f := range ve.Fields {
					got = append(got, f.Field)
				}
				if !slices.Equal(got, tt.fields) {
					t.Errorf("fields = %v, want %v (%v)", got, tt.fields, err)
				}
			})
		}
		// Nothing was queued, and the uploads are still free.
		if n := ie.count(t, "SELECT COUNT(*) FROM jobs"); n != 0 {
			t.Errorf("%d jobs queued by rejected requests", n)
		}
		if n := ie.count(t, "SELECT COUNT(*) FROM uploads WHERE status = 'complete'"); n != 3 {
			t.Errorf("%d complete uploads, want 3", n)
		}
		// Another user can't use them.
		if _, _, err := ie.svc.Import(ctx, sites.Import{ClientGroupID: ie.group, ArchiveUploadID: archive, DumpUploadID: dump, CreatedByUserID: id.New()}); err == nil {
			t.Error("another user's import took the uploads")
		}
	})
}
