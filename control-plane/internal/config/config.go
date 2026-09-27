// Package config loads control plane configuration.
//
// Precedence, lowest to highest: built-in defaults, the config file,
// PRESSFLEET_* environment variables, then command-line flags (applied by the
// CLI). See docs/configuration.md.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/mail"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/rchouinard/pressfleet/control-plane/internal/logging"
	"github.com/rchouinard/pressfleet/control-plane/internal/redact"
)

// DefaultPath is where the config file is read from when no path is given.
// A missing file at the default path is not an error.
const DefaultPath = "/etc/pressfleet/pressfleet.yaml"

// EnvConfigPath names the environment variable that selects the config file.
const EnvConfigPath = "PRESSFLEET_CONFIG"

// Config is the control plane configuration.
type Config struct {
	// DataDir holds control plane state and rendered Compose projects.
	DataDir string `yaml:"data_dir"`
	// Listen is the HTTP listen address for the API and dashboard.
	Listen string `yaml:"listen"`
	// Log configures logging.
	Log LogConfig `yaml:"log"`
	// DashboardDomain is where the dashboard and API are served (§4.1).
	DashboardDomain string `yaml:"dashboard_domain"`
	// PreviewBaseDomain has a wildcard DNS record pointing at this host;
	// previews are served from its subdomains (§4.1).
	PreviewBaseDomain string `yaml:"preview_base_domain"`
	// PublicIP is this host's public address. It is optional and only used
	// by preflight DNS checks on hosts behind NAT, where no network
	// interface carries the public address.
	PublicIP string `yaml:"public_ip"`
	// Database configures the control plane state database.
	Database DatabaseConfig `yaml:"database"`
	// MasterKeyFile is a file holding the platform master key. It must be
	// outside DataDir (§4.1).
	MasterKeyFile string `yaml:"master_key_file"`
	// MasterKey is the platform master key itself. It is only read from the
	// environment, so it never ends up in a config file; it wins over
	// MasterKeyFile.
	MasterKey redact.Secret `yaml:"-"`
	// Jobs configures the background job workers.
	Jobs JobsConfig `yaml:"jobs"`
	// HTTP configures how the API sees requests that come through the
	// reverse proxy.
	HTTP HTTPConfig `yaml:"http"`
	// MariaDB is the admin connection to the shared MariaDB server that
	// holds site databases.
	MariaDB MariaDBConfig `yaml:"mariadb"`
	// Audit configures the audit log (§4.6).
	Audit AuditConfig `yaml:"audit"`
	// PlatformProject is the Compose project name of the platform services,
	// which names their containers: the provisioner connects the
	// platform's MariaDB container to each site network.
	PlatformProject string `yaml:"platform_project"`
	// SMTP is the relay sites send email through (§5.6).
	SMTP SMTPConfig `yaml:"smtp"`
	// WordPressSecurity is the install-wide WordPress security set (§6.1).
	WordPressSecurity WordPressSecurityConfig `yaml:"wordpress_security"`
}

// Values of smtp.encryption.
const (
	SMTPEncryptionSTARTTLS = "starttls" // plain connection upgraded with STARTTLS (required)
	SMTPEncryptionTLS      = "tls"      // TLS from the start (SMTPS)
	SMTPEncryptionNone     = "none"     // no TLS, e.g. a relay on the same host or network
)

// SMTPPasswordSetting names smtp.password when it is sealed with the master
// key (secrets.Keyring.SealSetting); the name is authenticated with the
// ciphertext.
const SMTPPasswordSetting = "smtp.password"

// sealedSettingPrefix starts every value made by secrets.Keyring.SealSetting.
// Validate only checks the shape; the control plane decrypts it at startup.
const sealedSettingPrefix = "pfenc1:"

