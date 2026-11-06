package config

import (
	"errors"
	"fmt"
	"net/netip"
	"path/filepath"
	"regexp"
)

// SiteDriverKubernetes is the Kubernetes driver's name, the value of
// Config.Driver (§4.8).
const SiteDriverKubernetes = "kubernetes"

// MaxSiteNamespacePrefix is the longest kubernetes.site_namespace_prefix:
// a namespace name is at most 63 characters, and site IDs take 26.
const MaxSiteNamespacePrefix = 63 - 26

// KubernetesConfig configures the Kubernetes driver (§4.8). It applies only
// when driver is kubernetes; the Helm chart sets it for the control plane.
type KubernetesConfig struct {
	// Kubeconfig is a kubeconfig file the driver connects with, for
	// development outside the cluster. Empty, the default, uses the control
	// plane pod's service account.
	Kubeconfig string `yaml:"kubeconfig"`
	// PlatformNamespace is the namespace of the platform services (the
	// control plane, Traefik, MariaDB, Valkey): where repository tasks run
	// and where site NetworkPolicies allow traffic to and from.
	PlatformNamespace string `yaml:"platform_namespace"`
	// SiteNamespacePrefix is put before a site's ID to name its namespace:
	// pf-<site-id> by default.
	SiteNamespacePrefix string `yaml:"site_namespace_prefix"`
	// StorageClasses selects the storage classes of the volumes the driver
	// and the chart create.
	StorageClasses KubernetesStorageClasses `yaml:"storage_classes"`
	// SiteVolumeSize is the size each site's wp-content volume asks for,
	// a Kubernetes quantity in Mi, Gi, or Ti. A volume can grow, if its
	// storage class allows expansion, but never shrink.
	SiteVolumeSize string `yaml:"site_volume_size"`
	// TraefikService is the name of Traefik's Service in the platform
	// namespace, which uptime and certificate checks connect to.
	TraefikService string `yaml:"traefik_service"`
	// PodCIDRs are the cluster's pod network ranges. Site nginx trusts
	// forwarded headers from them (NGINX_TRUSTED_PROXIES), and the site
	// NetworkPolicy narrows that to Traefik's pods.
	PodCIDRs []string `yaml:"pod_cidrs"`
	// ImagePullSecrets name Secrets in the platform namespace that site
	// and task pods pull images with, for private registries. A pod can
	// only use a Secret of its own namespace, so the driver copies them
	// into each site namespace.
	ImagePullSecrets []string `yaml:"image_pull_secrets"`
}

// KubernetesStorageClasses names the storage classes of the Kubernetes
// install's volumes. Empty means the cluster's default storage class.
type KubernetesStorageClasses struct {
	// Sites is the ReadWriteOnce class of each site's wp-content volume.
	Sites string `yaml:"sites"`
	// Data is the ReadWriteMany class of the control plane's data
	// directory, which task pods mount too.
	Data string `yaml:"data"`
}

// DefaultSiteVolumeSize is the default kubernetes.site_volume_size.
const DefaultSiteVolumeSize = "10Gi"

// volumeSize matches the quantities kubernetes.site_volume_size accepts:
// a whole number of binary units, which reads the same to operators and to
// the API server.
var volumeSize = regexp.MustCompile(`^[1-9][0-9]{0,5}(Mi|Gi|Ti)$`)

// DefaultKubernetes matches the Helm chart's defaults.
func DefaultKubernetes() KubernetesConfig {
	return KubernetesConfig{
		PlatformNamespace:   "pressfleet",
		SiteNamespacePrefix: "pf-",
		SiteVolumeSize:      DefaultSiteVolumeSize,
		TraefikService:      "traefik",
	}
}

// SiteNamespace returns the namespace of a site.
func (k KubernetesConfig) SiteNamespace(siteID string) string {
	return k.SiteNamespacePrefix + siteID
}

