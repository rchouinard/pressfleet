package repository_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/backup/repository"
	"github.com/rchouinard/pressfleet/control-plane/internal/backup/repository/repositorytest"
	"github.com/rchouinard/pressfleet/control-plane/internal/config"
	"github.com/rchouinard/pressfleet/control-plane/internal/id"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner/provisionertest"
	"github.com/rchouinard/pressfleet/control-plane/internal/redact"
	"github.com/rchouinard/pressfleet/control-plane/internal/secrets"
	"github.com/rchouinard/pressfleet/control-plane/internal/secrets/secretstest"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
)

// resticBinary returns the restic binary the tests run. mise.toml pins it,
// so `mise exec -- make test` has it on PATH.
func resticBinary(t *testing.T) string {
	t.Helper()
	p, err := exec.LookPath("restic")
	if err != nil {
		t.Fatal("restic is not on PATH; run the tests with `mise exec -- make test` after `mise install`")
	}
	return p
}

type fixture struct {
	m       *repository.Manager
	local   *repositorytest.LocalRunner
	db      *store.DB
	secrets secrets.Store
	layout  config.Layout
}

func newFixture(t *testing.T, db *store.DB, opts repository.Options) fixture {
	t.Helper()
	st := secrets.NewStore(db, secretstest.Keyring(t, 1))
	opts.DB, opts.Secrets, opts.Logger = db, st, storetest.Logger()
	if opts.Layout.Root == "" {
		opts.Layout = config.Layout{Root: t.TempDir()}
	}
	local := repositorytest.NewLocalRunner(t)
	if opts.Tasks == nil {
		opts.Tasks = local
	}
	m, err := repository.New(opts)
	if err != nil {
		t.Fatal(err)
	}
	return fixture{m: m, local: local, db: db, secrets: st, layout: opts.Layout}
}

func (f fixture) group(t *testing.T, name string) string {
	t.Helper()
	g := store.ClientGroup{Name: name}
	if err := store.NewClientGroups(f.db).Create(context.Background(), &g); err != nil {
		t.Fatal(err)
	}
	return g.ID
}

func (f fixture) count(t *testing.T, table string) int {
	t.Helper()
	var n int
	// table is a constant from the test.
	if err := f.db.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM "+table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func (f fixture) password(t *testing.T, r repository.Repository) redact.Secret {
	t.Helper()
	pw, err := f.secrets.Get(context.Background(), r.SecretRef)
	if err != nil {
		t.Fatal(err)
	}
	return pw
}

// restic runs plain restic, as an operator or client would, with only the
// given environment.
func restic(t *testing.T, env []string, args ...string) ([]byte, error) {
	t.Helper()
	cmd := exec.Command(resticBinary(t), append([]string{"--no-cache"}, args...)...)
	cmd.Env = append([]string{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir()}, env...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return out, errors.New(err.Error() + ": " + stderr.String())
	}
	return out, nil
}

func TestEnsure(t *testing.T) {
	// restic derives keys slowly on purpose; run beside the others.
	t.Parallel()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		ctx := context.Background()
		f := newFixture(t, db, repository.Options{})
		groupID := f.group(t, "Acme")

		if _, err := f.m.Get(ctx, groupID); !errors.Is(err, repository.ErrNoRepository) {
			t.Fatalf("Get before Ensure: err = %v, want ErrNoRepository", err)
		}
		r, err := f.m.Ensure(ctx, groupID)
		if err != nil {
			t.Fatal(err)
		}
		wantDir, _ := f.layout.BackupRepositoryDir(groupID)
		if r.ClientGroupID != groupID || r.Backend != repository.BackendLocal || r.Location != wantDir || r.SecretRef == "" {
			t.Errorf("Ensure = %+v", r)
		}
		if _, err := os.Stat(filepath.Join(wantDir, "config")); err != nil {
			t.Errorf("repository not initialized: %v", err)
		}
		g, err := store.NewClientGroups(db).Get(ctx, groupID)
		if err != nil || g.BackupRepository != wantDir {
			t.Errorf("client group backup_repository = %q, %v; want %s", g.BackupRepository, err, wantDir)
		}

		again, err := f.m.Ensure(ctx, groupID)
		if err != nil || again.SecretRef != r.SecretRef || again.Location != r.Location {
			t.Fatalf("second Ensure = %+v, %v; want %+v", again, err, r)
		}
		if n := f.count(t, "secrets"); n != 1 {
			t.Errorf("secrets = %d, want 1", n)
		}

		// Losing the repository's files (but not the record) re-creates it
		// with the stored password.
		if err := os.RemoveAll(wantDir); err != nil {
			t.Fatal(err)
		}
		if again, err = f.m.Ensure(ctx, groupID); err != nil || again.SecretRef != r.SecretRef {
			t.Fatalf("Ensure after losing the files = %+v, %v", again, err)
		}
		if _, err := restic(t, []string{"RESTIC_REPOSITORY=" + wantDir, "RESTIC_PASSWORD=" + f.password(t, r).Reveal()}, "cat", "config"); err != nil {
			t.Errorf("re-created repository doesn't open with the stored password: %v", err)
		}

		// A client group can't be deleted while its repository exists.
		if err := store.NewClientGroups(db).Delete(ctx, groupID); !errors.Is(err, store.ErrReference) {
			t.Errorf("deleting the group: err = %v, want ErrReference", err)
		}
	})
}