// SMTPConfig is the SMTP relay sites send email through. Without a host,
// sites send no email: the site images have no local mail transfer agent.
type SMTPConfig struct {
	// Host is the relay's host name or IP address. Empty disables outbound
	// email.
	Host string `yaml:"host"`
	// Port is the relay's port; 0 means the standard port for Encryption
	// (see EffectivePort).
	Port int `yaml:"port"`
	// Username and Password authenticate with the relay; leave both empty
	// for a relay that doesn't require authentication.
	Username string `yaml:"username"`
	// Password is encrypted with the master key under the name
	// SMTPPasswordSetting ("pfenc1:..."), as pressfleet init writes it, so
	// neither the config file nor the rendered platform project holds it in
	// plaintext.
	Password string `yaml:"password"`
	// Encryption is SMTPEncryptionSTARTTLS (the default), SMTPEncryptionTLS,
	// or SMTPEncryptionNone.
	Encryption string `yaml:"encryption"`
	// FromEmail and FromName replace WordPress's default sender
	// (wordpress@<site domain>), which relays usually refuse. Empty keeps
	// WordPress's default.
	FromEmail string `yaml:"from_email"`
	FromName  string `yaml:"from_name"`
}

// Enabled reports whether sites send email through the relay.
func (s SMTPConfig) Enabled() bool { return s.Host != "" }

// EffectivePort returns Port, or the standard port for Encryption if Port
// is 0.
func (s SMTPConfig) EffectivePort() int {
	if s.Port != 0 {
		return s.Port
	}
	switch s.Encryption {
	case SMTPEncryptionTLS:
		return 465
	case SMTPEncryptionNone:
		return 25
	default:
		return 587
	}
}

// Validate reports every problem with the SMTP settings, or nil.
func (s SMTPConfig) Validate() error {
	var errs []error
	if !s.Enabled() {
		if s.Port != 0 || s.Username != "" || s.Password != "" || s.FromEmail != "" || s.FromName != "" {
			errs = append(errs, errors.New("smtp.host is required when other smtp settings are set"))
		}
		return errors.Join(errs...)
	}
	if !validHost(s.Host) {
		errs = append(errs, fmt.Errorf("smtp.host must be a host name or IP address, got %q", s.Host))
	}
	if s.Port < 0 || s.Port > 65535 {
		errs = append(errs, fmt.Errorf("smtp.port must be between 1 and 65535, got %d", s.Port))
	}
	switch s.Encryption {
	case SMTPEncryptionSTARTTLS, SMTPEncryptionTLS, SMTPEncryptionNone:
	default:
		errs = append(errs, fmt.Errorf("smtp.encryption must be %q, %q, or %q, got %q",
			SMTPEncryptionSTARTTLS, SMTPEncryptionTLS, SMTPEncryptionNone, s.Encryption))
	}
	if (s.Username == "") != (s.Password == "") {
		errs = append(errs, errors.New("smtp.username and smtp.password must be set together"))
	}
	if strings.ContainsAny(s.Username, "\r\n") {
		errs = append(errs, errors.New("smtp.username must not contain line breaks"))
	}
	if s.Password != "" && !strings.HasPrefix(s.Password, sealedSettingPrefix) {
		// The value is not quoted back: it is probably a plaintext password.
		errs = append(errs, errors.New("smtp.password must be encrypted with the master key (pressfleet init writes it that way), not plaintext"))
	}
	if s.FromEmail != "" {
		if a, err := mail.ParseAddress(s.FromEmail); err != nil || a.Address != s.FromEmail {
			errs = append(errs, fmt.Errorf("smtp.from_email must be an email address like wordpress@agency.example, got %q", s.FromEmail))
		}
	}
	if strings.ContainsAny(s.FromName, "\r\n") {
		errs = append(errs, errors.New("smtp.from_name must not contain line breaks"))
	}
	return errors.Join(errs...)
}

// validHost accepts an IP address or a host name, including a single-label
// name such as a relay container on a Docker network.
func validHost(s string) bool {
	if _, err := netip.ParseAddr(s); err == nil {
		return true
	}
	return ValidDomain(s) || (!strings.Contains(s, ".") && ValidDomain(s+".local"))
}

// AuditConfig configures the audit log.
type AuditConfig struct {
	// RetentionDays is how long audit events are kept; a daily job deletes
	// older ones.
	RetentionDays int `yaml:"retention_days"`
}

