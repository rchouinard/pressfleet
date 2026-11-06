package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/rchouinard/pressfleet/control-plane/internal/config"
	"github.com/rchouinard/pressfleet/control-plane/internal/doctor"
	"github.com/rchouinard/pressfleet/control-plane/internal/ingress"
	"github.com/rchouinard/pressfleet/control-plane/internal/platform"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner/compose"
	"github.com/rchouinard/pressfleet/control-plane/internal/redact"
	"github.com/rchouinard/pressfleet/control-plane/internal/skew"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/upgradestate"
)

// Platform upgrades (design doc §4.7, #95).
//
// `pressfleet upgrade` moves the platform to the definition embedded in the
// CLI running it: its templates and its images, the control plane at the
// CLI's own version. Migrations are forward-only, but the upgrade keeps what
// a rollback needs (a copy of the platform project from before it, and the
// images the services ran), and if the platform doesn't come back healthy it
// rolls back to the previous release on its own (upgrade_rollback.go,
// #249). With --no-rollback, or when the rollback can't finish, it prints
// the recovery steps (recoveryTemplate) with the actual paths filled in.
// docs/upgrading.md documents the same steps, and a test keeps the two in
// sync.
//
// The CLI never touches the state database: the control plane migrates it
// when it starts, after backing up an SQLite database (store.DB.Migrate),
// and records the run in a marker (#246), which tells the CLI whether there
// is a backup to restore. The CLI restores it with a one-off container of
// the previous control plane image.

// Upgrade service names. They name the platform's Compose services, whose
// recreation interrupts every site (interrupting).
const (
	mariaDBService = "mariadb"
	redisService   = "redis"
)

// interrupting are the platform services every site depends on: recreating
// one interrupts all sites for as long as it takes to start again.
var interrupting = []string{ingress.TraefikService, mariaDBService, redisService}

// Files of the upgrade directory (config.Layout.UpgradeDir).
const (
	upgradeRecordFile  = upgradestate.RecordFile
	previousPlatform   = "previous-platform"
	stagedPlatform     = "next-platform"
	upgradeReadyChecks = 30
)

// upgradeReadyInterval is the time between readiness checks of the upgraded
// control plane; tests shorten it.
var upgradeReadyInterval = 2 * time.Second

// Status of an upgrade in its record. They are a contract with the control
// plane, which holds rollouts while one is unconfirmed (upgradestate).
const (
	upgradeInProgress     = upgradestate.InProgress
	upgradeFailed         = upgradestate.Failed
	upgradeSucceeded      = upgradestate.Succeeded
	upgradeRollingBack    = upgradestate.RollingBack
	upgradeRolledBack     = upgradestate.RolledBack
	upgradeRollbackFailed = upgradestate.RollbackFailed
)

// exitRolledBack is the exit code of an upgrade that failed and was rolled
// back. 0 means upgraded, and 1 failed and not rolled back, as any other
// error exits (#245).
const exitRolledBack = 2

// UpgradeProject is the part of a Compose project that upgrade drives.
// compose.Project implements it; tests inject fakes.
type UpgradeProject interface {
	ControlPlaneProbe
	Up(ctx context.Context, opts compose.UpOptions) error
	Pull(ctx context.Context, opts compose.PullOptions) error
	ConfigHashes(ctx context.Context) (map[string]string, error)
	FileMounts(ctx context.Context) (map[string][]string, error)
	ImageDigests(ctx context.Context, image string) ([]string, error)
	ListDir(ctx context.Context, service, dir string) ([]string, error)
	ReadFile(ctx context.Context, service, path string) ([]byte, error)
	// StopServices and RunService serve the rollback: it stops the
	// control plane, and restores the state database with a one-off
	// container of the previous release's.
	StopServices(ctx context.Context, services ...string) error
	RunService(ctx context.Context, service string, command ...string) ([]byte, error)
}

// upgradeFlags are the flags of `pressfleet upgrade`.
type upgradeFlags struct {
	yes               bool
	skipPreflight     bool
	noRollback        bool
	controlPlaneImage string
}

// upgradeRecord is the record of the last upgrade, upgrade.json in the
// upgrade directory. A failed upgrade's record is how a retry knows to keep
// the platform project saved by the first attempt.
type upgradeRecord struct {
	From      string    `json:"from"`
	To        string    `json:"to"`
	StartedAt time.Time `json:"started_at"`
	Status    string    `json:"status"`
	// PreviousImages are the images the services ran before the upgrade.
	PreviousImages map[string]serviceImage `json:"previous_images"`
	// Recreated are the services the upgrade recreated, and Forced those
	// among them only because a file they mount changed; the recovery
	// recreates Forced on the previous files.
	Recreated []string `json:"recreated"`
	Forced    []string `json:"forced,omitempty"`
	// MariaDBSeries is the release series (X.Y) MariaDB ran before the
	// upgrade; a rollback never crosses a series change (#245). Empty in
	// records of releases before #249, and when there was no MariaDB.
	MariaDBSeries string `json:"mariadb_series,omitempty"`
	// Rollback is the progress of the upgrade's rollback, if one started.
	Rollback *rollbackRecord `json:"rollback,omitempty"`
}