// keyFiles returns the names of the key files of a local repository.
func keyFiles(t *testing.T, dir string) []string {
	t.Helper()
	des, err := os.ReadDir(filepath.Join(dir, "keys"))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, de := range des {
		names = append(names, de.Name())
	}
	return names
}

// Concurrent first backups of a new group call Ensure at once. Each restic
// init writes a master key, and with more than one the repository is
// unreadable for good (#188).
func TestEnsureConcurrent(t *testing.T) {
	t.Parallel()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		ctx := context.Background()
		f := newFixture(t, db, repository.Options{})
		// Plain restic broke 14 of 20 trials with four inits, so a few
		// groups of eight make a missing lock all but certain to show.
		for _, name := range []string{"Acme", "Beta", "Gamma"} {
			groupID := f.group(t, name)
			const callers = 8
			recs := make([]repository.Repository, callers)
			errs := make([]error, callers)
			var wg sync.WaitGroup
			start := make(chan struct{})
			for i := range callers {
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start
					recs[i], errs[i] = f.m.Ensure(ctx, groupID)
				}()
			}
			close(start)
			wg.Wait()
			for i, err := range errs {
				if err != nil {
					t.Fatalf("%s: Ensure #%d: %v", name, i, err)
				}
				if recs[i].SecretRef != recs[0].SecretRef {
					t.Errorf("%s: Ensure #%d used secret %s, #0 used %s", name, i, recs[i].SecretRef, recs[0].SecretRef)
				}
			}
			if keys := keyFiles(t, recs[0].Location); len(keys) != 1 {
				t.Errorf("%s: %d key files %v, want 1", name, len(keys), keys)
			}
			env := []string{"RESTIC_REPOSITORY=" + recs[0].Location, "RESTIC_PASSWORD=" + f.password(t, recs[0]).Reveal()}
			if _, err := restic(t, env, "cat", "config"); err != nil {
				t.Errorf("%s: the repository doesn't open: %v", name, err)
			}
		}
		if n := f.count(t, "secrets"); n != 3 {
			t.Errorf("secrets = %d, want 3", n)
		}
	})
}

// A control plane that stops between restic's writing the master key and
// the config leaves a key without a config. The next Ensure must not add a
// second key to it.
func TestEnsureAfterInterruptedInit(t *testing.T) {
	t.Parallel()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		ctx := context.Background()
		f := newFixture(t, db, repository.Options{})
		groupID := f.group(t, "Acme")
		r, err := f.m.Ensure(ctx, groupID)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(filepath.Join(r.Location, "config")); err != nil {
			t.Fatal(err)
		}
		if _, err := f.m.Ensure(ctx, groupID); err != nil {
			t.Fatalf("Ensure after an interrupted init: %v", err)
		}
		if keys := keyFiles(t, r.Location); len(keys) != 1 {
			t.Errorf("%d key files %v, want 1", len(keys), keys)
		}
		env := []string{"RESTIC_REPOSITORY=" + r.Location, "RESTIC_PASSWORD=" + f.password(t, r).Reveal()}
		if _, err := restic(t, env, "cat", "config"); err != nil {
			t.Errorf("the repository doesn't open: %v", err)
		}

		// Snapshots without a config aren't an interrupted init: the keys
		// stay.
		if err := os.Remove(filepath.Join(r.Location, "config")); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(r.Location, "snapshots", "0123"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := f.m.Ensure(ctx, groupID); !errors.Is(err, repository.ErrRepositoryUnfinished) {
			t.Errorf("Ensure over a repository with snapshots and no config: err = %v, want ErrRepositoryUnfinished", err)
		}
		if keys := keyFiles(t, r.Location); len(keys) != 1 {
			t.Errorf("keys were removed: %v", keys)
		}
	})
}

