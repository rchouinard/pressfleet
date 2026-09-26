package cli

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/rchouinard/pressfleet/control-plane/internal/secrets"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
)

// loadKeyring loads the platform master key, failing fast if it is missing
// or invalid.
func (a *app) loadKeyring() (*secrets.Keyring, error) {
	key, err := secrets.LoadMasterKey(secrets.KeySource{
		Value:   a.cfg.MasterKey,
		File:    a.cfg.MasterKeyFile,
		DataDir: a.cfg.DataDir,
	}, a.log)
	if err != nil {
		return nil, err
	}
	return secrets.NewKeyring(key)
}

// verifyMasterKey checks the key against the one recorded in the state
// database, recording it on first use.
func (a *app) verifyMasterKey(ctx context.Context, db *store.DB, keys *secrets.Keyring) error {
	firstUse, err := secrets.Verify(context.WithoutCancel(ctx), db, keys)
	if err != nil {
		return err
	}
	if firstUse {
		a.log.Info("master key registered with this installation", "key_id", keys.KeyID())
	} else {
		a.log.Info("master key verified", "key_id", keys.KeyID())
	}
	return nil
}

func (a *app) keyCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "key",
		Short: "Manage the platform master key",
	}
	cmd.AddCommand(a.keyGenerateCommand())
	return cmd
}

func (a *app) keyGenerateCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "generate",
		Short: "Print a new platform master key",
		Long: `Print a new random platform master key on standard output.

The master key encrypts every stored secret, including each client group's
backup repository key. If the host is lost and the key was not saved
elsewhere, all backups are unrecoverable. Store it somewhere safe, such as a
password manager, separate from the host and its backups.

Supply it to the control plane with $` + secrets.EnvMasterKey + ` or a key file
(master_key_file) outside the data directory, e.g.

  (umask 077; pressfleet key generate > /etc/pressfleet/master.key)

This command does not read the configuration or the state database.`,
		Args:        cobra.NoArgs,
		Annotations: map[string]string{skipConfig: "true"},
		RunE: func(cmd *cobra.Command, _ []string) error {
			key := secrets.GenerateMasterKey()
			if _, err := fmt.Fprintln(cmd.OutOrStdout(), key.Encode().Reveal()); err != nil {
				return err
			}
			_, err := fmt.Fprintln(cmd.ErrOrStderr(),
				"Save this key somewhere safe, separate from this host and its backups. Without it, backups cannot be restored.")
			return err
		},
	}
}