// Retention returns RetentionDays as a duration.
func (a AuditConfig) Retention() time.Duration {
	return time.Duration(a.RetentionDays) * 24 * time.Hour
}

// MaxAuditRetentionDays bounds audit.retention_days at about ten years.
const MaxAuditRetentionDays = 3660

// MariaDBConfig configures the control plane's admin connection to the
// shared MariaDB server.
type MariaDBConfig struct {
	// Address is the server's host:port as seen from the control plane.
	Address string `yaml:"address"`
	// AdminUser is the account that creates site databases and users.
	AdminUser string `yaml:"admin_user"`
	// AdminPasswordFile holds the admin user's password. The platform
	// mounts the MariaDB root password there as a Compose secret; the
	// password itself is never part of the configuration.
	AdminPasswordFile string `yaml:"admin_password_file"`
}

// Values of http.secure_cookies.
const (
	SecureCookiesAuto   = "auto"
	SecureCookiesAlways = "always"
)

// HTTPConfig configures request handling behind the reverse proxy.
type HTTPConfig struct {
	// SecureCookies is "auto" (the default, also spelled "": mark the
	// session cookie Secure when the request arrived over HTTPS, directly or
	// via a trusted proxy) or "always".
	SecureCookies string `yaml:"secure_cookies"`
	// TrustedProxies lists addresses or CIDR networks whose
	// X-Forwarded-For and X-Forwarded-Proto headers are believed. Empty
	// means the default: loopback and private networks.
	TrustedProxies []string `yaml:"trusted_proxies"`
}

// TrustedProxyPrefixes parses TrustedProxies. It returns nil (use the
// default) if the list is empty.
func (h HTTPConfig) TrustedProxyPrefixes() ([]netip.Prefix, error) {
	var out []netip.Prefix
	for _, s := range h.TrustedProxies {
		s = strings.TrimSpace(s)
		p, err := netip.ParsePrefix(s)
		if err != nil {
			a, aerr := netip.ParseAddr(s)
			if aerr != nil {
				return nil, fmt.Errorf("http.trusted_proxies: %q is not an IP address or CIDR network", s)
			}
			p = netip.PrefixFrom(a, a.BitLen())
		}
		out = append(out, p.Masked())
	}
	return out, nil
}

// JobsConfig configures the background job workers (§5).
type JobsConfig struct {
	// Concurrency is how many jobs run at once.
	Concurrency int `yaml:"concurrency"`
}

// MaxJobsConcurrency bounds jobs.concurrency. Jobs mostly wait on Docker
// and databases, but each one holds resources on a single host.
const MaxJobsConcurrency = 64

// LogConfig configures logging.
type LogConfig struct {
	Level  string `yaml:"level"`  // debug, info, warn, error
	Format string `yaml:"format"` // text, json
}

// Database drivers.
const (
	DriverSQLite   = "sqlite"
	DriverPostgres = "postgres"
)

// DatabaseConfig configures the control plane state database.
type DatabaseConfig struct {
	Driver string `yaml:"driver"` // sqlite, postgres
	// SQLitePath is the SQLite database file; empty means
	// <data_dir>/state/pressfleet.db.
	SQLitePath string `yaml:"sqlite_path"`
	// PostgresDSN is the PostgreSQL connection string (URL or key=value).
	PostgresDSN redact.Secret `yaml:"postgres_dsn"`
}

// Default returns the built-in defaults.
func Default() Config {
	return Config{
		DataDir:  "/var/lib/pressfleet",
		Listen:   ":8080",
		Log:      LogConfig{Level: "info", Format: logging.FormatText},
		Database: DatabaseConfig{Driver: DriverSQLite},
		Jobs:     JobsConfig{Concurrency: 4},
		Audit:    AuditConfig{RetentionDays: 365},
		// platform.ProjectName; config can't import package platform.
		PlatformProject: "pressfleet",
		MariaDB: MariaDBConfig{ //nolint:gosec // G101: the path of the password file, not a credential
			Address:           "mariadb:3306",
			AdminUser:         "root",
			AdminPasswordFile: "/run/secrets/mariadb_root_password",
		},
		SMTP:              SMTPConfig{Encryption: SMTPEncryptionSTARTTLS},
		WordPressSecurity: DefaultWordPressSecurity(),
	}
}

