package cli

import (
	"bufio"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"go.yaml.in/yaml/v3"

	"github.com/rchouinard/pressfleet/control-plane/internal/auth"
	"github.com/rchouinard/pressfleet/control-plane/internal/config"
	"github.com/rchouinard/pressfleet/control-plane/internal/doctor"
	"github.com/rchouinard/pressfleet/control-plane/internal/logging"
	"github.com/rchouinard/pressfleet/control-plane/internal/platform"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner/compose"
	"github.com/rchouinard/pressfleet/control-plane/internal/redact"
	"github.com/rchouinard/pressfleet/control-plane/internal/secrets"
	"github.com/rchouinard/pressfleet/control-plane/internal/sftpgw"
)

// masterKeyWarning is the master key warning of design doc §4.1, which
// `pressfleet init` must print verbatim. A test checks it against the
// design doc and docs/installation.md.
const masterKeyWarning = "The master key is required to decrypt backup repository keys. " +
	"If the host is lost and the master key was not saved elsewhere, all backups are unrecoverable. " +
	"Setup requires the operator to download the key and confirm it has been stored somewhere safe " +
	"(e.g. a password manager), separate from the host and its backups."

// PlatformProject is the part of the platform Compose project that init
// drives. compose.Project implements it; tests inject fakes.
type PlatformProject interface {
	Up(ctx context.Context, opts compose.UpOptions) error
	ExecInput(ctx context.Context, stdin io.Reader, service string, command ...string) ([]byte, error)
}

// controlPlaneService is the platform Compose service running the control
// plane.
const controlPlaneService = "control-plane"

// platformStartTimeout bounds waiting for the platform services to become
// healthy. The first start pulls images, which dominates on slow links.
const platformStartTimeout = 10 * time.Minute

// Sentinel errors for init.
var (
	// errExistingInstall means init found an existing installation and
	// --force wasn't given.
	errExistingInstall = errors.New("PressFleet is already installed on this host")
	// errProjectInUse means another platform uses the Compose project name.
	errProjectInUse = errors.New("the platform's Compose project name is in use")
	// errInputClosed means standard input ended while init was prompting.
	errInputClosed = errors.New("standard input closed while waiting for an answer (use --non-interactive for automation)")
	// errKeyNotConfirmed means the operator didn't confirm the master key
	// is stored off-host.
	errKeyNotConfirmed = errors.New("the master key must be stored off-host before installation continues")
)

// initFlags are the flags of `pressfleet init`.
type initFlags struct {
	nonInteractive     bool
	force              bool
	skipPreflight      bool
	confirmKeyStored   bool
	adminPasswordStdin bool
	controlPlaneImage  string
	// acmeStaging is --acme-staging; acmeStagingSet says it was given.
	acmeStaging    bool
	acmeStagingSet bool
}

// initAnswers are the settings init collects. Later milestones add the
// alert channels here and in initQuestions.
type initAnswers struct {
	DashboardDomain   string
	PreviewBaseDomain string
	PublicIP          string
	DataDir           string
	MasterKeyFile     string
	AdminEmail        string
	smtpAnswers
	backupAnswers
	ACMEEmail    string
	ACMECAServer string
	dnsAnswers
}

// initQuestion is one setting init collects: from its flag if given, else by
// prompting (interactive mode) or from its default (non-interactive mode).
type initQuestion struct {
	// flag names the flag that answers the question.
	flag string
	// usage is the flag's help text.
	usage string
	// prompt is shown in interactive mode.
	prompt string
	// optional questions may be left empty.
	optional bool
	// requiredWhen, if set, makes an optional question required given the
	// answers so far.
	requiredWhen func(a *initAnswers) bool
	// flagOnly questions are never prompted for: their answer comes from
	// the flag, the environment, or the default. They are for settings few
	// operators change.
	flagOnly bool
	// field returns where the answer goes. Its value before asking is the
	// default.
	field func(*initAnswers) *string
	// check validates an answer given the answers so far; nil accepts
	// anything.
	check func(value string, a *initAnswers) error
	// skip, if set, says not to prompt for the question given the answers
	// so far. A flag or the environment still answers it.
	skip func(a *initAnswers) bool
}

