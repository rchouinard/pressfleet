package cli

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/rchouinard/pressfleet/control-plane/internal/config"
	"github.com/rchouinard/pressfleet/control-plane/internal/logging"
	"github.com/rchouinard/pressfleet/control-plane/internal/redact"
	"github.com/rchouinard/pressfleet/control-plane/internal/secrets"
	"github.com/rchouinard/pressfleet/control-plane/internal/sftpgw"
	"github.com/rchouinard/pressfleet/control-plane/internal/skew"
)

// `pressfleet init --target kubernetes` (design doc §4.8, #224) asks init's
// questions and writes what the Helm chart reads: a values file, and the
// Secrets the chart mounts but never creates. It never talks to the
// cluster: the operator applies the Secrets and installs the chart, and
// doctor checks the cluster when the control plane starts.

// The init targets (--target).
const (
	targetFlag       = "target"
	targetCompose    = "compose"
	targetKubernetes = "kubernetes"
)

// The files init writes for Kubernetes, in --output-dir.
const (
	kubernetesValuesFile  = "values.yaml"
	kubernetesSecretsFile = "secrets.yaml"
)

// defaultKubernetesOutputDir is where init writes for Kubernetes by
// default, relative to the working directory.
const defaultKubernetesOutputDir = "pressfleet-kubernetes"

// chartReference is the Helm chart's OCI reference, where the release
// pipeline publishes it (#227).
const chartReference = "oci://ghcr.io/rchouinard/pressfleet/charts/pressfleet"

// The Secrets init writes and the keys the chart reads from them: the
// chart's defaults (charts/pressfleet/values.yaml), so that the values file
// needn't name them.
//
//nolint:gosec // G101: the names of Secrets and their keys, not credentials
const (
	secretMasterKey          = "pressfleet-master-key"
	secretMasterKeyKey       = "master.key"
	secretPlatform           = "pressfleet-platform"
	secretMariaDBRootKey     = "mariadb-root-password"
	secretRedisAdminKey      = "redis-admin-password"
	secretSSHHostKeyKey      = "ssh-host-key"
	secretPostgres           = "pressfleet-postgres"
	secretPostgresKey        = "dsn"
	secretConfig             = "pressfleet-config"
	secretConfigKey          = "pressfleet.yaml"
	secretDNSCredentials     = "pressfleet-dns-credentials"
	secretACMECA             = "pressfleet-acme-ca"
	secretACMECAKey          = "ca.pem"
	kubernetesPlatformDir    = "/var/lib/pressfleet"
	kubernetesMasterKeyMount = "/run/secrets/master_key"
)

// Sentinel errors for init --target kubernetes.
var (
	// errOutputExists means the output directory already holds what init
	// writes, and --force wasn't given.
	errOutputExists = errors.New("the output directory already holds a Kubernetes install's files")
	// errTargetFlag means a flag of the other target was given.
	errTargetFlag = errors.New("flag doesn't apply to this target")
)

// kubernetesAnswers are the settings init collects only for Kubernetes.
type kubernetesAnswers struct {
	Namespace         string
	PodCIDRs          string // comma-separated
	StorageClassSites string
	StorageClassData  string
	// PostgresDSNFile holds the state database's DSN. Only its content is
	// kept, in the pressfleet-postgres Secret.
	PostgresDSNFile string
	// postgresDSN is the DSN: from PostgresDSNFile, the environment
	// (PRESSFLEET_DATABASE_POSTGRES_DSN), or the existing Secret.
	postgresDSN redact.Secret
}

// kubernetesCheck validates one Kubernetes setting by validating the
// default settings with it set.
func kubernetesCheck(set func(*config.KubernetesConfig)) error {
	k := config.DefaultKubernetes()
	set(&k)
	return k.Validate()
}

