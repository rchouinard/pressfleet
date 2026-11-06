package kubernetes

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"text/template"

	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
	"github.com/rchouinard/pressfleet/templates"
)

// SiteManifestDir is the directory in a site's directory
// (<data_dir>/sites/<site-id>/) that holds its rendered manifests.
const SiteManifestDir = "kubernetes"

const (
	templateExt = ".yaml.tmpl"
	manifestExt = ".yaml"
)

// renderVersion is the part of the template fingerprint that the embedded
// templates don't cover: the Go code that renders a site's manifests.
//
// Bump it in any change that alters what RenderSite writes for an existing
// site with the same SiteConfig and install-wide configuration, so that the
// change reaches sites through the templates rollout, one site at a time
// with a health check, instead of all at once through the startup
// reconciliation (§4.7, §5.5). Changes to templates/k8s change the
// fingerprint by themselves, and changes that render nothing differently
// need no bump. It is the Compose driver's rule, with the Kubernetes
// driver's own counter.
const renderVersion = 1

// templateFingerprint hashes the embedded templates with renderVersion,
// once: they can't change while the process runs.
var templateFingerprint = sync.OnceValue(func() string {
	fp, err := provisioner.FingerprintTemplates(templates.Kubernetes(), renderVersion)
	if err != nil {
		// The templates are embedded in the binary: failing to read them
		// is a build defect.
		panic(err)
	}
	return fp
})

// siteTemplates parses the embedded templates once.
var siteTemplates = sync.OnceValues(func() (*template.Template, error) {
	return template.New("").Option("missingkey=error").Funcs(template.FuncMap{"q": quoteYAML}).
		ParseFS(templates.Kubernetes(), "*"+templateExt)
})

// manifestData is what the templates in templates/k8s see.
type manifestData struct {
	// SiteID and ClientGroupID identify the site, in labels.
	SiteID        string
	ClientGroupID string
	// Namespace is the site's namespace, which holds all its objects.
	Namespace string
	// PlatformNamespace holds the platform services.
	PlatformNamespace string
}

// RenderSite renders the site's manifests from templates/k8s into
// SiteManifestDir in the site's directory, one file per template, where
// operators can inspect them. A file whose template no longer exists is
// removed, and unchanged files aren't rewritten. Every rendered file must
// decode into objects Apply accepts. It returns the manifest directory,
// for ApplyDir.
func (p *Provisioner) RenderSite(cfg *provisioner.SiteConfig) (string, error) {
	if err := cfg.Validate(); err != nil {
		return "", err
	}
	if err := p.validSiteNamespace(cfg.ID); err != nil {
		return "", err
	}
	siteDir, err := p.layout.SiteDir(cfg.ID)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrInvalidManifest, err)
	}
	data := manifestData{
		SiteID:            cfg.ID,
		ClientGroupID:     cfg.ClientGroupID,
		Namespace:         p.SiteNamespace(cfg.ID),
		PlatformNamespace: p.platformNamespace,
	}
	dir := filepath.Join(siteDir, SiteManifestDir)
	if err := renderManifests(dir, data); err != nil {
		return "", fmt.Errorf("rendering site %s: %w", cfg.ID, err)
	}
	return dir, nil
}

// renderManifests executes every template into dir.
func renderManifests(dir string, data manifestData) error {
	tmpl, err := siteTemplates()
	if err != nil {
		return fmt.Errorf("parsing site templates: %w", err)
	}
	rendered := map[string][]byte{}
	for _, t := range tmpl.Templates() {
		name, ok := strings.CutSuffix(t.Name(), templateExt)
		if !ok {
			continue // the unnamed root template
		}
		var buf bytes.Buffer
		if err := t.Execute(&buf, data); err != nil {
			return fmt.Errorf("rendering %s: %w", t.Name(), err)
		}
		if _, err := DecodeManifests(buf.Bytes()); err != nil {
			return fmt.Errorf("rendering %s: %w", t.Name(), err)
		}
		rendered[name+manifestExt] = buf.Bytes()
	}
	// Only the owner: later manifests hold the site's Secrets.
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	existing, err := filepath.Glob(filepath.Join(dir, "*"+manifestExt))
	if err != nil {
		return err
	}
	for _, f := range existing {
		if _, ok := rendered[filepath.Base(f)]; !ok {
			if err := os.Remove(f); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return err
			}
		}
	}
	for name, content := range rendered {
		if err := writeIfChanged(filepath.Join(dir, name), content); err != nil {
			return err
		}
	}
	return nil
}

// writeIfChanged makes path hold content, mode 0600, through a temporary
// file renamed over it, so a reader sees the old or the new file, never a
// mix. Unchanged content isn't rewritten.
func writeIfChanged(path string, content []byte) error {
	if old, err := os.ReadFile(path); err == nil && bytes.Equal(old, content) { //nolint:gosec // G304: a manifest in the site's directory
		return os.Chmod(path, 0o600)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+"-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(content); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	// CreateTemp makes the file 0600 already.
	return os.Rename(tmp.Name(), path)
}

// quoteYAML writes v as a JSON string, which YAML reads as the same string
// whatever it holds.
func quoteYAML(v any) (string, error) {
	b, err := json.Marshal(fmt.Sprint(v))
	return string(b), err
}
