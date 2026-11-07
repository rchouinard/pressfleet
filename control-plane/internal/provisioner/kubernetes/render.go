package kubernetes

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"text/template"

	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
	"github.com/rchouinard/pressfleet/control-plane/internal/wpconfig"
	"github.com/rchouinard/pressfleet/templates"
)

// SiteManifestDir is the directory in a site's directory
// (<data_dir>/sites/<site-id>/) that holds its rendered manifests.
const SiteManifestDir = "kubernetes"

const (
	templateExt = ".yaml.tmpl"
	manifestExt = ".yaml"
)

// Names the site manifests share with the rest of the install.
const (
	// RenderedLabel marks every object rendered from templates/k8s, with
	// the value "true". The driver deletes the objects with it that no
	// template renders any more, since server-side apply doesn't; objects
	// without it are left alone.
	RenderedLabel = "pressfleet.io/rendered"
	// ComponentLabel is the label of the site's pod, with the value
	// SiteComponent: the Deployment's selector, and what the site's
	// Service and NetworkPolicies select.
	ComponentLabel = "pressfleet.io/component"
	SiteComponent  = "site"
	// SiteDeployment is the name of the site's Deployment.
	SiteDeployment = "site"
	// ControlPlaneServiceAccount is the control plane's ServiceAccount in
	// the platform namespace, as the Helm chart names it. Each site
	// namespace binds SiteClusterRole to it.
	ControlPlaneServiceAccount = "control-plane"
)

// SiteClusterRole is the ClusterRole that the Helm chart defines for what
// the control plane may do in a site's namespace: pressfleet:<platform
// namespace>:site. The chart's admission policy lets the control plane bind
// no other, and only in namespaces labeled as PressFleet's sites.
func SiteClusterRole(platformNamespace string) string {
	return "pressfleet:" + platformNamespace + ":site"
}

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

// siteTemplates parses the embedded templates once: the manifests
// (*.yaml.tmpl) and the pieces they share (helpers.tmpl).
var siteTemplates = sync.OnceValues(func() (*template.Template, error) {
	return template.New("").Option("missingkey=error").Funcs(template.FuncMap{"q": quoteYAML}).
		ParseFS(templates.Kubernetes(), "*.tmpl")
})

// manifestData is what the templates in templates/k8s see.
type manifestData struct {
	// SiteID and ClientGroupID identify the site, in labels.
	SiteID        string
	ClientGroupID string
	// Namespace is the site's namespace, which holds all its objects.
	Namespace string
	// PlatformNamespace holds the platform services and the control
	// plane's ServiceAccount.
	PlatformNamespace          string
	ControlPlaneServiceAccount string
	SiteClusterRole            string
	// UID is the site's UID and GID.
	UID    int
	Images siteImages
	// PHP and Nginx are the containers' requests and limits.
	PHP, Nginx     containerResources
	PHPMemoryLimit string
	PHPMaxChildren int
	EnableCache    bool
	NginxSecurity  provisioner.NginxSecurity
	// TrustedProxies is NGINX_TRUSTED_PROXIES: the pod network's ranges.
	TrustedProxies string
	// WPConfig is wp-config.php in base64, and WPConfigChecksum its
	// SHA-256, which rolls the pod when it changes.
	WPConfig         string
	WPConfigChecksum string
	ImagePullSecrets []string
	// StorageClass is the wp-content volume's class, empty for the
	// cluster's default, and VolumeSize its size.
	StorageClass string
	VolumeSize   string
	Egress       egress
	// Routing and Maintenance are the site's routes (routing.go).
	Routing     routingData
	Maintenance maintenanceData
}

// siteImages are a site's container images.
type siteImages struct {
	PHP   string
	Nginx string
}

// containerResources are one container's requests and limits, as
// Kubernetes quantities.
type containerResources struct {
	CPURequest, CPULimit       string
	MemoryRequest, MemoryLimit string
}

// cpuRequestShare is the share of a container's CPU limit that it
// requests. Compose reserves no CPU, so sites share the host's CPUs and
// burst into idle ones; Kubernetes would make a missing request equal to
// the limit and schedule every site as if it always used its whole limit.
// A tenth of it is about what an idle site uses, with room to spare.
const cpuRequestShare = 10

// minCPURequestMillis is the smallest CPU request of a container.
const minCPURequestMillis = 10

