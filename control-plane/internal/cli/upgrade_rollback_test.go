package cli_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/cli"
	"github.com/rchouinard/pressfleet/control-plane/internal/platform"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
)

// usePostgres makes the environment's state database PostgreSQL. The CLI
// never opens it, so no server is needed.
func (e *upgradeEnv) usePostgres() {
	e.env["PRESSFLEET_DATABASE_DRIVER"] = "postgres"
	e.env["PRESSFLEET_DATABASE_POSTGRES_DSN"] = "postgres://pressfleet@db/pressfleet"
}

// migrateOnUp makes the upgraded control plane migrate its state database
// once, as it starts: it records the run in a marker, and with SQLite backs
// the database up first. It returns the backup's path (empty with
// PostgreSQL).
func (e *upgradeEnv) migrateOnUp(dialect store.Dialect) string {
	e.t.Helper()
	backups := e.path("state", "backups")
	if err := os.MkdirAll(backups, 0o700); err != nil {
		e.t.Fatal(err)
	}
	now := time.Now().UTC()
	stamp := now.Format("20060102T150405.000Z")
	m := store.MigrationMarker{
		Status: store.MigrationSucceeded, StartedAt: now, FinishedAt: now, Version: "v1.2.3-test", Dialect: dialect,
		FromSchema: 29, ToSchema: 31, ReachedSchema: 31,
	}
	files := map[string][]byte{}
	if dialect == store.SQLite {
		m.Backup = filepath.Join(backups, fmt.Sprintf("pressfleet-%s-schema0029.db", stamp))
		files[filepath.Base(m.Backup)] = nil
	}
	marker, err := json.Marshal(m)
	if err != nil {
		e.t.Fatal(err)
	}
	files[fmt.Sprintf("migration-%s-schema0029.json", stamp)] = marker
	done := false
	e.platform.onUp = func() {
		if done {
			return
		}
		done = true
		for name, data := range files {
			if err := os.WriteFile(filepath.Join(backups, name), data, 0o600); err != nil {
				e.t.Error(err)
			}
		}
	}
	return m.Backup
}

// rollback is the record's rollback progress.
type rollback struct {
	Done              []string `json:"done"`
	StateRestoredFrom string   `json:"state_restored_from"`
	StoppedAt         string   `json:"stopped_at"`
	Error             string   `json:"error"`
	Cause             string   `json:"cause"`
}

func (e *upgradeEnv) rollback() (status string, rb rollback) {
	e.t.Helper()
	data, err := os.ReadFile(e.path("upgrade", "upgrade.json"))
	if err != nil {
		e.t.Fatal(err)
	}
	var rec struct {
		Status   string    `json:"status"`
		Rollback *rollback `json:"rollback"`
	}
	if err := json.Unmarshal(data, &rec); err != nil {
		e.t.Fatal(err)
	}
	if rec.Rollback != nil {
		rb = *rec.Rollback
	}
	return rec.Status, rb
}

// checkRolledBack checks that the platform is back on v1.2.2 with the
// project from before the upgrade.
func (e *upgradeEnv) checkRolledBack(r result, original string) {
	e.t.Helper()
	if r.code != 2 {
		e.t.Fatalf("exit %d, want 2\n%s\n%s", r.code, r.stdout, r.stderr)
	}
	if !strings.Contains(r.stdout, "the platform is back on v1.2.2 and healthy") {
		e.t.Errorf("output doesn't say the platform is back:\n%s", r.stdout)
	}
	if got := readFile(e.t, e.path("platform", "compose.yaml")); got != original {
		e.t.Error("the platform project from before the upgrade wasn't put back")
	}
	if e.platform.version != "v1.2.2" || !e.platform.ready {
		e.t.Errorf("the control plane runs %s (ready %t), want v1.2.2", e.platform.version, e.platform.ready)
	}
	if status, _ := e.rollback(); status != "rolled-back" {
		e.t.Errorf("status %q, want rolled-back", status)
	}
	if _, err := os.Stat(e.path("upgrade", "next-platform")); !errors.Is(err, os.ErrNotExist) {
		e.t.Errorf("staging directory left behind: %v", err)
	}
}