// A repository already broken by racing inits is reported, with its key
// files, and left alone.
func TestEnsureDamagedRepository(t *testing.T) {
	t.Parallel()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		ctx := context.Background()
		f := newFixture(t, db, repository.Options{})
		groupID := f.group(t, "Acme")
		r, err := f.m.Ensure(ctx, groupID)
		if err != nil {
			t.Fatal(err)
		}
		pw := f.password(t, r).Reveal()

		// What racing inits leave: key files for master keys the config
		// isn't encrypted with. Which key restic tries first isn't
		// deterministic, so the key the config needs is set aside rather
		// than left to chance: two stray keys stay, neither of which can
		// verify the config.
		keysDir := filepath.Join(r.Location, "keys")
		orig := keyFiles(t, r.Location)[0]
		origData, err := os.ReadFile(filepath.Join(keysDir, orig))
		if err != nil {
			t.Fatal(err)
		}
		var strays []string
		for range 2 {
			other := filepath.Join(t.TempDir(), "other")
			if _, err := restic(t, []string{"RESTIC_REPOSITORY=" + other, "RESTIC_PASSWORD=" + pw}, "init"); err != nil {
				t.Fatal(err)
			}
			name := keyFiles(t, other)[0]
			data, err := os.ReadFile(filepath.Join(other, "keys", name))
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(keysDir, name), data, 0o600); err != nil {
				t.Fatal(err)
			}
			strays = append(strays, name)
		}
		if err := os.Remove(filepath.Join(keysDir, orig)); err != nil {
			t.Fatal(err)
		}

		_, err = f.m.Ensure(ctx, groupID)
		if !errors.Is(err, repository.ErrRepositoryDamaged) {
			t.Fatalf("Ensure of a damaged repository: err = %v, want ErrRepositoryDamaged", err)
		}
		for _, want := range append(keyFiles(t, r.Location), "issue #188", "2 key files") {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error doesn't mention %q: %v", want, err)
			}
		}
		if keys := keyFiles(t, r.Location); len(keys) != 2 {
			t.Errorf("Ensure changed the key files: %v", keys)
		}

		// The repair the error describes works: keep the key the config
		// opens with, delete the others.
		for _, k := range strays {
			if err := os.Remove(filepath.Join(keysDir, k)); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(filepath.Join(keysDir, orig), origData, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := f.m.Ensure(ctx, groupID); err != nil {
			t.Errorf("Ensure after the repair: %v", err)
		}
	})
}

