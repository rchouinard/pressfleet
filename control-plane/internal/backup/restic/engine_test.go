package restic_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/rchouinard/pressfleet/control-plane/internal/backup"
	"github.com/rchouinard/pressfleet/control-plane/internal/backup/repository"
	"github.com/rchouinard/pressfleet/control-plane/internal/backup/restic"
	"github.com/rchouinard/pressfleet/control-plane/internal/config"
	"github.com/rchouinard/pressfleet/control-plane/internal/id"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner/provisionertest"
	"github.com/rchouinard/pressfleet/control-plane/internal/secrets"
	"github.com/rchouinard/pressfleet/control-plane/internal/secrets/secretstest"
	"github.com/rchouinard/pressfleet/control-plane/internal/sitedb"
	"github.com/rchouinard/pressfleet/control-plane/internal/sitedb/sitedbtest"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
)

// resticBinary returns the restic binary. mise.toml pins it, so
// `mise exec -- make test` has it on PATH.
func resticBinary(t *testing.T) string {
	t.Helper()
	p, err := exec.LookPath("restic")
	if err != nil {
		t.Fatal("restic is not on PATH; run the tests with `mise exec -- make test` after `mise install`")
	}
	return p
}

// fixture is an Engine on a real local repository, with a fake task runner
// that runs restic on the host instead of in a container: it maps the task
// paths (/site/wp-content, /transfer, /repository) to host directories.
// Snapshots therefore hold host paths; the Docker end-to-end test checks
// the real ones.
type fixture struct {
	engine *restic.Engine
	db     *store.DB
	repos  *repository.Manager
	prov   *provisionertest.Fake
	dbs    *sitedbtest.Fake
	layout config.Layout
	restic string

	mu sync.Mutex
	// siteRoots maps site IDs to a directory standing for the site's
	// WordPress root: wp-content, and next to it core and wp-config.php,
	// which must never be backed up.
	siteRoots map[string]string
	// dumps records the dump each task found in its transfer directory.
	dumps []string
	// output, if set, replaces the task's result.
	output func(task provisioner.Task) (*provisioner.TaskResult, error)
}