func TestUpgradeRollsBackSQLiteMigration(t *testing.T) {
	e := newUpgradeEnv(t, "v1.2.2")
	original := readFile(t, e.path("platform", "compose.yaml"))
	backup := e.migrateOnUp(store.SQLite)
	e.platform.upErr = errors.New("container pressfleet-control-plane-1 is unhealthy")
	// While the upgraded control plane runs, it writes a site's route into
	// Traefik's dynamic directory, and the release adds a file to the
	// project; the rollback keeps the one and removes the other.
	route := e.path("platform", platform.TraefikDynamicDir, "site-1234.yaml")
	added := e.path("platform", "added", "by-the-release.conf")
	migrate := e.platform.onUp
	once := false
	e.platform.onUp = func() {
		migrate()
		if once {
			return
		}
		once = true
		if err := os.MkdirAll(filepath.Dir(added), 0o755); err != nil {
			t.Error(err)
		}
		for _, f := range []string{route, added} {
			if err := os.WriteFile(f, []byte("new\n"), 0o644); err != nil {
				t.Error(err)
			}
		}
	}

	r := e.upgrade("")
	e.checkRolledBack(r, original)
	for _, want := range []string{"Error: the upgrade to v1.2.3-test failed: starting the upgraded platform"} {
		if !strings.Contains(r.stderr, want) {
			t.Errorf("stderr lacks %q:\n%s", want, r.stderr)
		}
	}
	for _, want := range []string{"Rolling back to v1.2.2", "Restoring the state database from " + backup, "restored from " + backup} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("stdout lacks %q:\n%s", want, r.stdout)
		}
	}
	if strings.Contains(r.stderr, "docker compose") {
		t.Errorf("a successful rollback printed the recovery steps:\n%s", r.stderr)
	}
	if want := [][]string{{"control-plane"}}; !slices.EqualFunc(e.platform.stops, want, slices.Equal) {
		t.Errorf("stopped %v, want %v", e.platform.stops, want)
	}
	prev := e.path("upgrade", "previous-platform")
	if len(e.platform.runs) != 1 || e.platform.runs[0].dir != prev ||
		!slices.Equal(e.platform.runs[0].command, []string{"state", "restore", backup}) {
		t.Errorf("runs = %+v, want the state restore in %s", e.platform.runs, prev)
	}
	// The upgrade's up, then the rollback's.
	if len(e.platform.ups) != 2 || e.platform.ups[1].ForceRecreate || !e.platform.ups[1].Wait {
		t.Errorf("ups = %+v", e.platform.ups)
	}
	if got := readFile(t, route); got != "new\n" {
		t.Errorf("the rollback changed a route in Traefik's dynamic directory: %q", got)
	}
	if _, err := os.Stat(filepath.Dir(added)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the rollback kept a directory the release added: %v", err)
	}
	if _, rb := e.rollback(); rb.StateRestoredFrom != backup || !slices.Equal(rb.Done, []string{"stop-control-plane", "restore-state", "restore-project", "start-services", "wait-ready"}) ||
		!strings.Contains(rb.Cause, "unhealthy") {
		t.Errorf("rollback = %+v", rb)
	}

	// The rolled-back upgrade can be run again from scratch, which saves
	// the project again.
	e.platform.upErr, e.platform.onUp = nil, nil
	r = e.upgrade("")
	if r.code != 0 || strings.Contains(r.stdout, "didn't finish") {
		t.Fatalf("upgrade after the rollback: exit %d\n%s\n%s", r.code, r.stdout, r.stderr)
	}
}

func TestUpgradeRollsBackWithoutMigration(t *testing.T) {
	for _, postgres := range []bool{false, true} {
		t.Run(fmt.Sprintf("postgres=%t", postgres), func(t *testing.T) {
			e := newUpgradeEnv(t, "v1.2.2")
			args := []string{}
			if postgres {
				e.usePostgres()
				args = append(args, "--yes")
			}
			original := readFile(t, e.path("platform", "compose.yaml"))
			e.platform.upErr = errors.New("unhealthy")
			r := e.upgrade("", args...)
			e.checkRolledBack(r, original)
			if len(e.platform.runs) != 0 {
				t.Errorf("restored the state database without a migration: %+v", e.platform.runs)
			}
			if !strings.Contains(r.stdout, "didn't migrate the state database") {
				t.Errorf("output doesn't say the state database was kept:\n%s", r.stdout)
			}
		})
	}
}