// serviceImage is the image a service's container ran.
type serviceImage struct {
	Image   string   `json:"image"`
	ImageID string   `json:"image_id"`
	Digests []string `json:"digests,omitempty"`
}

func (a *app) upgradeCommand() *cobra.Command {
	var f upgradeFlags
	cmd := &cobra.Command{
		Use:   "upgrade",
		Short: "Upgrade the platform to this CLI's version",
		Long: `Upgrade the platform services to the definition released with this CLI:
the control plane at the CLI's own version, and the pinned MariaDB, object
cache, Traefik, and VictoriaMetrics images. Only services whose definition
changed are recreated, and the control plane migrates the state database
when it starts, backing up an SQLite database first.

The platform project from before the upgrade is kept in
<data_dir>/upgrade/previous-platform. If the platform doesn't come back
healthy, or upgrade is interrupted (SIGINT, SIGTERM) while applying it,
upgrade rolls back to the previous release on its own: it stops the control
plane, restores the state database if the upgrade migrated it (SQLite only;
with PostgreSQL it stops and prints the steps), puts the platform project
back, and starts the previous release's services. A second interrupt stops
the rollback. If the rollback can't finish, or with --no-rollback, upgrade
prints the steps that return the platform to the previous release instead
(see docs/upgrading.md).

Exit codes: 0 upgraded; 2 failed and rolled back; 1 failed and not rolled
back.

If the last upgrade was interrupted (its CLI was killed), upgrade offers to
retry it or roll it back, and won't upgrade to another release over it.

Before recreating Traefik, MariaDB, or the object cache, which interrupts
every site, and before upgrading with a PostgreSQL state database, upgrade
asks for confirmation; --yes gives it.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			u := &upgrader{a: a, f: f, in: bufio.NewReader(a.opts.Stdin), out: a.opts.Stdout}
			return u.run(cmd.Context())
		},
	}
	fl := cmd.Flags()
	fl.BoolVar(&f.yes, "yes", false, "don't ask for confirmation")
	fl.BoolVar(&f.noRollback, "no-rollback", false,
		"if the upgrade fails, don't roll back: leave the platform for debugging and print the steps that return it to the previous release")
	fl.BoolVar(&f.skipPreflight, "skip-preflight", false,
		"upgrade even if required host checks fail, and start the control plane without them (development only)")
	fl.StringVar(&f.controlPlaneImage, "control-plane-image", "",
		"control plane image (development only; default: the image released with this CLI)")
	return cmd
}

// upgrader carries one run of `pressfleet upgrade`.
type upgrader struct {
	a   *app
	f   upgradeFlags
	in  *bufio.Reader
	out io.Writer

	name        string
	layout      config.Layout
	platformDir string
	upgradeDir  string
	from, to    string
	images      platform.Images
	project     UpgradeProject
	staged      UpgradeProject
	record      upgradeRecord
	// keepPrevious says a failed upgrade's saved project is kept.
	keepPrevious bool
	// rollBackInterrupted says the operator chose to roll back an
	// interrupted upgrade rather than retry it.
	rollBackInterrupted bool
	// fromReady says the control plane answered ready when the upgrade
	// started.
	fromReady bool
	// recreate are the services the upgrade recreates; forced are those
	// recreated only because a file they mount changed.
	recreate, forced []string
	// changed are the rendered files whose content changes.
	changed         []string
	metricsPassword redact.Secret
}

func (u *upgrader) printf(format string, args ...any) {
	_, _ = fmt.Fprintf(u.out, format, args...)
}

func (u *upgrader) run(ctx context.Context) error {
	u.name = u.a.platformProjectName()
	u.layout = u.a.cfg.Layout()
	u.platformDir = u.layout.PlatformDir()
	u.upgradeDir = u.layout.UpgradeDir()
	u.to = u.a.opts.Version
	if !exists(filepath.Join(u.platformDir, platform.ComposeFile)) {
		return fmt.Errorf("there is no platform project in %s: install PressFleet with `pressfleet init`", u.platformDir)
	}
	u.project = u.a.upgradeProject(u.name, u.platformDir, nil)

	if err := u.preflight(ctx); err != nil {
		return err
	}
	if u.rollBackInterrupted {
		cause := errors.New("it was interrupted")
		if rb := u.record.Rollback; rb != nil && rb.Cause != "" {
			cause = errors.New(rb.Cause)
		}
		return u.rollBackFailed(ctx, cause)
	}
	if err := u.stage(); err != nil {
		return err
	}
	if err := u.plan(ctx); err != nil {
		return err
	}
	if u.keepPrevious {
		// The failed attempt rendered the project already, so it compares
		// equal; its services still need what that attempt meant to do.
		u.recreate = union(u.recreate, u.record.Recreated)
		u.forced = union(u.forced, u.record.Forced)
	} else if len(u.recreate) == 0 && len(u.changed) == 0 {
		// Even across versions (development builds): saving the project
		// again would replace the one a recovery returns to.
		u.printf("The platform already matches this release's definition; nothing to upgrade.\n")
		return os.RemoveAll(filepath.Join(u.upgradeDir, stagedPlatform))
	}
	if err := u.confirm(); err != nil {
		return err
	}

	// Pull before changing anything: a failed pull leaves the platform as it
	// was, and the services are down only while they restart.
	u.printf("Pulling images...\n")
	if err := u.staged.Pull(ctx, compose.PullOptions{Missing: true}); err != nil {
		return fmt.Errorf("pulling the new images (nothing was changed): %w", err)
	}
	if err := u.savePrevious(ctx); err != nil {
		return err
	}

	// From here on the platform changes, and a failure, including an
	// interrupt (which cancels ctx), rolls it back.
	if err := u.apply(ctx); err != nil {
		if ctx.Err() != nil {
			err = fmt.Errorf("interrupted: %w", err)
		}
		if u.f.noRollback {
			return u.fail(context.WithoutCancel(ctx), err)
		}
		return u.rollBackFailed(ctx, err)
	}
	u.record.Status = upgradeSucceeded
	if err := u.writeRecord(); err != nil {
		return err
	}
	if err := os.RemoveAll(filepath.Join(u.upgradeDir, stagedPlatform)); err != nil {
		return err
	}
	u.printf("\nThe platform is upgraded to %s and healthy.\n", u.to)
	if len(u.recreate) > 0 {
		u.printf("  Recreated: %s\n", strings.Join(u.recreate, ", "))
	}
	u.printf("  The platform project from before the upgrade is kept in %s until the next upgrade.\n",
		filepath.Join(u.upgradeDir, previousPlatform))
	if u.metricsPassword != "" {
		u.printf("\nCreated a public metrics credential; its password is %s. Store it now; it can't be shown again.\n",
			u.metricsPassword.Reveal())
	}
	return nil
}

// preflight checks that the upgrade can start, without changing anything.
func (u *upgrader) preflight(ctx context.Context) error {
	cp, err := u.a.controlPlaneStatus(ctx)
	if err != nil {
		return err
	}
	if !cp.Installed {
		return fmt.Errorf("the platform has no control plane container: start it with `docker compose --project-name %s "+
			"--project-directory %s up --detach`, or reinstall with `pressfleet init --force`", u.name, u.platformDir)
	}
	u.from = cp.Version
	u.fromReady = cp.Info != nil && cp.Info.Ready
	if err := u.checkVersions(); err != nil {
		return err
	}
	if err := u.loadRecord(); err != nil {
		return err
	}
	if u.rollBackInterrupted {
		// The rollback returns to what ran before; the checks of the new
		// release don't apply.
		return nil
	}
	if err := u.hostChecks(ctx); err != nil {
		return err
	}
	if u.images, err = u.a.platformImages(u.f.controlPlaneImage); err != nil {
		return err
	}
	return u.checkMariaDBSeries(ctx)
}

