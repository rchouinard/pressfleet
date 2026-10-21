package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/rchouinard/pressfleet/control-plane/internal/adminlogin"
	"github.com/rchouinard/pressfleet/control-plane/internal/backup/repository"
	"github.com/rchouinard/pressfleet/control-plane/internal/backup/statebackup"
	"github.com/rchouinard/pressfleet/control-plane/internal/secrets"
	"github.com/rchouinard/pressfleet/control-plane/internal/sites"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
)

// Disaster recovery of the state database (#96, package backup/statebackup).
// These commands run inside the control plane container, which owns the
// state database (§4.7): `state backup` and `state backups` next to the
// running control plane (docker compose exec), `state recover` in a
// one-off container while it is stopped, as the host's `pressfleet
// recover` runs it.

// stateBackupWait bounds how long `state backup` waits for its job.
const stateBackupWait = 30 * time.Minute

// stateRepository returns a repository manager for the state repository
// alone: it doesn't open the state database, which `state recover` is about
// to replace.
func (a *app) stateRepository(keys *secrets.Keyring) (*repository.Manager, error) {
	s3, err := backupS3(a.cfg.Backup, keys)
	if err != nil {
		return nil, err
	}
	return repository.New(repository.Options{
		Layout: a.cfg.Layout(), Backend: a.cfg.Backup.Backend, S3: s3,
		StatePassword: keys.StateBackupPassword(), Logger: a.log.With("component", "state-backup"),
	})
}

func (a *app) stateBackupCommand() *cobra.Command {
	var noWait bool
	cmd := &cobra.Command{
		Use:   "backup",
		Short: "Back up the state database for disaster recovery now",
		Long: `Queue a backup of the state database into the state repository now, instead
of at its next scheduled time (backup.state.interval_hours), and wait for
the running control plane to take it. See docs/disaster-recovery.md.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			db, err := a.openStateDB(ctx)
			if err != nil {
				return err
			}
			defer func() { _ = db.Close() }()
			job, err := statebackup.Enqueue(ctx, db)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if noWait {
				_, err = fmt.Fprintf(out, "Queued state backup job %s\n", job.ID)
				return err
			}
			done, err := waitForJob(ctx, db, job.ID, stateBackupWait)
			if err != nil {
				return err
			}
			if done.State != store.JobSucceeded {
				return fmt.Errorf("state backup job %s %s: %s", job.ID, done.State, done.Error)
			}
			var result struct {
				SnapshotID string `json:"snapshot_id"`
			}
			_ = json.Unmarshal(done.Result, &result)
			_, err = fmt.Fprintf(out, "Backed up the state database as snapshot %s (job %s)\n", result.SnapshotID, job.ID)
			return err
		},
	}
	cmd.Flags().BoolVar(&noWait, "no-wait", false, "queue the backup and return without waiting for it")
	return cmd
}

// waitForJob polls a job until it is finished or the timeout passes.
func waitForJob(ctx context.Context, db *store.DB, jobID string, timeout time.Duration) (store.Job, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		j, err := store.NewJobs(db).Get(ctx, jobID)
		if err != nil {
			return store.Job{}, err
		}
		if j.State.Final() {
			return j, nil
		}
		select {
		case <-ctx.Done():
			return store.Job{}, fmt.Errorf("job %s is still %s; is the control plane running? (%w)", jobID, j.State, ctx.Err())
		case <-tick.C:
		}
	}
}

func (a *app) stateBackupsCommand() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "backups",
		Short: "List the state database's disaster recovery backups",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			keys, err := a.loadKeyring()
			if err != nil {
				return err
			}
			repo, err := a.stateRepository(keys)
			if err != nil {
				return err
			}
			snaps, err := statebackup.List(cmd.Context(), repo)
			if err != nil {
				return err
			}
			if asJSON {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(snaps)
			}
			return printStateBackups(cmd.OutOrStdout(), repo.StateLocation(), snaps)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the backups as JSON")
	return cmd
}

func printStateBackups(w io.Writer, location string, snaps []statebackup.Snapshot) error {
	if len(snaps) == 0 {
		_, err := fmt.Fprintf(w, "No state backups in %s\n", location)
		return err
	}
	if _, err := fmt.Fprintf(w, "State backups in %s, oldest first:\n\n", location); err != nil {
		return err
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "ID\tTAKEN (UTC)\tSCHEMA\tDATABASE\tRELEASE")
	for _, s := range snaps {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%d\t%s\t%s\n", shortID(s.ID), s.Time.UTC().Format("2006-01-02 15:04:05"), s.SchemaVersion, s.Dialect, s.Version)
	}
	return tw.Flush()
}

// shortID is restic's short form of a snapshot ID.
func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// stateRecoverFlags are the flags of `state recover` and `recover`.
type stateRecoverFlags struct {
	snapshot string
	force    bool
}

func (a *app) stateRecoverCommand() *cobra.Command {
	var f stateRecoverFlags
	cmd := &cobra.Command{
		Use:   "recover",
		Short: "Replace the state database with a disaster recovery backup (control plane stopped)",
		Long: `Replace the state database with a state backup from the state repository,
to rebuild the platform on a new host (see docs/disaster-recovery.md). The
host's "pressfleet recover" runs it in a one-off control plane container
with the control plane stopped; run that instead.

The backup must open with this installation's master key, and its login
key must decrypt with it (a missing or unusable one is regenerated when the
control plane starts). Unfinished jobs from the old host are failed, and
every site that was running or stopped gets a job that recreates it from
its newest backup once the control plane starts. Nothing changes unless all
of that succeeds.

It refuses to replace a database that already has client groups or sites,
which a fresh installation doesn't, unless --force is given.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.stateRecover(cmd.Context(), cmd.OutOrStdout(), f)
		},
	}
	cmd.Flags().StringVar(&f.snapshot, "snapshot", "latest", "the state backup to restore: its ID (8 characters or more) or latest")
	cmd.Flags().BoolVar(&f.force, "force", false, "replace a state database that already has client groups or sites")
	return cmd
}

