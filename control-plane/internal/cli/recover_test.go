package cli_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/rchouinard/pressfleet/control-plane/internal/adminlogin"
	"github.com/rchouinard/pressfleet/control-plane/internal/backup/repository"
	"github.com/rchouinard/pressfleet/control-plane/internal/backup/statebackup"
	"github.com/rchouinard/pressfleet/control-plane/internal/cli"
	"github.com/rchouinard/pressfleet/control-plane/internal/config"
	"github.com/rchouinard/pressfleet/control-plane/internal/doctor/doctortest"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner/compose"
	"github.com/rchouinard/pressfleet/control-plane/internal/secrets"
	"github.com/rchouinard/pressfleet/control-plane/internal/secrets/secretstest"
	"github.com/rchouinard/pressfleet/control-plane/internal/sites"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
)

// oldHost is a platform's data directory with a state backup in its state
// repository: one client group, one running site, and the login key.
func oldHost(t *testing.T) (dataDir, siteID, loginKey string) {
	t.Helper()
	ctx := context.Background()
	dataDir = t.TempDir()
	keys := secretstest.Keyring(t, 1)
	seedStateDB(t, dataDir, func(db *store.DB) {
		if _, err := secrets.Verify(ctx, db, keys); err != nil {
			t.Fatal(err)
		}
		sec := secrets.NewStore(db, keys)
		signer, err := adminlogin.Ensure(ctx, db, sec)
		if err != nil {
			t.Fatal(err)
		}
		loginKey = signer.PublicKeyBase64()
		groups, err := store.NewClientGroups(db).List(ctx, store.ClientGroupFilter{})
		if err != nil {
			t.Fatal(err)
		}
		site := &store.Site{
			ClientGroupID: groups[0].ID, Name: "Acme", Slug: "acme", Status: store.SiteRunning,
			PreviewDomain: "acme.preview.example", PHPVersion: "8.4", WPCoreVersion: "7.1.2", CPULimit: "1.0",
			MemoryReservationMB: 128, MemoryLimitMB: 512, UID: 20001, WPSaltsSecretRef: "salts", AdminUsername: "admin",
			AdminEmail: "admin@example.com",
		}
		if err := store.NewSites(db).Create(ctx, site); err != nil {
			t.Fatal(err)
		}
		siteID = site.ID
		layout := config.Layout{Root: dataDir}
		repo, err := repository.New(repository.Options{
			Layout: layout, StatePassword: keys.StateBackupPassword(), Logger: storetest.Logger(),
		})
		if err != nil {
			t.Fatal(err)
		}
		svc, err := statebackup.New(statebackup.Options{DB: db, Repository: repo, TmpDir: layout.TmpDir(), Logger: storetest.Logger()})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := svc.Backup(ctx); err != nil {
			t.Fatal(err)
		}
	})
	return dataDir, siteID, loginKey
}

// newHost is a fresh installation's data directory with the old host's
// backups copied in.
func newHost(t *testing.T, oldDataDir string) string {
	t.Helper()
	dataDir := t.TempDir()
	src := config.Layout{Root: oldDataDir}.StateBackupRepositoryDir()
	dst := config.Layout{Root: dataDir}.StateBackupRepositoryDir()
	if err := os.CopyFS(dst, os.DirFS(src)); err != nil {
		t.Fatal(err)
	}
	cfg := store.Config{Driver: store.SQLite, SQLitePath: filepath.Join(dataDir, "state", "pressfleet.db")}
	db := storetest.OpenUnmigrated(t, cfg)
	if _, err := db.Migrate(context.Background(), t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if _, err := secrets.Verify(context.Background(), db, secretstest.Keyring(t, 1)); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	return dataDir
}

func TestStateRecover(t *testing.T) {
	ctx := context.Background()
	old, siteID, loginKey := oldHost(t)
	dataDir := newHost(t, old)

	r := run(t, ctx, []string{"state", "backups", "--data-dir", dataDir}, nil)
	if r.code != 0 || !strings.Contains(r.stdout, "State backups in "+filepath.Join(dataDir, "backups", "state")) || !strings.Contains(r.stdout, "sqlite") {
		t.Fatalf("state backups: exit %d\n%s%s", r.code, r.stdout, r.stderr)
	}

	// Another master key opens neither the repository nor the state.
	r = run(t, ctx, []string{"state", "recover", "--data-dir", dataDir}, map[string]string{"PRESSFLEET_MASTER_KEY": secretstest.KeyText(t, 2)})
	if r.code != 1 || !strings.Contains(r.stderr, "password") {
		t.Errorf("recover with another master key: exit %d, stderr: %s", r.code, r.stderr)
	}

	r = run(t, ctx, []string{"state", "recover", "--data-dir", dataDir}, nil)
	if r.code != 0 {
		t.Fatalf("recover: exit %d, stderr: %s", r.code, r.stderr)
	}
	for _, want := range []string{
		"Restoring state backup ",
		"opens with this installation's master key",
		"login key was restored and decrypts with the master key",
		"1 sites will be recovered",
	} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("stdout missing %q:\n%s", want, r.stdout)
		}
	}

	cfg := store.Config{Driver: store.SQLite, SQLitePath: filepath.Join(dataDir, "state", "pressfleet.db")}
	db := storetest.OpenUnmigrated(t, cfg)
	site, err := store.NewSites(db).Get(ctx, siteID)
	if err != nil || site.Status != store.SiteUpdating {
		t.Fatalf("restored site = %+v, %v", site, err)
	}
	page, err := store.NewJobs(db).List(ctx, store.JobFilter{Types: []string{sites.RecoverJobType}, Limit: 10})
	if err != nil || len(page.Jobs) != 1 || page.Jobs[0].TargetID != siteID {
		t.Fatalf("recover jobs = %+v, %v", page.Jobs, err)
	}
	signer, err := adminlogin.Ensure(ctx, db, secrets.NewStore(db, secretstest.Keyring(t, 1)))
	if err != nil || signer.PublicKeyBase64() != loginKey {
		t.Errorf("login key after the restore: %v (same as before: %v)", err, err == nil && signer.PublicKeyBase64() == loginKey)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	// Now the state has a site: a second recover needs --force.
	r = run(t, ctx, []string{"state", "recover", "--data-dir", dataDir}, nil)
	if r.code != 1 || !strings.Contains(r.stderr, "already has client groups or sites") {
		t.Errorf("recover over existing state: exit %d, stderr: %s", r.code, r.stderr)
	}
	r = run(t, ctx, []string{"state", "recover", "--force", "--snapshot", "nonexistent", "--data-dir", dataDir}, nil)
	if r.code != 1 || !strings.Contains(r.stderr, "no state backup has that ID") {
		t.Errorf("recover of an unknown backup: exit %d, stderr: %s", r.code, r.stderr)
	}
	r = run(t, ctx, []string{"state", "recover", "--force", "--data-dir", dataDir}, nil)
	if r.code != 0 || !strings.Contains(r.stdout, "The replaced database was saved as") {
		t.Errorf("recover --force: exit %d\n%s%s", r.code, r.stdout, r.stderr)
	}
}

