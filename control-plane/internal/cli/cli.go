// Package cli implements the pressfleet command tree.
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"

	"github.com/spf13/cobra"

	"github.com/rchouinard/pressfleet/control-plane/internal/config"
	"github.com/rchouinard/pressfleet/control-plane/internal/doctor"
	"github.com/rchouinard/pressfleet/control-plane/internal/logging"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
	"github.com/rchouinard/pressfleet/control-plane/internal/releasecheck"
)

// Options configures a CLI invocation.
type Options struct {
	Version string
	// Commit is the commit the binary was built from, when set at build
	// time; otherwise the VCS stamp Go embeds is used.
	Commit string
	// Stdin is read by commands that accept secrets on standard input;
	// nil means os.Stdin.
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
	// Env looks up environment variables; nil means os.LookupEnv.
	Env config.LookupEnv
	// HostProbe and DockerProbe feed preflight checks; nil means the real
	// host and Docker. Tests inject fakes.
	HostProbe   doctor.HostProbe
	DockerProbe doctor.DockerProbe
	// ClusterProbe feeds the Kubernetes install's preflight checks; nil
	// means the real cluster. Tests inject fakes.
	ClusterProbe doctor.ClusterProbe
	// PlatformProject returns the platform Compose project `pressfleet init`
	// starts; nil means the real one on the local Docker daemon. Tests
	// inject fakes.
	PlatformProject func(name, dir string) PlatformProject
	// ControlPlane returns the probe the host CLI uses to find the control
	// plane of the named platform project and ask its version; nil means
	// the real platform on the local Docker daemon. Tests inject fakes.
	ControlPlane func(project string) ControlPlaneProbe
	// UpgradeProject returns the platform Compose project `pressfleet
	// upgrade` drives: the one in dir, read from files when they aren't
	// dir's own (nil); nil means the real one on the local Docker daemon.
	// Tests inject fakes.
	UpgradeProject func(name, dir string, files []string) UpgradeProject
	// RecoverProject returns the platform Compose project `pressfleet
	// recover` drives; nil means the real one on the local Docker daemon.
	// Tests inject fakes.
	RecoverProject func(name, dir string) RecoverProject
	// PlatformProjectName overrides the platform's Compose project name, so
	// tests against real Docker don't touch an actual installation.
	PlatformProjectName string
	// ReleaseSource lists PressFleet releases for the control plane's
	// release check; nil means GitHub. Tests inject fakes.
	ReleaseSource releasecheck.Source
	// RepositoryTasks runs the repository tasks of the commands that open
	// backup repositories (`repository`, `state`); nil means the Compose
	// driver on the local Docker daemon. Tests inject a runner that runs
	// restic on the host.
	RepositoryTasks provisioner.RepositoryTaskRunner
	// DNSCredentialsDir overrides where the control plane writes DNS
	// provider credentials for Traefik (ingress.DNSCredentialsDir), for
	// tests.
	DNSCredentialsDir string
}

// skipConfig marks commands that must work without a valid configuration.
const skipConfig = "pressfleet/skip-config"

// logConfigWarnings marks a command that logs config deprecation warnings
// itself instead of printing them, as `serve` does.
const logConfigWarnings = "pressfleet/log-config-warnings"

// app holds state shared by all commands during one invocation.
type app struct {
	opts       Options
	configPath string
	cfg        config.Config
	log        *slog.Logger
}

// Run executes the CLI with args (excluding the program name) and returns
// the process exit code.
func Run(ctx context.Context, args []string, opts Options) int {
	if opts.Env == nil {
		opts.Env = os.LookupEnv
	}
	if opts.Stdin == nil {
		opts.Stdin = os.Stdin
	}
	a := &app{opts: opts}
	root := a.rootCommand()
	root.SetArgs(args)
	root.SetOut(opts.Stdout)
	root.SetErr(opts.Stderr)
	if err := root.ExecuteContext(ctx); err != nil {
		var ee *exitError
		if !errors.As(err, &ee) || ee.err != nil {
			fmt.Fprintf(opts.Stderr, "Error: %v\n", err)
		}
		if ee != nil {
			return ee.code
		}
		return 1
	}
	return 0
}