// A state backup restored on a host with another data directory carries the
// old directory in its local repository records; RelocateLocal points them
// at the new one, on both dialects.
func TestRelocateLocal(t *testing.T) {
	t.Parallel()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		ctx := context.Background()
		f := newFixture(t, db, repository.Options{})
		local, other := f.group(t, "Acme"), f.group(t, "Beta")
		if _, err := f.m.Ensure(ctx, local); err != nil {
			t.Fatal(err)
		}
		// A repository on S3 keeps its location.
		const s3Location = "s3:https://s3.example.com/bucket/other"
		ref, err := f.secrets.Put(ctx, secrets.KindResticRepoKey, "pw")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx,
			"INSERT INTO backup_repositories (client_group_id, backend, location, secret_ref, created_at) VALUES (?, ?, ?, ?, ?)",
			other, repository.BackendS3, s3Location, string(ref), time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
		if err := store.NewClientGroups(db).SetBackupRepository(ctx, other, s3Location); err != nil {
			t.Fatal(err)
		}
		before, err := store.NewClientGroups(db).Get(ctx, local)
		if err != nil {
			t.Fatal(err)
		}

		newRoot := t.TempDir()
		layout := config.Layout{Root: newRoot}
		want, _ := layout.BackupRepositoryDir(local)
		n, err := repository.RelocateLocal(ctx, db, layout)
		if err != nil || n != 1 {
			t.Fatalf("RelocateLocal = %d, %v; want 1", n, err)
		}
		if rec, err := f.m.Get(ctx, local); err != nil || rec.Location != want {
			t.Errorf("local record = %+v, %v; want location %s", rec, err, want)
		}
		g, err := store.NewClientGroups(db).Get(ctx, local)
		if err != nil || g.BackupRepository != want || !g.UpdatedAt.Equal(before.UpdatedAt) {
			t.Errorf("client group = %q (updated %v), %v; want %s, not updated since %v", g.BackupRepository, g.UpdatedAt, err, want, before.UpdatedAt)
		}
		if rec, err := f.m.Get(ctx, other); err != nil || rec.Location != s3Location {
			t.Errorf("S3 record = %+v, %v; want it unchanged", rec, err)
		}
		if n, err := repository.RelocateLocal(ctx, db, layout); err != nil || n != 0 {
			t.Errorf("second RelocateLocal = %d, %v; want nothing to do", n, err)
		}
	})
}

func TestEnsureErrors(t *testing.T) {
	t.Parallel()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		ctx := context.Background()
		f := newFixture(t, db, repository.Options{})
		if _, err := f.m.Ensure(ctx, id.New()); !errors.Is(err, repository.ErrClientGroupNotFound) {
			t.Errorf("Ensure for a missing group: err = %v, want ErrClientGroupNotFound", err)
		}
		for _, bad := range []string{"", "../x", "ABC"} {
			if _, err := f.m.Ensure(ctx, bad); !errors.Is(err, repository.ErrInvalidClientGroupID) {
				t.Errorf("Ensure(%q): err = %v, want ErrInvalidClientGroupID", bad, err)
			}
		}
		if n := f.count(t, "secrets") + f.count(t, "backup_repositories"); n != 0 {
			t.Errorf("rows left behind: %d", n)
		}

		// A repository that the stored password doesn't open (say, the
		// state database was restored from an older backup) is left alone.
		groupID := f.group(t, "Acme")
		dir, _ := f.layout.BackupRepositoryDir(groupID)
		if _, err := restic(t, []string{"RESTIC_REPOSITORY=" + dir, "RESTIC_PASSWORD=someone else's"}, "init"); err != nil {
			t.Fatal(err)
		}
		if _, err := f.m.Ensure(ctx, groupID); !errors.Is(err, repository.ErrWrongPassword) {
			t.Errorf("Ensure over a foreign repository: err = %v, want ErrWrongPassword", err)
		}
		if _, err := restic(t, []string{"RESTIC_REPOSITORY=" + dir, "RESTIC_PASSWORD=someone else's"}, "cat", "config"); err != nil {
			t.Errorf("foreign repository was changed: %v", err)
		}
	})
}

// Acceptance (#51): two groups' repositories use different keys.
func TestGroupsUseDifferentKeys(t *testing.T) {
	t.Parallel()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		ctx := context.Background()
		f := newFixture(t, db, repository.Options{})
		a, err := f.m.Ensure(ctx, f.group(t, "Acme"))
		if err != nil {
			t.Fatal(err)
		}
		b, err := f.m.Ensure(ctx, f.group(t, "Beta"))
		if err != nil {
			t.Fatal(err)
		}
		pwA, pwB := f.password(t, a), f.password(t, b)
		if pwA == pwB || len(pwA.Reveal()) < 40 {
			t.Fatalf("passwords are equal or short (%d characters)", len(pwA.Reveal()))
		}
		if a.Location == b.Location || a.SecretRef == b.SecretRef {
			t.Errorf("groups share a repository or secret: %+v, %+v", a, b)
		}
		for _, tc := range []struct {
			repo string
			pw   redact.Secret
			ok   bool
		}{
			{a.Location, pwA, true},
			{b.Location, pwB, true},
			{b.Location, pwA, false},
			{a.Location, pwB, false},
		} {
			_, err := restic(t, []string{"RESTIC_REPOSITORY=" + tc.repo, "RESTIC_PASSWORD=" + tc.pw.Reveal()}, "cat", "config")
			if (err == nil) != tc.ok {
				t.Errorf("opening %s: err = %v, want success %v", tc.repo, err, tc.ok)
			}
		}
	})
}

