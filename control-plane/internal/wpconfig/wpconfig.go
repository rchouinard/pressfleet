// Package wpconfig generates a site's wp-config.php from its record (design
// doc §5.5): database credentials, keys and salts, the table prefix, the
// constants the site images require (images/README.md, "wp-config.php
// requirements"), and agency constants from the site's configuration.
//
// The file is generated, never hand-edited, and mounted read-only into the
// php container. Generation is orchestrator-agnostic: the Compose driver
// bind-mounts the rendered file, and a Kubernetes driver would put the same
// bytes in a Secret.
//
// # Escaping
//
// Every value is emitted as a PHP literal, never interpolated into code:
// strings as single-quoted literals in which only backslash and the single
// quote are special (both are escaped), booleans and integers as bare
// literals. Constant names are validated against PHP's identifier syntax.
// wp-config.php is an injection boundary: a value that could close its
// string would run as PHP with the site's database credentials.
package wpconfig

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"

	"github.com/rchouinard/pressfleet/control-plane/internal/redact"
	"github.com/rchouinard/pressfleet/control-plane/internal/sitedb"
)

// ErrInvalidParams is returned by Render for parameters that can't be
// rendered. Error messages name the offending field but never its value.
var ErrInvalidParams = errors.New("wpconfig: invalid parameters")

// DefaultTablePrefix is used when Params.TablePrefix is empty.
const DefaultTablePrefix = "wp_"

// Paths inside the site containers (images/README.md, "Filesystem").
const (
	MUPluginDir      = "/opt/pressfleet/mu-plugins"
	MUPluginURL      = "/_pressfleet/mu-plugins"
	FastCGICachePath = "/var/cache/pressfleet/fastcgi"
)

const (
	defaultDBCharset  = "utf8mb4"
	maxConstantLength = 128
)

// Params is everything wp-config.php is generated from.
type Params struct {
	// DB holds the site's database name, user, and password, as stored by
	// the DatabaseManager (sitedb.UnmarshalCredentials).
	DB sitedb.Credentials
	// DBHost is the MariaDB server as the php container reaches it,
	// "host" or "host:port".
	DBHost string
	// TablePrefix is the WordPress table prefix; empty means
	// DefaultTablePrefix. Restored or imported sites may use another one.
	TablePrefix string
	// Salts are the site's keys and salts (GenerateSalts, UnmarshalSalts).
	Salts Salts
	// Redis configures the Redis Object Cache plugin. Nil (the default until
	// M2) emits nothing.
	Redis *Redis
	// SMTP configures outbound mail for the platform mu-plugin. Nil (the
	// default until M2) emits nothing.
	SMTP *SMTP
	// Constants are agency-specific constants from the site's configuration,
	// defined in this order after the platform's.
	Constants []Constant
}

// Redis is a site's connection to the shared Redis server (§6): an ACL user
// restricted to the site's key prefix.
type Redis struct {
	Host     string
	Port     int
	Username string
	Password redact.Secret
	// Prefix is prepended to every key (WP_REDIS_PREFIX); it must match the
	// ACL user's key pattern.
	Prefix string
}

// SMTP is the relay the site sends mail through (§5.6). The platform
// mu-plugin reads these constants and configures PHPMailer.
type SMTP struct {
	Host     string
	Port     int
	Username string
	Password redact.Secret
	// Encryption is "tls" (STARTTLS), "ssl" (implicit TLS), or "" (none).
	Encryption string
	FromEmail  string
	FromName   string
}

// Constant is an agency-specific constant. Value must be a string,
// redact.Secret, bool, or int; secrets are rendered like strings.
type Constant struct {
	Name  string
	Value any
}

var (
	// constantName is PHP's identifier syntax, restricted to ASCII.
	constantName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	// tablePrefix is what WordPress itself accepts (wpdb::set_prefix).
	tablePrefix = regexp.MustCompile(`^[A-Za-z0-9_]+$`)
)

// reservedConstants are set by the platform, or must not be set because they
// would break the image's layout or the go-live workflow. Agency constants
// can't use them. Names are compared case-insensitively so a look-alike such
// as "db_password" is rejected too.
var reservedConstants = map[string]bool{
	"ABSPATH": true, "WPMU_PLUGIN_DIR": true, "WPMU_PLUGIN_URL": true, "FS_METHOD": true,
	"WP_AUTO_UPDATE_CORE": true, "DISABLE_WP_CRON": true, "RT_WP_NGINX_HELPER_CACHE_PATH": true,
	// Go-live changes the URLs in the database (§5.2); constants would pin them.
	"WP_HOME": true, "WP_SITEURL": true,
	// wp-content is the site volume at the image's fixed path.
	"WP_CONTENT_DIR": true, "WP_PLUGIN_DIR": true,
}