// envVars maps environment variables to the fields they set.
var envVars = []struct {
	name  string
	field func(*Config) *string
}{
	{"PRESSFLEET_DATA_DIR", func(c *Config) *string { return &c.DataDir }},
	{"PRESSFLEET_LISTEN", func(c *Config) *string { return &c.Listen }},
	{"PRESSFLEET_LOG_LEVEL", func(c *Config) *string { return &c.Log.Level }},
	{"PRESSFLEET_LOG_FORMAT", func(c *Config) *string { return &c.Log.Format }},
	{"PRESSFLEET_DASHBOARD_DOMAIN", func(c *Config) *string { return &c.DashboardDomain }},
	{"PRESSFLEET_PREVIEW_BASE_DOMAIN", func(c *Config) *string { return &c.PreviewBaseDomain }},
	{"PRESSFLEET_PUBLIC_IP", func(c *Config) *string { return &c.PublicIP }},
	{"PRESSFLEET_DATABASE_DRIVER", func(c *Config) *string { return &c.Database.Driver }},
	{"PRESSFLEET_DATABASE_SQLITE_PATH", func(c *Config) *string { return &c.Database.SQLitePath }},
	{"PRESSFLEET_DATABASE_POSTGRES_DSN", func(c *Config) *string { return (*string)(&c.Database.PostgresDSN) }},
	{"PRESSFLEET_MASTER_KEY_FILE", func(c *Config) *string { return &c.MasterKeyFile }},
	{"PRESSFLEET_MASTER_KEY", func(c *Config) *string { return (*string)(&c.MasterKey) }},
	{"PRESSFLEET_HTTP_SECURE_COOKIES", func(c *Config) *string { return &c.HTTP.SecureCookies }},
	{"PRESSFLEET_MARIADB_ADDRESS", func(c *Config) *string { return &c.MariaDB.Address }},
	{"PRESSFLEET_MARIADB_ADMIN_USER", func(c *Config) *string { return &c.MariaDB.AdminUser }},
	{"PRESSFLEET_MARIADB_ADMIN_PASSWORD_FILE", func(c *Config) *string { return &c.MariaDB.AdminPasswordFile }},
	{"PRESSFLEET_PLATFORM_PROJECT", func(c *Config) *string { return &c.PlatformProject }},
	{"PRESSFLEET_SMTP_HOST", func(c *Config) *string { return &c.SMTP.Host }},
	{"PRESSFLEET_SMTP_USERNAME", func(c *Config) *string { return &c.SMTP.Username }},
	{"PRESSFLEET_SMTP_PASSWORD", func(c *Config) *string { return &c.SMTP.Password }},
	{"PRESSFLEET_SMTP_ENCRYPTION", func(c *Config) *string { return &c.SMTP.Encryption }},
	{"PRESSFLEET_SMTP_FROM_EMAIL", func(c *Config) *string { return &c.SMTP.FromEmail }},
	{"PRESSFLEET_SMTP_FROM_NAME", func(c *Config) *string { return &c.SMTP.FromName }},
}

// envTrustedProxies sets http.trusted_proxies as a comma-separated list.
const envTrustedProxies = "PRESSFLEET_HTTP_TRUSTED_PROXIES"

// LookupEnv looks up an environment variable, like os.LookupEnv.
type LookupEnv func(key string) (string, bool)