// checkVersions applies the upgrade policy: no downgrades, and the control
// plane must be at least skew.MinUpgradeFrom. Development builds can't be
// ordered, so they are only warned about.
func (u *upgrader) checkVersions() error {
	if u.from == "" {
		u.a.warnf("can't tell which version the control plane runs; not checking the upgrade order")
		return nil
	}
	to, toErr := skew.Parse(u.to)
	from, fromErr := skew.Parse(u.from)
	if toErr != nil || fromErr != nil {
		u.a.warnf("not checking the upgrade order from %s to %s: development build", u.from, u.to)
		return nil
	}
	oldest, err := skew.Parse(skew.MinUpgradeFrom)
	if err != nil {
		return err
	}
	switch {
	case to.Compare(from) < 0:
		return fmt.Errorf("the control plane runs %s, newer than this CLI (%s); the control plane can't be downgraded. "+
			"Use a pressfleet CLI %s or later", from, to, from)
	case from.Compare(oldest) < 0:
		return fmt.Errorf("%s upgrades control planes from %s or later, and this one runs %s. "+
			"Upgrade it with the CLI of an intermediate release first (see the release notes)", to, oldest, from)
	case to.Compare(from) == 0:
		u.printf("The control plane already runs %s; applying this release's platform definition again.\n", to)
	default:
		u.printf("Upgrading the platform from %s to %s.\n", from, to)
	}
	return nil
}