// reservedPrefixes cover the database settings, the Redis Object Cache
// plugin's settings, and the platform's own constants.
var reservedPrefixes = []string{"DB_", "WP_REDIS_", "PRESSFLEET_"}

func init() {
	for _, f := range (&Salts{}).fields() {
		reservedConstants[f.name] = true
	}
}

// Render generates wp-config.php. The output contains the database password
// and salts: write it only to files readable by the site, and never log it.
func Render(p Params) ([]byte, error) {
	if p.TablePrefix == "" {
		p.TablePrefix = DefaultTablePrefix
	}
	if err := p.validate(); err != nil {
		return nil, err
	}

	var w writer
	w.line("<?php")
	w.line("/**")
	w.line(" * WordPress configuration generated by PressFleet (design doc §5.5).")
	w.line(" *")
	w.line(" * Do not edit. The control plane regenerates this file from the site's")
	w.line(" * record and mounts it read-only; set agency constants through the site's")
	w.line(" * configuration in PressFleet instead.")
	w.line(" */")
	w.line("")

	w.line("// ** Database ** //")
	w.define("DB_NAME", p.DB.Database)
	w.define("DB_USER", p.DB.Username)
	w.define("DB_PASSWORD", p.DB.Password)
	w.define("DB_HOST", p.DBHost)
	w.define("DB_CHARSET", defaultDBCharset)
	w.define("DB_COLLATE", "")
	w.line("")

	w.line("// ** Authentication keys and salts ** //")
	for _, f := range p.Salts.fields() {
		w.define(f.name, *f.value)
	}
	w.line("")

	w.line("$table_prefix = " + literal(p.TablePrefix) + ";")
	w.line("")

	w.line("// ** Platform ** //")
	w.line("// Must-use plugins ship in the site image, outside wp-content, so they are")
	w.line("// versioned with the image and can't be removed from within WordPress.")
	w.define("WPMU_PLUGIN_DIR", MUPluginDir)
	w.line("// Root-relative, so it stays valid when the site's domain changes.")
	w.define("WPMU_PLUGIN_URL", MUPluginURL)
	w.line("// Core is root-owned and read-only; without this WordPress's ownership")
	w.line("// check would ask for FTP credentials to update plugins and themes.")
	w.define("FS_METHOD", "direct")
	w.line("// Core updates ship as new site images (§5.5).")
	w.define("WP_AUTO_UPDATE_CORE", false)
	w.line("// The control plane's scheduler runs WP-Cron (§5.6).")
	w.define("DISABLE_WP_CRON", true)
	w.line("// Nginx Helper purges the FastCGI cache in this volume, shared with nginx.")
	w.define("RT_WP_NGINX_HELPER_CACHE_PATH", FastCGICachePath)
	w.line("// HTTPS behind the reverse proxy needs no code here: nginx sets HTTPS=on")
	w.line("// for requests a trusted proxy forwarded over HTTPS. WP_HOME and")
	w.line("// WP_SITEURL are deliberately not defined; go-live changes the site's URL")
	w.line("// in the database (§5.2).")
	w.line("")

	if r := p.Redis; r != nil {
		w.line("// ** Object cache (Redis Object Cache plugin) ** //")
		w.define("WP_REDIS_HOST", r.Host)
		w.define("WP_REDIS_PORT", r.Port)
		w.line("// ACL user and password (§6).")
		w.line("define( 'WP_REDIS_PASSWORD', array( " + literal(r.Username) + ", " + literal(r.Password.Reveal()) + " ) );")
		w.define("WP_REDIS_PREFIX", r.Prefix)
		w.line("// The ACL denies FLUSHDB; flush only this site's keys.")
		w.define("WP_REDIS_SELECTIVE_FLUSH", true)
		w.line("")
	}

	if m := p.SMTP; m != nil {
		w.line("// ** Outbound email ** //")
		w.define("PRESSFLEET_SMTP_HOST", m.Host)
		w.define("PRESSFLEET_SMTP_PORT", m.Port)
		w.define("PRESSFLEET_SMTP_USERNAME", m.Username)
		w.define("PRESSFLEET_SMTP_PASSWORD", m.Password)
		w.define("PRESSFLEET_SMTP_ENCRYPTION", m.Encryption)
		w.define("PRESSFLEET_SMTP_FROM_EMAIL", m.FromEmail)
		w.define("PRESSFLEET_SMTP_FROM_NAME", m.FromName)
		w.line("")
	}

	if len(p.Constants) > 0 {
		w.line("// ** Site constants ** //")
		for _, c := range p.Constants {
			w.define(c.Name, c.Value)
		}
		w.line("")
	}

	w.line("if ( ! defined( 'ABSPATH' ) ) {")
	w.line("\tdefine( 'ABSPATH', __DIR__ . '/' );")
	w.line("}")
	w.line("require_once ABSPATH . 'wp-settings.php';")
	return w.buf.Bytes(), nil
}

