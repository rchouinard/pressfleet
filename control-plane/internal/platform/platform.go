// Package platform renders the platform services Compose project: the
// control plane and MariaDB now, Traefik, Redis, and VictoriaMetrics in later
// milestones (design doc §4.1).
//
// The host CLI renders the project from templates embedded in its binary
// (package templates) into <data_dir>/platform/. Rendered files are never
// edited by hand; re-rendering overwrites them (§4.7).
package platform

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"text/template"

	"go.yaml.in/yaml/v3"

	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner/compose"
	"github.com/rchouinard/pressfleet/control-plane/internal/redact"
	"github.com/rchouinard/pressfleet/templates"
)

// ProjectName is the Compose project name of the platform services.
const ProjectName = "pressfleet"

// Memory limits from the §4.1 capacity budget. The control plane shares its
// 256 MiB budget line with Traefik, which gets the remaining 64 MiB.
const (
	DefaultMariaDBMemoryMB      = 768
	DefaultControlPlaneMemoryMB = 192
)

// DefaultListen is the control plane's listen address inside its container.
const DefaultListen = ":8080"

// ErrInvalidConfig is returned by Render for a Config that can't be rendered.
var ErrInvalidConfig = errors.New("platform: invalid config")

// Images are the image references of the platform services.
type Images struct {
	ControlPlane string
	MariaDB      string
}

// DefaultImages returns the images pinned for this release: third-party
// images by digest from the embedded images.yaml, and the control plane
// image tagged with version, the host CLI's own release version.
func DefaultImages(version string) (Images, error) {
	data, err := fs.ReadFile(templates.Platform(), "images.yaml")
	if err != nil {
		return Images{}, fmt.Errorf("reading platform images: %w", err)
	}
	var list struct {
		ControlPlane string `yaml:"control-plane"`
		MariaDB      string `yaml:"mariadb"`
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&list); err != nil {
		return Images{}, fmt.Errorf("parsing platform images: %w", err)
	}
	if version == "" {
		return Images{}, fmt.Errorf("%w: control plane version is required", ErrInvalidConfig)
	}
	return Images{ControlPlane: list.ControlPlane + ":" + version, MariaDB: list.MariaDB}, nil
}

// Config is everything needed to render the platform project.
type Config struct {
	// ProjectName is the Compose project name; empty means ProjectName.
	// Tests use a unique name so they don't touch a real installation.
	ProjectName string
	// DataDir is the host's absolute data directory. It is mounted into the
	// control plane at the identical path (see compose.yaml.tmpl for why).
	DataDir string
	// Listen is the control plane listen address; empty means DefaultListen.
	Listen string
	// Images are the service images, normally from DefaultImages.
	Images Images
	// Memory limits in MiB; zero means the §4.1 default.
	ControlPlaneMemoryMB int
	MariaDBMemoryMB      int
	// MariaDBRootPassword is written to secrets/mariadb_root_password.
	MariaDBRootPassword redact.Secret
	// MasterKeyFile is the host path of the platform master key file. It is
	// given to the control plane as a Compose secret and must be outside
	// DataDir, which is backed up (§4.1). The key itself never passes
	// through the rendered project.
	MasterKeyFile string
	// SkipPreflight starts the control plane even if required host checks
	// fail. Only for development machines and CI; never for real installs.
	SkipPreflight bool
}

// withDefaults fills zero fields and validates the result.
func (c Config) withDefaults() (Config, error) {
	if c.ProjectName == "" {
		c.ProjectName = ProjectName
	}
	if c.Listen == "" {
		c.Listen = DefaultListen
	}
	if c.ControlPlaneMemoryMB == 0 {
		c.ControlPlaneMemoryMB = DefaultControlPlaneMemoryMB
	}
	if c.MariaDBMemoryMB == 0 {
		c.MariaDBMemoryMB = DefaultMariaDBMemoryMB
	}

	var errs []error
	if !filepath.IsAbs(c.DataDir) || filepath.Clean(c.DataDir) != c.DataDir {
		errs = append(errs, fmt.Errorf("data dir must be a clean absolute path, got %q", c.DataDir))
	}
	if c.Images.ControlPlane == "" || c.Images.MariaDB == "" {
		errs = append(errs, errors.New("control plane and MariaDB images are required"))
	}
	if c.ControlPlaneMemoryMB < 64 {
		errs = append(errs, fmt.Errorf("control plane memory must be at least 64 MiB, got %d", c.ControlPlaneMemoryMB))
	}
	if c.MariaDBMemoryMB < 512 {
		errs = append(errs, fmt.Errorf("MariaDB memory must be at least 512 MiB, got %d", c.MariaDBMemoryMB))
	}
	if !filepath.IsAbs(c.MasterKeyFile) || filepath.Clean(c.MasterKeyFile) != c.MasterKeyFile {
		errs = append(errs, fmt.Errorf("master key file must be a clean absolute path, got %q", c.MasterKeyFile))
	} else if rel, err := filepath.Rel(c.DataDir, c.MasterKeyFile); err == nil && filepath.IsLocal(rel) {
		errs = append(errs, fmt.Errorf("master key file %s must not be inside the data directory", c.MasterKeyFile))
	}
	if c.MariaDBRootPassword.Reveal() == "" {
		errs = append(errs, errors.New("MariaDB root password is required"))
	}
	if strings.ContainsAny(c.MariaDBRootPassword.Reveal(), "\r\n") {
		// The MariaDB entrypoint reads the secret file as a single line.
		errs = append(errs, errors.New("MariaDB root password must not contain line breaks"))
	}
	if err := errors.Join(errs...); err != nil {
		return c, fmt.Errorf("%w: %w", ErrInvalidConfig, err)
	}
	return c, nil
}

