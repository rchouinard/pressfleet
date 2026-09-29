package cli

import (
	"github.com/spf13/cobra"

	"github.com/rchouinard/pressfleet/control-plane/internal/deploy"
)

// siteTaskCommand runs the operations the control plane starts in site
// task containers (package deploy's sitetask.go), in its own image, as the
// site's UID. It isn't for people, so it is hidden, and it needs no
// configuration: a task has no data directory or secrets.
func (a *app) siteTaskCommand() *cobra.Command {
	return &cobra.Command{
		Use:                "site-task <operation> [flags]",
		Short:              "Run a control plane operation inside a site task container",
		Hidden:             true,
		DisableFlagParsing: true,
		Annotations:        map[string]string{skipConfig: "true"},
		RunE: func(cmd *cobra.Command, args []string) error {
			err := deploy.RunTaskCommand(args, cmd.OutOrStdout())
			if err != nil && deploy.IsPermanentTaskError(err) {
				return &exitError{code: deploy.ExitPermanent, err: err}
			}
			return err
		},
	}
}