// With PostgreSQL, which PressFleet doesn't back up, a migrated state
// database stops the rollback before it changes anything.
func TestUpgradeRollbackRefusesMigratedPostgres(t *testing.T) {
	e := newUpgradeEnv(t, "v1.2.2")
	e.usePostgres()
	e.migrateOnUp(store.Postgres)
	e.platform.upErr = errors.New("unhealthy")
	r := e.upgrade("", "--yes")
	if r.code != 1 {
		t.Fatalf("exit %d, want 1\n%s\n%s", r.code, r.stdout, r.stderr)
	}
	for _, want := range []string{
		"PostgreSQL state database, which PressFleet doesn't back up",
		"nothing was rolled back",
		"2. Restore the PostgreSQL state database from the backup you took before the upgrade",
		"cp -a " + e.path("upgrade", "previous-platform") + "/. " + e.path("platform") + "/",
	} {
		if !strings.Contains(r.stderr, want) {
			t.Errorf("stderr lacks %q:\n%s", want, r.stderr)
		}
	}
	if len(e.platform.stops) != 0 || len(e.platform.runs) != 0 || len(e.platform.ups) != 1 {
		t.Errorf("the refused rollback changed the platform: stops %v, runs %v, ups %v", e.platform.stops, e.platform.runs, e.platform.ups)
	}
	if status, _ := e.rollback(); status != "failed" {
		t.Errorf("status %q, want failed", status)
	}
}

// A rollback that fails at a step stops there, records where, and prints
// the manual steps; the next upgrade resumes it.
func TestUpgradeRollbackFailsAtEachStep(t *testing.T) {
	tests := []struct {
		step    string
		want    string
		breakIt func(e *upgradeEnv)
		fix     func(e *upgradeEnv)
	}{
		{
			step: "stop-control-plane", want: "step 1, stopping the control plane: docker: stop failed",
			breakIt: func(e *upgradeEnv) { e.platform.stopErr = errors.New("docker: stop failed") },
			fix:     func(e *upgradeEnv) { e.platform.stopErr = nil },
		},
		{
			step: "restore-state", want: "step 2, restoring the state database",
			breakIt: func(e *upgradeEnv) { e.platform.runErr = errors.New("database is locked") },
			fix:     func(e *upgradeEnv) { e.platform.runErr = nil },
		},
		{
			step: "restore-project", want: "step 3, putting the previous platform project back",
			breakIt: func(e *upgradeEnv) {
				// Something that isn't a regular file in the saved project.
				prev := e.path("upgrade", "previous-platform", "link")
				e.platform.onStop = func() {
					if err := os.Symlink("compose.yaml", prev); err != nil && !errors.Is(err, os.ErrExist) {
						e.t.Error(err)
					}
				}
			},
			fix: func(e *upgradeEnv) {
				e.platform.onStop = nil
				if err := os.Remove(e.path("upgrade", "previous-platform", "link")); err != nil {
					e.t.Error(err)
				}
			},
		},
		{
			step: "start-services", want: "step 4, starting the previous release's services: unhealthy too",
			breakIt: func(e *upgradeEnv) { e.platform.rollbackUpErr = errors.New("unhealthy too") },
			fix:     func(e *upgradeEnv) { e.platform.rollbackUpErr = nil },
		},
		{
			step: "wait-ready", want: "step 4, waiting for the previous control plane: the control plane runs v1.2.3-test, not v1.2.2",
			breakIt: func(e *upgradeEnv) { e.platform.rollbackVersion = "v1.2.3-test" },
			fix:     func(e *upgradeEnv) { e.platform.rollbackVersion = "" },
		},
	}
	for _, tt := range tests {
		t.Run(tt.step, func(t *testing.T) {
			e := newUpgradeEnv(t, "v1.2.2")
			original := readFile(t, e.path("platform", "compose.yaml"))
			backup := e.migrateOnUp(store.SQLite)
			e.platform.upErr = errors.New("unhealthy")
			tt.breakIt(e)
			r := e.upgrade("")
			if r.code != 1 {
				t.Fatalf("exit %d, want 1\n%s\n%s", r.code, r.stdout, r.stderr)
			}
			for _, want := range []string{
				"The rollback stopped at " + tt.want,
				"To return to v1.2.2",
				"state restore " + backup,
				"up --detach --wait --remove-orphans",
			} {
				if !strings.Contains(r.stderr, want) {
					t.Errorf("stderr lacks %q:\n%s", want, r.stderr)
				}
			}
			status, rb := e.rollback()
			if status != "rollback-failed" || rb.StoppedAt != tt.step || rb.Error == "" {
				t.Errorf("status %q, rollback %+v", status, rb)
			}
			restored := len(e.platform.runs)

			// The next upgrade offers to resume the rollback, and --yes
			// does.
			tt.fix(e)
			r = e.upgrade("", "--yes")
			if !strings.Contains(r.stdout, "The rollback of the last upgrade, from v1.2.2 to v1.2.3-test, stopped") ||
				!strings.Contains(r.stdout, "Resuming the rollback") {
				t.Errorf("the next upgrade doesn't resume the rollback:\n%s", r.stdout)
			}
			e.checkRolledBack(r, original)
			// The state database is restored once.
			wantRuns := 1
			if tt.step == "stop-control-plane" || tt.step == "restore-state" {
				wantRuns = restored + 1
			}
			if len(e.platform.runs) != wantRuns {
				t.Errorf("ran %+v, want %d state restores in all", e.platform.runs, wantRuns)
			}
		})
	}
}

