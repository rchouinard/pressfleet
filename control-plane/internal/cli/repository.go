package cli

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/rchouinard/pressfleet/control-plane/internal/backup/repository"
	"github.com/rchouinard/pressfleet/control-plane/internal/config"
	"github.com/rchouinard/pressfleet/control-plane/internal/redact"
	"github.com/rchouinard/pressfleet/control-plane/internal/secrets"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
)

// Repository commands operate on the state database and the repositories,
// so they run inside the control plane container (design doc §4.7), e.g.
//
//	docker compose exec control-plane pressfleet repository export <client-group-id> /var/lib/pressfleet/tmp/acme

// backupS3 returns the S3 settings of backup repositories, decrypting the
// secret access key. It is zero with the local backend.
func backupS3(cfg config.BackupConfig, keys *secrets.Keyring) (repository.S3Config, error) {
	if cfg.Backend != config.BackupBackendS3 {
		return repository.S3Config{}, nil
	}
	secret, err := keys.OpenSetting(config.BackupS3SecretAccessKeySetting, cfg.S3.SecretAccessKey)
	if err != nil {
		return repository.S3Config{}, fmt.Errorf("decrypting the backup S3 secret access key: %w", err)
	}
	return repository.S3Config{
		Endpoint:        cfg.S3.Endpoint,
		Bucket:          cfg.S3.Bucket,
		Prefix:          cfg.S3.Prefix,
		Region:          cfg.S3.Region,
		AccessKeyID:     redact.Secret(cfg.S3.AccessKeyID),
		SecretAccessKey: secret,
	}, nil
}

// backupRepositories returns the manager of client group backup
// repositories.
func (a *app) backupRepositories(db *store.DB, secretStore secrets.Store, keys *secrets.Keyring) (*repository.Manager, error) {
	s3, err := backupS3(a.cfg.Backup, keys)
	if err != nil {
		return nil, err
	}
	return repository.New(repository.Options{
		DB:      db,
		Secrets: secretStore,
		Layout:  a.cfg.Layout(),
		Backend: a.cfg.Backup.Backend,
		S3:      s3,
		Logger:  a.log.With("component", "backup-repositories"),
	})
}

// withRepositories opens the state database and runs fn with the
// repository manager.
func (a *app) withRepositories(ctx context.Context, fn func(*repository.Manager) error) error {
	keys, err := a.loadKeyring()
	if err != nil {
		return err
	}
	db, err := a.openStateDB(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	if err := a.verifyMasterKey(ctx, db, keys); err != nil {
		return err
	}
	m, err := a.backupRepositories(db, secrets.NewStore(db, keys), keys)
	if err != nil {
		return err
	}
	return fn(m)
}

func (a *app) repositoryCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "repository",
		Short: "Manage client group backup repositories (run inside the control plane container)",
		Long: `Manage the Restic repository of each client group, which holds the
backups of the group's sites (see docs/configuration.md, "Backup
repositories").

Repositories are created on a group's first backup; "create" does it ahead
of time. Offboarding a client is "export" followed by "delete".`,
	}
	cmd.AddCommand(a.repositoryCreateCommand(), a.repositoryExportCommand(), a.repositoryDeleteCommand())
	return cmd
}

func (a *app) repositoryCreateCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "create <client-group-id>",
		Short: "Create a client group's backup repository if it doesn't exist",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.withRepositories(cmd.Context(), func(m *repository.Manager) error {
				r, err := m.Ensure(cmd.Context(), args[0])
				if err != nil {
					return err
				}
				_, err = fmt.Fprintf(cmd.OutOrStdout(), "Backup repository of client group %s: %s\n", r.ClientGroupID, r.Location)
				return err
			})
		},
	}
}

func (a *app) repositoryExportCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "export <client-group-id> <directory>",
		Short: "Copy a client group's backup repository and its password into a directory",
		Long: `Copy a client group's backup repository, its password, and a README into a
new or empty directory, for handing over to the client when offboarding. The
copy is a local Restic repository (S3 repositories are downloaded) that
plain restic opens with the password file alone; export checks that before
it finishes.

The directory must be an absolute path. Inside the control plane container
only the data directory is shared with the host, so export somewhere under
it, e.g. <data_dir>/tmp/<name>, and move the result off the host. The
export holds the password in plaintext: treat it like the backups
themselves, and delete it once handed over.

Don't run it while the repository is being pruned.`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			dest := filepath.Clean(args[1])
			return a.withRepositories(cmd.Context(), func(m *repository.Manager) error {
				e, err := m.Export(cmd.Context(), args[0], dest)
				if err != nil {
					return err
				}
				_, err = fmt.Fprintf(cmd.OutOrStdout(), "Exported the backup repository of client group %s to %s\n"+
					"  Repository:     %s\n  Password file:  %s\n", args[0], e.Dir, e.RepositoryDir, e.PasswordFile)
				return err
			})
		},
	}
}

func (a *app) repositoryDeleteCommand() *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "delete <client-group-id>",
		Short: "Permanently delete a client group's backup repository",
		Long: `Permanently delete a client group's backup repository, every backup in it,
and its password. It is refused while the group still has sites. Export the
repository first if the client should keep the backups.

Deleting a group that has no repository does nothing.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !yes {
				return errors.New("deleting a backup repository can't be undone; pass --yes to confirm")
			}
			return a.withRepositories(cmd.Context(), func(m *repository.Manager) error {
				if err := m.Delete(cmd.Context(), args[0]); err != nil {
					return err
				}
				_, err := fmt.Fprintf(cmd.OutOrStdout(), "Deleted the backup repository of client group %s\n", args[0])
				return err
			})
		},
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "confirm that every backup of the client group is deleted")
	return cmd
}