// initQuestions are asked in order.
var initQuestions = []initQuestion{
	{
		flag:   "dashboard-domain",
		usage:  "domain serving the dashboard and API, e.g. fleet.agency.example (also $PRESSFLEET_DASHBOARD_DOMAIN)",
		prompt: "Dashboard domain, where the dashboard and API are served (e.g. fleet.agency.example)",
		field:  func(a *initAnswers) *string { return &a.DashboardDomain },
		check:  func(v string, _ *initAnswers) error { return checkDomain(v) },
	},
	{
		flag:   "preview-base-domain",
		usage:  "domain whose wildcard record points at this host, e.g. preview.agency.example (also $PRESSFLEET_PREVIEW_BASE_DOMAIN)",
		prompt: "Preview base domain; *.<domain> must point at this host (e.g. preview.agency.example)",
		field:  func(a *initAnswers) *string { return &a.PreviewBaseDomain },
		check: func(v string, _ *initAnswers) error {
			if strings.HasPrefix(v, "*.") {
				return fmt.Errorf("give the base domain without the wildcard, e.g. %s", strings.TrimPrefix(v, "*."))
			}
			return checkDomain(v)
		},
	},
	{
		flag:     "public-ip",
		usage:    "this host's public IP address, only needed behind NAT (optional; also $PRESSFLEET_PUBLIC_IP)",
		prompt:   "Public IP address of this host, only needed behind NAT (optional)",
		optional: true,
		field:    func(a *initAnswers) *string { return &a.PublicIP },
		check: func(v string, _ *initAnswers) error {
			if _, err := netip.ParseAddr(v); err != nil {
				return fmt.Errorf("%q is not an IP address", v)
			}
			return nil
		},
	},
	{
		// Answered by the global --data-dir flag, which init shares.
		flag:   "data-dir",
		prompt: "Data directory for sites, state, and rendered projects",
		field:  func(a *initAnswers) *string { return &a.DataDir },
		check:  func(v string, _ *initAnswers) error { return checkCleanAbs(v) },
	},
	{
		flag:   "master-key-file",
		usage:  "where to write the master key, outside the data directory (default next to the config file; also $PRESSFLEET_MASTER_KEY_FILE)",
		prompt: "Master key file, outside the data directory",
		field:  func(a *initAnswers) *string { return &a.MasterKeyFile },
		check: func(v string, a *initAnswers) error {
			if err := checkCleanAbs(v); err != nil {
				return err
			}
			if isWithin(v, a.DataDir) {
				return fmt.Errorf("%w (%s): keep the key separate from the host's backups", secrets.ErrKeyFileInDataDir, a.DataDir)
			}
			return nil
		},
	},
	{
		flag:   "admin-email",
		usage:  "email address of the first admin user",
		prompt: "Email address of the first admin user",
		field:  func(a *initAnswers) *string { return &a.AdminEmail },
		check: func(v string, _ *initAnswers) error {
			if !auth.ValidEmail(auth.NormalizeEmail(v)) {
				return fmt.Errorf("%q is not an email address like admin@agency.example", v)
			}
			return nil
		},
	},
	{
		flag:     "acme-email",
		usage:    "contact address for the Let's Encrypt account, used for important notices (optional; also $PRESSFLEET_ACME_EMAIL)",
		prompt:   "Contact email address for Let's Encrypt (optional)",
		optional: true,
		field:    func(a *initAnswers) *string { return &a.ACMEEmail },
		check:    func(v string, _ *initAnswers) error { return config.CheckACMEEmail(v) },
	},
	{
		flag:     "acme-ca-server",
		usage:    "ACME directory URL of another certificate authority, e.g. a Pebble server for tests (default Let's Encrypt; also $PRESSFLEET_ACME_CA_SERVER)",
		optional: true,
		flagOnly: true,
		field:    func(a *initAnswers) *string { return &a.ACMECAServer },
		check:    func(v string, _ *initAnswers) error { return config.CheckACMEServer(v) },
	},
}

func checkDomain(v string) error {
	if !config.ValidDomain(v) {
		return fmt.Errorf("%q is not a domain name like fleet.agency.example", v)
	}
	return nil
}

func checkCleanAbs(v string) error {
	if !filepath.IsAbs(v) || filepath.Clean(v) != v {
		return fmt.Errorf("%q must be a clean absolute path", v)
	}
	return nil
}

// isWithin reports whether path is dir or inside it.
func isWithin(path, dir string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && filepath.IsLocal(rel)
}

