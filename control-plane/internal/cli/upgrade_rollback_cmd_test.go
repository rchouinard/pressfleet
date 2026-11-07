package cli_test

import (
	"errors"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/rchouinard/pressfleet/control-plane/internal/store"
)

// upgraded returns an environment upgraded from v1.2.2 to v1.2.3-test,
// with a migration of the state database if dialect isn't empty, and the
// platform project from before the upgrade. It returns the pre-migration
// backup (SQLite only).
func upgraded(t *testing.T, dialect store.Dialect) (e *upgradeEnv, original, backup string) {
	t.Helper()
	e = newUpgradeEnv(t, "v1.2.2")
	original = readFile(t, e.path("platform", "compose.yaml"))
	if dialect == store.Postgres {
		e.usePostgres()
	}
	if dialect != "" {
		backup = e.migrateOnUp(dialect)
	}
	if r := e.upgrade("", "--yes"); r.code != 0 {
		t.Fatalf("upgrade: exit %d\n%s\n%s", r.code, r.stdout, r.stderr)
	}
	e.platform.onUp = nil
	return e, original, backup
}

const blockingChanges = `{"since":"2026-11-05T12:00:00Z",
"blocking":[{"kind":"site.create","at":"2026-11-05T12:05:00Z","job_id":"job-1","job_state":"succeeded","site":{"id":"s1","slug":"acme"}},
  {"kind":"site.core_update","at":"2026-11-05T12:06:00Z","job_id":"job-2","job_state":"succeeded","site":{"id":"s2","slug":"blog"}}],
"converged":[{"kind":"site.template_update","at":"2026-11-05T12:07:00Z","job_id":"job-3","job_state":"succeeded","site":{"id":"s2","slug":"blog"}}],
"lost":[{"kind":"user.create","at":"2026-11-05T12:08:00Z","audit_event_id":"ev-1","target_type":"user","target_id":"u1"}]}`

// unchanged checks that a refused --rollback changed nothing: the upgrade
// is still the succeeded one.
func (e *upgradeEnv) unchanged(r result) {
	e.t.Helper()
	if r.code != 1 {
		e.t.Fatalf("exit %d, want 1\n%s\n%s", r.code, r.stdout, r.stderr)
	}
	if len(e.platform.stops) != 0 || len(e.platform.runs) != 0 {
		e.t.Errorf("a refused rollback stopped %v and ran %v", e.platform.stops, e.platform.runs)
	}
	if status, _ := e.rollback(); status != "succeeded" {
		e.t.Errorf("status %q, want succeeded", status)
	}
	if e.platform.version != "v1.2.3-test" {
		e.t.Errorf("the control plane runs %s", e.platform.version)
	}
}

func TestRollbackAfterUpgrade(t *testing.T) {
	e, original, backup := upgraded(t, store.SQLite)

	r := e.upgrade("", "--rollback", "--yes")
	e.checkRolledBack(r, original)
	for _, want := range []string{
		"Rolling back the upgrade from v1.2.2 to v1.2.3-test",
		"What the control plane changed since",
		"The state database is restored from " + backup,
		"restored from " + backup,
	} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("stdout lacks %q:\n%s", want, r.stdout)
		}
	}
	if len(e.platform.changesAsked) != 1 || e.platform.changesAsked[0][0] != "--since" {
		t.Errorf("admin changes asked with %v", e.platform.changesAsked)
	}
	if !slices.Equal(e.platform.runs[0].command, []string{"state", "restore", backup}) {
		t.Errorf("runs = %+v", e.platform.runs)
	}
	if _, rb := e.rollback(); !strings.Contains(rb.Cause, "--rollback") {
		t.Errorf("rollback = %+v", rb)
	}

	// Only once.
	r = e.upgrade("", "--rollback", "--yes")
	if r.code != 1 || !strings.Contains(r.stderr, "rolled back already") {
		t.Errorf("second --rollback: exit %d\n%s\n%s", r.code, r.stdout, r.stderr)
	}
}

func TestRollbackRefusals(t *testing.T) {
	t.Run("no record", func(t *testing.T) {
		e := newUpgradeEnv(t, "v1.2.2")
		r := e.upgrade("", "--rollback", "--yes")
		if r.code != 1 || !strings.Contains(r.stderr, "nothing to roll back") {
			t.Errorf("exit %d\n%s\n%s", r.code, r.stdout, r.stderr)
		}
	})
	t.Run("no saved project", func(t *testing.T) {
		e, _, _ := upgraded(t, store.SQLite)
		if err := os.RemoveAll(e.path("upgrade", "previous-platform")); err != nil {
			t.Fatal(err)
		}
		r := e.upgrade("", "--rollback", "--yes")
		e.unchanged(r)
		if !strings.Contains(r.stderr, "isn't in") {
			t.Errorf("stderr:\n%s", r.stderr)
		}
	})
	t.Run("force without rollback", func(t *testing.T) {
		e := newUpgradeEnv(t, "v1.2.2")
		if r := e.upgrade("", "--force"); r.code != 1 || !strings.Contains(r.stderr, "--force applies only to --rollback") {
			t.Errorf("exit %d\n%s", r.code, r.stderr)
		}
	})
	t.Run("with no-rollback", func(t *testing.T) {
		e := newUpgradeEnv(t, "v1.2.2")
		if r := e.upgrade("", "--rollback", "--no-rollback"); r.code != 1 || !strings.Contains(r.stderr, "can't be combined") {
			t.Errorf("exit %d\n%s", r.code, r.stderr)
		}
	})
}