// taskEnv is the environment a task has with access: its Env, with the
// repository and the secret files (written to a temporary directory) at the
// host paths for what a task sees at the provisioner.Task*Path paths.
func taskEnv(t *testing.T, access repository.TaskAccess) []string {
	t.Helper()
	secrets := t.TempDir()
	for name, v := range access.SecretFiles {
		if err := os.WriteFile(filepath.Join(secrets, name), []byte(v.Reveal()), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	paths := strings.NewReplacer(provisioner.TaskRepositoryPath, access.RepositoryDir, provisioner.TaskSecretsPath, secrets)
	var env []string
	for k, v := range access.Env {
		env = append(env, k+"="+paths.Replace(v))
	}
	return env
}

// backupFile backs up a file into the group's repository with plain restic,
// the way a site task will: with TaskAccess's environment and the
// repository directory (mounted at provisioner.TaskRepositoryPath in a
// task).
func backupFile(t *testing.T, access repository.TaskAccess, dir string) {
	t.Helper()
	if _, err := restic(t, taskEnv(t, access), "backup", "--tag", "site-1", dir); err != nil {
		t.Fatal(err)
	}
}

// Acceptance (#51): the offboarding export can be restored with plain
// restic.
func TestExportRestoresWithPlainRestic(t *testing.T) {
	t.Parallel()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		ctx := context.Background()
		f := newFixture(t, db, repository.Options{})
		groupID := f.group(t, "Acme")
		access, err := f.m.TaskAccess(ctx, groupID)
		if err != nil {
			t.Fatal(err)
		}
		wantDir, _ := f.layout.BackupRepositoryDir(groupID)
		if access.RepositoryDir != wantDir || access.Env["RESTIC_REPOSITORY"] != provisioner.TaskRepositoryPath ||
			len(access.Env) != 2 || access.Env["RESTIC_PASSWORD_FILE"] != provisioner.TaskSecretPath(repository.PasswordFile) ||
			len(access.SecretFiles) != 1 || access.SecretFiles[repository.PasswordFile] == "" {
			t.Fatalf("TaskAccess = %+v", access)
		}

		src := t.TempDir()
		content := []byte("<?php // a theme file\n")
		if err := os.WriteFile(filepath.Join(src, "functions.php"), content, 0o600); err != nil {
			t.Fatal(err)
		}
		backupFile(t, access, src)
		// A stale lock isn't carried over into the export.
		if err := os.WriteFile(filepath.Join(wantDir, "locks", "0000stale"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}

		dest := filepath.Join(f.layout.TmpDir(), "acme-export")
		exp, err := f.m.Export(ctx, groupID, dest)
		if err != nil {
			t.Fatal(err)
		}
		if exp.Dir != dest || exp.RepositoryDir != filepath.Join(dest, "repository") || exp.PasswordFile != filepath.Join(dest, "password.txt") {
			t.Errorf("Export = %+v", exp)
		}
		if info, err := os.Stat(exp.PasswordFile); err != nil || info.Mode().Perm() != 0o600 {
			t.Errorf("password file: %v, %v", info, err)
		}
		readme, err := os.ReadFile(filepath.Join(dest, "README.txt"))
		if err != nil || !strings.Contains(string(readme), groupID) {
			t.Errorf("README: %v", err)
		}
		if entries, _ := os.ReadDir(filepath.Join(exp.RepositoryDir, "locks")); len(entries) != 0 {
			t.Errorf("export has locks: %v", entries)
		}
		// The check ran in a repository task that sees the export and its
		// password file, not in this process.
		var checks int
		for _, c := range f.local.Calls() {
			if slices.Contains(c.Task.Command, "check") {
				checks++
				if c.Repository != groupID || c.Task.TransferDir != dest || c.Task.RepositoryDir != exp.RepositoryDir ||
					c.Task.Env["RESTIC_PASSWORD_FILE"] != provisioner.TaskTransferPath+"/password.txt" || len(c.Task.SecretFiles) != 0 {
					t.Errorf("export check task = %+v", c)
				}
			}
		}
		if checks != 1 {
			t.Errorf("export ran %d check tasks, want 1", checks)
		}

		// Remove the original, then restore from the export with nothing
		// but restic, the exported repository, and its password file.
		if err := f.m.Delete(ctx, groupID); err != nil {
			t.Fatal(err)
		}
		target := t.TempDir()
		env := []string{"RESTIC_REPOSITORY=" + exp.RepositoryDir, "RESTIC_PASSWORD_FILE=" + exp.PasswordFile}
		if _, err := restic(t, env, "restore", "latest", "--tag", "site-1", "--target", target); err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(filepath.Join(target, src, "functions.php"))
		if err != nil || !bytes.Equal(got, content) {
			t.Errorf("restored file = %q, %v; want %q", got, err, content)
		}

		// Destinations that aren't empty or absolute are refused.
		other := f.group(t, "Beta")
		if _, err := f.m.Ensure(ctx, other); err != nil {
			t.Fatal(err)
		}
		for _, bad := range []string{dest, "relative/dir", filepath.Join(t.TempDir(), "elsewhere"), f.layout.TmpDir()} {
			if _, err := f.m.Export(ctx, other, bad); !errors.Is(err, repository.ErrExportDestination) {
				t.Errorf("Export to %s: err = %v, want ErrExportDestination", bad, err)
			}
		}
		if _, err := f.m.Export(ctx, groupID, filepath.Join(f.layout.TmpDir(), "gone")); !errors.Is(err, repository.ErrNoRepository) {
			t.Errorf("Export of a deleted repository: err = %v, want ErrNoRepository", err)
		}
	})
}

func TestDelete(t *testing.T) {
	t.Parallel()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		ctx := context.Background()
		f := newFixture(t, db, repository.Options{})
		groupID := f.group(t, "Acme")
		r, err := f.m.Ensure(ctx, groupID)
		if err != nil {
			t.Fatal(err)
		}

		// Not while the group has sites.
		siteID, now := id.New(), time.Now()
		if _, err := db.ExecContext(ctx, `INSERT INTO sites
			(id, client_group_id, name, slug, status, preview_domain, php_version, wp_core_version, uid, created_at, updated_at)
			VALUES (?, ?, 'Site', 'site', 'running', 'site.preview.example', '8.4', '6.9.1', 20000, ?, ?)`,
			siteID, groupID, now, now); err != nil {
			t.Fatal(err)
		}
		if err := f.m.Delete(ctx, groupID); !errors.Is(err, repository.ErrGroupHasSites) {
			t.Fatalf("Delete with sites: err = %v, want ErrGroupHasSites", err)
		}
		if _, err := os.Stat(r.Location); err != nil {
			t.Fatalf("repository removed despite the refusal: %v", err)
		}
		if _, err := db.ExecContext(ctx, "DELETE FROM sites WHERE id = ?", siteID); err != nil {
			t.Fatal(err)
		}
		// The records of the deleted site's backups go with the repository.
		if err := store.NewBackupRecords(db).Create(ctx, &store.BackupRecord{SiteID: siteID, ClientGroupID: groupID, Kind: store.BackupManual}); err != nil {
			t.Fatal(err)
		}

		if err := f.m.Delete(ctx, groupID); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(r.Location); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("repository directory still exists: %v", err)
		}
		if n := f.count(t, "secrets") + f.count(t, "backup_repositories") + f.count(t, "backup_records"); n != 0 {
			t.Errorf("rows left behind: %d", n)
		}
		if g, err := store.NewClientGroups(db).Get(ctx, groupID); err != nil || g.BackupRepository != "" {
			t.Errorf("client group backup_repository = %q, %v; want empty", g.BackupRepository, err)
		}
		// Idempotent, and the group can now be deleted.
		if err := f.m.Delete(ctx, groupID); err != nil {
			t.Errorf("second Delete: %v", err)
		}
		if err := store.NewClientGroups(db).Delete(ctx, groupID); err != nil {
			t.Errorf("deleting the group: %v", err)
		}
	})
}

