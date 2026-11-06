package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/platform"
	"github.com/rchouinard/pressfleet/control-plane/internal/skew"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
)

// Rolling back an upgrade (design doc §4.7, #249).
//
// A failed upgrade returns the platform to the release before it with the
// same steps the manual recovery prints (recoveryTemplate), run by the CLI:
// stop the control plane, restore the state database if the upgrade
// migrated it, put the platform project back, start the previous release's
// services, and wait for its control plane. Each step can run again, and
// the upgrade record keeps the rollback's progress, so a rollback that
// stops can be resumed by the next `pressfleet upgrade`, or finished by
// hand with the printed steps. `pressfleet upgrade --rollback` (#250) runs
// the same sequence (upgrader.rollBack).

// Steps of the rollback, as the upgrade record names them.
const (
	stepStopControlPlane = "stop-control-plane"
	stepRestoreState     = "restore-state"
	stepRestoreProject   = "restore-project"
	stepStartServices    = "start-services"
	stepWaitReady        = "wait-ready"
)

// rollbackSteps describes the steps, in order, as the recovery steps
// number them (waiting for the control plane is part of step 4 there).
var rollbackSteps = []struct{ name, desc string }{
	{stepStopControlPlane, "1, stopping the control plane"},
	{stepRestoreState, "2, restoring the state database"},
	{stepRestoreProject, "3, putting the previous platform project back"},
	{stepStartServices, "4, starting the previous release's services"},
	{stepWaitReady, "4, waiting for the previous control plane"},
}

func stepDesc(name string) string {
	for _, s := range rollbackSteps {
		if s.name == name {
			return s.desc
		}
	}
	return name
}

// rollbackRecord is a rollback's progress in the upgrade record.
type rollbackRecord struct {
	StartedAt time.Time `json:"started_at"`
	// Cause is why the rollback started: the upgrade's error.
	Cause string `json:"cause,omitempty"`
	// Done are the steps that finished, in this run or an earlier one.
	// A resumed rollback skips only the state restore (listed only if it
	// restored a backup): the other steps are cheap to run again, and
	// must be, because what they find may have changed since.
	Done []string `json:"done,omitempty"`
	// StateRestoredFrom is the pre-migration backup the state database
	// was restored from.
	StateRestoredFrom string     `json:"state_restored_from,omitempty"`
	FinishedAt        *time.Time `json:"finished_at,omitempty"`
	// StoppedAt is the step a stopped rollback stopped at, empty if it
	// refused to start; Error is why.
	StoppedAt string `json:"stopped_at,omitempty"`
	Error     string `json:"error,omitempty"`
}

// Errors of a rollback that refuses to start. Nothing has changed then.
var (
	// errRollbackRefused means the rollback can't start.
	errRollbackRefused = errors.New("the rollback can't start")
	// errPostgresMigrated means the upgrade migrated a PostgreSQL state
	// database, which PressFleet doesn't back up: the previous control
	// plane would refuse it, and only the operator's backup brings it
	// back.
	errPostgresMigrated = errors.New("the upgrade migrated the PostgreSQL state database, which PressFleet doesn't back up")
)

// rollbackError is a rollback that stopped. step is empty if it refused to
// start, and mig is what it knew about the upgrade's migrations (migKnown
// false if it couldn't tell).
type rollbackError struct {
	step     string
	err      error
	mig      upgradeMigration
	migKnown bool
}

func (e *rollbackError) Error() string {
	if e.step == "" {
		return e.err.Error()
	}
	return fmt.Sprintf("step %s: %v", stepDesc(e.step), e.err)
}

func (e *rollbackError) Unwrap() error { return e.err }

// rollbackResult is what a finished rollback did.
type rollbackResult struct {
	// RestoredFrom is the backup the state database was restored from;
	// empty if the upgrade didn't migrate it.
	RestoredFrom string
}