// resources turns one container's share of a site's resources into
// requests and limits. A site's memory reservation and limit become the
// memory request and limit (§4.1).
func resources(c provisioner.ContainerResources) (containerResources, error) {
	cpu, err := provisioner.ParseCPUMillis(c.CPUs)
	if err != nil {
		return containerResources{}, err
	}
	return containerResources{
		CPURequest:    strconv.Itoa(max(cpu/cpuRequestShare, minCPURequestMillis)) + "m",
		CPULimit:      strconv.Itoa(cpu) + "m",
		MemoryRequest: strconv.Itoa(c.MemoryReservationMB) + "Mi",
		MemoryLimit:   strconv.Itoa(c.MemoryLimitMB) + "Mi",
	}, nil
}

// siteInputs is what rendering needs beyond the SiteConfig: what the
// driver resolved from the site's references.
type siteInputs struct {
	images   siteImages
	wpConfig []byte
}

// RenderSite resolves the site's images and secrets and renders its
// manifests from templates/k8s into SiteManifestDir in the site's
// directory, one file per template, where operators can inspect them. A
// file whose template no longer exists is removed, and unchanged files
// aren't rewritten. Every rendered object must be one Apply accepts, in the
// site's namespace (the namespace itself aside), and labeled RenderedLabel.
// It returns the manifest directory.
func (p *Provisioner) RenderSite(ctx context.Context, cfg *provisioner.SiteConfig) (string, error) {
	if cfg == nil {
		return "", fmt.Errorf("%w: site config is required", ErrInvalidSite)
	}
	if err := cfg.Validate(); err != nil {
		return "", fmt.Errorf("%w: %w", ErrInvalidSite, err)
	}
	if err := p.validSiteNamespace(cfg.ID); err != nil {
		return "", err
	}
	siteDir, err := p.layout.SiteDir(cfg.ID)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrInvalidSite, err)
	}
	in, err := p.resolve(ctx, cfg)
	if err != nil {
		return "", fmt.Errorf("site %s: %w", cfg.ID, err)
	}
	data, err := p.manifestData(cfg, in)
	if err != nil {
		return "", fmt.Errorf("site %s: %w", cfg.ID, err)
	}
	dir := filepath.Join(siteDir, SiteManifestDir)
	if err := renderManifests(dir, data); err != nil {
		return "", fmt.Errorf("rendering site %s: %w", cfg.ID, err)
	}
	if err := renderMaintenance(siteDir, data); err != nil {
		return "", fmt.Errorf("rendering site %s: %w", cfg.ID, err)
	}
	return dir, nil
}

// manifestData assembles what the templates see.
func (p *Provisioner) manifestData(cfg *provisioner.SiteConfig, in siteInputs) (manifestData, error) {
	split, err := provisioner.SplitResources(cfg.Resources)
	if err != nil {
		return manifestData{}, fmt.Errorf("%w: %w", ErrInvalidSite, err)
	}
	php, err := resources(split.PHP)
	if err != nil {
		return manifestData{}, fmt.Errorf("%w: %w", ErrInvalidSite, err)
	}
	nginx, err := resources(split.Nginx)
	if err != nil {
		return manifestData{}, fmt.Errorf("%w: %w", ErrInvalidSite, err)
	}
	security, err := provisioner.NginxSecuritySettings(p.security)
	if err != nil {
		return manifestData{}, fmt.Errorf("%w: %w", ErrInvalidSite, err)
	}
	if len(p.podCIDRs) == 0 {
		// Config.Validate requires them with this driver; without them
		// nginx would trust its own link only, and WordPress would see
		// every request as plain HTTP from Traefik.
		return manifestData{}, fmt.Errorf("%w: kubernetes.pod_cidrs is not set", ErrInvalidSite)
	}
	routing, maintenance, err := siteRouting(cfg, p.security.Headers)
	if err != nil {
		return manifestData{}, err
	}
	sum := sha256.Sum256(in.wpConfig)
	return manifestData{
		SiteID:                     cfg.ID,
		ClientGroupID:              cfg.ClientGroupID,
		Namespace:                  p.SiteNamespace(cfg.ID),
		PlatformNamespace:          p.platformNamespace,
		ControlPlaneServiceAccount: ControlPlaneServiceAccount,
		SiteClusterRole:            SiteClusterRole(p.platformNamespace),
		UID:                        cfg.UID,
		Images:                     in.images,
		PHP:                        php,
		Nginx:                      nginx,
		PHPMemoryLimit:             strconv.Itoa(split.PHPMemoryLimitMB) + "M",
		PHPMaxChildren:             split.PHPMaxChildren,
		EnableCache:                cfg.EnableCache,
		NginxSecurity:              security,
		TrustedProxies:             strings.Join(p.podCIDRs, " "),
		WPConfig:                   base64.StdEncoding.EncodeToString(in.wpConfig),
		WPConfigChecksum:           hex.EncodeToString(sum[:]),
		ImagePullSecrets:           p.pullSecrets,
		StorageClass:               p.storageClass,
		VolumeSize:                 p.volumeSize,
		Egress:                     p.egress,
		Routing:                    routing,
		Maintenance:                maintenance,
	}, nil
}