// templateData is what the templates see. It deliberately has no secrets:
// credentials go only into files under secrets/.
type templateData struct {
	ProjectName          string
	DataDir              string
	Listen               string
	Images               Images
	ControlPlaneMemoryMB int
	MariaDBMemoryMB      int
	MariaDBBufferPoolMB  int
	MasterKeyFile        string
	MasterKeySecretPath  string
	SkipPreflight        bool
}

// output is one rendered file, relative to the project directory.
type output struct {
	path     string
	template string // empty for secrets
	secret   redact.Secret
}

// MasterKeySecretPath is where the control plane container sees the master
// key file.
const MasterKeySecretPath = "/run/secrets/master_key" //nolint:gosec // G101: a path, not a credential

// Files rendered by Render, relative to the project directory.
const (
	ComposeFile               = "compose.yaml"
	MariaDBConfigFile         = "mariadb/pressfleet.cnf"
	MariaDBRootPasswordSecret = "secrets/mariadb_root_password"
)

// Render writes the platform Compose project into dir, normally
// config.Layout.PlatformDir(). It is safe to run again: every file is
// replaced atomically. Secret files get mode 0600; other files 0644 because
// the MariaDB server must be able to read its config file.
func Render(dir string, cfg Config) error {
	cfg, err := cfg.withDefaults()
	if err != nil {
		return err
	}
	data := templateData{
		ProjectName:          cfg.ProjectName,
		DataDir:              cfg.DataDir,
		Listen:               cfg.Listen,
		Images:               cfg.Images,
		ControlPlaneMemoryMB: cfg.ControlPlaneMemoryMB,
		MariaDBMemoryMB:      cfg.MariaDBMemoryMB,
		MariaDBBufferPoolMB:  bufferPoolMB(cfg.MariaDBMemoryMB),
		MasterKeyFile:        cfg.MasterKeyFile,
		MasterKeySecretPath:  MasterKeySecretPath,
		SkipPreflight:        cfg.SkipPreflight,
	}
	tmpl, err := template.New("").Option("missingkey=error").Funcs(template.FuncMap{"q": quote}).
		ParseFS(templates.Platform(), "*.tmpl")
	if err != nil {
		return fmt.Errorf("parsing platform templates: %w", err)
	}

	outputs := []output{
		{path: ComposeFile, template: "compose.yaml.tmpl"},
		{path: MariaDBConfigFile, template: "mariadb.cnf.tmpl"},
		{path: MariaDBRootPasswordSecret, secret: cfg.MariaDBRootPassword},
	}
	for _, o := range outputs {
		var content []byte
		mode := os.FileMode(0o644)
		if o.template != "" {
			var buf bytes.Buffer
			if err := tmpl.ExecuteTemplate(&buf, o.template, data); err != nil {
				return fmt.Errorf("rendering %s: %w", o.path, err)
			}
			content = buf.Bytes()
		} else {
			content = []byte(o.secret.Reveal())
			mode = 0o600
		}
		if err := writeFile(filepath.Join(dir, o.path), content, mode); err != nil {
			return err
		}
	}
	return nil
}

// MariaDBContainer is the name of the MariaDB container of the platform
// project called projectName (normally ProjectName). The Compose provisioner
// connects this container to each site network (§4.1 Networking). Compose v2
// names containers <project>-<service>-<index>, and MariaDB has one replica.
func MariaDBContainer(projectName string) string {
	return projectName + "-mariadb-1"
}

// Project returns the Compose project for a rendered platform directory.
func Project(dir string) compose.Project {
	return compose.Project{Name: ProjectName, Dir: dir}
}

// bufferPoolMB gives InnoDB half of MariaDB's memory, rounded down to the
// 128 MiB chunk size that MariaDB would otherwise round to itself.
func bufferPoolMB(memoryMB int) int {
	return max(128, memoryMB/2/128*128)
}

// quote renders s as a double-quoted YAML scalar. JSON string syntax is valid
// YAML; '$' is doubled so Compose doesn't treat it as interpolation.
func quote(s string) (string, error) {
	b, err := json.Marshal(s)
	if err != nil {
		return "", err
	}
	return strings.ReplaceAll(string(b), "$", "$$"), nil
}

// writeFile atomically replaces path with content. Writing to a temporary
// file first also means a pre-existing file with looser permissions never
// briefly holds a secret.
func writeFile(path string, content []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("creating %s: %w", dir, err)
	}
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	tmp := f.Name()
	defer func() { _ = os.Remove(tmp) }() // no-op after a successful rename

	// CreateTemp uses 0600; Chmod sets the final mode regardless of umask.
	if err := f.Chmod(mode); err != nil {
		_ = f.Close()
		return fmt.Errorf("writing %s: %w", path, err)
	}
	if _, err := f.Write(content); err != nil {
		_ = f.Close()
		return fmt.Errorf("writing %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return nil
}