// fakeRecoverProject records what recover asks of the platform project.
type fakeRecoverProject struct {
	mu     sync.Mutex
	calls  []string
	runOut string
	runErr error
}

func (f *fakeRecoverProject) project(string, string) cli.RecoverProject { return f }

func (f *fakeRecoverProject) StopServices(_ context.Context, services ...string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "stop "+strings.Join(services, " "))
	return nil
}

func (f *fakeRecoverProject) RunService(_ context.Context, service string, command ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "run "+service+" "+strings.Join(command, " "))
	return []byte(f.runOut), f.runErr
}

func (f *fakeRecoverProject) Up(_ context.Context, opts compose.UpOptions) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "up "+strings.Join(opts.Services, " "))
	return nil
}

func runRecover(t *testing.T, cp *fakeControlPlane, p *fakeRecoverProject, args ...string) result {
	t.Helper()
	var stdout, stderr syncBuffer
	emptyConfig := filepath.Join(t.TempDir(), "pressfleet.yaml")
	if err := os.WriteFile(emptyConfig, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	code := cli.Run(context.Background(), append([]string{"recover", "--data-dir", t.TempDir()}, args...), cli.Options{
		Version: "v1.2.3",
		Stdout:  &stdout,
		Stderr:  &stderr,
		Env: func(k string) (string, bool) {
			if k == config.EnvConfigPath {
				return emptyConfig, true
			}
			return "", false
		},
		HostProbe:      doctortest.Healthy(),
		DockerProbe:    doctortest.HealthyDocker(),
		ControlPlane:   cp.probe,
		RecoverProject: p.project,
	})
	return result{code, stdout.String(), stderr.String()}
}

func TestRecover(t *testing.T) {
	p := &fakeRecoverProject{runOut: "Restored 42 rows in 40 tables.\n"}
	r := runRecover(t, running("v1.2.3"), p, "--snapshot", "abcdef12")
	if r.code != 0 {
		t.Fatalf("exit %d, stderr: %s", r.code, r.stderr)
	}
	want := []string{"stop control-plane", "run control-plane state recover --snapshot abcdef12", "up control-plane"}
	if !slices.Equal(p.calls, want) {
		t.Errorf("calls = %q, want %q", p.calls, want)
	}
	if !strings.Contains(r.stdout, "Restored 42 rows") || !strings.Contains(r.stdout, "recreating the sites") {
		t.Errorf("stdout:\n%s", r.stdout)
	}

	// A failed restore still starts the control plane again.
	p = &fakeRecoverProject{runErr: errors.New("the state database already has client groups or sites")}
	r = runRecover(t, running("v1.2.3"), p, "--force")
	want = []string{"stop control-plane", "run control-plane state recover --snapshot latest --force", "up control-plane"}
	if r.code != 1 || !slices.Equal(p.calls, want) || !strings.Contains(r.stderr, "nothing was changed") {
		t.Errorf("failed restore: exit %d, calls %q, stderr: %s", r.code, p.calls, r.stderr)
	}

	// Without a platform there is nothing to restore into.
	p = &fakeRecoverProject{}
	r = runRecover(t, &fakeControlPlane{}, p)
	if r.code != 1 || len(p.calls) != 0 || !strings.Contains(r.stderr, "run pressfleet init first") {
		t.Errorf("not installed: exit %d, calls %q, stderr: %s", r.code, p.calls, r.stderr)
	}
}