// kubernetesInitQuestions are asked only with --target kubernetes, after
// the public IP.
var kubernetesInitQuestions = []initQuestion{
	{
		flag:   "namespace",
		usage:  "Kubernetes namespace to install the platform into, the Helm release's (--target kubernetes; default pressfleet)",
		prompt: "Namespace to install the platform into",
		field:  func(a *initAnswers) *string { return &a.Namespace },
		check: func(v string, _ *initAnswers) error {
			return kubernetesCheck(func(k *config.KubernetesConfig) { k.PlatformNamespace = v })
		},
	},
	{
		flag: "pod-cidrs",
		usage: "comma-separated pod network ranges of the cluster, e.g. 10.42.0.0/16 on k3s or 10.244.0.0/16 on kind " +
			"(--target kubernetes; also $PRESSFLEET_KUBERNETES_POD_CIDRS)",
		prompt: "Pod network ranges of the cluster, comma-separated (e.g. 10.42.0.0/16 on k3s, 10.244.0.0/16 on kind)",
		field:  func(a *initAnswers) *string { return &a.PodCIDRs },
		check: func(v string, _ *initAnswers) error {
			return kubernetesCheck(func(k *config.KubernetesConfig) { k.PodCIDRs = splitComma(v) })
		},
	},
	{
		flag: "storage-class-sites",
		usage: "ReadWriteOnce storage class of site volumes and the platform's own (--target kubernetes; " +
			"default the cluster's default class; also $PRESSFLEET_KUBERNETES_STORAGE_CLASSES_SITES)",
		prompt:   "ReadWriteOnce storage class for site volumes (optional; empty for the cluster's default)",
		optional: true,
		field:    func(a *initAnswers) *string { return &a.StorageClassSites },
		check: func(v string, _ *initAnswers) error {
			return kubernetesCheck(func(k *config.KubernetesConfig) { k.StorageClasses.Sites = v })
		},
	},
	{
		flag: "storage-class-data",
		usage: "ReadWriteMany storage class of the control plane's data directory (--target kubernetes; " +
			"default the cluster's default class; also $PRESSFLEET_KUBERNETES_STORAGE_CLASSES_DATA)",
		prompt:   "ReadWriteMany storage class for the control plane's data directory (optional; empty for the cluster's default)",
		optional: true,
		field:    func(a *initAnswers) *string { return &a.StorageClassData },
		check: func(v string, _ *initAnswers) error {
			return kubernetesCheck(func(k *config.KubernetesConfig) { k.StorageClasses.Data = v })
		},
	},
	{
		flag: "postgres-dsn-file",
		usage: "file holding the DSN of the PostgreSQL state database, e.g. postgres://pressfleet:...@db:5432/pressfleet " +
			"(--target kubernetes; also $PRESSFLEET_DATABASE_POSTGRES_DSN)",
		prompt:   "File holding the DSN of the PostgreSQL state database (read once; kept only in the Secret)",
		optional: true,
		requiredWhen: func(a *initAnswers) bool {
			// The environment or the existing Secret may give it.
			return a.postgresDSN == ""
		},
		field: func(a *initAnswers) *string { return &a.PostgresDSNFile },
		check: func(v string, _ *initAnswers) error {
			_, err := readPostgresDSNFile(v)
			return err
		},
	},
}

// composeOnlyInitFlags are init's flags that only apply with --target
// compose.
var composeOnlyInitFlags = []string{
	"data-dir", "master-key-file", "admin-email", "admin-password-stdin", "skip-preflight", "control-plane-image",
}

// kubernetesOnlyInitFlags are init's flags that only apply with --target
// kubernetes.
func kubernetesOnlyInitFlags() []string {
	flags := []string{"output-dir"}
	for _, q := range kubernetesInitQuestions {
		flags = append(flags, q.flag)
	}
	return flags
}

// kubernetesInitQuestionList returns the questions init asks with --target
// kubernetes: the Compose questions without the host's paths and the admin
// user, which can only be created once the control plane runs, and with
// the Kubernetes questions.
func kubernetesInitQuestionList() []initQuestion {
	var qs []initQuestion
	for _, q := range allInitQuestions() {
		switch q.flag {
		case "data-dir", "master-key-file", "admin-email":
			continue
		case "preview-base-domain":
			q.prompt = "Preview base domain; *.<domain> must point at Traefik's Service (e.g. preview.agency.example)"
		case "public-ip":
			q.prompt = "Public IP address where custom domains must point, the address of Traefik's Service (optional)"
			qs = append(qs, q)
			qs = append(qs, kubernetesInitQuestions...)
			continue
		case "acme-ca-certificates":
			q.usage = "PEM file of extra root certificates to trust for the ACME server; with --target kubernetes, " +
				"init puts it in a Secret"
		}
		qs = append(qs, q)
	}
	return qs
}