func TestRollbackBlockingChanges(t *testing.T) {
	e, original, _ := upgraded(t, store.SQLite)
	e.platform.changes = blockingChanges

	r := e.upgrade("", "--rollback", "--yes")
	e.unchanged(r)
	for _, want := range []string{"site.create  site acme (job job-1, succeeded)", "site.template_update  site blog",
		"user.create  user u1"} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("stdout lacks %q:\n%s", want, r.stdout)
		}
	}
	for _, want := range []string{"site acme was created after the upgrade", "site blog had its WordPress core updated", "--force"} {
		if !strings.Contains(r.stderr, want) {
			t.Errorf("stderr lacks %q:\n%s", want, r.stderr)
		}
	}

	r = e.upgrade("", "--rollback", "--yes", "--force")
	e.checkRolledBack(r, original)
	if !strings.Contains(r.stdout, "rolling back anyway (--force) leaves behind") {
		t.Errorf("stdout doesn't say what --force leaves behind:\n%s", r.stdout)
	}
}

func TestRollbackWithoutMigration(t *testing.T) {
	for _, dialect := range []store.Dialect{store.SQLite, store.Postgres} {
		t.Run(string(dialect), func(t *testing.T) {
			e := newUpgradeEnv(t, "v1.2.2")
			original := readFile(t, e.path("platform", "compose.yaml"))
			if dialect == store.Postgres {
				e.usePostgres()
			}
			if r := e.upgrade("", "--yes"); r.code != 0 {
				t.Fatalf("upgrade: exit %d\n%s\n%s", r.code, r.stdout, r.stderr)
			}
			// Blocking changes don't matter: the database isn't restored.
			e.platform.changes = blockingChanges

			r := e.upgrade("", "--rollback", "--yes")
			e.checkRolledBack(r, original)
			if len(e.platform.changesAsked) != 0 || len(e.platform.runs) != 0 {
				t.Errorf("asked changes %v, ran %v", e.platform.changesAsked, e.platform.runs)
			}
			for _, want := range []string{"didn't migrate the state database", "nothing recorded is lost"} {
				if !strings.Contains(r.stdout, want) {
					t.Errorf("stdout lacks %q:\n%s", want, r.stdout)
				}
			}
		})
	}
}

func TestRollbackMigratedPostgres(t *testing.T) {
	e, _, _ := upgraded(t, store.Postgres)

	r := e.upgrade("", "--rollback", "--yes")
	e.unchanged(r)
	for _, want := range []string{"migrated the PostgreSQL state database", "docker compose", "backup you took before the upgrade"} {
		if !strings.Contains(r.stderr, want) {
			t.Errorf("stderr lacks %q:\n%s", want, r.stderr)
		}
	}
}

func TestRollbackConfirmation(t *testing.T) {
	e, original, backup := upgraded(t, store.SQLite)

	r := e.upgrade("n\n", "--rollback")
	e.unchanged(r)
	for _, want := range []string{"Roll back to v1.2.2? [y/N]", "the release before the upgrade to v1.2.3-test that started at",
		"restored from " + backup, "everything the control plane recorded since is lost"} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("stdout lacks %q:\n%s", want, r.stdout)
		}
	}
	if !strings.Contains(r.stderr, "rollback canceled") {
		t.Errorf("stderr:\n%s", r.stderr)
	}

	r = e.upgrade("y\n", "--rollback")
	e.checkRolledBack(r, original)
}

func TestRollbackControlPlaneNotAnswering(t *testing.T) {
	t.Run("succeeded", func(t *testing.T) {
		e, original, _ := upgraded(t, store.SQLite)
		e.platform.infoErr = errors.New("connection refused")
		r := e.upgrade("", "--rollback", "--yes")
		e.unchanged(r)
		if !strings.Contains(r.stderr, "can't tell what changed since the upgrade") {
			t.Errorf("stderr:\n%s", r.stderr)
		}

		e.platform.onStop = func() { e.platform.infoErr = nil }
		r = e.upgrade("", "--rollback", "--yes", "--force")
		e.checkRolledBack(r, original)
	})
	t.Run("failed with no-rollback", func(t *testing.T) {
		e := newUpgradeEnv(t, "v1.2.2")
		original := readFile(t, e.path("platform", "compose.yaml"))
		e.migrateOnUp(store.SQLite)
		e.platform.upErr = errors.New("container pressfleet-control-plane-1 is unhealthy")
		if r := e.upgrade("", "--no-rollback"); r.code != 1 {
			t.Fatalf("upgrade: exit %d\n%s\n%s", r.code, r.stdout, r.stderr)
		}
		e.platform.onUp = nil
		e.platform.infoErr = errors.New("connection refused")
		e.platform.onStop = func() { e.platform.infoErr = nil }

		r := e.upgrade("", "--rollback", "--yes")
		e.checkRolledBack(r, original)
		if !strings.Contains(r.stdout, "held the WordPress core, image, and template rollouts") {
			t.Errorf("stdout doesn't say why skipping the check is safe:\n%s", r.stdout)
		}
		if len(e.platform.changesAsked) != 0 {
			t.Errorf("asked changes %v", e.platform.changesAsked)
		}
	})
}