func newFixture(t *testing.T, db *store.DB) *fixture {
	t.Helper()
	f := &fixture{
		db: db, prov: provisionertest.New(), dbs: sitedbtest.New(),
		layout: config.Layout{Root: t.TempDir()}, restic: resticBinary(t), siteRoots: map[string]string{},
	}
	if err := f.layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	var err error
	f.repos, err = repository.New(repository.Options{
		DB: db, Secrets: secrets.NewStore(db, secretstest.Keyring(t, 1)), Layout: f.layout,
		Restic: f.restic, Logger: storetest.Logger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	f.prov.RunTaskFunc = func(siteID string, task provisioner.Task) (*provisioner.TaskResult, error) {
		return f.runTask(t, siteID, task)
	}
	f.engine, err = restic.New(restic.Options{
		DB: db, Repositories: f.repos, Databases: f.dbs, Tasks: f.prov, Layout: f.layout, Logger: storetest.Logger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// runTask runs the task's restic command on the host.
func (f *fixture) runTask(t *testing.T, siteID string, task provisioner.Task) (*provisioner.TaskResult, error) {
	f.mu.Lock()
	root := f.siteRoots[siteID]
	dump, _ := os.ReadFile(filepath.Join(task.TransferDir, restic.DatabaseDumpName))
	f.dumps = append(f.dumps, string(dump))
	output := f.output
	f.mu.Unlock()
	if output != nil {
		return output(task)
	}
	paths := strings.NewReplacer(
		provisioner.TaskWPContentPath, filepath.Join(root, "wp-content"),
		provisioner.TaskTransferPath, task.TransferDir,
		provisioner.TaskRepositoryPath, task.RepositoryDir,
	)
	if task.Command[0] != "restic" {
		t.Errorf("task command = %q", task.Command)
	}
	cmd := exec.Command(f.restic, task.Command[1:]...)
	for i, a := range cmd.Args {
		cmd.Args[i] = paths.Replace(a)
	}
	cmd.Env = []string{"HOME=" + t.TempDir()}
	for k, v := range task.Env {
		cmd.Env = append(cmd.Env, k+"="+paths.Replace(v))
	}
	for k, v := range task.SecretEnv {
		cmd.Env = append(cmd.Env, k+"="+v.Reveal())
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return nil, &provisioner.ExecError{ExitCode: ee.ExitCode(), Stdout: stdout.String(), Stderr: stderr.String()}
	}
	if err != nil {
		return nil, err
	}
	return &provisioner.TaskResult{Stdout: stdout.String(), Stderr: stderr.String()}, nil
}

// site creates a site record, its runtime in the fake provisioner, its
// database, and a WordPress root with some wp-content.
func (f *fixture) site(t *testing.T, groupID, slug string) store.Site {
	t.Helper()
	ctx := context.Background()
	uid, err := store.AllocateSiteUID(ctx, f.db)
	if err != nil {
		t.Fatal(err)
	}
	s := store.Site{
		ClientGroupID: groupID, Name: slug, Slug: slug, Status: store.SiteRunning, PreviewDomain: slug + ".preview.test",
		PHPVersion: "8.4", WPCoreVersion: "6.9.1", UID: uid, WPSaltsSecretRef: "salts",
	}
	if err := store.NewSites(f.db).Create(ctx, &s); err != nil {
		t.Fatal(err)
	}
	if _, err := f.prov.CreateSite(ctx, siteConfig(s)); err != nil {
		t.Fatal(err)
	}
	f.dbs.SetData(s.ID, []byte("-- dump of "+slug+"\nCREATE TABLE wp_options (option_id int);\n"))
	root := t.TempDir()
	for name, content := range map[string]string{
		"wp-content/plugins/hello/hello.php":       "<?php // " + slug,
		"wp-content/uploads/2026/09/photo.jpg":     "jpeg " + slug,
		"wp-content/themes/twentytwenty/style.css": "body{}",
		"wp-config.php":           "<?php define('DB_PASSWORD', 'secret');",
		"wp-includes/version.php": "<?php $wp_version = '6.9.1';",
		"wp-admin/index.php":      "<?php",
	} {
		p := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	f.mu.Lock()
	f.siteRoots[s.ID] = root
	f.mu.Unlock()
	return s
}

// siteConfig is a valid runtime configuration of the site.
func siteConfig(s store.Site) *provisioner.SiteConfig {
	return &provisioner.SiteConfig{
		ID: s.ID, ClientGroupID: s.ClientGroupID, PreviewDomain: s.PreviewDomain, PHPVersion: s.PHPVersion,
		WPCoreVersion: s.WPCoreVersion, DBSecretRef: "db", WPSaltsSecretRef: "salts", UID: s.UID,
		Resources: provisioner.DefaultResourceLimits(),
	}
}

func (f *fixture) group(t *testing.T, name string) string {
	t.Helper()
	g := store.ClientGroup{Name: name}
	if err := store.NewClientGroups(f.db).Create(context.Background(), &g); err != nil {
		t.Fatal(err)
	}
	return g.ID
}

// tmpEntries lists what is left in the data directory's tmp/.
func (f *fixture) tmpEntries(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(f.layout.TmpDir())
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func (f *fixture) tasks() []provisioner.Task {
	var tasks []provisioner.Task
	for _, c := range f.prov.Calls() {
		if c.Op == "RunTask" {
			tasks = append(tasks, *c.Task)
		}
	}
	return tasks
}

// lsNode is one line of `restic ls --json`.
type lsNode struct {
	Type string `json:"type"`
	Path string `json:"path"`
}

// files lists the files in a snapshot.
func (f *fixture) files(t *testing.T, groupID string, snap backup.SnapshotID) []string {
	t.Helper()
	out, err := f.repos.Restic(context.Background(), groupID, "ls", "--json", string(snap))
	if err != nil {
		t.Fatal(err)
	}
	var files []string
	for line := range strings.Lines(string(out)) {
		var n lsNode
		if err := json.Unmarshal([]byte(line), &n); err != nil {
			t.Fatalf("restic ls line %q: %v", line, err)
		}
		if n.Type == "file" {
			files = append(files, n.Path)
		}
	}
	return files
}

func TestBackupAndListSnapshots(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		f := newFixture(t, db)
		groupID := f.group(t, "Acme")
		site := f.site(t, groupID, "acme")
		other := f.site(t, groupID, "acme-shop")
		root := f.siteRoots[site.ID]

		if snaps, err := f.engine.ListSnapshots(ctx, site.ID); err != nil || snaps == nil || len(snaps) != 0 {
			t.Errorf("ListSnapshots before the first backup = %v, %v", snaps, err)
		}

		backupID := id.New()
		snap, err := f.engine.Backup(ctx, site.ID, backup.Options{Kind: "manual", BackupID: backupID})
		if err != nil {
			t.Fatal(err)
		}
		if len(snap.ID) != 64 || snap.SiteID != site.ID || snap.WPCoreVersion != "6.9.1" || snap.Kind != "manual" ||
			snap.BackupID != backupID || snap.Time.IsZero() || snap.FileCount != 4 || snap.SizeBytes <= 0 || snap.AddedBytes <= 0 ||
			len(snap.Warnings) != 0 {
			t.Errorf("snapshot = %+v", snap)
		}
		wantTags := []string{"site:" + site.ID, "wp-core:6.9.1", "kind:manual", "backup:" + backupID}
		if !slices.Equal(snap.Tags, wantTags) {
			t.Errorf("tags = %q, want %q", snap.Tags, wantTags)
		}

		// The task: wp-content read-only, the dump in the transfer
		// directory, the local repository, and restic given exactly the two
		// snapshot paths.
		tasks := f.tasks()
		if len(tasks) != 1 {
			t.Fatalf("tasks = %+v", tasks)
		}
		task := tasks[0]
		repoDir, _ := f.layout.BackupRepositoryDir(groupID)
		if task.WPContent != provisioner.MountReadOnly || task.RepositoryDir != repoDir ||
			filepath.Dir(task.TransferDir) != f.layout.TmpDir() || task.Env["RESTIC_REPOSITORY"] != provisioner.TaskRepositoryPath ||
			task.SecretEnv["RESTIC_PASSWORD"] == "" || task.Image != "" || task.Timeout != restic.DefaultTaskTimeout {
			t.Errorf("task = %+v", task)
		}
		wantCmd := []string{"restic", "backup", "--json", "--quiet", "--no-scan", "--retry-lock", "30m0s", "--host", "site-" + site.ID,
			"--tag", "site:" + site.ID, "--tag", "wp-core:6.9.1", "--tag", "kind:manual", "--tag", "backup:" + backupID,
			"/site/wp-content", "/transfer/database.sql"}
		if !slices.Equal(task.Command, wantCmd) {
			t.Errorf("command = %q\nwant      %q", task.Command, wantCmd)
		}
		for _, a := range task.Command {
			if strings.Contains(a, "wp-config") || strings.Contains(a, "wp-includes") || strings.Contains(a, "wp-admin") {
				t.Errorf("command names %q", a)
			}
		}
		if len(f.dumps) != 1 || !strings.Contains(f.dumps[0], "dump of acme\n") {
			t.Errorf("the task found the dump %q", f.dumps)
		}
		if left := f.tmpEntries(t); len(left) != 0 {
			t.Errorf("left in tmp/: %v", left)
		}

		// The snapshot holds wp-content and the dump, and neither core nor
		// wp-config.php.
		files := f.files(t, groupID, snap.ID)
		slices.Sort(files)
		var rel []string
		for _, p := range files {
			switch {
			case strings.HasPrefix(p, root+"/"):
				rel = append(rel, strings.TrimPrefix(p, root+"/"))
			case filepath.Base(p) == restic.DatabaseDumpName:
				rel = append(rel, "(dump)")
			default:
				t.Errorf("snapshot holds %s", p)
			}
		}
		wantFiles := []string{"(dump)", "wp-content/plugins/hello/hello.php", "wp-content/themes/twentytwenty/style.css", "wp-content/uploads/2026/09/photo.jpg"}
		slices.Sort(rel)
		if !slices.Equal(rel, wantFiles) {
			t.Errorf("snapshot files = %q, want %q", rel, wantFiles)
		}

		// A retry of the same backup finds the snapshot instead of taking
		// another.
		again, err := f.engine.Backup(ctx, site.ID, backup.Options{Kind: "manual", BackupID: backupID})
		if err != nil || again.ID != snap.ID {
			t.Errorf("repeated Backup = %+v, %v; want snapshot %s", again, err, snap.ID)
		}
		if n := len(f.tasks()); n != 1 {
			t.Errorf("repeated Backup ran a task: %d tasks", n)
		}

		// Another backup of the site and one of another site in the same
		// repository.
		second, err := f.engine.Backup(ctx, site.ID, backup.Options{Kind: "scheduled"})
		if err != nil {
			t.Fatal(err)
		}
		if second.BackupID != "" || slices.ContainsFunc(second.Tags, func(t string) bool { return strings.HasPrefix(t, "backup:") }) {
			t.Errorf("snapshot without a backup ID = %+v", second)
		}
		otherSnap, err := f.engine.Backup(ctx, other.ID, backup.Options{Kind: "manual"})
		if err != nil {
			t.Fatal(err)
		}

		snaps, err := f.engine.ListSnapshots(ctx, site.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(snaps) != 2 || snaps[0].ID != snap.ID || snaps[1].ID != second.ID {
			t.Fatalf("ListSnapshots = %+v, want [%s %s]", snaps, snap.ID, second.ID)
		}
		first := snaps[0]
		if first.SiteID != site.ID || first.WPCoreVersion != "6.9.1" || first.Kind != "manual" || first.BackupID != backupID ||
			first.SizeBytes != snap.SizeBytes || first.FileCount != snap.FileCount || first.AddedBytes != snap.AddedBytes ||
			!first.Time.Equal(snap.Time) || !slices.Equal(first.Tags, wantTags) {
			t.Errorf("listed %+v, backed up %+v", first, snap)
		}
		if snaps[1].Kind != "scheduled" || snaps[0].Time.After(snaps[1].Time) {
			t.Errorf("second listed = %+v", snaps[1])
		}
		if snaps, err := f.engine.ListSnapshots(ctx, other.ID); err != nil || len(snaps) != 1 || snaps[0].ID != otherSnap.ID {
			t.Errorf("other site's snapshots = %+v, %v", snaps, err)
		}

		// A deleted site's snapshots are found through its backup records.
		rec := store.BackupRecord{SiteID: other.ID, ClientGroupID: groupID, Kind: store.BackupManual}
		if err := store.NewBackupRecords(db).Create(ctx, &rec); err != nil {
			t.Fatal(err)
		}
		if _, err := store.NewSites(db).Delete(ctx, other.ID); err != nil {
			t.Fatal(err)
		}
		if snaps, err := f.engine.ListSnapshots(ctx, other.ID); err != nil || len(snaps) != 1 || snaps[0].ID != otherSnap.ID {
			t.Errorf("deleted site's snapshots = %+v, %v", snaps, err)
		}
	})
}

func TestBackupErrors(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		f := newFixture(t, db)
		groupID := f.group(t, "Acme")
		site := f.site(t, groupID, "acme")

		if _, err := f.engine.Backup(ctx, id.New(), backup.Options{}); !errors.Is(err, backup.ErrSiteNotFound) {
			t.Errorf("Backup of a missing site: %v, want ErrSiteNotFound", err)
		}
		if _, err := f.engine.ListSnapshots(ctx, id.New()); !errors.Is(err, backup.ErrSiteNotFound) {
			t.Errorf("ListSnapshots of a missing site: %v, want ErrSiteNotFound", err)
		}
		for _, opts := range []backup.Options{{Kind: "Manual,x"}, {BackupID: "not-an-id"}} {
			if _, err := f.engine.Backup(ctx, site.ID, opts); err == nil {
				t.Errorf("Backup accepted %+v", opts)
			}
		}
		if _, err := f.engine.Backup(ctx, "../x", backup.Options{}); err == nil {
			t.Error("Backup accepted an invalid site ID")
		}

		// A failed dump: no task, nothing left behind.
		f.dbs.Err = func(op, _ string) error {
			if op == "Dump" {
				return sitedb.ErrDatabaseNotFound
			}
			return nil
		}
		if _, err := f.engine.Backup(ctx, site.ID, backup.Options{BackupID: id.New()}); !errors.Is(err, sitedb.ErrDatabaseNotFound) {
			t.Errorf("Backup with a failing dump: %v", err)
		}
		f.dbs.Err = nil
		if n := len(f.tasks()); n != 0 {
			t.Errorf("tasks after a failed dump: %d", n)
		}
		if left := f.tmpEntries(t); len(left) != 0 {
			t.Errorf("left in tmp/ after a failed dump: %v", left)
		}

		// A failed task: restic's reason in the error, nothing left behind.
		f.output = func(provisioner.Task) (*provisioner.TaskResult, error) {
			return nil, &provisioner.ExecError{ExitCode: 1, Stderr: `{"message_type":"exit_error","code":1,"message":"Fatal: unable to open repository"}` + "\n"}
		}
		_, err := f.engine.Backup(ctx, site.ID, backup.Options{BackupID: id.New()})
		if !errors.Is(err, restic.ErrBackupFailed) || !strings.Contains(err.Error(), "Fatal: unable to open repository") {
			t.Errorf("Backup with a failing task: %v", err)
		}
		if left := f.tmpEntries(t); len(left) != 0 {
			t.Errorf("left in tmp/ after a failed task: %v", left)
		}

		// The site's runtime is gone.
		f.output = nil
		if err := f.prov.DeleteSite(ctx, site.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := f.engine.Backup(ctx, site.ID, backup.Options{}); !errors.Is(err, provisioner.ErrSiteNotFound) {
			t.Errorf("Backup of a site without a runtime: %v", err)
		}
		if left := f.tmpEntries(t); len(left) != 0 {
			t.Errorf("left in tmp/ after a missing runtime: %v", left)
		}

		// An earlier attempt's transfer directory is replaced.
		if _, err := f.prov.CreateSite(ctx, siteConfig(site)); err != nil {
			t.Fatal(err)
		}
		backupID := id.New()
		stale := filepath.Join(f.layout.TmpDir(), "backup-"+site.ID+"-"+backupID)
		if err := os.MkdirAll(filepath.Join(stale, "junk"), 0o700); err != nil {
			t.Fatal(err)
		}
		if _, err := f.engine.Backup(ctx, site.ID, backup.Options{BackupID: backupID}); err != nil {
			t.Fatal(err)
		}
		if left := f.tmpEntries(t); len(left) != 0 {
			t.Errorf("left in tmp/ after a retried backup: %v", left)
		}
	})
}

func TestRestoreAndPruneAreNotImplemented(t *testing.T) {
	f := newFixture(t, storetest.Open(t, store.SQLite))
	ctx := context.Background()
	if err := f.engine.Restore(ctx, id.New(), "abc", id.New()); !errors.Is(err, errors.ErrUnsupported) || !strings.Contains(err.Error(), "/issues/55") {
		t.Errorf("Restore = %v", err)
	}
	if err := f.engine.Prune(ctx, id.New()); !errors.Is(err, errors.ErrUnsupported) || !strings.Contains(err.Error(), "/issues/54") {
		t.Errorf("Prune = %v", err)
	}
}

func TestNewRequiresDependencies(t *testing.T) {
	if _, err := restic.New(restic.Options{}); err == nil {
		t.Error("New accepted no dependencies")
	}
}