// Load builds a Config from defaults, the config file, and the environment.
//
// The config file is path if non-empty, else $PRESSFLEET_CONFIG, else
// DefaultPath. An explicitly chosen file must exist. Load does not validate
// the result; call Validate after applying flag overrides.
func Load(path string, env LookupEnv) (Config, error) {
	if env == nil {
		env = os.LookupEnv
	}
	cfg := Default()

	explicit := true
	if path == "" {
		path, explicit = env(EnvConfigPath)
		if !explicit || path == "" {
			path, explicit = DefaultPath, false
		}
	}
	switch err := loadFile(&cfg, path); {
	case err == nil:
	case !explicit && errors.Is(err, fs.ErrNotExist):
		// No config file at the default location; defaults and env apply.
	default:
		return Config{}, err
	}

	if err := ApplyEnv(&cfg, env); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// ApplyEnv overrides cfg with PRESSFLEET_* environment variables. Load
// calls it; `pressfleet init` uses it on its own because it runs before a
// config file exists.
func ApplyEnv(cfg *Config, env LookupEnv) error {
	if env == nil {
		env = os.LookupEnv
	}
	for _, v := range envVars {
		if val, ok := env(v.name); ok {
			*v.field(cfg) = val
		}
	}
	if val, ok := env("PRESSFLEET_JOBS_CONCURRENCY"); ok {
		n, err := strconv.Atoi(val)
		if err != nil {
			return fmt.Errorf("PRESSFLEET_JOBS_CONCURRENCY must be a whole number, got %q", val)
		}
		cfg.Jobs.Concurrency = n
	}
	if val, ok := env("PRESSFLEET_AUDIT_RETENTION_DAYS"); ok {
		n, err := strconv.Atoi(val)
		if err != nil {
			return fmt.Errorf("PRESSFLEET_AUDIT_RETENTION_DAYS must be a whole number, got %q", val)
		}
		cfg.Audit.RetentionDays = n
	}
	if val, ok := env("PRESSFLEET_SMTP_PORT"); ok {
		n, err := strconv.Atoi(val)
		if err != nil {
			return fmt.Errorf("PRESSFLEET_SMTP_PORT must be a whole number, got %q", val)
		}
		cfg.SMTP.Port = n
	}
	if val, ok := env(envTrustedProxies); ok {
		cfg.HTTP.TrustedProxies = nil
		for p := range strings.SplitSeq(val, ",") {
			if p = strings.TrimSpace(p); p != "" {
				cfg.HTTP.TrustedProxies = append(cfg.HTTP.TrustedProxies, p)
			}
		}
	}
	return cfg.WordPressSecurity.applyEnv(env)
}

func loadFile(cfg *Config, path string) error {
	data, err := os.ReadFile(path) //nolint:gosec // G304: reading the operator-chosen config file is the point
	if err != nil {
		return fmt.Errorf("reading config: %w", err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(cfg); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("parsing config %s: %w", path, err)
	}
	return nil
}

// Validate reports every problem with the config, or nil if it is usable.
func (c Config) Validate() error {
	var errs []error
	if !filepath.IsAbs(c.DataDir) {
		errs = append(errs, fmt.Errorf("data_dir must be an absolute path, got %q", c.DataDir))
	}
	if c.Listen == "" {
		errs = append(errs, errors.New("listen is required"))
	}
	if _, err := logging.ParseLevel(c.Log.Level); err != nil {
		errs = append(errs, fmt.Errorf("log.level: %w", err))
	}
	if c.Log.Format != logging.FormatText && c.Log.Format != logging.FormatJSON {
		errs = append(errs, fmt.Errorf("log.format must be %q or %q, got %q", logging.FormatText, logging.FormatJSON, c.Log.Format))
	}
	for _, d := range []struct{ key, value string }{
		{"dashboard_domain", c.DashboardDomain},
		{"preview_base_domain", c.PreviewBaseDomain},
	} {
		if d.value != "" && !ValidDomain(d.value) {
			errs = append(errs, fmt.Errorf("%s must be a domain name like fleet.example.com, got %q", d.key, d.value))
		}
	}
	if c.PublicIP != "" {
		if _, err := netip.ParseAddr(c.PublicIP); err != nil {
			errs = append(errs, fmt.Errorf("public_ip must be an IP address, got %q", c.PublicIP))
		}
	}
	switch c.Database.Driver {
	case DriverSQLite:
		if c.Database.SQLitePath != "" && !filepath.IsAbs(c.Database.SQLitePath) {
			errs = append(errs, fmt.Errorf("database.sqlite_path must be an absolute path, got %q", c.Database.SQLitePath))
		}
	case DriverPostgres:
		if c.Database.PostgresDSN == "" {
			errs = append(errs, errors.New("database.postgres_dsn is required when database.driver is postgres"))
		}
	default:
		errs = append(errs, fmt.Errorf("database.driver must be %q or %q, got %q", DriverSQLite, DriverPostgres, c.Database.Driver))
	}
	if c.HTTP.SecureCookies != "" && c.HTTP.SecureCookies != SecureCookiesAuto && c.HTTP.SecureCookies != SecureCookiesAlways {
		errs = append(errs, fmt.Errorf("http.secure_cookies must be %q or %q, got %q", SecureCookiesAuto, SecureCookiesAlways, c.HTTP.SecureCookies))
	}
	if _, err := c.HTTP.TrustedProxyPrefixes(); err != nil {
		errs = append(errs, err)
	}
	if c.MasterKeyFile != "" && !filepath.IsAbs(c.MasterKeyFile) {
		errs = append(errs, fmt.Errorf("master_key_file must be an absolute path, got %q", c.MasterKeyFile))
	}
	if c.Jobs.Concurrency < 1 || c.Jobs.Concurrency > MaxJobsConcurrency {
		errs = append(errs, fmt.Errorf("jobs.concurrency must be between 1 and %d, got %d", MaxJobsConcurrency, c.Jobs.Concurrency))
	}
	if c.Audit.RetentionDays < 1 || c.Audit.RetentionDays > MaxAuditRetentionDays {
		errs = append(errs, fmt.Errorf("audit.retention_days must be between 1 and %d, got %d", MaxAuditRetentionDays, c.Audit.RetentionDays))
	}
	if host, port, err := net.SplitHostPort(c.MariaDB.Address); err != nil || host == "" || !validPort(port) {
		errs = append(errs, fmt.Errorf("mariadb.address must be host:port, like mariadb:3306, got %q", c.MariaDB.Address))
	}
	if c.MariaDB.AdminUser == "" || strings.ContainsAny(c.MariaDB.AdminUser, " \t\r\n") {
		errs = append(errs, fmt.Errorf("mariadb.admin_user must be a user name without spaces, got %q", c.MariaDB.AdminUser))
	}
	if !filepath.IsAbs(c.MariaDB.AdminPasswordFile) {
		errs = append(errs, fmt.Errorf("mariadb.admin_password_file must be an absolute path, got %q", c.MariaDB.AdminPasswordFile))
	}
	if !composeProjectName.MatchString(c.PlatformProject) {
		errs = append(errs, fmt.Errorf("platform_project must be a Compose project name (lowercase letters, digits, - and _), got %q", c.PlatformProject))
	}
	if err := c.SMTP.Validate(); err != nil {
		errs = append(errs, err)
	}
	if err := c.WordPressSecurity.validate(); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// composeProjectName matches the project names Docker Compose accepts.
var composeProjectName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

// validPort reports whether s is a TCP port number.
func validPort(s string) bool {
	n, err := strconv.Atoi(s)
	return err == nil && n > 0 && n <= 65535 && strconv.Itoa(n) == s
}

// ValidDomain reports whether s is a domain name with at least two labels.
// It rejects schemes, ports, paths, and wildcards, which are easy mistakes
// when pasting a URL or a DNS record.
func ValidDomain(s string) bool {
	s = strings.TrimSuffix(s, ".")
	if len(s) > 253 {
		return false
	}
	labels := strings.Split(s, ".")
	if len(labels) < 2 {
		return false
	}
	for _, l := range labels {
		if l == "" || len(l) > 63 || l[0] == '-' || l[len(l)-1] == '-' {
			return false
		}
		for _, r := range l {
			if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '-' {
				return false
			}
		}
	}
	return true
}

// SQLitePath returns the SQLite database file, applying the default.
func (c Config) SQLitePath() string {
	if c.Database.SQLitePath != "" {
		return c.Database.SQLitePath
	}
	return filepath.Join(c.Layout().StateDir(), "pressfleet.db")
}

// Layout returns the data directory layout.
func (c Config) Layout() Layout {
	return Layout{Root: c.DataDir}
}