// checkTargetFlags refuses the flags of the other target.
func checkTargetFlags(target string, changed func(string) bool) error {
	var wrong []string
	switch target {
	case targetCompose:
		wrong = kubernetesOnlyInitFlags()
	case targetKubernetes:
		wrong = composeOnlyInitFlags
	default:
		return fmt.Errorf("--target must be %s or %s, got %q", targetCompose, targetKubernetes, target)
	}
	for _, name := range wrong {
		if changed(name) {
			return fmt.Errorf("--%s: %w --target %s", name, errTargetFlag, target)
		}
	}
	return nil
}

// kubernetesSecret is a Secret manifest as init writes and reads it.
type kubernetesSecret struct {
	APIVersion string `yaml:"apiVersion"`
	Kind       string `yaml:"kind"`
	Metadata   struct {
		Name      string            `yaml:"name"`
		Namespace string            `yaml:"namespace,omitempty"`
		Labels    map[string]string `yaml:"labels,omitempty"`
	} `yaml:"metadata"`
	Type       string            `yaml:"type,omitempty"`
	StringData map[string]string `yaml:"stringData,omitempty"`
	// Data is only read, in case the operator wrote base64 values.
	Data map[string]string `yaml:"data,omitempty"`
}

// value returns the Secret's value for key, from stringData or data.
func (s kubernetesSecret) value(key string) (redact.Secret, bool) {
	if v, ok := s.StringData[key]; ok {
		return redact.Secret(v), true
	}
	if v, ok := s.Data[key]; ok {
		raw, err := base64.StdEncoding.DecodeString(v)
		if err == nil {
			return redact.Secret(raw), true
		}
	}
	return "", false
}

// newKubernetesSecret returns a Secret manifest.
func newKubernetesSecret(name, namespace string, data map[string]string) kubernetesSecret {
	s := kubernetesSecret{APIVersion: "v1", Kind: "Secret", Type: "Opaque", StringData: data}
	s.Metadata.Name, s.Metadata.Namespace = name, namespace
	s.Metadata.Labels = map[string]string{"app.kubernetes.io/part-of": "pressfleet"}
	return s
}

// existingSecrets are the Secrets of an earlier run, by name.
type existingSecrets map[string]kubernetesSecret

// get returns the value of key in the Secret name, or "".
func (e existingSecrets) get(name, key string) redact.Secret {
	v, _ := e[name].value(key)
	return v
}

// readKubernetesSecrets reads the Secret manifests init wrote earlier.
func readKubernetesSecrets(path string) (existingSecrets, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: the operator-chosen output directory
	if errors.Is(err, fs.ErrNotExist) {
		return existingSecrets{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading the existing Secrets: %w", err)
	}
	out := existingSecrets{}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	for {
		var s kubernetesSecret
		err := dec.Decode(&s)
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			// Never quote it: the file holds secrets.
			return nil, fmt.Errorf("parsing the existing Secrets in %s: not YAML Secret manifests", path)
		}
		if s.Kind == "Secret" {
			out[s.Metadata.Name] = s
		}
	}
}

// kubernetesValues is the chart values file init writes. It only sets what
// the answers give; everything else keeps the chart's defaults, and the
// Secret names are the chart's defaults.
type kubernetesValues struct {
	DashboardDomain   string                     `yaml:"dashboardDomain"`
	PreviewBaseDomain string                     `yaml:"previewBaseDomain"`
	PublicIP          string                     `yaml:"publicIP,omitempty"`
	Kubernetes        kubernetesValuesKubernetes `yaml:"kubernetes"`
	ACME              *kubernetesValuesACME      `yaml:"acme,omitempty"`
}

type kubernetesValuesKubernetes struct {
	PodCIDRs       []string                       `yaml:"podCIDRs"`
	StorageClasses kubernetesValuesStorageClasses `yaml:"storageClasses"`
}

type kubernetesValuesStorageClasses struct {
	Sites string `yaml:"sites"`
	Data  string `yaml:"data"`
}