func (a *app) initCommand() *cobra.Command {
	var f initFlags
	flagValues := map[string]*string{}
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Install and configure PressFleet on this host",
		Long: `Install PressFleet on this host: collect settings, run preflight checks,
generate the platform master key, write the config file, start the platform
services, and create the first admin user.

By default init prompts for each setting. With --non-interactive it takes
settings from flags and PRESSFLEET_* environment variables instead, for
automation; --confirm-key-stored is then required.

init refuses to run over an existing installation unless --force is given.
Even then it never replaces an existing master key: it reuses it.

See docs/installation.md.`,
		Args:        cobra.NoArgs,
		Annotations: map[string]string{skipConfig: "true", checkSkew: "true"},
		RunE: func(cmd *cobra.Command, _ []string) error {
			given := map[string]string{}
			for name := range flagValues {
				if fl := cmd.Flags().Lookup(name); fl != nil && fl.Changed {
					given[name] = fl.Value.String()
				}
			}
			if fl := cmd.Flags().Lookup("data-dir"); fl.Changed {
				given["data-dir"] = fl.Value.String()
			}
			f.acmeStagingSet = cmd.Flags().Changed("acme-staging")
			return a.runInit(cmd.Context(), f, given)
		},
	}
	fl := cmd.Flags()
	for _, q := range allInitQuestions() {
		if q.usage != "" {
			flagValues[q.flag] = fl.String(q.flag, "", q.usage)
		}
	}
	fl.BoolVar(&f.nonInteractive, "non-interactive", false, "take settings from flags and the environment instead of prompting")
	fl.BoolVar(&f.confirmKeyStored, "confirm-key-stored", false,
		"confirm that the printed master key will be stored off-host (required with --non-interactive)")
	fl.BoolVar(&f.adminPasswordStdin, "admin-password-stdin", false,
		"read the admin password from the first line of standard input instead of generating one (--non-interactive only)")
	fl.BoolVar(&f.force, "force", false, "run over an existing installation, reusing its master key")
	fl.BoolVar(&f.skipPreflight, "skip-preflight", false, "install even if required host checks fail (development only)")
	fl.StringVar(&f.controlPlaneImage, "control-plane-image", "",
		"control plane image (development only; default: the image released with this CLI)")
	fl.BoolVar(&f.acmeStaging, "acme-staging", false,
		"get certificates from Let's Encrypt's staging environment, which browsers don't trust (also $PRESSFLEET_ACME_STAGING)")
	return cmd
}

// installer carries one run of `pressfleet init`.
type installer struct {
	a          *app
	f          initFlags
	in         *bufio.Reader
	out        io.Writer
	configPath string
	// metricsPassword is the password of a public metrics credential
	// startPlatform just created, which is shown once.
	metricsPassword redact.Secret
}

func (a *app) runInit(ctx context.Context, f initFlags, given map[string]string) error {
	log, err := logging.New(a.opts.Stderr, "info", logging.FormatText)
	if err != nil {
		return err
	}
	a.log = log
	inst := &installer{a: a, f: f, in: bufio.NewReader(a.opts.Stdin), out: a.opts.Stdout, configPath: a.resolveConfigPath()}
	return inst.run(ctx, given)
}

// resolveConfigPath returns the config file init writes: --config, else
// $PRESSFLEET_CONFIG, else the default path.
func (a *app) resolveConfigPath() string {
	if a.configPath != "" {
		return a.configPath
	}
	if p, ok := a.opts.Env(config.EnvConfigPath); ok && p != "" {
		return p
	}
	return config.DefaultPath
}

func (i *installer) printf(format string, args ...any) {
	_, _ = fmt.Fprintf(i.out, format, args...)
}