// renderManifests executes every manifest template into dir.
func renderManifests(dir string, data manifestData) error {
	tmpl, err := siteTemplates()
	if err != nil {
		return fmt.Errorf("parsing site templates: %w", err)
	}
	rendered := map[string][]byte{}
	for _, t := range tmpl.Templates() {
		name, ok := strings.CutSuffix(t.Name(), templateExt)
		if !ok {
			continue // the root, helpers.tmpl, and the templates it defines
		}
		var buf bytes.Buffer
		if err := t.Execute(&buf, data); err != nil {
			return fmt.Errorf("rendering %s: %w", t.Name(), err)
		}
		objs, err := DecodeManifests(buf.Bytes())
		if err != nil {
			return fmt.Errorf("rendering %s: %w", t.Name(), err)
		}
		for _, obj := range objs {
			if err := checkRendered(obj, data.Namespace); err != nil {
				return fmt.Errorf("rendering %s: %w", t.Name(), err)
			}
		}
		rendered[name+manifestExt] = buf.Bytes()
	}
	// Only the owner: the manifests hold the site's wp-config.php.
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return syncManifests(dir, rendered)
}

// checkRendered holds the templates to what the driver relies on: an
// object it can prune (RenderedLabel), in the site's namespace, so deleting
// the namespace deletes it.
func checkRendered(obj interface {
	GetKind() string
	GetName() string
	GetNamespace() string
	GetLabels() map[string]string
}, namespace string) error {
	if obj.GetLabels()[RenderedLabel] != "true" {
		return fmt.Errorf("%w: %s %s isn't labeled %s", ErrInvalidManifest, obj.GetKind(), obj.GetName(), RenderedLabel)
	}
	if ns := obj.GetNamespace(); (ns != "" && ns != namespace) || (ns == "" && obj.GetName() != namespace) {
		return fmt.Errorf("%w: %s %s is outside the site's namespace %s", ErrInvalidManifest, obj.GetKind(), obj.GetName(), namespace)
	}
	return nil
}

// syncManifests makes the manifest files (*.yaml) of dir those of files:
// others are removed, and unchanged ones aren't rewritten.
func syncManifests(dir string, files map[string][]byte) error {
	existing, err := filepath.Glob(filepath.Join(dir, "*"+manifestExt))
	if err != nil {
		return err
	}
	for _, f := range existing {
		if _, ok := files[filepath.Base(f)]; !ok {
			if err := os.Remove(f); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return err
			}
		}
	}
	for name, content := range files {
		if err := writeIfChanged(filepath.Join(dir, name), content); err != nil {
			return err
		}
	}
	return nil
}

// readManifests reads the manifest files (*.yaml) of dir, by name.
func readManifests(dir string) (map[string][]byte, error) {
	files, err := filepath.Glob(filepath.Join(dir, "*"+manifestExt))
	if err != nil {
		return nil, err
	}
	out := make(map[string][]byte, len(files))
	for _, f := range files {
		content, err := os.ReadFile(f) //nolint:gosec // G304: a manifest the driver rendered into the site's directory
		if err != nil {
			return nil, err
		}
		out[filepath.Base(f)] = content
	}
	return out, nil
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

// wpConfigParams is how wp-config.php reaches the platform services from a
// site's namespace; the rest of wpconfig.Params is the Compose driver's.
func (p *Provisioner) wpConfigParams() wpconfig.Params {
	return wpconfig.Params{
		DBHost:         p.dbHost,
		SMTP:           p.smtp,
		LoginPublicKey: p.loginKey,
		Security:       provisioner.WordPressSecurity(p.security),
	}
}