type kubernetesValuesACME struct {
	Email                string                     `yaml:"email,omitempty"`
	CAServer             string                     `yaml:"caServer,omitempty"`
	Staging              bool                       `yaml:"staging,omitempty"`
	DNSProvider          string                     `yaml:"dnsProvider,omitempty"`
	DNSCredentialsSecret string                     `yaml:"dnsCredentialsSecret,omitempty"`
	DNSResolvers         []string                   `yaml:"dnsResolvers,omitempty"`
	CACertificates       *kubernetesValuesSecretRef `yaml:"caCertificates,omitempty"`
}

type kubernetesValuesSecretRef struct {
	SecretName string `yaml:"secretName"`
	Key        string `yaml:"key"`
}

// kubernetesFileConfig is the config file init puts in the
// pressfleet-config Secret: the settings the chart doesn't pass as
// variables. Its secrets are sealed with the master key, as on Compose.
type kubernetesFileConfig struct {
	SMTP   *fileSMTP   `yaml:"smtp,omitempty"`
	Backup *fileBackup `yaml:"backup,omitempty"`
}

const kubernetesValuesHeader = `# Values of the PressFleet Helm chart, written by
# pressfleet init --target kubernetes. Every other value keeps the chart's
# default (charts/pressfleet/values.yaml). The Secrets it reads are in
# secrets.yaml, beside this file.
`

const kubernetesConfigHeader = `# PressFleet configuration, written by pressfleet init --target kubernetes.
# The chart passes the settings it shares with the control plane as
# PRESSFLEET_* variables; see docs/configuration.md for the others.
`

// kubernetesFiles are the paths init writes for Kubernetes.
type kubernetesFiles struct {
	dir, values, secrets string
}

func (a *app) runKubernetesInit(f initFlags, given map[string]string) error {
	log, err := logging.New(a.opts.Stderr, "info", logging.FormatText)
	if err != nil {
		return err
	}
	a.log = log
	inst := &installer{a: a, f: f, in: bufio.NewReader(a.opts.Stdin), out: a.opts.Stdout, questions: kubernetesInitQuestionList()}
	return inst.runKubernetes(given)
}

func (i *installer) runKubernetes(given map[string]string) error {
	if i.f.nonInteractive && !i.f.confirmKeyStored {
		return fmt.Errorf("%w: pass --confirm-key-stored with --non-interactive once you have a way to store the printed key", errKeyNotConfirmed)
	}
	dir, err := filepath.Abs(i.f.outputDir)
	if err != nil {
		return err
	}
	files := kubernetesFiles{dir: dir, values: filepath.Join(dir, kubernetesValuesFile), secrets: filepath.Join(dir, kubernetesSecretsFile)}
	if !i.f.force {
		var found []string
		for _, p := range []string{files.values, files.secrets} {
			if exists(p) {
				found = append(found, p)
			}
		}
		if len(found) > 0 {
			return fmt.Errorf("%w (found %s). Run with --force to write them again; the existing master key and generated credentials are kept",
				errOutputExists, strings.Join(found, ", "))
		}
	}
	existing, err := readKubernetesSecrets(files.secrets)
	if err != nil {
		return err
	}
	answers, err := i.kubernetesDefaults(files, existing)
	if err != nil {
		return err
	}
	if !i.f.nonInteractive {
		i.printf("PressFleet setup for Kubernetes. Press Enter to accept the value in brackets.\n" +
			"Nothing here talks to the cluster: init writes a values file and the Secrets for you to apply.\n\n")
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
	if answers.PostgresDSNFile != "" {
		if answers.postgresDSN, err = readPostgresDSNFile(answers.PostgresDSNFile); err != nil {
			return fmt.Errorf("--postgres-dsn-file: %w", err)
		}
	}
	dnsCreds, err := answers.kubernetesDNSCredentials(existing)
	if err != nil {
		return err
	}
	caPEM, err := answers.kubernetesACMECA(existing)
	if err != nil {
		return err
	}

	cfg := answers.kubernetesConfig()
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("invalid configuration:\n%w", err)
	}
	i.a.cfg = cfg

	key, err := i.kubernetesMasterKey(existing)
	if err != nil {
		return err
	}
	if err := answers.sealSMTPPassword(key); err != nil {
		return err
	}
	if err := answers.sealBackupSecret(key); err != nil {
		return err
	}
	manifests, err := answers.kubernetesSecrets(key, existing, dnsCreds, caPEM)
	if err != nil {
		return err
	}
	values, err := answers.kubernetesValuesFile(dnsCreds != nil, caPEM != "")
	if err != nil {
		return err
	}
	if err := i.writeKubernetesFiles(files, values, manifests); err != nil {
		return err
	}
	i.printKubernetesNextSteps(answers, files)
	return nil
}

