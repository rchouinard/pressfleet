package cli

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"
)

// On Kubernetes, `helm upgrade` takes the place of `pressfleet upgrade`
// (design doc §4.7, §4.8). The chart's pre-upgrade hook
// (charts/pressfleet/templates/upgrade-preflight.yaml) runs `pressfleet
// admin upgrade-preflight` on the new control plane image before anything
// changes, with what runs now, which the chart looks up: the control plane's
// version and MariaDB's image. The control plane migrates the state
// database when it starts, as on Compose; backing up the PostgreSQL state
// database first, and restoring it with `helm rollback`, are the
// operator's.

// errUpgradeRefused means the upgrade preflight refused an upgrade.
var errUpgradeRefused = errors.New("upgrade refused")

// kubernetesPreflightFlags are the flags of `admin upgrade-preflight`.
type kubernetesPreflightFlags struct {
	fromVersion      string
	fromMariaDBImage string
	toMariaDBImage   string
}

func (a *app) adminUpgradePreflightCommand() *cobra.Command {
	var f kubernetesPreflightFlags
	cmd := &cobra.Command{
		Use:   "upgrade-preflight",
		Short: "Check that a Helm upgrade to this release is allowed (the chart's pre-upgrade hook)",
		Long: `Apply the upgrade policy (design doc §4.7) to a Helm upgrade to this
release: refuse a downgrade, an upgrade from a release older than the oldest
this one upgrades from, and a change of MariaDB's release series. The Helm
chart runs it as a pre-upgrade hook, on the new control plane image, with
what runs now. Empty values mean the chart found nothing running, and that
check is skipped.

It needs no configuration and doesn't touch the state database.`,
		Args: cobra.NoArgs,
		// The hook has no config file or Secrets: only the flags.
		Annotations: map[string]string{skipConfig: "true"},
		RunE: func(cmd *cobra.Command, _ []string) error {
			return kubernetesUpgradePreflight(cmd.OutOrStdout(), a.opts.Version, f)
		},
	}
	fl := cmd.Flags()
	fl.StringVar(&f.fromVersion, "from-version", "", "the version the control plane runs now (empty: none)")
	fl.StringVar(&f.fromMariaDBImage, "from-mariadb-image", "", "the MariaDB image that runs now (empty: none)")
	fl.StringVar(&f.toMariaDBImage, "to-mariadb-image", "", "the MariaDB image of the new release (empty: an external MariaDB)")
	return cmd
}

// devChart reports whether a version is the chart's in the repository,
// which the release pipeline stamps when it packages it (#227). It parses
// as a release, but orders before every real one.
func devChart(version string) bool {
	return strings.TrimPrefix(version, "v") == "0.0.0-dev"
}

// kubernetesUpgradePreflight checks a Helm upgrade to version to.
func kubernetesUpgradePreflight(out io.Writer, to string, f kubernetesPreflightFlags) error {
	printf := func(format string, args ...any) { _, _ = fmt.Fprintf(out, format, args...) }
	if f.fromVersion == "" {
		printf("No control plane runs in this namespace yet; nothing to check.\n")
	} else {
		order, err := classifyUpgrade(f.fromVersion, to)
		if err != nil {
			return err
		}
		if devChart(f.fromVersion) || devChart(to) {
			order.Step = stepUnordered
		}
		switch order.Step {
		case stepUnordered:
			printf("Not checking the upgrade order from %s to %s: development build.\n", f.fromVersion, to)
		case stepDown:
			return fmt.Errorf("%w: the control plane runs %s, newer than this chart's %s, and a control plane can't be downgraded. "+
				"Upgrade to %s or later; to undo an upgrade, use helm rollback with your PostgreSQL backup from before it (docs/upgrading.md)",
				errUpgradeRefused, order.From, order.To, order.From)
		case stepTooOld:
			return fmt.Errorf("%w: %s upgrades control planes from %s or later, and this one runs %s. "+
				"Upgrade to an intermediate release first (see the release notes)", errUpgradeRefused, order.To, order.Oldest, order.From)
		case stepSame:
			printf("The control plane already runs %s.\n", order.To)
		default:
			printf("Upgrading the control plane from %s to %s.\n", order.From, order.To)
		}
	}
	if f.fromMariaDBImage == "" || f.toMariaDBImage == "" {
		// No MariaDB runs yet, or it is external, which is the operator's.
		return nil
	}
	running, next := MariaDBSeries(f.fromMariaDBImage), MariaDBSeries(f.toMariaDBImage)
	switch {
	case running == "":
		return fmt.Errorf("%w: can't tell the MariaDB release series of the running image %s, so can't check that the upgrade keeps it",
			errUpgradeRefused, f.fromMariaDBImage)
	case running != next:
		return fmt.Errorf("%w: %s moves MariaDB from the %s to the %s release series, which PressFleet doesn't support upgrading yet; "+
			"nothing was changed", errUpgradeRefused, to, running, next)
	}
	printf("MariaDB stays in the %s release series.\n", running)
	return nil
}