// rollbackSignals returns a context for the rollback that the next SIGINT
// or SIGTERM cancels. main cancels the CLI's own context on the first one
// and keeps catching them until the CLI returns, so the signal that started
// a rollback doesn't stop it, and a second one does. Tests replace it.
var rollbackSignals = func(ctx context.Context) (context.Context, context.CancelFunc) {
	return signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
}

// rollBack returns the platform to the release before the upgrade in
// u.record (design doc §4.7). It needs the record and the project fields
// of u (name, layout, platformDir, upgradeDir, project), and runs every step
// on ctx; callers give it one an interrupt doesn't cancel by accident. It
// refuses to start, changing nothing, when it can't finish safely: without
// a saved platform project, across a MariaDB series change, when it can't
// tell whether the upgrade migrated the state database, and when the
// upgrade migrated a PostgreSQL one. The record's status follows its
// progress: rolling-back, then rolled-back or rollback-failed (or failed
// again if it refused). Errors are *rollbackError.
func (u *upgrader) rollBack(ctx context.Context, cause string) (rollbackResult, error) {
	if u.record.Rollback == nil || (u.record.Status != upgradeRollingBack && u.record.Status != upgradeRollbackFailed) {
		u.record.Rollback = &rollbackRecord{StartedAt: time.Now().UTC(), Cause: cause}
	} else {
		u.printf("Resuming the rollback that started at %s.\n", u.record.Rollback.StartedAt.Format(time.RFC3339))
	}
	rb := u.record.Rollback
	rb.StoppedAt, rb.Error, rb.FinishedAt = "", "", nil
	postgres := u.a.cfg.Database.Driver == string(store.Postgres)

	mig, known, err := u.checkRollback(ctx, postgres)
	if err != nil {
		rerr := &rollbackError{err: err, mig: mig, migKnown: known}
		if len(rb.Done) == 0 {
			// Nothing changed: the upgrade is as failed as it was.
			u.record.Status = upgradeFailed
		} else {
			u.record.Status = upgradeRollbackFailed
		}
		rb.Error = err.Error()
		if werr := u.writeRecord(); werr != nil {
			u.a.warnf("recording the rollback: %v", werr)
		}
		return rollbackResult{}, rerr
	}

	u.record.Status = upgradeRollingBack
	if err := u.writeRecord(); err != nil {
		return rollbackResult{}, &rollbackError{err: fmt.Errorf("recording the rollback: %w", err), mig: mig, migKnown: known}
	}
	step := func(name string, run func() error) error {
		if err := run(); err != nil {
			u.record.Status = upgradeRollbackFailed
			rb.StoppedAt, rb.Error = name, err.Error()
			if werr := u.writeRecord(); werr != nil {
				u.a.warnf("recording the rollback: %v", werr)
			}
			return &rollbackError{step: name, err: err, mig: mig, migKnown: known}
		}
		// The state restore is done only if it ran: without a migration,
		// a resumed rollback looks again.
		restored := name != stepRestoreState || rb.StateRestoredFrom != ""
		if restored && !slices.Contains(rb.Done, name) {
			rb.Done = append(rb.Done, name)
		}
		if err := u.writeRecord(); err != nil {
			u.a.warnf("recording the rollback: %v", err)
		}
		return nil
	}

	if err := step(stepStopControlPlane, func() error {
		u.printf("  Stopping the control plane.\n")
		return u.project.StopServices(ctx, controlPlaneService)
	}); err != nil {
		return rollbackResult{}, err
	}
	if err := step(stepRestoreState, func() error {
		if slices.Contains(rb.Done, stepRestoreState) {
			u.printf("  The state database was restored from %s already.\n", rb.StateRestoredFrom)
			return nil
		}
		// Read again with the control plane stopped: it may have
		// migrated since the check.
		var err error
		if mig, err = u.findMigration(ctx); err != nil {
			known = false
			return fmt.Errorf("%w: %w", errMigrationUnknown, err)
		}
		switch {
		case !mig.Migrated():
			u.printf("  The upgrade didn't migrate the state database; it stays as it is.\n")
			return nil
		case postgres:
			return errPostgresMigrated
		case mig.Backup == "":
			return errors.New("the upgrade migrated the state database, but there is no backup from before the migration")
		}
		u.printf("  Restoring the state database from %s with the previous control plane image.\n", mig.Backup)
		prev := u.a.upgradeProject(u.name, filepath.Join(u.upgradeDir, previousPlatform), nil)
		out, err := prev.RunService(ctx, controlPlaneService, "state", "restore", mig.Backup)
		if len(out) > 0 {
			u.printf("%s\n", indent(strings.TrimRight(string(out), "\n")))
		}
		if err != nil {
			return fmt.Errorf("restoring %s: %w", mig.Backup, err)
		}
		rb.StateRestoredFrom = mig.Backup
		return nil
	}); err != nil {
		return rollbackResult{}, err
	}
	if err := step(stepRestoreProject, func() error {
		prev := filepath.Join(u.upgradeDir, previousPlatform)
		u.printf("  Putting the platform project from before the upgrade back from %s.\n", prev)
		return restoreProject(prev, u.platformDir)
	}); err != nil {
		return rollbackResult{}, err
	}
	if err := step(stepStartServices, func() error {
		u.printf("  Starting the previous release's services.\n")
		if err := u.upPlatform(ctx, nil); err != nil {
			return err
		}
		if len(u.record.Forced) == 0 {
			return nil
		}
		u.printf("  Recreating %s on the previous files.\n", strings.Join(u.record.Forced, ", "))
		return u.upPlatform(ctx, u.record.Forced)
	}); err != nil {
		return rollbackResult{}, err
	}
	if err := step(stepWaitReady, func() error {
		u.printf("  Waiting for the control plane to report ready at %s.\n", u.record.From)
		return u.waitReady(ctx, u.record.From)
	}); err != nil {
		return rollbackResult{}, err
	}

	now := time.Now().UTC()
	rb.FinishedAt = &now
	u.record.Status = upgradeRolledBack
	if err := u.writeRecord(); err != nil {
		u.a.warnf("recording the rollback: %v", err)
	}
	if err := os.RemoveAll(filepath.Join(u.upgradeDir, stagedPlatform)); err != nil {
		u.a.warnf("removing the staged platform project: %v", err)
	}
	return rollbackResult{RestoredFrom: rb.StateRestoredFrom}, nil
}