// kubernetesDefaults returns the initial answers: the built-in defaults,
// overridden by an earlier run's files (with --force) and by the
// environment.
func (i *installer) kubernetesDefaults(files kubernetesFiles, existing existingSecrets) (initAnswers, error) {
	cfg := config.Default()
	if data := existing.get(secretConfig, secretConfigKey); data != "" {
		// A broken file just doesn't provide defaults; it is replaced.
		_ = yaml.Unmarshal([]byte(data.Reveal()), &cfg)
	}
	if data, err := os.ReadFile(files.values); err == nil {
		var v kubernetesValues
		if yaml.Unmarshal(data, &v) == nil {
			v.applyTo(&cfg)
		}
	}
	if ns := existing[secretMasterKey].Metadata.Namespace; ns != "" {
		cfg.Kubernetes.PlatformNamespace = ns
	}
	if err := config.ApplyEnv(&cfg, i.a.opts.Env); err != nil {
		return initAnswers{}, err
	}
	a := initAnswers{
		DashboardDomain:   cfg.DashboardDomain,
		PreviewBaseDomain: cfg.PreviewBaseDomain,
		PublicIP:          cfg.PublicIP,
		smtpAnswers:       smtpDefaults(cfg.SMTP),
		backupAnswers:     backupDefaults(cfg.Backup),
		ACMEEmail:         cfg.ACME.Email,
		ACMECAServer:      cfg.ACME.CAServer,
		dnsAnswers:        dnsDefaults(cfg.ACME),
		kubernetesAnswers: kubernetesAnswers{
			Namespace:         cfg.Kubernetes.PlatformNamespace,
			PodCIDRs:          strings.Join(cfg.Kubernetes.PodCIDRs, ","),
			StorageClassSites: cfg.Kubernetes.StorageClasses.Sites,
			StorageClassData:  cfg.Kubernetes.StorageClasses.Data,
			postgresDSN:       cfg.Database.PostgresDSN,
		},
	}
	// On Kubernetes the DNS credentials are a Secret, never sealed in the
	// config, and the CA certificates are a Secret rather than a path.
	a.DNSCredentials, a.ACMECACertificates = "", ""
	if a.postgresDSN == "" {
		a.postgresDSN = existing.get(secretPostgres, secretPostgresKey)
	}
	return a, nil
}

// applyTo sets what an earlier values file says in cfg.
func (v kubernetesValues) applyTo(cfg *config.Config) {
	cfg.DashboardDomain, cfg.PreviewBaseDomain, cfg.PublicIP = v.DashboardDomain, v.PreviewBaseDomain, v.PublicIP
	cfg.Kubernetes.PodCIDRs = v.Kubernetes.PodCIDRs
	cfg.Kubernetes.StorageClasses = config.KubernetesStorageClasses{Sites: v.Kubernetes.StorageClasses.Sites, Data: v.Kubernetes.StorageClasses.Data}
	if v.ACME != nil {
		cfg.ACME.Email, cfg.ACME.CAServer, cfg.ACME.Staging = v.ACME.Email, v.ACME.CAServer, v.ACME.Staging
		cfg.ACME.DNSProvider, cfg.ACME.DNSResolvers = v.ACME.DNSProvider, v.ACME.DNSResolvers
	}
}