// The first interrupt during the apply starts the rollback, which runs to
// the end on a context of its own.
func TestUpgradeRollsBackOnInterrupt(t *testing.T) {
	e := newUpgradeEnv(t, "v1.2.2")
	original := readFile(t, e.path("platform", "compose.yaml"))
	ctx, interrupt := context.WithCancel(context.Background())
	defer interrupt()
	e.platform.onUp = interrupt
	r := e.upgradeCtx(ctx, "v1.2.3-test", "")
	e.checkRolledBack(r, original)
	if !strings.Contains(r.stderr, "failed: interrupted") {
		t.Errorf("stderr doesn't name the interrupt:\n%s", r.stderr)
	}
}

// A second interrupt stops the rollback, which prints the manual steps.
func TestUpgradeSecondInterruptStopsRollback(t *testing.T) {
	e := newUpgradeEnv(t, "v1.2.2")
	backup := e.migrateOnUp(store.SQLite)
	ctx, interrupt := context.WithCancel(context.Background())
	defer interrupt()
	migrate := e.platform.onUp
	e.platform.onUp = func() { migrate(); interrupt() }
	var second context.CancelFunc
	cli.RollbackSignals(t, func(ctx context.Context) (context.Context, context.CancelFunc) {
		ctx, second = context.WithCancel(ctx)
		return ctx, second
	})
	e.platform.onStop = func() { second() }
	r := e.upgradeCtx(ctx, "v1.2.3-test", "")
	if r.code != 1 {
		t.Fatalf("exit %d, want 1\n%s\n%s", r.code, r.stdout, r.stderr)
	}
	for _, want := range []string{"The rollback was interrupted at step 2", "state restore " + backup} {
		if !strings.Contains(r.stderr, want) {
			t.Errorf("stderr lacks %q:\n%s", want, r.stderr)
		}
	}
	if len(e.platform.runs) != 0 {
		t.Errorf("the interrupted rollback went on: %+v", e.platform.runs)
	}
	if status, rb := e.rollback(); status != "rollback-failed" || rb.StoppedAt != "restore-state" {
		t.Errorf("status %q, rollback %+v", status, rb)
	}
}

