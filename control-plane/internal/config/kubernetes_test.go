package config_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/rchouinard/pressfleet/control-plane/internal/config"
)

func TestKubernetesDefaults(t *testing.T) {
	k := config.Default().Kubernetes
	if k.PlatformNamespace != "pressfleet" || k.SiteNamespacePrefix != "pf-" || k.TraefikService != "traefik" || k.Kubeconfig != "" || k.DataClaim != "control-plane-data" {
		t.Errorf("defaults = %+v", k)
	}
	if got := k.SiteNamespace("01hzy3k5q7m9c2v4b6n8r0t1wx"); got != "pf-01hzy3k5q7m9c2v4b6n8r0t1wx" {
		t.Errorf("SiteNamespace = %q", got)
	}
}

func TestKubernetesFromEnvAndFile(t *testing.T) {
	file := "driver: kubernetes\nkubernetes:\n  platform_namespace: fleet\n  storage_classes:\n    sites: fast\n    data: nfs\n  pod_cidrs: [10.244.0.0/16]\n"
	cfg, err := config.Load(writeFile(t, file), env(map[string]string{
		"PRESSFLEET_KUBERNETES_SITE_NAMESPACE_PREFIX": "site-",
		"PRESSFLEET_KUBERNETES_IMAGE_PULL_SECRETS":    "ghcr, quay",
		"PRESSFLEET_KUBERNETES_KUBECONFIG":            "/home/dev/.kube/config",
		"PRESSFLEET_KUBERNETES_DATA_CLAIM":            "pressfleet-data",
	}))
	if err != nil {
		t.Fatal(err)
	}
	k := cfg.Kubernetes
	if cfg.Driver != config.SiteDriverKubernetes || k.PlatformNamespace != "fleet" || k.SiteNamespacePrefix != "site-" ||
		k.StorageClasses != (config.KubernetesStorageClasses{Sites: "fast", Data: "nfs"}) ||
		!slices.Equal(k.PodCIDRs, []string{"10.244.0.0/16"}) || !slices.Equal(k.ImagePullSecrets, []string{"ghcr", "quay"}) ||
		k.Kubeconfig != "/home/dev/.kube/config" || k.TraefikService != "traefik" || k.DataClaim != "pressfleet-data" {
		t.Errorf("Kubernetes = %+v", k)
	}
}

func TestValidateKubernetes(t *testing.T) {
	// kubernetesDriver is a valid configuration of the Kubernetes driver.
	kubernetesDriver := func(c *config.Config) {
		c.Driver = config.SiteDriverKubernetes
		c.Database = config.DatabaseConfig{Driver: config.DriverPostgres, PostgresDSN: "postgres://pressfleet@db/pressfleet"}
		c.Kubernetes.PodCIDRs = []string{"10.42.0.0/16", "fd00:42::/56"}
	}
	tests := []struct {
		name   string
		mutate func(*config.Config)
		want   string
	}{
		{"compose default", func(*config.Config) {}, ""},
		{"kubernetes", kubernetesDriver, ""},
		{"kubernetes on SQLite", func(c *config.Config) {
			kubernetesDriver(c)
			c.Database = config.DatabaseConfig{Driver: config.DriverSQLite}
		}, `driver kubernetes requires database.driver "postgres"`},
		{"kubernetes without pod CIDRs", func(c *config.Config) {
			kubernetesDriver(c)
			c.Kubernetes.PodCIDRs = nil
		}, "kubernetes.pod_cidrs is required"},
		{"compose ignores missing pod CIDRs", func(c *config.Config) { c.Kubernetes.PodCIDRs = nil }, ""},
		{"relative kubeconfig", func(c *config.Config) { c.Kubernetes.Kubeconfig = "kubeconfig" }, "kubernetes.kubeconfig"},
		{"platform namespace", func(c *config.Config) { c.Kubernetes.PlatformNamespace = "Fleet" }, "kubernetes.platform_namespace"},
		{"empty platform namespace", func(c *config.Config) { c.Kubernetes.PlatformNamespace = "" }, "kubernetes.platform_namespace"},
		{"empty prefix", func(c *config.Config) { c.Kubernetes.SiteNamespacePrefix = "" }, "kubernetes.site_namespace_prefix"},
		{"prefix starting with -", func(c *config.Config) { c.Kubernetes.SiteNamespacePrefix = "-pf" }, "kubernetes.site_namespace_prefix"},
		{"prefix too long", func(c *config.Config) {
			c.Kubernetes.SiteNamespacePrefix = strings.Repeat("p", config.MaxSiteNamespacePrefix+1)
		}, "kubernetes.site_namespace_prefix"},
		{"longest prefix", func(c *config.Config) {
			c.Kubernetes.SiteNamespacePrefix = strings.Repeat("p", config.MaxSiteNamespacePrefix)
		}, ""},
		{"storage class", func(c *config.Config) { c.Kubernetes.StorageClasses.Data = "NFS client" }, "kubernetes.storage_classes.data"},
		{"dotted storage class", func(c *config.Config) { c.Kubernetes.StorageClasses.Sites = "ebs.csi.aws.com" }, ""},
		{"traefik service", func(c *config.Config) { c.Kubernetes.TraefikService = "traefik.svc" }, "kubernetes.traefik_service"},
		{"pod CIDR", func(c *config.Config) { c.Kubernetes.PodCIDRs = []string{"10.42.0.0"} }, "kubernetes.pod_cidrs"},
		{"pod CIDR with host bits", func(c *config.Config) { c.Kubernetes.PodCIDRs = []string{"10.42.0.1/16"} }, "kubernetes.pod_cidrs"},
		{"pull secret", func(c *config.Config) { c.Kubernetes.ImagePullSecrets = []string{"ghcr_token"} }, "kubernetes.image_pull_secrets"},
		{"data claim", func(c *config.Config) { c.Kubernetes.DataClaim = "" }, "kubernetes.data_claim"},
		{"volume size", func(c *config.Config) { c.Kubernetes.SiteVolumeSize = "50Gi" }, ""},
		{"volume size in decimal units", func(c *config.Config) { c.Kubernetes.SiteVolumeSize = "10G" }, "kubernetes.site_volume_size"},
		{"empty volume size", func(c *config.Config) { c.Kubernetes.SiteVolumeSize = "" }, "kubernetes.site_volume_size"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config.Default()
			tt.mutate(&cfg)
			err := cfg.Validate()
			switch {
			case tt.want == "" && err != nil:
				t.Errorf("Validate() = %v, want nil", err)
			case tt.want != "" && (err == nil || !strings.Contains(err.Error(), tt.want)):
				t.Errorf("Validate() = %v, want error mentioning %s", err, tt.want)
			}
		})
	}
}