func (i *installer) run(ctx context.Context, given map[string]string) error {
	if i.f.adminPasswordStdin && !i.f.nonInteractive {
		return errors.New("--admin-password-stdin needs --non-interactive, because interactive mode reads answers from standard input")
	}
	if i.f.nonInteractive && !i.f.confirmKeyStored {
		return fmt.Errorf("%w: pass --confirm-key-stored with --non-interactive once you have a way to store the printed key", errKeyNotConfirmed)
	}
	if !filepath.IsAbs(i.configPath) {
		return fmt.Errorf("config file path %q must be absolute", i.configPath)
	}
	// Check for the config file before asking anything, so an operator
	// doesn't answer every question only to be refused.
	if !i.f.force && exists(i.configPath) {
		return i.refuseExisting([]string{"config file " + i.configPath})
	}

	answers, err := i.defaults()
	if err != nil {
		return err
	}
	if !i.f.nonInteractive {
		i.printf("PressFleet setup. Press Enter to accept the value in brackets.\n\n")
	}
	if err := i.ask(&answers, given); err != nil {
		return err
	}
	if err := answers.readSMTPPassword(); err != nil {
		return err
	}
	if err := answers.readBackupSecret(); err != nil {
		return err
	}
	if i.f.acmeStagingSet {
		answers.ACMEStaging = i.f.acmeStaging
	}
	if err := answers.readDNSCredentials(); err != nil {
		return err
	}
	var adminPassword redact.Secret
	if i.f.adminPasswordStdin {
		if adminPassword, err = i.readPassword(); err != nil {
			return err
		}
	}

	found := existingInstall(answers, i.configPath)
	if len(found) > 0 && !i.f.force {
		return i.refuseExisting(found)
	}
	if err := i.checkProjectName(ctx, answers.DataDir); err != nil {
		return err
	}

	cfg := answers.config(i.a.cfg)
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("invalid configuration:\n%w", err)
	}
	i.a.cfg = cfg
	if err := i.preflight(ctx); err != nil {
		return err
	}

	key, err := i.masterKey(answers)
	if err != nil {
		return err
	}
	if err := answers.sealSMTPPassword(key); err != nil {
		return err
	}
	if err := answers.sealBackupSecret(key); err != nil {
		return err
	}
	if err := answers.sealDNSCredentials(key); err != nil {
		return err
	}
	if err := i.writeConfig(answers); err != nil {
		return err
	}
	if err := i.startPlatform(ctx, answers); err != nil {
		return err
	}
	if err := i.recreateTraefik(ctx, answers); err != nil {
		return err
	}
	generated := adminPassword == ""
	if generated {
		adminPassword = generatePassword()
	}
	created, err := i.createAdmin(ctx, answers.AdminEmail, adminPassword)
	if err != nil {
		return err
	}

	i.printf("\nPressFleet is installed and running.\n\n")
	i.printf("  Config file:      %s\n", i.configPath)
	i.printf("  Data directory:   %s\n", answers.DataDir)
	i.printf("  Master key file:  %s (key ID %s)\n", answers.MasterKeyFile, key.ID())
	i.printf("  Dashboard:        https://%s/\n", answers.DashboardDomain)
	i.printDNSSummary(answers)
	if smtp := answers.smtpConfig(); smtp.Enabled() {
		i.printf("  SMTP relay:       %s:%d (%s)\n", smtp.Host, smtp.EffectivePort(), smtp.Encryption)
	}
	i.printBackupSummary(answers)
	i.printf("  SFTP gateway:     sftp -P %d <site-slug>@%s\n", i.a.cfg.SFTP.Port, answers.DashboardDomain)
	i.printMetricsSummary()
	i.printf("  Admin user:       %s\n", auth.NormalizeEmail(answers.AdminEmail))
	if created && generated {
		i.printf("  Admin password:   %s\n\nStore the admin password now; it can't be shown again.\n", adminPassword.Reveal())
	}
	return nil
}

// printMetricsSummary tells where Grafana connects, and the credential when
// this run created it.
func (i *installer) printMetricsSummary() {
	domain := i.a.cfg.Metrics.PublicDomain
	if domain == "" {
		return
	}
	i.printf("  Metrics API:      https://%s/ (basic auth as %s)\n", domain, platform.MetricsAccessUsername)
	if i.metricsPassword != "" {
		i.printf("  Metrics password: %s\n\nStore the metrics password now; it can't be shown again. "+
			"`pressfleet metrics-access rotate` replaces it.\n\n", i.metricsPassword.Reveal())
	}
}

func (i *installer) refuseExisting(found []string) error {
	return fmt.Errorf("%w (found %s). Run with --force to reinstall; the existing master key is kept", errExistingInstall, strings.Join(found, ", "))
}

// defaults returns the initial answers: built-in defaults overridden by the
// environment and, when reinstalling, by the existing config file.
func (i *installer) defaults() (initAnswers, error) {
	cfg := config.Default()
	if err := config.ApplyEnv(&cfg, i.a.opts.Env); err != nil {
		return initAnswers{}, err
	}
	if exists(i.configPath) {
		// Only reached with --force. A broken file just doesn't provide
		// defaults; it is replaced below.
		if existing, err := config.Load(i.configPath, i.a.opts.Env); err == nil {
			cfg = existing
		}
	}
	i.a.cfg = cfg
	a := initAnswers{
		DashboardDomain:   cfg.DashboardDomain,
		PreviewBaseDomain: cfg.PreviewBaseDomain,
		PublicIP:          cfg.PublicIP,
		DataDir:           cfg.DataDir,
		MasterKeyFile:     cfg.MasterKeyFile,
		smtpAnswers:       smtpDefaults(cfg.SMTP),
		backupAnswers:     backupDefaults(cfg.Backup),
		ACMEEmail:         cfg.ACME.Email,
		ACMECAServer:      cfg.ACME.CAServer,
		dnsAnswers:        dnsDefaults(cfg.ACME),
	}
	if a.MasterKeyFile == "" {
		a.MasterKeyFile = filepath.Join(filepath.Dir(i.configPath), "master.key")
	}
	return a, nil
}