// PodPrefixes returns PodCIDRs parsed. Validate has rejected malformed ones.
func (k KubernetesConfig) PodPrefixes() ([]netip.Prefix, error) {
	out := make([]netip.Prefix, 0, len(k.PodCIDRs))
	for _, s := range k.PodCIDRs {
		p, err := netip.ParsePrefix(s)
		if err != nil || p != p.Masked() {
			return nil, fmt.Errorf("kubernetes.pod_cidrs must be CIDR networks like 10.42.0.0/16, got %q", s)
		}
		out = append(out, p)
	}
	return out, nil
}

// dnsLabel matches an RFC 1123 label, the names of namespaces and
// Services.
var dnsLabel = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// dnsSubdomain matches an RFC 1123 subdomain, the names of most other
// objects (storage classes, Secrets).
var dnsSubdomain = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]{0,251}[a-z0-9])?$`)

// namespacePrefix matches the start of an RFC 1123 label.
var namespacePrefix = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// Validate reports every problem with the Kubernetes settings, or nil.
// Their form is checked whatever the driver; what the Kubernetes driver
// requires is checked by Config.Validate.
func (k KubernetesConfig) Validate() error {
	var errs []error
	if k.Kubeconfig != "" && !filepath.IsAbs(k.Kubeconfig) {
		errs = append(errs, fmt.Errorf("kubernetes.kubeconfig must be an absolute path, got %q", k.Kubeconfig))
	}
	if !dnsLabel.MatchString(k.PlatformNamespace) {
		errs = append(errs, fmt.Errorf("kubernetes.platform_namespace must be a namespace name (lowercase letters, digits, and -), got %q", k.PlatformNamespace))
	}
	if !namespacePrefix.MatchString(k.SiteNamespacePrefix) || len(k.SiteNamespacePrefix) > MaxSiteNamespacePrefix {
		errs = append(errs, fmt.Errorf("kubernetes.site_namespace_prefix must be 1 to %d lowercase letters, digits, and -, starting with a letter or digit, got %q",
			MaxSiteNamespacePrefix, k.SiteNamespacePrefix))
	}
	for _, sc := range []struct{ key, value string }{
		{"kubernetes.storage_classes.sites", k.StorageClasses.Sites},
		{"kubernetes.storage_classes.data", k.StorageClasses.Data},
	} {
		if sc.value != "" && !dnsSubdomain.MatchString(sc.value) {
			errs = append(errs, fmt.Errorf("%s must be a storage class name, got %q", sc.key, sc.value))
		}
	}
	if !volumeSize.MatchString(k.SiteVolumeSize) {
		errs = append(errs, fmt.Errorf("kubernetes.site_volume_size must be a whole number of Mi, Gi, or Ti, like 10Gi, got %q", k.SiteVolumeSize))
	}
	if !dnsLabel.MatchString(k.TraefikService) {
		errs = append(errs, fmt.Errorf("kubernetes.traefik_service must be a Service name, got %q", k.TraefikService))
	}
	if _, err := k.PodPrefixes(); err != nil {
		errs = append(errs, err)
	}
	for _, s := range k.ImagePullSecrets {
		if !dnsSubdomain.MatchString(s) {
			errs = append(errs, fmt.Errorf("kubernetes.image_pull_secrets must be Secret names, got %q", s))
		}
	}
	return errors.Join(errs...)
}

// validateKubernetesDriver reports what driver: kubernetes requires of the
// rest of the configuration.
func (c Config) validateKubernetesDriver() error {
	if c.Driver != SiteDriverKubernetes {
		return nil
	}
	var errs []error
	if c.Database.Driver != DriverPostgres {
		// The chart has no volume for SQLite, which isn't safe on the
		// ReadWriteMany network storage of the data directory: the state
		// database is a PostgreSQL the operator provides (§4.8).
		errs = append(errs, fmt.Errorf("driver kubernetes requires database.driver %q, got %q", DriverPostgres, c.Database.Driver))
	}
	if len(c.Kubernetes.PodCIDRs) == 0 {
		errs = append(errs, errors.New("kubernetes.pod_cidrs is required when driver is kubernetes: site nginx trusts forwarded headers from the pod network"))
	}
	return errors.Join(errs...)
}