// loadRecord reads the last upgrade's record. If that upgrade didn't
// finish and the platform project hasn't been put back since, this run is
// a retry: it keeps the project saved by that upgrade, the one a rollback
// must return to, rather than saving the failed upgrade's. An upgrade that
// was interrupted (its CLI killed while it applied the release or rolled it
// back) can be retried or rolled back, but not replaced by an upgrade to
// another release (#245).
func (u *upgrader) loadRecord() error {
	rec, err := readUpgradeRecord(u.upgradeDir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if rec.Status == upgradeSucceeded || rec.Status == upgradeRolledBack {
		return nil
	}
	same, err := sameFile(filepath.Join(u.platformDir, platform.ComposeFile),
		filepath.Join(u.upgradeDir, previousPlatform, platform.ComposeFile))
	if err != nil {
		return err
	}
	// A rollback that stopped may have put the project back, or part of
	// it, without starting the services, so it is resumed, unless the
	// previous release's control plane runs and is ready: then the
	// rollback, or the operator with the recovery steps, finished it.
	stopped := rec.Status == upgradeRollingBack || rec.Status == upgradeRollbackFailed
	if same && (!stopped || (u.fromReady && rec.From != "" && u.from == rec.From)) {
		u.printf("The last upgrade, to %s, didn't finish, and its platform project was put back since.\n", rec.To)
		return nil
	}
	if rec.Status != upgradeFailed {
		return u.resolveInterrupted(rec)
	}
	u.printf("The last upgrade, to %s, didn't finish. Retrying; the platform project from before it (%s) stays in %s.\n",
		rec.To, rec.From, filepath.Join(u.upgradeDir, previousPlatform))
	u.record, u.keepPrevious = rec, true
	return nil
}

// readUpgradeRecord reads the upgrade record in dir.
func readUpgradeRecord(dir string) (upgradeRecord, error) {
	path := filepath.Join(dir, upgradeRecordFile)
	data, err := os.ReadFile(path) //nolint:gosec // G304: the upgrade record in the data directory
	if errors.Is(err, fs.ErrNotExist) {
		return upgradeRecord{}, err
	}
	if err != nil {
		return upgradeRecord{}, fmt.Errorf("reading the last upgrade's record: %w", err)
	}
	var rec upgradeRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return upgradeRecord{}, fmt.Errorf("reading the last upgrade's record %s: %w", path, err)
	}
	return rec, nil
}

// hostChecks runs the doctor's runtime checks, which the upgraded control
// plane runs again when it starts.
func (u *upgrader) hostChecks(ctx context.Context) error {
	if u.f.skipPreflight {
		u.printf("PREFLIGHT CHECKS SKIPPED: --skip-preflight is for development machines only.\n")
		return nil
	}
	rep := doctor.Run(ctx, u.a.doctorOptions(), doctor.ScopeRuntime)
	if failures := rep.Failures(); len(failures) > 0 {
		if err := rep.WriteText(u.out); err != nil {
			return err
		}
		names := make([]string, len(failures))
		for n, f := range failures {
			names[n] = f.ID
		}
		return fmt.Errorf("required host checks failed: %s (fix them and run upgrade again)", strings.Join(names, ", "))
	}
	return nil
}

// mariaDBSeries matches the MariaDB release series (X.Y) in an image
// reference such as docker.io/library/mariadb:12.3.3@sha256:....
var mariaDBSeries = regexp.MustCompile(`:(\d+\.\d+)(?:\.\d+)?(?:[-@]|$)`)

// MariaDBSeries returns the MariaDB release series of an image reference, or
// "" if its tag doesn't name one.
func MariaDBSeries(image string) string {
	if i := strings.LastIndex(image, "/"); i >= 0 {
		image = image[i:]
	}
	m := mariaDBSeries.FindStringSubmatch(image)
	if m == nil {
		return ""
	}
	return m[1]
}

// checkMariaDBSeries refuses to move MariaDB to another release series.
// Series changes are one-way (the data directory is upgraded in place, and
// MARIADB_AUTO_UPGRADE would do that silently), and supporting them is
// future work; each PressFleet release pins one series.
func (u *upgrader) checkMariaDBSeries(ctx context.Context) error {
	c, err := u.project.ServiceContainer(ctx, mariaDBService)
	if errors.Is(err, compose.ErrNoContainer) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("finding MariaDB: %w", err)
	}
	running, next := MariaDBSeries(c.Image), MariaDBSeries(u.images.MariaDB)
	switch {
	case running == "":
		return fmt.Errorf("can't tell the MariaDB release series of the running image %s, so can't check that "+
			"the upgrade keeps it", c.Image)
	case running != next:
		return fmt.Errorf("%s moves MariaDB from the %s to the %s release series, which PressFleet doesn't support "+
			"upgrading yet; nothing was changed", u.to, running, next)
	}
	return nil
}

// stage renders the new platform project into the staging directory, from
// where plan compares it with the current one.
func (u *upgrader) stage() error {
	dir := filepath.Join(u.upgradeDir, stagedPlatform)
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	var err error
	u.metricsPassword, err = renderPlatform(dir, u.renderConfig())
	if err != nil {
		return fmt.Errorf("rendering the new platform project: %w", err)
	}
	// Relative paths in the staged file resolve against the real project
	// directory, so the comparison sees the paths the services will use.
	// Naming the file stops Compose from merging an override file next to
	// it on its own, which the running project has.
	files := []string{filepath.Join(dir, platform.ComposeFile)}
	if override := filepath.Join(u.platformDir, composeOverrideFile); exists(override) {
		files = append(files, override)
	}
	u.staged = u.a.upgradeProject(u.name, u.platformDir, files)
	return nil
}

