package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/rchouinard/pressfleet/control-plane/internal/changes"
	"github.com/rchouinard/pressfleet/control-plane/internal/config"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner/compose"
	"github.com/rchouinard/pressfleet/control-plane/internal/releasecheck"
	"github.com/rchouinard/pressfleet/control-plane/internal/skew"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
)

// The admin interface is how the host CLI talks to the control plane
// (design doc §4.7). It is versioned separately from the public /api/v1:
// the host CLI runs `pressfleet admin <command>` in the control plane
// container, which answers with one JSON object on standard output. Going
// through `docker exec` rather than HTTP means the interface needs no
// network exposure or credentials of its own (access to the Docker socket is
// already root on the host), and it works while Traefik is down.
//
// adminInterfaceVersion changes only when a change is incompatible; new
// fields and commands keep the number. Every version keeps the "version"
// field of `admin info`, so a CLI can always tell an operator which CLI
// they need. From 1.0, an incompatible change must stay backward compatible
// for at least one minor release (§4.7).
const adminInterfaceVersion = 1

// checkSkew marks host commands that check the control plane's version
// before they run (§4.7). Commands that run inside the control plane
// container need no check: there the versions are the same binary.
// `version` and `upgrade` are exempt.
const checkSkew = "pressfleet/check-skew"

// imageVersionLabel is the OCI label holding an image's version, which the
// control plane image sets.
const imageVersionLabel = "org.opencontainers.image.version"

// controlPlaneProbeTimeout bounds finding the control plane and asking its
// version, so a hung Docker daemon can't block every command.
const controlPlaneProbeTimeout = 30 * time.Second

// adminInfo is the answer of `pressfleet admin info`.
type adminInfo struct {
	// Interface is adminInterfaceVersion of the control plane.
	Interface int `json:"interface"`
	// Version is the control plane's version.
	Version string `json:"version"`
	// Ready says whether the control plane reports ready (GET /readyz),
	// and ReadyError why not.
	Ready      bool   `json:"ready"`
	ReadyError string `json:"ready_error,omitempty"`
	// Release is the newest release the control plane's release check
	// found (#140), absent when the check is off or found none yet. Added
	// to interface 1: older CLIs ignore it.
	Release *adminRelease `json:"release,omitempty"`
}

// adminRelease is the release check's result in `pressfleet admin info`.
type adminRelease struct {
	LatestVersion string    `json:"latest_version"`
	ReleaseURL    string    `json:"release_url"`
	CheckedAt     time.Time `json:"checked_at"`
	// UpdateAvailable says whether LatestVersion is newer than the control
	// plane.
	UpdateAvailable bool `json:"update_available"`
}

// ControlPlaneProbe finds the control plane container and runs commands in
// it. compose.Project implements it; tests inject fakes.
type ControlPlaneProbe interface {
	ServiceContainer(ctx context.Context, service string) (compose.ContainerDetails, error)
	ExecService(ctx context.Context, service string, command ...string) ([]byte, error)
}

func (a *app) adminCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:    "admin",
		Short:  "Admin interface for the host CLI (runs inside the control plane container)",
		Hidden: true,
	}
	cmd.AddCommand(a.adminInfoCommand(), a.adminChangesCommand(), a.adminUpgradePreflightCommand())
	return cmd
}

func (a *app) adminInfoCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "info",
		Short: "Print the control plane's version and readiness as JSON",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			info := adminInfo{Interface: adminInterfaceVersion, Version: a.opts.Version}
			url, err := localURL(a.cfg.Listen, "/readyz")
			if err == nil {
				err = checkHealth(cmd.Context(), url)
			}
			info.Ready = err == nil
			if err != nil {
				info.ReadyError = err.Error()
			}
			info.Release = a.recordedRelease()
			return json.NewEncoder(cmd.OutOrStdout()).Encode(info)
		},
	}
}