// Restic runs group-level commands against the stored repository.
func TestRestic(t *testing.T) {
	t.Parallel()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		ctx := context.Background()
		f := newFixture(t, db, repository.Options{})
		groupID := f.group(t, "Acme")
		if _, err := f.m.Restic(ctx, groupID, "snapshots"); !errors.Is(err, repository.ErrNoRepository) {
			t.Errorf("Restic before Ensure: err = %v, want ErrNoRepository", err)
		}
		access, err := f.m.TaskAccess(ctx, groupID)
		if err != nil {
			t.Fatal(err)
		}
		src := t.TempDir()
		if err := os.WriteFile(filepath.Join(src, "a.txt"), []byte("streamed\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		backupFile(t, access, src)
		out, err := f.m.Restic(ctx, groupID, "snapshots", "--json")
		if err != nil || !strings.Contains(string(out), `"site-1"`) {
			t.Errorf("snapshots = %s, %v", out, err)
		}
		var dumped strings.Builder
		if err := f.m.ResticTo(ctx, groupID, &dumped, "dump", "latest", filepath.Join(src, "a.txt")); err != nil || dumped.String() != "streamed\n" {
			t.Errorf("ResticTo dump = %q, %v", dumped.String(), err)
		}
		if err := f.m.ResticTo(ctx, groupID, &dumped, "dump", "latest", "/nonexistent"); err == nil {
			t.Error("ResticTo of a failing command succeeded")
		}
		if _, err := f.m.Restic(ctx, groupID, "check"); err != nil {
			t.Errorf("check: %v", err)
		}
	})
}