func (u *upgrader) renderConfig() platformRender {
	return platformRender{
		cfg:           u.a.cfg,
		projectName:   u.name,
		images:        u.images,
		skipPreflight: u.f.skipPreflight,
		configFile:    u.a.mountedConfigPath(),
		secretsDir:    u.platformDir,
	}
}

// plan works out which services the upgrade recreates: those whose
// configuration hash changes, and those mounting a single file whose content
// changes, which Compose doesn't notice. Services mounting a directory see
// the new files without being recreated.
func (u *upgrader) plan(ctx context.Context) error {
	before, err := u.project.ConfigHashes(ctx)
	if err != nil {
		return fmt.Errorf("reading the current platform project: %w", err)
	}
	after, err := u.staged.ConfigHashes(ctx)
	if err != nil {
		return fmt.Errorf("reading the new platform project: %w", err)
	}
	mounts, err := u.staged.FileMounts(ctx)
	if err != nil {
		return fmt.Errorf("reading the new platform project: %w", err)
	}
	u.changed, err = changedFiles(filepath.Join(u.upgradeDir, stagedPlatform), u.platformDir)
	if err != nil {
		return err
	}
	for service, hash := range after {
		if before[service] != hash {
			u.recreate = append(u.recreate, service)
			continue
		}
		if slices.ContainsFunc(mounts[service], func(p string) bool { return slices.Contains(u.changed, p) }) {
			u.recreate = append(u.recreate, service)
			u.forced = append(u.forced, service)
		}
	}
	slices.Sort(u.recreate)
	slices.Sort(u.forced)
	return nil
}

// changedFiles lists the files of the staged project whose content differs
// from, or is missing in, the current project, as paths in the current one.
func changedFiles(staged, current string) ([]string, error) {
	var changed []string
	err := filepath.WalkDir(staged, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(staged, path)
		if err != nil {
			return err
		}
		target := filepath.Join(current, rel)
		same, err := sameFile(path, target)
		if err != nil {
			return err
		}
		if !same {
			changed = append(changed, target)
		}
		return nil
	})
	return changed, err
}

// sameFile reports whether two files exist and have the same content.
func sameFile(a, b string) (bool, error) {
	da, err := os.ReadFile(a) //nolint:gosec // G304: files in the data directory
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	db, err := os.ReadFile(b) //nolint:gosec // G304: files in the data directory
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	return bytes.Equal(da, db), nil
}