// ask fills answers from the given flag values, prompts, or defaults.
func (i *installer) ask(answers *initAnswers, given map[string]string) error {
	for _, q := range allInitQuestions() {
		field := q.field(answers)
		if v, ok := given[q.flag]; ok {
			if err := q.validate(v, answers); err != nil {
				return fmt.Errorf("--%s: %w", q.flag, err)
			}
			*field = v
			continue
		}
		if i.f.nonInteractive || q.flagOnly {
			if err := q.validate(*field, answers); err != nil {
				return fmt.Errorf("--%s: %w", q.flag, err)
			}
			continue
		}
		if q.skip != nil && q.skip(answers) {
			continue
		}
		for {
			prompt := q.prompt
			if *field != "" {
				prompt += " [" + *field + "]"
			}
			line, err := i.readLine(prompt + ": ")
			if err != nil {
				return err
			}
			v := strings.TrimSpace(line)
			if v == "" {
				v = *field
			}
			if err := q.validate(v, answers); err != nil {
				i.printf("  %v\n", err)
				continue
			}
			*field = v
			break
		}
	}
	return nil
}

// validate checks an answer, including whether a required one is missing.
func (q initQuestion) validate(v string, a *initAnswers) error {
	if v == "" {
		if q.optional && (q.requiredWhen == nil || !q.requiredWhen(a)) {
			return nil
		}
		return errors.New("a value is required")
	}
	if q.check == nil {
		return nil
	}
	return q.check(v, a)
}

// readLine prints prompt and reads one line of input without its line
// ending.
func (i *installer) readLine(prompt string) (string, error) {
	i.printf("%s", prompt)
	line, err := i.in.ReadString('\n')
	if errors.Is(err, io.EOF) {
		if line == "" {
			i.printf("\n")
			return "", errInputClosed
		}
		err = nil
	}
	if err != nil {
		return "", fmt.Errorf("reading standard input: %w", err)
	}
	return strings.TrimRight(line, "\r\n"), nil
}

func (i *installer) readPassword() (redact.Secret, error) {
	line, err := i.in.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", fmt.Errorf("reading the admin password from standard input: %w", err)
	}
	pw := redact.Secret(strings.TrimRight(line, "\r\n"))
	if err := auth.CheckPasswordPolicy(pw); err != nil {
		return "", fmt.Errorf("admin password: %w", err)
	}
	return pw, nil
}