// The services recreated only because a file they mount changed are
// recreated on the previous files.
func TestUpgradeRollbackRecreatesForcedServices(t *testing.T) {
	e := newUpgradeEnv(t, "v1.2.2")
	traefik := e.path("platform", platform.TraefikConfigFile)
	if err := os.WriteFile(traefik, []byte("# from an older release\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	original := readFile(t, e.path("platform", "compose.yaml"))
	e.platform.upErr = errors.New("unhealthy")
	r := e.upgrade("", "--yes")
	e.checkRolledBack(r, original)
	last := e.platform.ups[len(e.platform.ups)-1]
	if !slices.Equal(last.Services, []string{"traefik"}) || !last.ForceRecreate {
		t.Errorf("ups = %+v, want the last to force-recreate traefik", e.platform.ups)
	}
	if got := readFile(t, traefik); got != "# from an older release\n" {
		t.Errorf("the Traefik configuration wasn't put back: %q", got)
	}
}

// A rollback never crosses a MariaDB series change.
func TestUpgradeRollbackRefusesMariaDBSeriesChange(t *testing.T) {
	e := newUpgradeEnv(t, "v1.2.2")
	e.platform.onUp = func() { e.platform.mariaDBImage = "docker.io/library/mariadb:99.1.0" }
	e.platform.upErr = errors.New("unhealthy")
	r := e.upgrade("")
	if r.code != 1 || !strings.Contains(r.stderr, "MariaDB ran the 12.3 release series before the upgrade and runs 99.1 now") {
		t.Fatalf("exit %d\n%s\n%s", r.code, r.stdout, r.stderr)
	}
	if len(e.platform.stops) != 0 {
		t.Error("the refused rollback stopped the control plane")
	}
	if rec := e.record(); rec["mariadb_series"] != "12.3" || rec["status"] != "failed" {
		t.Errorf("record = %v", rec)
	}
}

// When the rollback leaves the CLI outside the version skew policy, the
// output says which CLI to install again.
func TestUpgradeRollbackNamesCLIToInstall(t *testing.T) {
	e := newUpgradeEnv(t, "v1.0.0")
	e.platform.upErr = errors.New("unhealthy")
	r := e.upgrade("")
	if r.code != 2 || !strings.Contains(r.stdout, "Install the pressfleet CLI v1.0.0 again") {
		t.Fatalf("exit %d\n%s\n%s", r.code, r.stdout, r.stderr)
	}
}

// An upgrade whose CLI was killed can be retried or rolled back, not
// replaced by an upgrade to another release.
func TestUpgradeInterrupted(t *testing.T) {
	interrupted := func(t *testing.T) (*upgradeEnv, string) {
		t.Helper()
		e := newUpgradeEnv(t, "v1.2.2")
		original := readFile(t, e.path("platform", "compose.yaml"))
		e.platform.upErr = errors.New("unhealthy")
		if r := e.upgrade("", "--no-rollback"); r.code != 1 {
			t.Fatalf("exit %d\n%s\n%s", r.code, r.stdout, r.stderr)
		}
		// As if the CLI had been killed during the apply.
		path := e.path("upgrade", "upgrade.json")
		data := strings.Replace(readFile(t, path), `"status": "failed"`, `"status": "in-progress"`, 1)
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
		e.platform.ups = nil
		return e, original
	}

	t.Run("needs an answer", func(t *testing.T) {
		e, _ := interrupted(t)
		for _, stdin := range []string{"", "cancel\n"} {
			r := e.upgrade(stdin)
			if r.code != 1 || !strings.Contains(r.stdout, "was interrupted") {
				t.Fatalf("stdin %q: exit %d\n%s\n%s", stdin, r.code, r.stdout, r.stderr)
			}
		}
		if len(e.platform.ups) != 0 || len(e.platform.stops) != 0 {
			t.Error("changed the platform without an answer")
		}
	})
	t.Run("roll back", func(t *testing.T) {
		e, original := interrupted(t)
		r := e.upgrade("rollback\n")
		e.checkRolledBack(r, original)
		if !strings.Contains(r.stderr, "failed: it was interrupted") {
			t.Errorf("stderr = %s", r.stderr)
		}
	})
	t.Run("retry", func(t *testing.T) {
		e, _ := interrupted(t)
		e.platform.upErr = nil
		r := e.upgrade("retry\n")
		if r.code != 0 || !strings.Contains(r.stdout, "Retrying the upgrade to v1.2.3-test") {
			t.Fatalf("exit %d\n%s\n%s", r.code, r.stdout, r.stderr)
		}
		if rec := e.record(); rec["status"] != "succeeded" || rec["from"] != "v1.2.2" {
			t.Errorf("record = %v", rec)
		}
	})
	t.Run("another release", func(t *testing.T) {
		e, _ := interrupted(t)
		r := e.upgradeCtx(context.Background(), "v1.2.4-test", "", "--yes")
		if r.code != 1 || !strings.Contains(r.stdout, "won't upgrade to v1.2.4-test over it") {
			t.Fatalf("exit %d\n%s\n%s", r.code, r.stdout, r.stderr)
		}
		if len(e.platform.ups) != 0 || e.platform.pulls != 1 {
			t.Errorf("changed the platform: ups %v, pulls %d", e.platform.ups, e.platform.pulls)
		}
	})
}

// A rollback that stopped and was finished with the recovery steps doesn't
// stand in the way of the next upgrade.
func TestUpgradeAfterRecoveringStoppedRollback(t *testing.T) {
	e := newUpgradeEnv(t, "v1.2.2")
	e.platform.upErr = errors.New("unhealthy")
	e.platform.rollbackVersion = "v1.2.3-test"
	if r := e.upgrade(""); r.code != 1 {
		t.Fatalf("exit %d, want 1\n%s\n%s", r.code, r.stdout, r.stderr)
	}
	// The operator runs the recovery steps.
	e.platform.version, e.platform.ready = "v1.2.2", true
	e.platform.upErr, e.platform.rollbackVersion = nil, ""
	r := e.upgrade("")
	if r.code != 0 || !strings.Contains(r.stdout, "was put back since") {
		t.Fatalf("exit %d\n%s\n%s", r.code, r.stdout, r.stderr)
	}
	if rec := e.record(); rec["status"] != "succeeded" {
		t.Errorf("record = %v", rec)
	}
}
