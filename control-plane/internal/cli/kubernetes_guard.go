package cli

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/rchouinard/pressfleet/control-plane/internal/config"
)

// errComposeOnly means a command that drives the platform Compose project
// was run against a Kubernetes install.
var errComposeOnly = errors.New("this command manages the platform of a Docker Compose install")

// kubernetesAlternatives are the host commands that drive the platform
// Compose project, by command path, with what takes their place on
// Kubernetes (design doc §4.8).
var kubernetesAlternatives = map[string]string{
	"pressfleet upgrade": "upgrade with helm upgrade, after applying the new chart's CRDs; " +
		"undo one with helm rollback and your PostgreSQL backup (docs/upgrading.md, \"Kubernetes\")",
	"pressfleet recover": "restore a state backup with the chart's recovery values " +
		"(docs/disaster-recovery.md, \"Kubernetes\")",
	"pressfleet metrics-access rotate": "the chart doesn't publish the metrics API (metrics.public_domain)",
}

// kubernetesVersionNote stands in for the control plane's version in
// `pressfleet version` on a Kubernetes install.
const kubernetesVersionNote = "control plane: the release of its Helm chart (helm list); " +
	"the CLI and control plane version skew rules don't apply on Kubernetes"

// onKubernetes reports whether cmd runs for a Kubernetes install: the
// configuration's driver is kubernetes (inside the control plane pod, or
// with a kubeconfig), or it is `init --target kubernetes`, which runs
// without a configuration.
func (a *app) onKubernetes(cmd *cobra.Command) bool {
	if a.cfg.Driver == config.SiteDriverKubernetes {
		return true
	}
	f := cmd.Flags().Lookup(targetFlag)
	return f != nil && f.Value.String() == targetKubernetes
}

// refuseOnKubernetes refuses the Compose-only host commands on a
// Kubernetes install, saying what to do instead.
func (a *app) refuseOnKubernetes(cmd *cobra.Command) error {
	if a.cfg.Driver != config.SiteDriverKubernetes {
		return nil
	}
	if alt, ok := kubernetesAlternatives[cmd.CommandPath()]; ok {
		return fmt.Errorf("%w, and this install's driver is kubernetes: %s", errComposeOnly, alt)
	}
	return nil
}