// A restic task gets the repository password in a secret file, never in the
// environment or the arguments, and no part of the control plane's own
// environment.
func TestResticTaskSecrets(t *testing.T) {
	t.Setenv("PRESSFLEET_MASTER_KEY", "pfmk1:do-not-leak")
	t.Setenv("RESTIC_PASSWORD", "inherited")
	fake := provisionertest.New()
	fake.RunRepositoryTaskFunc = func(string, provisioner.Task) (*provisioner.TaskResult, error) {
		return &provisioner.TaskResult{}, nil
	}
	db := storetest.Open(t, store.SQLite)
	f := newFixture(t, db, repository.Options{Tasks: fake, TaskImage: "pressfleet/control-plane:test"})
	groupID := f.group(t, "Acme")
	r, err := f.m.Ensure(context.Background(), groupID)
	if err != nil {
		t.Fatal(err)
	}
	pw := f.password(t, r).Reveal()
	var ran int
	for _, c := range fake.Calls() {
		if c.Op != "RunRepositoryTask" {
			continue
		}
		ran++
		task := *c.Task
		if c.Repository != groupID || task.Image != "pressfleet/control-plane:test" {
			t.Errorf("task for repository %q with image %q", c.Repository, task.Image)
		}
		if got := task.SecretFiles[repository.PasswordFile].Reveal(); got != pw {
			t.Errorf("password file = %q, want the repository password", got)
		}
		if task.Env["RESTIC_PASSWORD_FILE"] != provisioner.TaskSecretPath(repository.PasswordFile) || task.Env["RESTIC_REPOSITORY"] != provisioner.TaskRepositoryPath {
			t.Errorf("env = %v", task.Env)
		}
		if task.RepositoryDir != r.Location || task.BackupNetwork {
			t.Errorf("repository dir %q, backup network %v; want %q, false", task.RepositoryDir, task.BackupNetwork, r.Location)
		}
		all := strings.Join(task.Command, "\n")
		for k, v := range task.Env {
			all += "\n" + k + "=" + v
		}
		for _, leak := range []string{pw, "do-not-leak", "inherited"} {
			if strings.Contains(all, leak) {
				t.Errorf("%q reaches the task's command or environment:\n%s", leak, all)
			}
		}
		if task.Command[0] != "restic" || task.Command[1] != "--no-cache" || task.WPContent != provisioner.MountNone {
			t.Errorf("command = %v, wp-content %v", task.Command, task.WPContent)
		}
	}
	if ran == 0 {
		t.Fatal("Ensure ran no repository task")
	}
}

