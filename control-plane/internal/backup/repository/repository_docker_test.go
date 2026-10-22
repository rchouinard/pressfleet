package repository_test

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/backup/repository"
	"github.com/rchouinard/pressfleet/control-plane/internal/catalog"
	"github.com/rchouinard/pressfleet/control-plane/internal/config"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner/compose"
	"github.com/rchouinard/pressfleet/control-plane/internal/redact"
	"github.com/rchouinard/pressfleet/control-plane/internal/secrets"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
)

// noSecrets and noImages stand in for the site parts of the Compose driver,
// which repository tasks don't use.
type noSecrets struct{}

func (noSecrets) Get(context.Context, secrets.Ref) (redact.Secret, error) {
	return "", errors.New("no secrets")
}

type noImages struct{}

func (noImages) Images(string, string) (catalog.Images, error) {
	return catalog.Images{}, errors.New("no images")
}

// dockerFixture is a fixture whose repository tasks run in real containers
// of the control plane image, as in production. Skipped unless
// PRESSFLEET_DOCKER_TESTS=1 and PRESSFLEET_TEST_CONTROL_PLANE_IMAGE names a
// local control plane image (make image).
func dockerFixture(t *testing.T, db *store.DB) (f fixture, prov *compose.Provisioner, image string) {
	t.Helper()
	if os.Getenv("PRESSFLEET_DOCKER_TESTS") != "1" {
		t.Skip("set PRESSFLEET_DOCKER_TESTS=1 to run tests against Docker")
	}
	image = os.Getenv("PRESSFLEET_TEST_CONTROL_PLANE_IMAGE")
	if image == "" {
		t.Skip("set PRESSFLEET_TEST_CONTROL_PLANE_IMAGE to a locally built control plane image (make image)")
	}
	layout := config.Layout{Root: t.TempDir()}
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	prov, err := compose.NewProvisioner(compose.ProvisionerConfig{
		Layout: layout, Secrets: noSecrets{}, Images: noImages{}, MariaDBContainer: "pf-test-no-mariadb",
		TaskImage: image, Logger: storetest.Logger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	// The repositories' files belong to UID 60000, which a test running
	// as an ordinary user can't remove.
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if _, err := compose.DockerExec(ctx, nil, "run", "--rm", "--user", "0:0", "--label", "pressfleet.test=true",
			"--mount", "type=bind,source="+layout.Root+",target=/data", "--entrypoint", "rm", image, "-rf", "/data/backups", "/data/tmp"); err != nil {
			t.Errorf("cleanup: removing repositories: %v", err)
		}
	})
	f = newFixture(t, db, repository.Options{
		Layout: layout, Tasks: prov, TaskImage: image, StatePassword: "pf-docker-state-password",
	})
	return f, prov, image
}

func owner(t *testing.T, p string) (gid int, mode fs.FileMode) {
	t.Helper()
	info, err := os.Lstat(p)
	if err != nil {
		t.Fatal(err)
	}
	return int(info.Sys().(*syscall.Stat_t).Gid), info.Mode()
}

// Repository tasks, in real containers, create and use a repository that
// site tasks share through the repository group, and map restic's exit codes.
func TestRepositoryTasksDocker(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		ctx := context.Background()
		f, prov, image := dockerFixture(t, db)
		groupID := f.group(t, "Acme")

		// init in a task as UID 60000, then shared with the group.
		access, err := f.m.TaskAccess(ctx, groupID)
		if err != nil {
			t.Fatal(err)
		}
		err = filepath.WalkDir(access.RepositoryDir, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			gid, mode := owner(t, p)
			if gid != provisioner.RepositoryGID {
				t.Errorf("%s belongs to group %d, want %d", p, gid, provisioner.RepositoryGID)
			}
			if d.IsDir() && mode&fs.ModeSetgid == 0 {
				t.Errorf("directory %s is not setgid: %v", p, mode)
			}
			if !d.IsDir() && mode.Perm()&0o040 == 0 {
				t.Errorf("file %s mode %v is not group readable", p, mode)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}

		out, err := f.m.Restic(ctx, groupID, "snapshots", "--json")
		if err != nil || strings.TrimSpace(string(out)) != "[]" {
			t.Errorf("snapshots = %q, %v", out, err)
		}
		if _, err := f.m.Restic(ctx, groupID, "check"); err != nil {
			t.Errorf("check: %v", err)
		}
		// What a prune task writes, as UID 60000, stays readable by the group.
		if _, err := f.m.Restic(ctx, groupID, "prune"); err != nil {
			t.Errorf("prune: %v", err)
		}
		err = filepath.WalkDir(access.RepositoryDir, func(p string, d fs.DirEntry, err error) error {
			if gid, mode := owner(t, p); err == nil && !d.IsDir() && (gid != provisioner.RepositoryGID || mode.Perm()&0o040 == 0) {
				t.Errorf("%s written by prune: group %d mode %v", p, gid, mode)
			}
			return err
		})
		if err != nil {
			t.Fatal(err)
		}

		// A manager with another state password gets restic's exit code 12.
		wrong, err := repository.New(repository.Options{
			Layout: f.layout, Tasks: prov, TaskImage: image, StatePassword: "another password", Logger: storetest.Logger(),
		})
		if err != nil {
			t.Fatal(err)
		}

		// The state repository, in the same way, and the wrong password.
		if err := f.m.EnsureState(ctx); err != nil {
			t.Fatal(err)
		}
		var buf bytes.Buffer
		if err := f.m.StateRestic(ctx, nil, &buf, "snapshots", "--json"); err != nil || strings.TrimSpace(buf.String()) != "[]" {
			t.Errorf("state snapshots = %q, %v", buf.String(), err)
		}
		if gid, _ := owner(t, f.layout.StateBackupRepositoryDir()); gid != provisioner.RepositoryGID {
			t.Errorf("state repository belongs to group %d", gid)
		}
		if err := wrong.StateRestic(ctx, nil, &buf, "snapshots"); !errors.Is(err, repository.ErrWrongPassword) {
			t.Errorf("state snapshots with another password = %v, want ErrWrongPassword", err)
		}
		// Standard input reaches restic.
		buf.Reset()
		if err := f.m.StateRestic(ctx, strings.NewReader("a state dump"), &buf, "backup", "--quiet", "--stdin", "--stdin-filename", "state.jsonl"); err != nil {
			t.Errorf("state backup: %v", err)
		}
		buf.Reset()
		if err := f.m.StateRestic(ctx, nil, &buf, "dump", "latest", "/state.jsonl"); err != nil || buf.String() != "a state dump" {
			t.Errorf("state dump = %q, %v", buf.String(), err)
		}

		// Export: the check runs in a task over the copy, which comes back
		// to its owner.
		dest := filepath.Join(f.layout.TmpDir(), "acme-export")
		exp, err := f.m.Export(ctx, groupID, dest)
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range []string{exp.Dir, exp.RepositoryDir, exp.PasswordFile, filepath.Join(exp.RepositoryDir, "config")} {
			info, err := os.Stat(p)
			if err != nil {
				t.Fatal(err)
			}
			if uid := int(info.Sys().(*syscall.Stat_t).Uid); uid != os.Geteuid() {
				t.Errorf("%s belongs to user %d after the check, want %d", p, uid, os.Geteuid())
			}
		}
	})
}
