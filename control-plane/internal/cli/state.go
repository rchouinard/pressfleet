package cli

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/rchouinard/pressfleet/control-plane/internal/store"
)

// storeConfig maps the configuration to the store's.
func (a *app) storeConfig() store.Config {
	return store.Config{
		Driver:      store.Dialect(a.cfg.Database.Driver),
		SQLitePath:  a.cfg.SQLitePath(),
		PostgresDSN: a.cfg.Database.PostgresDSN,
		Version:     a.build().Version,
	}
}

// openStateDB opens the state database and migrates it, backing it up first
// if migrations are pending (design doc §4.7).
func (a *app) openStateDB(ctx context.Context) (*store.DB, error) {
	// Don't abandon a backup or migration halfway because of a shutdown
	// signal; both are short, and the next start would redo them anyway.
	ctx = context.WithoutCancel(ctx)
	db, err := store.Open(ctx, a.storeConfig(), a.log)
	if err != nil {
		return nil, err
	}
	res, err := db.Migrate(ctx, a.cfg.Layout().StateBackupsDir())
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	a.log.Info("state database ready", "driver", a.cfg.Database.Driver, "schema_version", res.To)
	return db, nil
}

func (a *app) stateCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "state",
		Short: "Manage the control plane state database (run inside the control plane container)",
	}
	cmd.AddCommand(a.stateRestoreCommand(), a.stateBackupCommand(), a.stateBackupsCommand(), a.stateRecoverCommand())
	return cmd
}

func (a *app) stateRestoreCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "restore <backup>",
		Short: "Replace the SQLite state database with a backup",
		Long: `Replace the SQLite state database with a backup taken before a migration.

The control plane must be stopped. The current database is backed up first,
so the restore can be undone. A bare file name is looked up in the backups
directory (<data_dir>/state/backups). PostgreSQL databases are restored with
your own PostgreSQL tooling.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			backupDir := a.cfg.Layout().StateBackupsDir()
			backup, err := resolveBackup(args[0], backupDir)
			if err != nil {
				return err
			}
			res, err := store.Restore(cmd.Context(), a.storeConfig(), backup, backupDir, a.log)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if _, err := fmt.Fprintf(out, "Restored %s (schema version %d) to %s\n", backup, res.SchemaVersion, a.cfg.SQLitePath()); err != nil {
				return err
			}
			if res.PreRestorePath != "" {
				_, err = fmt.Fprintf(out, "The previous database was saved as %s\n", res.PreRestorePath)
			}
			return err
		},
	}
}

// resolveBackup finds a backup given as a path or as a file name in the
// backups directory.
func resolveBackup(arg, backupDir string) (string, error) {
	if _, err := os.Stat(arg); err == nil || strings.ContainsRune(arg, filepath.Separator) {
		return arg, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	return filepath.Join(backupDir, arg), nil
}