// errMigrationUnknown means the rollback couldn't tell whether the upgrade
// migrated the state database.
var errMigrationUnknown = errors.New("can't tell whether the upgrade migrated the state database")

// checkRollback refuses a rollback that can't finish safely, before it
// changes anything, and returns the migrations it found; known is false if
// it didn't find out.
func (u *upgrader) checkRollback(ctx context.Context, postgres bool) (mig upgradeMigration, known bool, err error) {
	prev := filepath.Join(u.upgradeDir, previousPlatform)
	if !exists(filepath.Join(prev, platform.ComposeFile)) {
		return mig, false, fmt.Errorf("%w: there is no saved platform project in %s", errRollbackRefused, prev)
	}
	if err := u.checkRollbackMariaDB(ctx); err != nil {
		return mig, false, err
	}
	mig, err = u.findMigration(ctx)
	if err != nil {
		return mig, false, fmt.Errorf("%w: %w: %w", errRollbackRefused, errMigrationUnknown, err)
	}
	switch {
	case slices.Contains(u.record.Rollback.Done, stepRestoreState) || !mig.Migrated():
	case postgres:
		return mig, true, fmt.Errorf("%w: %w", errRollbackRefused, errPostgresMigrated)
	case mig.Backup == "":
		return mig, true, fmt.Errorf("%w: the upgrade migrated the state database, but there is no backup from before the migration", errRollbackRefused)
	}
	return mig, true, nil
}