// confirm states what the upgrade interrupts and asks to go ahead when it
// interrupts sites or can't restore the state database itself.
func (u *upgrader) confirm() error {
	if len(u.recreate) > 0 {
		u.printf("Services to recreate: %s.\n", strings.Join(u.recreate, ", "))
	} else {
		u.printf("No service's definition changed.\n")
	}
	var warnings []string
	if hit := slices.DeleteFunc(slices.Clone(u.recreate), func(s string) bool { return !slices.Contains(interrupting, s) }); len(hit) > 0 {
		warnings = append(warnings, fmt.Sprintf("Recreating %s interrupts every site until %s back up, usually under a minute.",
			strings.Join(hit, ", "), plural(len(hit), "it is", "they are")))
	}
	if u.a.cfg.Database.Driver == string(store.Postgres) {
		warnings = append(warnings, "The state database is PostgreSQL, which PressFleet doesn't back up: "+
			"if the upgrade fails, returning to the previous release needs a backup of it from before the upgrade. "+
			"Take one now if you haven't.")
	}
	if len(warnings) == 0 {
		return nil
	}
	for _, w := range warnings {
		u.printf("\nWARNING: %s\n", w)
	}
	if u.f.yes {
		u.printf("Confirmed with --yes.\n")
		return nil
	}
	for {
		u.printf("Continue with the upgrade? [y/N]: ")
		line, err := u.in.ReadString('\n')
		if err != nil && (!errors.Is(err, io.EOF) || line == "") {
			u.printf("\n")
			return errors.New("the upgrade needs confirmation: answer the prompt, or pass --yes")
		}
		switch strings.ToLower(strings.TrimSpace(line)) {
		case "y", "yes":
			return nil
		case "", "n", "no":
			return errors.New("upgrade canceled; nothing was changed")
		}
	}
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// savePrevious keeps what a manual recovery needs, before anything changes:
// a copy of the current platform project and the images the services run.
// A retry of a failed upgrade keeps the first attempt's.
func (u *upgrader) savePrevious(ctx context.Context) error {
	if u.keepPrevious {
		u.record.To = u.to
		u.record.Status = upgradeInProgress
		// A rollback of an earlier attempt, given up for this retry,
		// starts over if this one fails too.
		u.record.Rollback = nil
		u.record.Recreated = union(u.record.Recreated, u.recreate)
		u.record.Forced = union(u.record.Forced, u.forced)
		return u.writeRecord()
	}
	images := map[string]serviceImage{}
	hashes, err := u.project.ConfigHashes(ctx)
	if err != nil {
		return err
	}
	for service := range hashes {
		c, err := u.project.ServiceContainer(ctx, service)
		if errors.Is(err, compose.ErrNoContainer) {
			continue
		}
		if err != nil {
			return fmt.Errorf("recording the image of %s: %w", service, err)
		}
		img := serviceImage{Image: c.Image, ImageID: c.ImageID}
		if c.ImageID != "" {
			if img.Digests, err = u.project.ImageDigests(ctx, c.ImageID); err != nil {
				return fmt.Errorf("recording the image of %s: %w", service, err)
			}
		}
		images[service] = img
	}
	prev := filepath.Join(u.upgradeDir, previousPlatform)
	if err := os.RemoveAll(prev); err != nil {
		return err
	}
	if err := copyTree(u.platformDir, prev); err != nil {
		return fmt.Errorf("saving the current platform project: %w", err)
	}
	u.record = upgradeRecord{
		From: u.from, To: u.to, StartedAt: time.Now().UTC(), Status: upgradeInProgress,
		PreviousImages: images, Recreated: u.recreate, Forced: u.forced,
		MariaDBSeries: MariaDBSeries(images[mariaDBService].Image),
	}
	if err := u.writeRecord(); err != nil {
		return err
	}
	u.printf("Saved the current platform project in %s.\n", prev)
	return nil
}

func union(a, b []string) []string {
	out := append(slices.Clone(a), b...)
	slices.Sort(out)
	return slices.Compact(out)
}

func (u *upgrader) writeRecord() error {
	data, err := json.MarshalIndent(u.record, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(u.upgradeDir, 0o700); err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(u.upgradeDir, upgradeRecordFile), append(data, '\n'), 0o600)
}

// apply renders the new platform project in place, recreates the services
// that changed, and waits for the control plane to report ready at the new
// version.
func (u *upgrader) apply(ctx context.Context) error {
	if _, err := renderPlatform(u.platformDir, u.renderConfig()); err != nil {
		return fmt.Errorf("rendering the platform project: %w", err)
	}
	u.printf("Rendered the new platform project in %s.\n", u.platformDir)
	if len(u.forced) > 0 {
		err := u.project.Up(ctx, compose.UpOptions{
			Services: u.forced, ForceRecreate: true, Wait: true, WaitTimeout: platformStartTimeout,
		})
		if err != nil {
			return fmt.Errorf("recreating %s: %w", strings.Join(u.forced, ", "), err)
		}
	}
	u.printf("Starting the upgraded services; the control plane migrates its state database as it starts...\n")
	if err := u.project.Up(ctx, compose.UpOptions{Wait: true, WaitTimeout: platformStartTimeout}); err != nil {
		return fmt.Errorf("starting the upgraded platform: %w", err)
	}
	want := u.to
	if u.f.controlPlaneImage != "" {
		// A development image reports whatever version it was built as.
		want = ""
	}
	return u.waitReady(ctx, want)
}

// upPlatform brings the platform project's services up, waiting until they
// are healthy: all of them, or only forced, which are recreated even if
// their definition didn't change.
func (u *upgrader) upPlatform(ctx context.Context, forced []string) error {
	opts := compose.UpOptions{Wait: true, WaitTimeout: platformStartTimeout}
	if len(forced) > 0 {
		opts.Services, opts.ForceRecreate = forced, true
	}
	return u.project.Up(ctx, opts)
}

// runningImage returns the image a service's container runs; found is false
// if it has none.
func (u *upgrader) runningImage(ctx context.Context, service string) (image string, found bool, err error) {
	c, err := u.project.ServiceContainer(ctx, service)
	if errors.Is(err, compose.ErrNoContainer) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return c.Image, true, nil
}

// waitReady waits for the control plane to answer ready at version want
// (any version if want is empty). Compose's wait covers its healthcheck;
// this also checks the version, so a control plane that kept its old image
// is caught.
func (u *upgrader) waitReady(ctx context.Context, want string) error {
	var last error
	for range upgradeReadyChecks {
		cp, err := u.a.controlPlaneStatus(ctx)
		switch {
		case err != nil:
			last = err
		case cp.Info == nil:
			last = fmt.Errorf("the control plane doesn't answer: %w", cp.InfoErr)
		case !cp.Info.Ready:
			last = fmt.Errorf("the control plane isn't ready: %s", cp.Info.ReadyError)
		case want != "" && cp.Version != want:
			return fmt.Errorf("the control plane runs %s, not %s", cp.Version, want)
		default:
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(upgradeReadyInterval):
		}
	}
	return last
}

// fail records the failed upgrade and prints the recovery steps, without
// rolling back (--no-rollback). ctx must not be canceled.
func (u *upgrader) fail(ctx context.Context, cause error) error {
	u.record.Status = upgradeFailed
	if err := u.writeRecord(); err != nil {
		u.a.warnf("recording the failed upgrade: %v", err)
	}
	data := u.recoveryData()
	if !data.Postgres {
		mig, err := u.findMigration(ctx)
		if err != nil {
			// Not knowing is not the same as finding none: the steps
			// must not tell the operator that no restore is needed.
			u.a.warnf("looking for the pre-migration backup: %v", err)
			data.BackupUnchecked = true
		}
		data.Backup = mig.Backup
	}
	steps, err := renderRecovery(data)
	if err != nil {
		return errors.Join(cause, err)
	}
	_, _ = fmt.Fprintf(u.a.opts.Stderr, "Error: the upgrade to %s failed: %v\n\n%s", u.record.To, cause, steps)
	return &exitError{code: 1}
}

// recoveryData fills in the recovery steps from the upgrade record, except
// for the state backup, which needs findMigration.
func (u *upgrader) recoveryData() recoveryData {
	data := recoveryData{
		From:        u.record.From,
		To:          u.record.To,
		Project:     u.name,
		PlatformDir: u.platformDir,
		PreviousDir: filepath.Join(u.upgradeDir, previousPlatform),
		Postgres:    u.a.cfg.Database.Driver == string(store.Postgres),
		Forced:      strings.Join(u.record.Forced, " "),
	}
	if data.From == "" {
		data.From = "the previous release"
	}
	if img, ok := u.record.PreviousImages[controlPlaneService]; ok {
		data.PreviousImage = img.Image
		if len(img.Digests) > 0 {
			data.PreviousImage = img.Digests[0]
		}
	}
	if !data.Postgres {
		data.BackupsDir = u.layout.StateBackupsDir()
		data.Since = u.record.StartedAt.UTC().Format(backupTimeLayout)
	}
	return data
}

// renderRecovery renders the recovery steps.
func renderRecovery(data recoveryData) (string, error) {
	var buf bytes.Buffer
	err := recoveryTemplate.Execute(&buf, data)
	return buf.String(), err
}

// backupName matches the backups store.DB.Migrate takes before migrating:
// pressfleet-<timestamp>-schemaNNNN.db.
var backupName = regexp.MustCompile(`^pressfleet-(\d{8}T\d{6}\.\d{3}Z)-schema\d{4}\.db$`)

// backupTimeLayout is the timestamp layout of backups' names.
const backupTimeLayout = "20060102T150405.000Z"

// backupClockSkew is how much earlier than the upgrade's start a backup's
// timestamp may be and still count as taken by it. The control plane writes
// the timestamp with the clock of its container and the CLI takes the start
// from the host's, which differ when Docker runs in a VM (WSL, Docker
// Desktop). A few seconds of skew would otherwise hide the backup and bring
// back the false "no restore needed". The reverse risk, picking up an older
// backup, needs a migration within this window before the upgrade started,
// which is far less likely. The schema number in the name can't replace the
// comparison: the host doesn't know the previous release's schema version
// (it never opens the database, §4.7).
const backupClockSkew = 30 * time.Second

// dirLister lists the names in a directory.
type dirLister func(ctx context.Context, dir string) ([]string, error)

// fileReader reads a file.
type fileReader func(ctx context.Context, path string) ([]byte, error)

// backupsReader reads the state backups directory one way: from the host,
// or through a container of the control plane.
type backupsReader struct {
	list dirLister
	read fileReader
}

// hostListDir lists a directory with the host's own permissions. Tests
// replace it to simulate a directory the CLI can't read (as root, chmod
// wouldn't).
var hostListDir dirLister = func(_ context.Context, dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = e.Name()
	}
	return names, nil
}