func (p *Params) validate() error {
	var errs []error
	for _, f := range []struct{ name, value string }{
		{"database name", p.DB.Database},
		{"database user", p.DB.Username},
		{"database password", p.DB.Password.Reveal()},
		{"database host", p.DBHost},
	} {
		if f.value == "" {
			errs = append(errs, fmt.Errorf("%s is required", f.name))
		}
	}
	if !tablePrefix.MatchString(p.TablePrefix) {
		errs = append(errs, errors.New("table prefix must contain only letters, digits, and underscores"))
	}
	if err := p.Salts.validate(); err != nil {
		errs = append(errs, err)
	}
	if r := p.Redis; r != nil {
		if r.Host == "" || r.Username == "" || r.Password.Reveal() == "" || r.Prefix == "" {
			errs = append(errs, errors.New("redis: host, username, password, and prefix are required"))
		}
		if r.Port < 1 || r.Port > 65535 {
			errs = append(errs, errors.New("redis: port must be 1-65535"))
		}
	}
	if m := p.SMTP; m != nil {
		if m.Host == "" {
			errs = append(errs, errors.New("smtp: host is required"))
		}
		if m.Port < 1 || m.Port > 65535 {
			errs = append(errs, errors.New("smtp: port must be 1-65535"))
		}
		switch m.Encryption {
		case "", "tls", "ssl":
		default:
			errs = append(errs, errors.New(`smtp: encryption must be "tls", "ssl", or empty`))
		}
	}
	seen := map[string]bool{}
	for i, c := range p.Constants {
		if err := validateConstant(c); err != nil {
			errs = append(errs, fmt.Errorf("constant %d: %w", i+1, err))
			continue
		}
		upper := strings.ToUpper(c.Name)
		if seen[upper] {
			errs = append(errs, fmt.Errorf("constant %s is defined twice", c.Name))
		}
		seen[upper] = true
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidParams, err)
	}
	return nil
}

func validateConstant(c Constant) error {
	if len(c.Name) > maxConstantLength || !constantName.MatchString(c.Name) {
		// The name is not quoted back: it could be anything.
		return errors.New("name must be a PHP identifier of letters, digits, and underscores")
	}
	upper := strings.ToUpper(c.Name)
	if reservedConstants[upper] {
		return fmt.Errorf("%s is set by the platform", c.Name)
	}
	for _, prefix := range reservedPrefixes {
		if strings.HasPrefix(upper, prefix) {
			return fmt.Errorf("%s: names starting with %s are reserved for the platform", c.Name, prefix)
		}
	}
	switch v := c.Value.(type) {
	case string, redact.Secret, bool:
	case int:
		if int64(v) == math.MinInt64 {
			// PHP parses the literal's magnitude as a float before negating it.
			return fmt.Errorf("%s: integer out of range", c.Name)
		}
	default:
		return fmt.Errorf("%s: value must be a string, bool, or int, got %T", c.Name, c.Value)
	}
	return nil
}

// writer accumulates the file.
type writer struct {
	buf bytes.Buffer
}

func (w *writer) line(s string) {
	w.buf.WriteString(s)
	w.buf.WriteByte('\n')
}

// define emits define( 'NAME', value );. name is a validated identifier or
// one of the platform's own constants, so it needs no escaping, but it goes
// through literal anyway.
func (w *writer) define(name string, value any) {
	w.line("define( " + literal(name) + ", " + phpValue(value) + " );")
}

// phpValue renders a value validated by validateConstant (or one of the
// platform's own) as a PHP literal.
func phpValue(v any) string {
	switch v := v.(type) {
	case string:
		return literal(v)
	case redact.Secret:
		return literal(v.Reveal())
	case bool:
		if v {
			return "true"
		}
		return "false"
	case int:
		return strconv.Itoa(v)
	default:
		// Unreachable: validate rejects other types first.
		panic(fmt.Sprintf("wpconfig: unsupported value type %T", v))
	}
}

// literal renders s as a PHP single-quoted string. Inside single quotes PHP
// only interprets \\ and \' (a backslash before any other character is
// literal), so escaping exactly those two makes every byte sequence,
// including newlines, "?>", and invalid UTF-8, round-trip unchanged.
func literal(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 2)
	b.WriteByte('\'')
	for i := range len(s) {
		if c := s[i]; c == '\\' || c == '\'' {
			b.WriteByte('\\')
		}
		b.WriteByte(s[i])
	}
	b.WriteByte('\'')
	return b.String()
}
