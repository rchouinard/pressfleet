package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/rchouinard/pressfleet/control-plane/internal/config"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner/compose"
)

// RecoverProject is the part of the platform Compose project that `pressfleet
// recover` drives. compose.Project implements it; tests inject fakes.
type RecoverProject interface {
	StopServices(ctx context.Context, services ...string) error
	RunService(ctx context.Context, service string, command ...string) ([]byte, error)
	Up(ctx context.Context, opts compose.UpOptions) error
}

// errNotInstalled means recover found no platform to restore into.
var errNotInstalled = errors.New("no PressFleet platform is installed on this host")

func (a *app) recoverCommand() *cobra.Command {
	var f stateRecoverFlags
	cmd := &cobra.Command{
		Use:   "recover",
		Short: "Restore the platform's state from its backups on a new host",
		Long: `Rebuild a platform whose host was lost, from the master key and the backups
(see docs/disaster-recovery.md). Install first with "pressfleet init", giving
it the saved master key file and the same backup settings, then run recover.

Recover stops the control plane, replaces its state database with a state
backup from the state repository in a one-off control plane container (the
host CLI never touches the state database), and starts the control plane
again. The control plane then recreates every site that was running or
stopped from its newest backup, one job per site.

It refuses to replace a state database that already has client groups or
sites unless --force is given. If the restore fails, nothing changes and the
control plane is started again as it was.`,
		Annotations: map[string]string{checkSkew: "true"},
		Args:        cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.recoverPlatform(cmd.Context(), cmd.OutOrStdout(), f)
		},
	}
	cmd.Flags().StringVar(&f.snapshot, "snapshot", "latest", "the state backup to restore: its ID (8 characters or more) or latest; list them with `docker compose exec control-plane pressfleet state backups`")
	cmd.Flags().BoolVar(&f.force, "force", false, "replace a state database that already has client groups or sites")
	return cmd
}

// recoverProject returns the platform project recover drives.
func (a *app) recoverProject(name, dir string) RecoverProject {
	if a.opts.RecoverProject != nil {
		return a.opts.RecoverProject(name, dir)
	}
	return compose.Project{Name: name, Dir: dir}
}

func (a *app) recoverPlatform(ctx context.Context, out io.Writer, f stateRecoverFlags) error {
	cp, err := a.controlPlaneStatus(ctx)
	if err != nil {
		return err
	}
	if !cp.Installed {
		return fmt.Errorf("%w: run pressfleet init first, with the saved master key file and the same backup settings", errNotInstalled)
	}
	name := a.platformProjectName()
	project := a.recoverProject(name, config.Layout{Root: a.cfg.DataDir}.PlatformDir())
	printf := func(format string, args ...any) { _, _ = fmt.Fprintf(out, format, args...) }

	printf("Stopping the control plane; sites keep serving.\n")
	if err := project.StopServices(ctx, controlPlaneService); err != nil {
		return fmt.Errorf("stopping the control plane: %w", err)
	}
	args := []string{"state", "recover", "--snapshot", f.snapshot}
	if f.force {
		args = append(args, "--force")
	}
	output, runErr := project.RunService(context.WithoutCancel(ctx), controlPlaneService, args...)
	if len(output) > 0 {
		printf("%s\n", strings.TrimRight(string(output), "\n"))
	}
	printf("Starting the control plane...\n")
	upErr := project.Up(context.WithoutCancel(ctx), compose.UpOptions{
		Wait: true, WaitTimeout: platformStartTimeout, Services: []string{controlPlaneService},
	})
	if upErr != nil {
		upErr = fmt.Errorf("starting the control plane: %w (see `docker compose --project-name %s logs control-plane`)", upErr, name)
	}
	if runErr != nil {
		return errors.Join(fmt.Errorf("restoring the state: %w; nothing was changed", runErr), upErr)
	}
	if upErr != nil {
		return upErr
	}
	printf("\nThe platform's state is restored and the control plane is running. It is now recreating the sites\n" +
		"from their backups, one job per site; follow them on the dashboard's jobs page. Copy the backup\n" +
		"location off this host again if it isn't on S3, and keep the master key safe.\n")
	return nil
}