// existingInstall lists signs of an existing installation.
func existingInstall(a initAnswers, configPath string) []string {
	layout := config.Layout{Root: a.DataDir}
	var found []string
	for _, c := range []struct{ what, path string }{
		{"config file", configPath},
		{"master key file", a.MasterKeyFile},
		{"platform project", filepath.Join(layout.PlatformDir(), platform.ComposeFile)},
		{"state database", filepath.Join(layout.StateDir(), "pressfleet.db")},
	} {
		if exists(c.path) {
			found = append(found, c.what+" "+c.path)
		}
	}
	return found
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// config returns base with the answers applied.
func (a initAnswers) config(base config.Config) config.Config {
	base.DataDir = a.DataDir
	base.DashboardDomain = a.DashboardDomain
	base.PreviewBaseDomain = a.PreviewBaseDomain
	base.PublicIP = a.PublicIP
	base.MasterKeyFile = a.MasterKeyFile
	base.SMTP = a.smtpConfig()
	// Init only asks where backups go; the backup window, retention, and
	// checks keep what base has.
	answered := a.backupConfig()
	base.Backup.Backend, base.Backup.S3 = answered.Backend, answered.S3
	base.ACME = a.acmeConfig()
	return base
}

// preflight runs the install-scope checks and refuses on a required failure.
func (i *installer) preflight(ctx context.Context) error {
	if i.f.skipPreflight {
		i.printf("\nPREFLIGHT CHECKS SKIPPED: --skip-preflight is for development machines only.\n")
		return nil
	}
	i.printf("\nRunning preflight checks...\n\n")
	rep := doctor.Run(ctx, i.a.doctorOptions(), doctor.ScopeInstall)
	if err := rep.WriteText(i.out); err != nil {
		return err
	}
	if failures := rep.Failures(); len(failures) > 0 {
		names := make([]string, len(failures))
		for n, f := range failures {
			names[n] = f.ID
		}
		return fmt.Errorf("required preflight checks failed: %s (fix them and run init again, "+
			"or pass --skip-preflight on a development machine)", strings.Join(names, ", "))
	}
	return nil
}

// masterKey generates the master key, or reuses the existing one when
// reinstalling, and has the operator confirm it is stored off-host. A new
// key is written only after confirmation, so aborting leaves nothing behind.
func (i *installer) masterKey(a initAnswers) (secrets.MasterKey, error) {
	if exists(a.MasterKeyFile) {
		// Only reached with --force: never replace an existing key.
		key, err := secrets.LoadMasterKey(secrets.KeySource{File: a.MasterKeyFile, DataDir: a.DataDir}, i.a.log)
		if err != nil {
			return secrets.MasterKey{}, fmt.Errorf("reusing the existing master key: %w (init never replaces a master key; "+
				"move the file aside yourself if you really mean to)", err)
		}
		i.printf("\nReusing the existing master key in %s (key ID %s).\n", a.MasterKeyFile, key.ID())
		if err := i.confirmKey(key, false); err != nil {
			return secrets.MasterKey{}, err
		}
		return key, nil
	}

	key := secrets.GenerateMasterKey()
	i.printf("\nGenerated the platform master key (key ID %s):\n\n    %s\n", key.ID(), key.Encode().Reveal())
	if err := i.confirmKey(key, true); err != nil {
		return secrets.MasterKey{}, err
	}
	if err := writeKeyFile(a.MasterKeyFile, key); err != nil {
		return secrets.MasterKey{}, err
	}
	i.printf("Wrote the master key to %s (mode 0600).\n", a.MasterKeyFile)
	return key, nil
}

// confirmKey prints the §4.1 warning and requires confirmation that the key
// is stored off-host.
func (i *installer) confirmKey(key secrets.MasterKey, isNew bool) error {
	i.printf("\nWARNING: %s\n\n", masterKeyWarning)
	if i.f.nonInteractive {
		// --confirm-key-stored was checked up front.
		i.printf("Key storage confirmed with --confirm-key-stored.\n")
		return nil
	}
	what := "the key above"
	if !isNew {
		what = "this key"
	}
	for {
		line, err := i.readLine(fmt.Sprintf("Type \"yes\" or the key ID once %s is stored off-host (\"no\" to abort): ", what))
		if err != nil {
			return err
		}
		switch v := strings.TrimSpace(line); {
		case strings.EqualFold(v, "yes") || v == key.ID():
			return nil
		case strings.EqualFold(v, "no"):
			return errKeyNotConfirmed
		}
	}
}

// writeKeyFile writes a new key file readable only by its owner. It never
// replaces an existing file.
func writeKeyFile(path string, key secrets.MasterKey) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil { //nolint:gosec // G301: the config directory; the key file itself is 0600
		return fmt.Errorf("creating %s: %w", filepath.Dir(path), err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // G304: the operator-chosen key file
	if err != nil {
		return fmt.Errorf("writing the master key: %w", err)
	}
	_, err = fmt.Fprintln(f, key.Encode().Reveal())
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(path)
		return fmt.Errorf("writing the master key: %w", err)
	}
	return nil
}

// fileConfig is what init writes to the config file: only the settings it
// collects, so every other setting keeps its documented default.
type fileConfig struct {
	DataDir           string `yaml:"data_dir"`
	DashboardDomain   string `yaml:"dashboard_domain"`
	PreviewBaseDomain string `yaml:"preview_base_domain"`
	PublicIP          string `yaml:"public_ip,omitempty"`
	MasterKeyFile     string `yaml:"master_key_file"`
	// PlatformProject is only written for a name other than the default.
	PlatformProject string `yaml:"platform_project,omitempty"`

	// SMTP is only written when a relay is configured.
	SMTP *fileSMTP       `yaml:"smtp,omitempty"`
	ACME *fileACMEConfig `yaml:"acme,omitempty"`
	// Backup is only written for the S3 backend; local is the default.
	Backup *fileBackup `yaml:"backup,omitempty"`
	// SFTP is only written for a port other than the default.
	SFTP *fileSFTP `yaml:"sftp,omitempty"`
	// Metrics is only written when it differs from the defaults, so
	// reinstalling keeps these settings.
	Metrics *fileMetrics `yaml:"metrics,omitempty"`
}

// fileMetrics are the metrics settings init renders into the platform
// project, and so must keep in the config file.
type fileMetrics struct {
	RetentionDays int    `yaml:"retention_days,omitempty"`
	PublicDomain  string `yaml:"public_domain,omitempty"`
}

// metricsFileConfig returns the metrics settings init keeps, or nil.
func metricsFileConfig(m config.MetricsConfig) *fileMetrics {
	f := fileMetrics{PublicDomain: m.PublicDomain}
	if m.RetentionDays != config.DefaultMetricsRetentionDays {
		f.RetentionDays = m.RetentionDays
	}
	if f == (fileMetrics{}) {
		return nil
	}
	return &f
}

// platformProjectFileConfig returns the platform's Compose project name
// for the config file, or "" for the default, so that the commands run
// later find the platform init started.
func (i *installer) platformProjectFileConfig() string {
	if name := i.projectName(); name != platform.ProjectName {
		return name
	}
	return ""
}

const configHeader = `# PressFleet configuration, written by pressfleet init.
# See docs/configuration.md for every setting.
`

// writeConfig writes the config file. When reinstalling, the previous file
// is kept next to it with a .bak suffix, since it may hold settings init
// doesn't manage.
func (i *installer) writeConfig(a initAnswers) error {
	body, err := yaml.Marshal(fileConfig{
		DataDir:           a.DataDir,
		DashboardDomain:   a.DashboardDomain,
		PreviewBaseDomain: a.PreviewBaseDomain,
		PublicIP:          a.PublicIP,
		MasterKeyFile:     a.MasterKeyFile,
		PlatformProject:   i.platformProjectFileConfig(),
		SMTP:              a.smtpFileConfig(),
		ACME:              a.acmeFileConfig(),
		Backup:            a.backupFileConfig(),
		SFTP:              sftpFileConfig(i.a.cfg.SFTP),
		Metrics:           metricsFileConfig(i.a.cfg.Metrics),
	})
	if err != nil {
		return err
	}
	dir := filepath.Dir(i.configPath)
	if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // G301: the config holds no secrets
		return fmt.Errorf("creating %s: %w", dir, err)
	}
	if old, err := os.ReadFile(i.configPath); err == nil {
		if err := os.WriteFile(i.configPath+".bak", old, 0o600); err != nil { //nolint:gosec // G703: the operator-chosen config path
			return fmt.Errorf("backing up the config file: %w", err)
		}
		i.printf("Saved the previous config file as %s.bak.\n", i.configPath)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("reading the existing config file: %w", err)
	}
	if err := writeFileAtomic(i.configPath, append([]byte(configHeader), body...), 0o644); err != nil {
		return fmt.Errorf("writing the config file: %w", err)
	}
	i.printf("Wrote the config file %s.\n", i.configPath)
	return nil
}