// errNotFresh means state recover was refused because the state database
// isn't a fresh installation's.
var errNotFresh = errors.New("the state database already has client groups or sites")

func (a *app) stateRecover(ctx context.Context, out io.Writer, f stateRecoverFlags) error {
	// Loading must finish once started: it is one transaction, and a
	// half-done restore is no use to anyone.
	ctx = context.WithoutCancel(ctx)
	keys, err := a.loadKeyring()
	if err != nil {
		return err
	}
	if !f.force {
		if err := a.checkFreshState(ctx); err != nil {
			return err
		}
	}
	repo, err := a.stateRepository(keys)
	if err != nil {
		return err
	}
	snap, err := statebackup.Find(ctx, repo, f.snapshot)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(out, "Restoring state backup %s, taken %s (schema version %d, %s, release %s), from %s\n",
		shortID(snap.ID), snap.Time.UTC().Format(time.RFC3339), snap.SchemaVersion, snap.Dialect, orUnknown(snap.Version), repo.StateLocation()); err != nil {
		return err
	}
	var (
		loginKey adminlogin.RestoredKey
		recovery sites.Recovery
		moved    int
	)
	res, err := statebackup.Restore(ctx, repo, snap, statebackup.RestoreOptions{
		Store: a.storeConfig(), BackupDir: a.cfg.Layout().StateBackupsDir(), Logger: a.log,
		Check: func(ctx context.Context, q store.Querier) error {
			if _, err := secrets.VerifyIn(ctx, q, keys); err != nil {
				return err
			}
			var err error
			if loginKey, err = adminlogin.CheckRestored(ctx, q, secrets.NewStore(q, keys)); err != nil {
				return err
			}
			// The backup records where the old host kept the repositories.
			if moved, err = repository.RelocateLocal(ctx, q, a.cfg.Layout()); err != nil {
				return err
			}
			now := time.Now()
			if err := statebackup.SettleRestored(ctx, q, now); err != nil {
				return err
			}
			recovery, err = sites.PrepareRecovery(ctx, q, now)
			return err
		},
	})
	if err != nil {
		return err
	}
	return printRecovery(out, res, loginKey, recovery, moved, a.cfg.Layout().BackupRepositoriesDir())
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

// checkFreshState refuses to replace a state database that holds client
// groups or sites. A fresh installation has an admin user and default
// settings, but neither.
func (a *app) checkFreshState(ctx context.Context) error {
	db, err := a.openStateDB(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	var groups, siteCount int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM client_groups").Scan(&groups); err != nil {
		return err
	}
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM sites").Scan(&siteCount); err != nil {
		return err
	}
	if groups > 0 || siteCount > 0 {
		return fmt.Errorf("%w (%d client groups, %d sites); recover replaces all of it, so it is meant for a fresh installation. Pass --force to replace it anyway", errNotFresh, groups, siteCount)
	}
	return nil
}

func printRecovery(out io.Writer, res store.LoadResult, key adminlogin.RestoredKey, rec sites.Recovery, relocated int, reposDir string) error {
	var lines []string
	add := func(format string, args ...any) { lines = append(lines, fmt.Sprintf(format, args...)) }
	add("Restored %d rows in %d tables; the state database is at schema version %d.", res.Dump.Rows, res.Dump.Tables, res.SchemaVersion)
	if res.PreRestorePath != "" {
		add("The replaced database was saved as %s.", res.PreRestorePath)
	}
	add("The backup opens with this installation's master key.")
	switch key {
	case adminlogin.KeyRestored:
		add("The platform login key was restored and decrypts with the master key; sites keep their public key.")
	case adminlogin.KeyMissing:
		add("The backup has no platform login key; the control plane generates one when it starts, and sites get it as they are recovered.")
	case adminlogin.KeyDiscarded:
		add("The backup's platform login key was unusable and was removed; the control plane generates a new one when it starts, and sites get it as they are recovered.")
	}
	if relocated > 0 {
		add("Pointed %d local backup repositories at %s, where this host's data directory keeps them.", relocated, reposDir)
	}
	if rec.InterruptedJobs > 0 {
		add("Failed %d jobs the old host hadn't finished.", rec.InterruptedJobs)
	}
	add("%d sites will be recovered from their newest backups when the control plane starts.", len(rec.Recovering))
	if len(rec.Failed) > 0 {
		add("%d sites were being provisioned or deleted and are marked failed: %v.", len(rec.Failed), rec.Failed)
	}
	for _, l := range lines {
		if _, err := fmt.Fprintln(out, l); err != nil {
			return err
		}
	}
	return nil
}