// checkRollbackMariaDB refuses to roll back across a MariaDB release series
// change: MariaDB upgrades its data files to a new series in place, and the
// previous series can't read them (decision 7 of #245). Upgrades refuse
// series changes today, so this guards against a later release that
// allows them.
func (u *upgrader) checkRollbackMariaDB(ctx context.Context) error {
	before := u.record.MariaDBSeries
	if img, ok := u.record.PreviousImages[mariaDBService]; ok && before == "" {
		before = MariaDBSeries(img.Image)
	}
	if before == "" {
		return nil
	}
	image, found, err := u.runningImage(ctx, mariaDBService)
	if err != nil {
		return fmt.Errorf("%w: finding MariaDB: %w", errRollbackRefused, err)
	}
	if !found {
		return nil
	}
	switch now := MariaDBSeries(image); now {
	case before:
		return nil
	case "":
		return fmt.Errorf("%w: can't tell the MariaDB release series of the running image %s, so can't check that "+
			"the rollback keeps it at %s", errRollbackRefused, image, before)
	default:
		return fmt.Errorf("%w: MariaDB ran the %s release series before the upgrade and runs %s now; its data files "+
			"can't go back to an older series", errRollbackRefused, before, now)
	}
}

// rollBackFailed rolls back an upgrade that failed with cause, reports the
// outcome, and returns the exit code: exitRolledBack when the platform is
// back on the previous release, 1 with the recovery steps when it isn't.
// The rollback runs on a context of its own, which the interrupt that may
// have caused the failure doesn't cancel, and the next one does.
func (u *upgrader) rollBackFailed(ctx context.Context, cause error) error {
	stderr := u.a.opts.Stderr
	_, _ = fmt.Fprintf(stderr, "Error: the upgrade to %s failed: %v\n", u.record.To, cause)
	from := u.record.From
	if from == "" {
		from = "the previous release"
	}
	u.printf("\nRolling back to %s. Sites keep serving; interrupt again to stop the rollback.\n", from)
	rctx, stop := rollbackSignals(context.WithoutCancel(ctx))
	defer stop()
	res, err := u.rollBack(rctx, cause.Error())
	if err != nil {
		return u.rollbackStopped(context.WithoutCancel(ctx), rctx, err)
	}

	u.printf("\nThe upgrade to %s failed and was rolled back: the platform is back on %s and healthy.\n", u.record.To, from)
	if res.RestoredFrom != "" {
		u.printf("  The state database was restored from %s, the backup from before the upgrade migrated it: "+
			"anything the control plane recorded since the upgrade started is lost. The database it replaced was saved beside it.\n",
			res.RestoredFrom)
	} else {
		u.printf("  The upgrade didn't migrate the state database, which was kept as it was: nothing recorded is lost.\n")
	}
	u.printf("  The platform project from before the upgrade was put back, and the previous release's services started.\n")
	u.printf("  Fix the cause, and run pressfleet upgrade again to retry.\n")
	if note := u.skewNote(u.record.From); note != "" {
		u.printf("\n%s\n", note)
	}
	return &exitError{code: exitRolledBack}
}

// rollbackStopped reports a rollback that stopped, with the recovery steps
// that finish it from where it stopped, and returns exit code 1.
func (u *upgrader) rollbackStopped(ctx, rctx context.Context, err error) error {
	stderr := u.a.opts.Stderr
	var rerr *rollbackError
	if !errors.As(err, &rerr) {
		rerr = &rollbackError{err: err}
	}
	switch {
	case rerr.step == "" && errors.Is(err, errPostgresMigrated):
		_, _ = fmt.Fprintf(stderr, "\nThe upgrade migrated the PostgreSQL state database, which PressFleet doesn't back up, "+
			"so the upgrade can't roll back on its own; nothing was rolled back.\n")
	case rerr.step == "":
		_, _ = fmt.Fprintf(stderr, "\nThe rollback didn't start, and nothing was rolled back: %v\n", rerr.err)
	case rctx.Err() != nil:
		_, _ = fmt.Fprintf(stderr, "\nThe rollback was interrupted at step %s.\n", stepDesc(rerr.step))
	default:
		_, _ = fmt.Fprintf(stderr, "\nThe rollback stopped at step %s: %v\n", stepDesc(rerr.step), rerr.err)
	}
	if rerr.step != "" {
		_, _ = fmt.Fprintf(stderr, "The steps before it are done; running them again is safe.\n")
	}
	_, _ = fmt.Fprintf(stderr, "\n")

	data := u.recoveryData()
	if !data.Postgres {
		mig, known := rerr.mig, rerr.migKnown
		if !known {
			var ferr error
			if mig, ferr = u.findMigration(ctx); ferr != nil {
				u.a.warnf("looking for the pre-migration backup: %v", ferr)
			}
			known = ferr == nil
		}
		data.BackupUnchecked = !known
		data.Backup = mig.Backup
		if !mig.Migrated() {
			data.Backup = ""
		}
	}
	steps, terr := renderRecovery(data)
	if terr != nil {
		return errors.Join(err, terr)
	}
	_, _ = io.WriteString(stderr, steps)
	return &exitError{code: 1}
}