// exitError makes Run exit with a specific code. With a nil err nothing is
// printed, for commands that have already reported the problem.
type exitError struct {
	code int
	err  error
}

func (e *exitError) Error() string {
	if e.err == nil {
		return fmt.Sprintf("exit status %d", e.code)
	}
	return e.err.Error()
}

func (e *exitError) Unwrap() error { return e.err }

func (a *app) rootCommand() *cobra.Command {
	root := &cobra.Command{
		Use:               "pressfleet",
		Short:             "Self-hostable WordPress application platform",
		SilenceUsage:      true,
		SilenceErrors:     true,
		CompletionOptions: cobra.CompletionOptions{DisableDefaultCmd: true},
		PersistentPreRunE: a.preRun,
	}
	pf := root.PersistentFlags()
	pf.StringVar(&a.configPath, "config", "", fmt.Sprintf("config file (default $%s or %s)", config.EnvConfigPath, config.DefaultPath))
	pf.String("data-dir", "", "data directory (overrides config)")
	pf.String("log-level", "", "log level: debug, info, warn, error (overrides config)")
	pf.String("log-format", "", "log format: text, json (overrides config)")

	root.AddCommand(a.serveCommand(), a.initCommand(), a.upgradeCommand(), a.recoverCommand(), a.doctorCommand(), a.versionCommand())
	root.AddCommand(a.healthcheckCommand(), a.maintenancePageCommand(), a.sftpServerCommand(), a.siteTaskCommand())
	root.AddCommand(a.stateCommand())
	root.AddCommand(a.keyCommand())
	root.AddCommand(a.userCommand())
	root.AddCommand(a.repositoryCommand())
	root.AddCommand(a.metricsAccessCommand())
	root.AddCommand(a.adminCommand())
	return root
}

// flagOverrides maps flags to the config fields they override.
var flagOverrides = []struct {
	flag  string
	field func(*config.Config) *string
}{
	{"data-dir", func(c *config.Config) *string { return &c.DataDir }},
	{"log-level", func(c *config.Config) *string { return &c.Log.Level }},
	{"log-format", func(c *config.Config) *string { return &c.Log.Format }},
	{"listen", func(c *config.Config) *string { return &c.Listen }},
}

// preRun loads the configuration and, for host commands, checks that the
// CLI can manage the installed control plane (§4.7). On Kubernetes the
// chart and the control plane come from one release, so there is no skew to
// check (§4.8), and the Compose-only commands are refused.
func (a *app) preRun(cmd *cobra.Command, args []string) error {
	if err := a.loadConfig(cmd, args); err != nil {
		return err
	}
	if err := a.refuseOnKubernetes(cmd); err != nil {
		return err
	}
	if requiresSkewCheck(cmd) && !a.onKubernetes(cmd) {
		return a.enforceSkew(cmd.Context())
	}
	return nil
}

// loadConfig loads and validates configuration, applies flag overrides, and
// builds the logger.
func (a *app) loadConfig(cmd *cobra.Command, _ []string) error {
	if cmd.Annotations[skipConfig] != "" {
		return nil
	}
	cfg, err := config.Load(a.configPath, a.opts.Env)
	if err != nil {
		return err
	}
	for _, o := range flagOverrides {
		if f := cmd.Flags().Lookup(o.flag); f != nil && f.Changed {
			*o.field(&cfg) = f.Value.String()
		}
	}
	if cmd.Annotations[logConfigWarnings] == "" {
		for _, w := range cfg.Warnings {
			a.warnf("%s", w)
		}
	}
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("invalid configuration:\n%w", err)
	}
	log, err := logging.New(a.opts.Stderr, cfg.Log.Level, cfg.Log.Format)
	if err != nil {
		return err
	}
	a.cfg, a.log = cfg, log
	return nil
}