// writeFileAtomic replaces path with content via a temporary file.
func writeFileAtomic(path string, content []byte, mode os.FileMode) error {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() { _ = os.Remove(tmp) }() // no-op after the rename
	if err := f.Chmod(mode); err != nil {
		_ = f.Close()
		return err
	}
	if _, err := f.Write(content); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// startPlatform renders the platform project and starts it, waiting until
// its services are healthy.
func (i *installer) startPlatform(ctx context.Context, a initAnswers) error {
	layout := config.Layout{Root: a.DataDir}
	if err := layout.Ensure(); err != nil {
		return err
	}
	images, err := i.a.platformImages(i.f.controlPlaneImage)
	if err != nil {
		return err
	}
	dir := layout.PlatformDir()
	name := i.projectName()
	// The answers hold the sealed secrets, which i.a.cfg doesn't yet.
	i.metricsPassword, err = renderPlatform(dir, platformRender{
		cfg:           a.config(i.a.cfg),
		projectName:   name,
		images:        images,
		skipPreflight: i.f.skipPreflight,
		secretsDir:    dir,
	})
	if err != nil {
		return err
	}
	i.printf("Rendered the platform project in %s.\n", dir)

	i.printf("Starting the platform services; the first start pulls images and can take a few minutes...\n")
	project := i.a.platformProject(name, dir)
	if err := project.Up(ctx, compose.UpOptions{Wait: true, WaitTimeout: platformStartTimeout}); err != nil {
		return fmt.Errorf("starting the platform: %w (see the logs with `docker compose --project-name %s logs`)", err, name)
	}
	i.printf("The platform is running and healthy.\n")
	return nil
}

// platformProject returns the Compose project of the rendered platform.
func (a *app) platformProject(name, dir string) PlatformProject {
	if a.opts.PlatformProject != nil {
		return a.opts.PlatformProject(name, dir)
	}
	return compose.Project{Name: name, Dir: dir}
}

// mariaDBRootPassword returns the MariaDB root password already rendered
// into dir, or a new one. Reinstalling must reuse it: MariaDB only reads the
// secret when it initializes its data volume.
func mariaDBRootPassword(dir string) (redact.Secret, error) {
	data, err := os.ReadFile(filepath.Join(dir, platform.MariaDBRootPasswordSecret)) //nolint:gosec // G304: a file in the data directory
	switch {
	case err == nil && strings.TrimSpace(string(data)) != "":
		return redact.Secret(strings.TrimSpace(string(data))), nil
	case err == nil || errors.Is(err, fs.ErrNotExist):
		return redact.Secret(strings.ToLower(rand.Text())), nil
	default:
		return "", fmt.Errorf("reading the MariaDB root password: %w", err)
	}
}

// redisAdminPassword returns the object cache admin password already
// rendered into dir, or a new one. Reusing it keeps the password the
// running control plane holds valid: the server only reads its config file,
// which carries the password's hash, when it starts.
//
// Unlike site credentials, it is not stored encrypted in the state
// database: the host CLI never touches that database (§4.7), and the server
// needs the password before the control plane runs. Like the MariaDB root
// password, it lives in a 0600 Compose secret that only the control plane
// container mounts.
func redisAdminPassword(dir string) (redact.Secret, error) {
	data, err := os.ReadFile(filepath.Join(dir, platform.RedisAdminPasswordSecret)) //nolint:gosec // G304: a file in the data directory
	switch {
	case err == nil && strings.TrimSpace(string(data)) != "":
		return redact.Secret(strings.TrimSpace(string(data))), nil
	case err == nil || errors.Is(err, fs.ErrNotExist):
		return redact.Secret(rand.Text()), nil
	default:
		return "", fmt.Errorf("reading the Redis admin password: %w", err)
	}
}

// sshHostKey returns the SFTP gateway's host key already rendered into dir,
// or a new one. Reinstalling must reuse it: developers' SSH clients
// remember it (known_hosts) and refuse a gateway whose key changed.
func sshHostKey(dir string) (redact.Secret, error) {
	data, err := os.ReadFile(filepath.Join(dir, platform.SSHHostKeySecret)) //nolint:gosec // G304: a file in the data directory
	switch {
	case err == nil && strings.Contains(string(data), "PRIVATE KEY-----"):
		return redact.Secret(data), nil
	case err == nil || errors.Is(err, fs.ErrNotExist):
		key, err := sftpgw.GenerateHostKey()
		if err != nil {
			return "", fmt.Errorf("generating the SFTP gateway host key: %w", err)
		}
		return redact.Secret(key), nil
	default:
		return "", fmt.Errorf("reading the SFTP gateway host key: %w", err)
	}
}

// fileSFTP is the sftp section init writes.
type fileSFTP struct {
	Port int `yaml:"port"`
}

// sftpFileConfig returns the sftp section to write, nil for the default
// port.
func sftpFileConfig(s config.SFTPConfig) *fileSFTP {
	if s.Port == 0 || s.Port == platform.DefaultSFTPPort {
		return nil
	}
	return &fileSFTP{Port: s.Port}
}

// createAdmin creates the first admin by running `pressfleet user create`
// inside the control plane container, which owns the state database
// (§4.7). The password goes over stdin, never on the command line. It
// reports false if the user already exists, as when reinstalling.
func (i *installer) createAdmin(ctx context.Context, email string, password redact.Secret) (bool, error) {
	project := i.a.platformProject(i.projectName(), config.Layout{Root: i.a.cfg.DataDir}.PlatformDir())
	out, err := project.ExecInput(ctx, strings.NewReader(password.Reveal()+"\n"), controlPlaneService,
		"pressfleet", "user", "create", "--email", email, "--role", "admin", "--password-stdin")
	if err != nil {
		if strings.Contains(err.Error(), auth.ErrEmailTaken.Error()) {
			i.printf("The admin user %s already exists; left unchanged. "+
				"Use `pressfleet user reset-password` in the control plane container to set a new password.\n", email)
			return false, nil
		}
		return false, fmt.Errorf("creating the admin user: %w", err)
	}
	i.printf("%s\n", strings.TrimSpace(string(out)))
	return true, nil
}

func (i *installer) projectName() string {
	return i.a.platformProjectName()
}

// composeWorkingDirLabel is the label in which Compose records the project
// directory of a container's project.
const composeWorkingDirLabel = "com.docker.compose.project.working_dir"

// checkProjectName refuses to install under a Compose project name that a
// platform in another directory already uses on this Docker host: starting
// the new platform would recreate that one's services with this one's
// configuration. A reinstall (--force) in the same data directory reuses
// the name.
func (i *installer) checkProjectName(ctx context.Context, dataDir string) error {
	name := i.projectName()
	dir := config.Layout{Root: dataDir}.PlatformDir()
	c, err := i.a.controlPlaneProbe().ServiceContainer(ctx, controlPlaneService)
	if errors.Is(err, compose.ErrNoContainer) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("checking for another platform under the Compose project name %s: %w", name, err)
	}
	if other := c.Labels[composeWorkingDirLabel]; other != "" && filepath.Clean(other) != filepath.Clean(dir) {
		return fmt.Errorf("%w: a platform in %s already runs under the Compose project name %s. "+
			"Install this one under another name (platform_project, or PRESSFLEET_PLATFORM_PROJECT), or remove that platform first",
			errProjectInUse, other, name)
	}
	return nil
}

// generatePassword returns a random password: 26 base32 characters, 130
// bits.
func generatePassword() redact.Secret {
	return redact.Secret(strings.ToLower(rand.Text()))
}