// skewNote says which CLI to install again when this one is outside the
// version skew policy with the control plane's version from.
func (u *upgrader) skewNote(from string) string {
	cliV, err := skew.Parse(u.to)
	if err != nil {
		return ""
	}
	cpV, err := skew.Parse(from)
	if err != nil {
		return ""
	}
	var se *skew.Error
	if !errors.As(skew.Check(cliV, cpV), &se) {
		return ""
	}
	return fmt.Sprintf("This CLI (%s) can't manage a control plane at %s (%s). Install the pressfleet CLI %s again "+
		"(any of %s works) to manage the platform until you upgrade it.", u.to, from, se.Rule(), from, se.CompatibleCLIs())
}

// resolveInterrupted handles an upgrade whose CLI was killed while it
// applied the release or rolled it back: the record says in progress or
// rolling back, and no CLI is running it. The operator retries it or rolls
// it back; another release can't be upgraded to over it. --yes retries an
// interrupted upgrade and resumes an interrupted rollback.
func (u *upgrader) resolveInterrupted(rec upgradeRecord) error {
	from := rec.From
	if from == "" {
		from = "the previous release"
	}
	switch rec.Status {
	case upgradeInProgress:
		u.printf("The last upgrade, from %s to %s, was interrupted: its record says it is in progress, but it isn't running.\n", from, rec.To)
	case upgradeRollingBack:
		u.printf("The rollback of the last upgrade, from %s to %s, was interrupted.\n", from, rec.To)
	case upgradeRollbackFailed:
		reason := ""
		if rec.Rollback != nil && rec.Rollback.Error != "" {
			reason = ": " + rec.Rollback.Error
		}
		u.printf("The rollback of the last upgrade, from %s to %s, stopped%s.\n", from, rec.To, reason)
	default:
		u.printf("The last upgrade, from %s to %s, didn't finish (status %q).\n", from, rec.To, rec.Status)
	}
	u.record = rec
	canRetry := rec.To == u.to
	rollingBack := rec.Status == upgradeRollingBack || rec.Status == upgradeRollbackFailed
	retry := func() error {
		u.printf("Retrying the upgrade to %s; the platform project from before it (%s) stays in %s.\n",
			rec.To, from, filepath.Join(u.upgradeDir, previousPlatform))
		u.keepPrevious = true
		return nil
	}
	rollBack := func() error {
		u.rollBackInterrupted = true
		return nil
	}
	if !canRetry {
		u.printf("This CLI is %s, so it can't retry the upgrade to %s, and won't upgrade to %s over it. "+
			"It can roll the upgrade back to %s; then run pressfleet upgrade again.\n", u.to, rec.To, u.to, from)
	}
	if u.f.yes {
		switch {
		case rollingBack:
			u.printf("Resuming the rollback (--yes).\n")
			return rollBack()
		case canRetry:
			u.printf("Retrying (--yes).\n")
			return retry()
		}
		return fmt.Errorf("the last upgrade, to %s, was interrupted: roll it back by answering the prompt without --yes, "+
			"or retry it with the pressfleet CLI %s", rec.To, rec.To)
	}
	question := fmt.Sprintf("Retry the upgrade to %s, or roll back to %s? [retry/rollback/cancel]: ", rec.To, from)
	if !canRetry {
		question = fmt.Sprintf("Roll back to %s? [y/N]: ", from)
	}
	for {
		u.printf("%s", question)
		line, err := u.in.ReadString('\n')
		if err != nil && (!errors.Is(err, io.EOF) || line == "") {
			u.printf("\n")
			return fmt.Errorf("the last upgrade, to %s, was interrupted: answer the prompt to retry it or roll it back", rec.To)
		}
		switch answer := strings.ToLower(strings.TrimSpace(line)); {
		case canRetry && (answer == "retry" || answer == "r"):
			return retry()
		case canRetry && (answer == "rollback" || answer == "roll back" || answer == "b"),
			!canRetry && (answer == "y" || answer == "yes"):
			return rollBack()
		case answer == "", answer == "cancel", answer == "c", answer == "n", answer == "no":
			return errors.New("upgrade canceled; nothing was changed")
		}
	}
}

