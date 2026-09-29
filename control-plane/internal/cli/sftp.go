package cli

import (
	"github.com/spf13/cobra"

	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
	"github.com/rchouinard/pressfleet/control-plane/internal/sftpserver"
)

// sftpServerCommand is the SFTP gateway's per-session worker (package
// sftpserver): the gateway has the driver run it, as the site's user with
// only the site's wp-content mounted, and relays the client's SFTP session
// to its standard input and output. It needs no configuration, and writes
// nothing but the protocol to standard output.
func (a *app) sftpServerCommand() *cobra.Command {
	var root string
	cmd := &cobra.Command{
		Use:   "sftp-server",
		Short: "Serve SFTP on standard input and output (the SFTP gateway's per-session worker)",
		Long: "Serves one SFTP session on standard input and output, confined to --root, until standard input ends. " +
			"The SFTP gateway runs it in a short-lived worker for each session; operators don't run it themselves.",
		Args:        cobra.NoArgs,
		Annotations: map[string]string{skipConfig: "true"},
		RunE: func(*cobra.Command, []string) error {
			return sftpserver.Serve(a.opts.Stdin, a.opts.Stdout, root)
		},
	}
	cmd.Flags().StringVar(&root, "root", provisioner.TaskWPContentPath, "directory the client sees as /")
	return cmd
}