// readPostgresDSNFile returns the first line of a DSN file.
func readPostgresDSNFile(path string) (redact.Secret, error) {
	if err := checkCleanAbs(path); err != nil {
		return "", err
	}
	data, err := os.ReadFile(path) //nolint:gosec // G304: the operator-chosen DSN file
	if err != nil {
		return "", fmt.Errorf("reading the PostgreSQL DSN: %w", err)
	}
	dsn, _, _ := strings.Cut(string(data), "\n")
	dsn = strings.TrimSpace(dsn)
	if dsn == "" {
		return "", errors.New("the PostgreSQL DSN file is empty")
	}
	if !strings.HasPrefix(dsn, "postgres://") && !strings.HasPrefix(dsn, "postgresql://") && !strings.Contains(dsn, "=") {
		// Never quote it: it may hold a password.
		return "", errors.New("the PostgreSQL DSN must be a URL (postgres://user:password@host:5432/database) or key=value pairs")
	}
	return redact.Secret(dsn), nil
}

// kubernetesDNSCredentials returns the DNS provider's credentials for
// Traefik's Secret: new ones from --dns-credentials-file, or the existing
// Secret's. The chart needs them with a DNS provider.
func (a *initAnswers) kubernetesDNSCredentials(existing existingSecrets) (map[string]string, error) {
	if a.DNSProvider == "" {
		return nil, nil
	}
	creds := map[string]string{}
	if a.dnsNewCredentials != nil {
		for name, v := range a.dnsNewCredentials {
			creds[name] = v.Reveal()
		}
		// The answers only keep them for this Secret, never sealed.
		a.dnsNewCredentials = nil
		return creds, nil
	}
	if s, ok := existing[secretDNSCredentials]; ok {
		for name := range s.StringData {
			v, _ := s.value(name)
			creds[name] = v.Reveal()
		}
		for name := range s.Data {
			v, _ := s.value(name)
			creds[name] = v.Reveal()
		}
	}
	if len(creds) == 0 {
		return nil, fmt.Errorf("--dns-credentials-file is required with --dns-provider on Kubernetes: Traefik reads the credentials from the Secret %s", secretDNSCredentials)
	}
	return creds, nil
}

// kubernetesACMECA returns the extra ACME root certificates for their
// Secret: from --acme-ca-certificates, or the existing Secret's.
func (a *initAnswers) kubernetesACMECA(existing existingSecrets) (string, error) {
	if a.ACMECACertificates != "" {
		data, err := os.ReadFile(a.ACMECACertificates)
		if err != nil {
			return "", fmt.Errorf("--acme-ca-certificates: %w", err)
		}
		return string(data), nil
	}
	return existing.get(secretACMECA, secretACMECAKey).Reveal(), nil
}

// kubernetesConfig returns the configuration the control plane will run
// with, as far as init knows it, for validation: the chart sets the rest.
func (a initAnswers) kubernetesConfig() config.Config {
	cfg := config.Default()
	cfg.Driver = config.SiteDriverKubernetes
	cfg.DataDir = kubernetesPlatformDir
	cfg.MasterKeyFile = kubernetesMasterKeyMount
	cfg.DashboardDomain, cfg.PreviewBaseDomain, cfg.PublicIP = a.DashboardDomain, a.PreviewBaseDomain, a.PublicIP
	cfg.Database.Driver = config.DriverPostgres
	cfg.Database.PostgresDSN = a.postgresDSN
	cfg.SMTP = a.smtpConfig()
	answered := a.backupConfig()
	cfg.Backup.Backend, cfg.Backup.S3 = answered.Backend, answered.S3
	// Traefik takes the ACME settings from the chart, but they are checked
	// as the config file's would be. Credentials and root certificates are
	// Secrets here, not settings.
	cfg.ACME = a.acmeConfig()
	cfg.ACME.DNSCredentials, cfg.ACME.CACertificates = "", ""
	cfg.Kubernetes.PlatformNamespace = a.Namespace
	cfg.Kubernetes.PodCIDRs = splitComma(a.PodCIDRs)
	cfg.Kubernetes.StorageClasses = config.KubernetesStorageClasses{Sites: a.StorageClassSites, Data: a.StorageClassData}
	return cfg
}