// renderedDynamicFiles are the files of Traefik's dynamic directory that
// the platform project renders. The rest are the control plane's: the
// sites' domain and maintenance routes, which it writes at runtime.
var renderedDynamicFiles = []string{platform.TraefikDynamicConfigFile, platform.TraefikMetricsConfigFile}

// runtimeFile reports whether rel, a path relative to the platform
// directory, is in Traefik's dynamic directory and not rendered with the
// project. A rollback leaves those to the previous control plane, whose
// startup reconcile rewrites them: the saved copies would revert routes
// changed since the upgrade (decision 6 of #245).
func runtimeFile(rel string) bool {
	rel = filepath.ToSlash(rel)
	return strings.HasPrefix(rel, platform.TraefikDynamicDir+"/") && !slices.Contains(renderedDynamicFiles, rel)
}

// restoreProject puts the platform project saved in src back into dst:
// every rendered file of src is copied, with its mode, and the files and
// directories of dst that src lacks are removed, so the result has the
// rendered files src had, exactly. The control plane's runtime files in
// Traefik's dynamic directory (runtimeFile) aren't touched. Running it
// again changes nothing, and files whose content is the same aren't
// rewritten, so the containers that bind-mount them keep seeing them.
func restoreProject(src, dst string) error {
	err := filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if runtimeFile(rel) {
			return nil
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
			same, err := sameFile(path, target)
			if err != nil {
				return err
			}
			if same {
				return os.Chmod(target, info.Mode().Perm())
			}
			data, err := os.ReadFile(path) //nolint:gosec // G304: files in the data directory
			if err != nil {
				return err
			}
			return writeFileAtomic(target, data, info.Mode().Perm())
		default:
			return fmt.Errorf("%s: not a regular file or directory", path)
		}
	})
	if err != nil {
		return fmt.Errorf("copying the saved platform project back: %w", err)
	}
	root, err := os.OpenRoot(dst)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	dynamic := filepath.FromSlash(platform.TraefikDynamicDir)
	err = fs.WalkDir(root.FS(), ".", func(rel string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		rel = filepath.FromSlash(rel)
		// Traefik's dynamic directory, and the directories holding it,
		// stay even if the saved project has none: it's the control
		// plane's.
		if rel == dynamic || strings.HasPrefix(dynamic, rel+string(filepath.Separator)) {
			return nil
		}
		if runtimeFile(rel) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if _, err := os.Lstat(filepath.Join(src, rel)); err == nil {
			return nil
		} else if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		if d.IsDir() {
			if err := root.RemoveAll(rel); err != nil {
				return err
			}
			return fs.SkipDir
		}
		return root.Remove(rel)
	})
	if err != nil {
		return fmt.Errorf("removing the files the upgrade added: %w", err)
	}
	return nil
}

// indent indents every line of s for the rollback's output.
func indent(s string) string {
	return "    " + strings.ReplaceAll(s, "\n", "\n    ")
}