func TestNewOptions(t *testing.T) {
	db := storetest.Open(t, store.SQLite)
	base := repository.Options{DB: db, Logger: storetest.Logger(), Layout: config.Layout{Root: t.TempDir()}}
	for name, modify := range map[string]func(*repository.Options){
		"relative data dir": func(o *repository.Options) { o.Layout.Root = "data" },
		"unknown backend":   func(o *repository.Options) { o.Backend = "ftp" },
		"s3 without bucket": func(o *repository.Options) { o.Backend = repository.BackendS3 },
		"s3 with a bad URL": func(o *repository.Options) {
			o.Backend = repository.BackendS3
			o.S3 = repository.S3Config{Endpoint: "minio:9000", Bucket: "b"}
		},
		"no logger": func(o *repository.Options) { o.Logger = nil },
		"s3 with bad prefix": func(o *repository.Options) {
			o.Backend = repository.BackendS3
			o.S3 = repository.S3Config{Endpoint: "http://m", Bucket: "b", Prefix: "/x"}
		},
	} {
		opts := base
		modify(&opts)
		if _, err := repository.New(opts); err == nil {
			t.Errorf("%s: New succeeded", name)
		}
	}
}

func TestHooks(t *testing.T) {
	t.Parallel()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		ctx := context.Background()
		var created, deleted []string
		fail := errors.New("scheduling failed")
		var hookErr error
		f := newFixture(t, db, repository.Options{Deleted: func(_ context.Context, _ store.Querier, groupID string) error {
			deleted = append(deleted, groupID)
			return hookErr
		}, Created: func(ctx context.Context, q store.Querier, groupID string) error {
			// In the transaction: the record is visible through q.
			if ids, err := repository.ClientGroupIDs(ctx, q); err != nil || !slices.Contains(ids, groupID) {
				t.Errorf("in the hook, ClientGroupIDs = %v, %v; want %s", ids, err, groupID)
			}
			created = append(created, groupID)
			return hookErr
		}})
		acme, globex := f.group(t, "Acme"), f.group(t, "Globex")

		if ids, err := repository.ClientGroupIDs(ctx, db); err != nil || len(ids) != 0 {
			t.Errorf("ClientGroupIDs before any repository = %v, %v", ids, err)
		}
		// A failing hook rolls the record back, and the next Ensure tries
		// again.
		hookErr = fail
		if _, err := f.m.Ensure(ctx, acme); !errors.Is(err, fail) {
			t.Errorf("Ensure with a failing hook = %v", err)
		}
		if _, err := f.m.Get(ctx, acme); !errors.Is(err, repository.ErrNoRepository) {
			t.Errorf("Get after the hook failed = %v, want ErrNoRepository", err)
		}
		hookErr = nil
		for _, g := range []string{acme, acme, globex} {
			if _, err := f.m.Ensure(ctx, g); err != nil {
				t.Fatal(err)
			}
		}
		// Once per new repository.
		if want := []string{acme, acme, globex}; !slices.Equal(created, want) {
			t.Errorf("hook calls = %v, want %v", created, want)
		}
		want := []string{acme, globex}
		slices.Sort(want)
		if ids, err := repository.ClientGroupIDs(ctx, db); err != nil || !slices.Equal(ids, want) {
			t.Errorf("ClientGroupIDs = %v, %v; want %v", ids, err, want)
		}

		// A failing Deleted hook keeps the record, so Delete can be
		// retried; deleting a group without a repository calls nothing.
		hookErr = fail
		if err := f.m.Delete(ctx, acme); !errors.Is(err, fail) {
			t.Errorf("Delete with a failing hook = %v", err)
		}
		if _, err := f.m.Get(ctx, acme); err != nil {
			t.Errorf("Get after the hook failed = %v", err)
		}
		hookErr = nil
		for _, g := range []string{acme, acme} {
			if err := f.m.Delete(ctx, g); err != nil {
				t.Fatal(err)
			}
		}
		if want := []string{acme, acme}; !slices.Equal(deleted, want) {
			t.Errorf("Deleted hook calls = %v, want %v", deleted, want)
		}
	})
}