// kubernetesMasterKey generates the master key, or reuses the one in the
// existing Secrets, and has the operator confirm it is stored off-cluster.
func (i *installer) kubernetesMasterKey(existing existingSecrets) (secrets.MasterKey, error) {
	if encoded := existing.get(secretMasterKey, secretMasterKeyKey); encoded != "" {
		key, err := secrets.ParseMasterKey(encoded)
		if err != nil {
			return secrets.MasterKey{}, fmt.Errorf("reusing the existing master key: %w (init never replaces a master key; "+
				"move %s aside yourself if you really mean to)", err, kubernetesSecretsFile)
		}
		i.printf("\nReusing the existing master key (key ID %s).\n", key.ID())
		return key, i.confirmKey(key, false)
	}
	key := secrets.GenerateMasterKey()
	i.printf("\nGenerated the platform master key (key ID %s):\n\n    %s\n", key.ID(), key.Encode().Reveal())
	return key, i.confirmKey(key, true)
}

// kubernetesSecrets returns the Secret manifests: the generated
// credentials are reused from the existing Secrets, because MariaDB and
// Valkey keep the first ones and SSH clients remember the host key.
func (a initAnswers) kubernetesSecrets(key secrets.MasterKey, existing existingSecrets, dnsCreds map[string]string, caPEM string) ([]kubernetesSecret, error) {
	mariadb := existing.get(secretPlatform, secretMariaDBRootKey)
	if mariadb == "" {
		mariadb = redact.Secret(strings.ToLower(rand.Text()))
	}
	redis := existing.get(secretPlatform, secretRedisAdminKey)
	if redis == "" {
		redis = redact.Secret(rand.Text())
	}
	hostKey := existing.get(secretPlatform, secretSSHHostKeyKey)
	if !strings.Contains(hostKey.Reveal(), "PRIVATE KEY-----") {
		k, err := sftpgw.GenerateHostKey()
		if err != nil {
			return nil, fmt.Errorf("generating the SFTP gateway host key: %w", err)
		}
		hostKey = redact.Secret(k)
	}
	body, err := yaml.Marshal(kubernetesFileConfig{SMTP: a.smtpFileConfig(), Backup: a.backupFileConfig()})
	if err != nil {
		return nil, err
	}
	ns := a.Namespace
	out := []kubernetesSecret{
		newKubernetesSecret(secretMasterKey, ns, map[string]string{secretMasterKeyKey: key.Encode().Reveal() + "\n"}),
		newKubernetesSecret(secretPlatform, ns, map[string]string{
			secretMariaDBRootKey: mariadb.Reveal(),
			secretRedisAdminKey:  redis.Reveal(),
			secretSSHHostKeyKey:  hostKey.Reveal(),
		}),
		newKubernetesSecret(secretPostgres, ns, map[string]string{secretPostgresKey: a.postgresDSN.Reveal()}),
		newKubernetesSecret(secretConfig, ns, map[string]string{secretConfigKey: kubernetesConfigHeader + string(body)}),
	}
	if dnsCreds != nil {
		out = append(out, newKubernetesSecret(secretDNSCredentials, ns, dnsCreds))
	}
	if caPEM != "" {
		out = append(out, newKubernetesSecret(secretACMECA, ns, map[string]string{secretACMECAKey: caPEM}))
	}
	return out, nil
}

// kubernetesValuesFile returns the chart values file.
func (a initAnswers) kubernetesValuesFile(dnsCredentials, caCertificates bool) ([]byte, error) {
	v := kubernetesValues{
		DashboardDomain:   a.DashboardDomain,
		PreviewBaseDomain: a.PreviewBaseDomain,
		PublicIP:          a.PublicIP,
		Kubernetes: kubernetesValuesKubernetes{
			PodCIDRs:       splitComma(a.PodCIDRs),
			StorageClasses: kubernetesValuesStorageClasses{Sites: a.StorageClassSites, Data: a.StorageClassData},
		},
	}
	acme := kubernetesValuesACME{
		Email: a.ACMEEmail, CAServer: a.ACMECAServer, Staging: a.ACMEStaging,
		DNSProvider: a.DNSProvider, DNSResolvers: splitComma(a.DNSResolvers),
	}
	if dnsCredentials {
		acme.DNSCredentialsSecret = secretDNSCredentials
	}
	if caCertificates {
		acme.CACertificates = &kubernetesValuesSecretRef{SecretName: secretACMECA, Key: secretACMECAKey}
	}
	if acme.Email != "" || acme.CAServer != "" || acme.Staging || acme.DNSProvider != "" ||
		len(acme.DNSResolvers) > 0 || acme.CACertificates != nil {
		v.ACME = &acme
	}
	body, err := yaml.Marshal(v)
	if err != nil {
		return nil, err
	}
	return append([]byte(kubernetesValuesHeader), body...), nil
}