// hostReadFile reads a file with the host's own permissions.
var hostReadFile fileReader = func(_ context.Context, path string) ([]byte, error) {
	return os.ReadFile(path) //nolint:gosec // G304: a marker in the state backups directory
}

// upgradeMigration is what the CLI knows about the state database
// migrations during an upgrade.
type upgradeMigration struct {
	// Markers are the migration markers the control plane wrote since the
	// upgrade started, oldest first.
	Markers []store.MigrationMarker
	// Backup is the pre-migration backup holding the database as the
	// previous release left it, or empty if there is none.
	Backup string
}

// Migrated reports whether the upgrade may have migrated the state
// database. Without markers, written by a control plane older than #246,
// a pre-migration backup is the only sign.
func (m upgradeMigration) Migrated() bool {
	if len(m.Markers) == 0 {
		return m.Backup != ""
	}
	return slices.ContainsFunc(m.Markers, store.MigrationMarker.Changed)
}

// findMigration looks for the state database migrations of this upgrade.
// The backups directory belongs to root (the control plane container
// created it, mode 0700), so a CLI run by an operator in the docker group
// can't read it: then a container of the control plane reads it, the
// previous release's first, because the new image may be the broken one.
// Not through the admin interface: the control plane may be down.
func (u *upgrader) findMigration(ctx context.Context) (upgradeMigration, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	via := func(p UpgradeProject) backupsReader {
		return backupsReader{
			list: func(ctx context.Context, dir string) ([]string, error) {
				return p.ListDir(ctx, controlPlaneService, dir)
			},
			read: func(ctx context.Context, path string) ([]byte, error) {
				return p.ReadFile(ctx, controlPlaneService, path)
			},
		}
	}
	return findMigration(ctx, u.layout.StateBackupsDir(), u.record.StartedAt,
		backupsReader{list: hostListDir, read: hostReadFile},
		via(u.a.upgradeProject(u.name, filepath.Join(u.upgradeDir, previousPlatform), nil)),
		via(u.project))
}