func (a *app) adminChangesCommand() *cobra.Command {
	var since string
	cmd := &cobra.Command{
		Use:   "changes --since <time>",
		Short: "Print what the control plane changed since a time as JSON",
		Long: `Print what the control plane changed since a time, from its jobs and its
audit log, as one JSON object. The changes are grouped by what restoring
the state database would do about them: blocking, converged, and lost
(package changes). ` + "`pressfleet upgrade --rollback`" + ` asks this before it
restores the state database (design doc §4.7).

--since is an RFC 3339 time, such as 2026-11-05T12:00:00Z. The command
only reads the state database: it doesn't migrate it.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			t, err := time.Parse(time.RFC3339Nano, since)
			if err != nil {
				return fmt.Errorf("--since must be an RFC 3339 time, such as 2026-11-05T12:00:00Z: %w", err)
			}
			// Opened without migrating: a report changes nothing, and
			// the control plane it runs beside has migrated the database.
			db, err := store.Open(cmd.Context(), a.storeConfig(), a.log)
			if err != nil {
				return err
			}
			defer func() { _ = db.Close() }()
			r, err := changes.Since(cmd.Context(), db, t)
			if err != nil {
				return err
			}
			return json.NewEncoder(cmd.OutOrStdout()).Encode(r)
		},
	}
	cmd.Flags().StringVar(&since, "since", "", "list the changes at or after this RFC 3339 time (required)")
	_ = cmd.MarkFlagRequired("since")
	return cmd
}

// controlPlane is what the host CLI found out about the control plane.
type controlPlane struct {
	// Installed is false if the platform has no control plane container.
	Installed bool
	// Running says whether the container is running.
	Running bool
	// Version is the control plane's version; empty if unknown.
	Version string
	// Info is the admin interface's answer, nil when it didn't answer and
	// Version comes from the image metadata.
	Info *adminInfo
	// InfoErr is why the admin interface didn't answer.
	InfoErr error
}

// controlPlaneProbe returns the probe for the installed platform's control
// plane. Init runs without a configuration, so the defaults stand in.
func (a *app) controlPlaneProbe() ControlPlaneProbe {
	name := a.platformProjectName()
	dataDir := a.cfg.DataDir
	if dataDir == "" {
		dataDir = config.Default().DataDir
	}
	if a.opts.ControlPlane != nil {
		return a.opts.ControlPlane(name)
	}
	return compose.Project{Name: name, Dir: config.Layout{Root: dataDir}.PlatformDir()}
}

// controlPlaneStatus asks the control plane for its version through the
// admin interface. If the control plane doesn't answer, as when it is
// stopped or unhealthy, the version comes from its container's image.
func (a *app) controlPlaneStatus(ctx context.Context) (controlPlane, error) {
	ctx, cancel := context.WithTimeout(ctx, controlPlaneProbeTimeout)
	defer cancel()
	probe := a.controlPlaneProbe()
	c, err := probe.ServiceContainer(ctx, controlPlaneService)
	if errors.Is(err, compose.ErrNoContainer) {
		return controlPlane{}, nil
	} else if err != nil {
		return controlPlane{}, fmt.Errorf("finding the control plane: %w", err)
	}
	cp := controlPlane{Installed: true, Running: c.Running}
	if c.Running {
		cp.Info, cp.InfoErr = adminInfoFrom(probe.ExecService(ctx, controlPlaneService, "pressfleet", "admin", "info"))
		if cp.Info != nil {
			cp.Version = cp.Info.Version
			return cp, nil
		}
	} else {
		cp.InfoErr = errors.New("the control plane container isn't running")
	}
	cp.Version = imageVersion(c)
	return cp, nil
}

// adminInfoFrom parses the output of `pressfleet admin info`.
func adminInfoFrom(out []byte, err error) (*adminInfo, error) {
	if err != nil {
		return nil, err
	}
	var info adminInfo
	if err := json.Unmarshal(out, &info); err != nil {
		return nil, fmt.Errorf("parsing the admin interface's answer: %w", err)
	}
	if info.Version == "" {
		return nil, fmt.Errorf("the admin interface (version %d) didn't report a version", info.Interface)
	}
	return &info, nil
}

// imageVersion returns the version of a container's image: its version
// label, or else the tag of the image reference.
func imageVersion(c compose.ContainerDetails) string {
	if v := c.Labels[imageVersionLabel]; v != "" {
		return v
	}
	ref := c.Image
	if strings.Contains(ref, "@") {
		return ""
	}
	if i := strings.LastIndex(ref, ":"); i > strings.LastIndex(ref, "/") {
		return ref[i+1:]
	}
	return ""
}

// requiresSkewCheck reports whether cmd or one of its parents is marked
// with checkSkew.
func requiresSkewCheck(cmd *cobra.Command) bool {
	for c := cmd; c != nil; c = c.Parent() {
		if c.Annotations[checkSkew] != "" {
			return true
		}
	}
	return false
}

// enforceSkew refuses to continue if the CLI and the installed control
// plane are outside the version skew policy (§4.7). There is deliberately
// no override. Without an installed control plane there is nothing to
// check, and when the control plane's version can't be found out (Docker
// down, a development build) it warns and lets the command run, since
// commands like doctor are how such problems get diagnosed.
func (a *app) enforceSkew(ctx context.Context) error {
	cp, err := a.controlPlaneStatus(ctx)
	if err != nil {
		a.warnf("can't check the control plane's version: %v", err)
		return nil
	}
	if !cp.Installed {
		return nil
	}
	return a.skewError(cp)
}

// skewError returns the error explaining how to fix a CLI and control plane
// outside the policy, or nil.
func (a *app) skewError(cp controlPlane) error {
	if cp.Version == "" {
		a.warnf("can't check the control plane's version: %v, and its image has no version", cp.InfoErr)
		return nil
	}
	if cp.Version == a.opts.Version {
		return nil
	}
	cliV, cliErr := skew.Parse(a.opts.Version)
	cpV, cpErr := skew.Parse(cp.Version)
	if cliErr != nil || cpErr != nil {
		a.warnf("not checking version skew between the CLI (%s) and the control plane (%s): development build", a.opts.Version, cp.Version)
		return nil
	}
	var se *skew.Error
	if !errors.As(skew.Check(cliV, cpV), &se) {
		return nil
	}
	fix := fmt.Sprintf("install a pressfleet CLI %s to match the control plane; the control plane can't be downgraded", se.CompatibleCLIs())
	if se.CLINewer() {
		fix = fmt.Sprintf("upgrade the control plane to %s with `pressfleet upgrade`, or use a pressfleet CLI %s", cliV, se.CompatibleCLIs())
	}
	return fmt.Errorf("pressfleet CLI %s can't manage control plane %s: %s.\nTo fix this, %s",
		cliV, cpV, se.Rule(), fix)
}

// warnf prints a warning on standard error.
func (a *app) warnf(format string, args ...any) {
	_, _ = fmt.Fprintf(a.opts.Stderr, "Warning: "+format+"\n", args...)
}

// writeControlPlaneVersion writes the control plane line of `pressfleet
// version`, followed by the skew problem if there is one.
func (a *app) writeControlPlaneVersion(ctx context.Context, w io.Writer) error {
	cp, statusErr := a.controlPlaneStatus(ctx)
	var line string
	switch {
	case statusErr != nil:
		line = fmt.Sprintf("control plane: unknown (%v)", statusErr)
	case !cp.Installed:
		line = "control plane: not installed"
	case cp.Info != nil:
		line = fmt.Sprintf("control plane: %s (admin interface %d)", cp.Version, cp.Info.Interface)
		if !cp.Info.Ready {
			line += ", not ready"
		}
	case cp.Version != "":
		line = fmt.Sprintf("control plane: %s (from its image; not answering: %v)", cp.Version, cp.InfoErr)
	default:
		line = fmt.Sprintf("control plane: unknown (not answering: %v)", cp.InfoErr)
	}
	if _, err := fmt.Fprintln(w, line); err != nil {
		return err
	}
	if statusErr != nil || !cp.Installed {
		return nil
	}
	if err := a.skewError(cp); err != nil {
		_, werr := fmt.Fprintf(w, "\n%v\n", err)
		return werr
	}
	if cp.Info != nil && cp.Info.Release != nil && cp.Info.Release.UpdateAvailable {
		_, err := fmt.Fprintf(w, "\n%s\n", a.updateNotice(*cp.Info.Release))
		return err
	}
	return nil
}

// recordedRelease returns what the control plane's release check last
// recorded, or nil when the check is off or has found nothing. A file that
// can't be read is no answer, so admin info still reports the rest.
func (a *app) recordedRelease() *adminRelease {
	if !a.cfg.Updates.ReleaseCheck {
		return nil
	}
	r, ok, err := releasecheck.ReadFile(a.cfg.Layout().ReleaseCheckFile())
	if err != nil {
		a.log.Debug("reading the release check failed", "error", err)
	}
	if !ok {
		return nil
	}
	return &adminRelease{
		LatestVersion: r.LatestVersion, ReleaseURL: r.ReleaseURL, CheckedAt: r.CheckedAt,
		UpdateAvailable: r.UpdateAvailable(a.opts.Version),
	}
}

// updateNotice tells the operator a newer release is available and how to
// upgrade to it: `pressfleet upgrade` moves the platform to the CLI's own
// version, so an older CLI has to be replaced first. On Kubernetes, Helm
// does the upgrade instead (design doc §4.8).
func (a *app) updateNotice(r adminRelease) string {
	how := fmt.Sprintf("install the %s CLI from the release page and run `pressfleet upgrade`", r.LatestVersion)
	if a.cfg.Driver == config.SiteDriverKubernetes {
		how = "read its release notes, apply the new chart's CRDs, and run `helm upgrade` (docs/kubernetes.md, \"Upgrading\")"
	} else if _, err := skew.Parse(a.opts.Version); err == nil && !releasecheck.UpdateAvailable(a.opts.Version, r.LatestVersion) {
		how = "run `pressfleet upgrade` with this CLI"
	}
	return fmt.Sprintf("A newer PressFleet release is available: %s (%s).\nTo upgrade, %s.", r.LatestVersion, r.ReleaseURL, how)
}