// writeKubernetesFiles writes the values file and the Secrets. The Secrets
// file is readable only by its owner: it holds the master key and every
// credential. An earlier values file, which the operator may have edited,
// is kept with a .bak suffix.
func (i *installer) writeKubernetesFiles(files kubernetesFiles, values []byte, manifests []kubernetesSecret) error {
	if err := os.MkdirAll(files.dir, 0o700); err != nil {
		return fmt.Errorf("creating %s: %w", files.dir, err)
	}
	var b bytes.Buffer
	b.WriteString("# Secrets of the PressFleet Helm chart, written by pressfleet init --target kubernetes.\n" +
		"# They hold the master key and every platform credential: keep this file out of version control.\n")
	for _, s := range manifests {
		body, err := yaml.Marshal(s)
		if err != nil {
			return err
		}
		b.WriteString("---\n")
		b.Write(body)
	}
	if err := writeFileAtomic(files.secrets, b.Bytes(), 0o600); err != nil {
		return fmt.Errorf("writing the Secrets: %w", err)
	}
	if old, err := os.ReadFile(files.values); err == nil && !bytes.Equal(old, values) {
		if err := os.WriteFile(files.values+".bak", old, 0o600); err != nil { //nolint:gosec // G703: the operator-chosen output directory
			return fmt.Errorf("backing up the values file: %w", err)
		}
		i.printf("Saved the previous values file as %s.bak.\n", files.values)
	}
	if err := writeFileAtomic(files.values, values, 0o644); err != nil {
		return fmt.Errorf("writing the values file: %w", err)
	}
	return nil
}

// chartVersion is the chart version to install: the CLI's release.
func (i *installer) chartVersion() string {
	v, err := skew.Parse(i.a.opts.Version)
	if err != nil {
		return "<release>"
	}
	return strings.TrimPrefix(v.String(), "v")
}

func (i *installer) printKubernetesNextSteps(a initAnswers, files kubernetesFiles) {
	ns := a.Namespace
	i.printf("\nWrote the chart values to %s and the Secrets to %s.\n", files.values, files.secrets)
	i.printf("The Secrets file holds the master key and every credential (mode 0600): keep it out of version control.\n\n")
	i.printf("  Namespace:        %s\n", ns)
	i.printf("  Dashboard:        https://%s/\n", a.DashboardDomain)
	if a.DNSProvider != "" {
		i.printf("  Preview TLS:      wildcard *.%s via %s (DNS-01)\n", a.PreviewBaseDomain, a.DNSProvider)
	} else {
		i.printf("  Preview TLS:      one certificate per preview domain (HTTP-01)\n")
	}
	if c := a.backupConfig(); c.Backend == config.BackupBackendS3 {
		i.printf("  Backups:          %s/%s\n", strings.TrimSuffix(c.S3.Endpoint, "/"), c.S3.Bucket)
	} else {
		i.printf("  Backups:          the control plane's data volume (keep a copy off the cluster)\n")
	}
	i.printf("\nNothing was sent to the cluster. To install (docs/installation.md):\n\n")
	steps := []string{
		fmt.Sprintf("kubectl create namespace %s", ns),
		fmt.Sprintf("kubectl label namespace %s pod-security.kubernetes.io/enforce=restricted", ns),
		fmt.Sprintf("kubectl apply --server-side -f %s", files.secrets),
		fmt.Sprintf("helm install pressfleet %s --version %s --namespace %s --values %s --wait --timeout 15m",
			chartReference, i.chartVersion(), ns, files.values),
		fmt.Sprintf("kubectl -n %s get service traefik   # point the dashboard and *.%s at its address", ns, a.PreviewBaseDomain),
		fmt.Sprintf("kubectl -n %s exec deploy/control-plane -- pressfleet user create --email <you@agency.example> --role admin", ns),
	}
	for n, s := range steps {
		i.printf("  %d. %s\n", n+1, s)
	}
}