// findMigration returns the migrations the control plane recorded in dir at
// or after since (less the clock skew), the start of the upgrade. It reads
// the migration markers, and falls back to the pre-migration backups' file
// names when there are none, as a control plane older than the markers
// leaves. It never opens the state database (§4.7). It reads dir with host,
// or if that fails for another reason than a missing directory, with the
// first of fallbacks that works. If none does, the error says so, and
// whether a migration ran isn't known.
func findMigration(ctx context.Context, dir string, since time.Time, host backupsReader, fallbacks ...backupsReader) (upgradeMigration, error) {
	mig, missing, err := readMigration(ctx, host, dir, since)
	if missing {
		return upgradeMigration{}, nil
	}
	errs := []error{err}
	for _, r := range fallbacks {
		if err == nil {
			break
		}
		if mig, _, err = readMigration(ctx, r, dir, since); err != nil {
			errs = append(errs, err)
		}
	}
	if err != nil {
		return upgradeMigration{}, errors.Join(errs...)
	}
	return mig, nil
}

// readMigration reads the migrations since the upgrade's start from dir
// with r. missing reports that dir doesn't exist, so nothing migrated.
func readMigration(ctx context.Context, r backupsReader, dir string, since time.Time) (mig upgradeMigration, missing bool, err error) {
	names, err := r.list(ctx, dir)
	if err != nil {
		return mig, errors.Is(err, fs.ErrNotExist), err
	}
	after := since.Add(-backupClockSkew).Truncate(time.Millisecond)
	type found struct {
		name string
		at   time.Time
	}
	var markers []found
	for _, name := range names {
		if at, ok := store.MarkerTime(name); ok && !at.Before(after) {
			markers = append(markers, found{name, at})
		}
	}
	slices.SortFunc(markers, func(a, b found) int { return a.at.Compare(b.at) })
	for _, f := range markers {
		p := filepath.Join(dir, f.name)
		data, err := r.read(ctx, p)
		if err != nil {
			return upgradeMigration{}, false, fmt.Errorf("reading %s: %w", p, err)
		}
		m, err := store.ParseMigrationMarker(data)
		if err != nil {
			return upgradeMigration{}, false, fmt.Errorf("%s: %w", p, err)
		}
		mig.Markers = append(mig.Markers, m)
		// The oldest backup holds the database as the previous release
		// left it; a later run's backs up what an earlier one migrated.
		if mig.Backup == "" && m.Backup != "" {
			// The control plane names the backup by its own path; the
			// directory is mounted at the same path, but the name is all
			// that needs to agree.
			mig.Backup = filepath.Join(dir, filepath.Base(m.Backup))
		}
	}
	if len(mig.Markers) == 0 {
		if best := oldestBackup(names, since); best != "" {
			mig.Backup = filepath.Join(dir, best)
		}
	}
	return mig, false, nil
}

// oldestBackup picks, from the file names of a backups directory, the
// oldest pre-migration backup taken after the upgrade started: the first
// migration's, which holds the database as the previous release left it.
func oldestBackup(names []string, since time.Time) string {
	since = since.Add(-backupClockSkew).Truncate(time.Millisecond)
	var best string
	var bestAt time.Time
	for _, name := range names {
		m := backupName.FindStringSubmatch(name)
		if m == nil {
			continue
		}
		at, err := time.Parse(backupTimeLayout, m[1])
		if err != nil || at.Before(since) {
			continue
		}
		if best == "" || at.Before(bestAt) {
			best, bestAt = name, at
		}
	}
	return best
}

// copyTree copies the directory src to dst, which must not exist, keeping
// file modes. The platform project holds only directories and regular
// files.
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case d.IsDir():
			if err := os.MkdirAll(target, 0o700); err != nil {
				return err
			}
			return os.Chmod(target, info.Mode().Perm())
		case d.Type().IsRegular():
			data, err := os.ReadFile(path) //nolint:gosec // G304: files in the data directory
			if err != nil {
				return err
			}
			return os.WriteFile(target, data, info.Mode().Perm()) //nolint:gosec // G703: a copy within the data directory
		default:
			return fmt.Errorf("%s: not a regular file or directory", path)
		}
	})
}

// platformProjectName returns the platform's Compose project name.
func (a *app) platformProjectName() string {
	switch {
	case a.opts.PlatformProjectName != "":
		return a.opts.PlatformProjectName
	case a.cfg.PlatformProject != "":
		return a.cfg.PlatformProject
	}
	return platform.ProjectName
}

// composeOverrideFile is the file Compose merges into compose.yaml on its
// own. PressFleet doesn't render one, but the end-to-end tests do.
const composeOverrideFile = "compose.override.yaml"

// upgradeProject returns the platform's Compose project in dir, read from
// files if they aren't dir's own (nil).
func (a *app) upgradeProject(name, dir string, files []string) UpgradeProject {
	if a.opts.UpgradeProject != nil {
		return a.opts.UpgradeProject(name, dir, files)
	}
	return compose.Project{Name: name, Dir: dir, Files: files}
}
