package e2e

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"
	"golang.org/x/crypto/ssh"

	"github.com/rchouinard/pressfleet/control-plane/internal/cli"
	"github.com/rchouinard/pressfleet/control-plane/internal/redact"
)

// The Kubernetes end-to-end tests (#228) install the platform with the Helm
// chart on a kind cluster that scripts/kind/cluster.sh prepares, and drive
// it through the same API client as the Compose tests. They go through the
// kubectl and helm programs, never client-go: Kubernetes SDKs belong to the
// driver (§4.2).

// Environment variables that enable and configure the Kubernetes harness.
// `scripts/kind/cluster.sh env` prints them for its cluster.
const (
	// EnvK8sTests enables the Kubernetes tests ("1").
	EnvK8sTests = "PRESSFLEET_K8S_TESTS"
	// EnvK8sContext is the kubeconfig context of the cluster.
	EnvK8sContext = "PRESSFLEET_K8S_CONTEXT"
	// EnvK8sKubeconfig is the kubeconfig file; empty is kubectl's default.
	EnvK8sKubeconfig = "PRESSFLEET_K8S_KUBECONFIG"
	// EnvK8sRegistry is the registry (host:port) that the cluster's nodes
	// and pods reach over plain HTTP, holding pressfleet/control-plane and
	// the site images.
	EnvK8sRegistry = "PRESSFLEET_K8S_REGISTRY"
	// EnvK8sImageTag is the control plane image's tag in that registry.
	EnvK8sImageTag = "PRESSFLEET_K8S_IMAGE_TAG"
	// EnvK8sPushRegistry is the same registry as the host's Docker reaches
	// it (host:port), for tests that build images of their own.
	EnvK8sPushRegistry = "PRESSFLEET_K8S_PUSH_REGISTRY"
)

// Fixtures of the kind cluster (scripts/kind/services.yaml).
const (
	kindPodCIDR         = "10.244.0.0/16"
	kindDataClass       = "nfs"
	kindPostgresNS      = "pfk8s-postgres"
	kindPostgresDSNBase = "postgres://postgres:pfk8s-postgres@postgres." + kindPostgresNS + ".svc:5432/"
)

// KubeDataDir is the control plane's data directory in its pod, which the
// chart mounts the data volume at.
const KubeDataDir = "/var/lib/pressfleet"

// kubeBootTimeout bounds installing the chart and the first start of the
// control plane, which migrates a new state database.
const kubeBootTimeout = 10 * time.Minute

// Kube runs kubectl and helm against the test cluster.
type Kube struct {
	// Context and Kubeconfig select the cluster.
	Context, Kubeconfig string
}

// kubeFromEnv returns the cluster the environment names, skipping the test
// unless the Kubernetes tests are enabled.
func kubeFromEnv(t *testing.T) Kube {
	t.Helper()
	if os.Getenv(EnvK8sTests) != "1" {
		t.Skip("set " + EnvK8sTests + "=1 to run tests against a Kubernetes cluster (see make test-k8s-e2e)")
	}
	k := Kube{Context: os.Getenv(EnvK8sContext), Kubeconfig: os.Getenv(EnvK8sKubeconfig)}
	if k.Context == "" {
		t.Fatal(EnvK8sContext + " must name the cluster's kubeconfig context (scripts/kind/cluster.sh env)")
	}
	return k
}

// KubernetesTests reports whether the environment selects the Kubernetes
// target.
func KubernetesTests() bool { return os.Getenv(EnvK8sTests) == "1" }

func (k Kube) globalArgs() []string {
	args := []string{"--context", k.Context}
	if k.Kubeconfig != "" {
		args = append(args, "--kubeconfig", k.Kubeconfig)
	}
	return args
}

// Kubectl runs kubectl with args, and stdin if not nil, and returns its
// standard output. A failure carries standard error.
func (k Kube) Kubectl(ctx context.Context, stdin io.Reader, args ...string) ([]byte, error) {
	return k.run(ctx, stdin, "kubectl", append(k.globalArgs(), args...)...)
}

// Helm runs helm with args against the cluster.
func (k Kube) Helm(ctx context.Context, args ...string) ([]byte, error) {
	g := []string{"--kube-context", k.Context}
	if k.Kubeconfig != "" {
		g = append(g, "--kubeconfig", k.Kubeconfig)
	}
	return k.run(ctx, nil, "helm", append(g, args...)...)
}

func (k Kube) run(ctx context.Context, stdin io.Reader, prog string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, prog, args...) //nolint:gosec // G204: kubectl or helm, with the test's arguments
	cmd.Stdin = stdin
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return stdout.Bytes(), fmt.Errorf("%s %s: %w: %s", prog, strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

// MustKubectl is Kubectl, failing the test on an error.
func (k Kube) MustKubectl(t *testing.T, args ...string) string {
	t.Helper()
	out, err := k.Kubectl(t.Context(), nil, args...)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}

// KubePlatform is the Kubernetes side of a Platform started with
// StartKubernetes.
type KubePlatform struct {
	Kube
	// Namespace is the platform namespace and the Helm release's.
	Namespace string
	// SitePrefix is kubernetes.site_namespace_prefix: unique to the
	// platform, so platforms on one cluster can't see each other's sites.
	SitePrefix string
	// Registry is where the cluster pulls the tree's images from.
	Registry string
	// Image is the control plane image.
	Image string
	// ValuesDir holds what `pressfleet init --target kubernetes` wrote,
	// and the test's own values file.
	ValuesDir string
	// Database is the platform's state database in the cluster's
	// PostgreSQL.
	Database string
	// NodeIP is the address of the node Traefik runs on, which its
	// NodePorts answer on (externalTrafficPolicy Local).
	NodeIP string
}

// SiteNamespace returns the namespace of a site.
func (k *KubePlatform) SiteNamespace(siteID string) string { return k.SitePrefix + siteID }

// KubeOptions changes what StartKubernetes installs.
type KubeOptions struct {
	// Pebble gives the platform a certificate authority: Traefik gets its
	// certificates from Pebble, which validates HTTP-01 challenges against
	// Traefik's Service.
	Pebble bool
	// Chart is the chart to install: a packaged chart (PackageChart), or
	// empty for the tree's.
	Chart string
	// Values are chart values merged over the harness's.
	Values map[string]any
}

// StartPlatform starts a platform on the target the environment selects:
// Kubernetes when EnvK8sTests is set, else Compose. Tests that only use
// the API and the platform's driver-agnostic helpers (ExecControlPlane,
// DataFileSize) run on both.
func StartPlatform(t *testing.T) *Platform {
	t.Helper()
	if KubernetesTests() {
		return StartKubernetes(t, KubeOptions{})
	}
	return Start(t)
}

// StartKubernetes installs the platform on the test cluster as an operator
// would: `pressfleet init --target kubernetes` writes the values and the
// Secrets, and `helm install` installs the chart from the tree into a new
// namespace that enforces Pod Security restricted. Then the harness points
// the control plane at the tree's site images in the cluster's registry,
// creates the admin user with `kubectl exec`, and logs in through Traefik.
//
// The test reaches Traefik on its NodePorts at the node's address, and the
// API through Traefik with the dashboard domain, as a browser would.
// Everything is removed when the test ends; if it failed, the platform's
// state and logs are written to the test log first.
func StartKubernetes(t *testing.T, opts KubeOptions) *Platform {
	t.Helper()
	kube := kubeFromEnv(t)
	registry, tag := os.Getenv(EnvK8sRegistry), os.Getenv(EnvK8sImageTag)
	if registry == "" || tag == "" {
		t.Fatal(EnvK8sRegistry + " and " + EnvK8sImageTag + " must name the registry and the control plane image's tag (see make test-k8s-e2e)")
	}
	token := randomToken(8)
	k := &KubePlatform{
		Kube:       kube,
		Namespace:  "pfk-" + token,
		SitePrefix: "pfk-" + token + "-",
		Registry:   registry,
		Image:      registry + "/pressfleet/control-plane:" + tag,
		ValuesDir:  t.TempDir(),
		Database:   "pf_" + token,
	}
	p := &Platform{Kube: k, DataDir: KubeDataDir, AdminEmail: "admin@e2e.test", AdminPassword: redact.Secret(randomToken(26))}
	t.Cleanup(func() { p.kubeTeardown(t) })

	ctx, cancel := context.WithTimeout(t.Context(), kubeBootTimeout)
	defer cancel()
	t.Logf("installing platform in namespace %s", k.Namespace)
	k.createDatabase(ctx, t)
	var pebble *pebbleFiles
	if opts.Pebble {
		pebble = newPebbleFiles(t)
	}
	k.init(ctx, t, pebble)
	signer, err := ssh.ParsePrivateKey([]byte(k.InitSecret(t, "pressfleet-platform", "ssh-host-key")))
	if err != nil {
		t.Fatalf("the SSH host key init wrote: %v", err)
	}
	p.HostKey = signer.PublicKey()
	k.CreateNamespace(ctx, t)
	if out, err := k.RunChart(ctx, t, "install", opts.Chart, opts.Values); err != nil {
		t.Fatalf("helm install: %v\n%s", err, out)
	}
	if pebble != nil {
		p.Pebble = k.startPebble(ctx, t, pebble)
	}
	k.useTreeImages(ctx, t)
	p.kubeConnect(ctx, t)
	p.createAdminKube(ctx, t)
	p.Client = p.Login(t, p.AdminEmail, p.AdminPassword)
	return p
}

// CreateNamespace creates the platform namespace, enforcing Pod Security
// restricted, and applies the Secrets init wrote, as an operator does
// before installing the chart.
func (k *KubePlatform) CreateNamespace(ctx context.Context, t *testing.T) {
	t.Helper()
	k.MustKubectl(t, "create", "namespace", k.Namespace)
	// The chart's pods meet Pod Security restricted (charts/pressfleet).
	k.MustKubectl(t, "label", "namespace", k.Namespace, "pod-security.kubernetes.io/enforce=restricted")
	if _, err := k.Kubectl(ctx, nil, "apply", "-n", k.Namespace, "-f", filepath.Join(k.ValuesDir, "secrets.yaml")); err != nil {
		t.Fatal(err)
	}
}

// createDatabase creates the platform's state database.
func (k *KubePlatform) createDatabase(ctx context.Context, t *testing.T) {
	t.Helper()
	if _, err := k.psql(ctx, "postgres", "CREATE DATABASE "+k.Database); err != nil {
		t.Fatalf("creating the state database: %v", err)
	}
}

// psql runs a statement in a database of the cluster's PostgreSQL and
// returns its unaligned output.
func (k *KubePlatform) psql(ctx context.Context, database, sql string) ([]byte, error) {
	return k.Kubectl(ctx, nil, "exec", "-n", kindPostgresNS, "deploy/postgres", "--",
		"psql", "-U", "postgres", "-d", database, "-v", "ON_ERROR_STOP=1", "-tA", "-c", sql)
}

// EmptyDatabase replaces the state database with an empty one, as an
// operator's new PostgreSQL would have.
func (k *KubePlatform) EmptyDatabase(ctx context.Context, t *testing.T) {
	t.Helper()
	if _, err := k.psql(ctx, "postgres", "DROP DATABASE IF EXISTS "+k.Database+" WITH (FORCE)"); err != nil {
		t.Fatalf("dropping the state database: %v", err)
	}
	k.createDatabase(ctx, t)
}

// DumpDatabase backs up the state database with pg_dump, as an operator
// does before an upgrade (docs/upgrading.md), and returns the archive.
func (k *KubePlatform) DumpDatabase(ctx context.Context, t *testing.T) []byte {
	t.Helper()
	out, err := k.Kubectl(ctx, nil, "exec", "-n", kindPostgresNS, "deploy/postgres", "--",
		"pg_dump", "-U", "postgres", "--format=custom", k.Database)
	if err != nil {
		t.Fatalf("pg_dump: %v", err)
	}
	return out
}

// RestoreDatabase replaces the state database with a DumpDatabase archive.
// Nothing may use the database meanwhile.
func (k *KubePlatform) RestoreDatabase(ctx context.Context, t *testing.T, dump []byte) {
	t.Helper()
	k.EmptyDatabase(ctx, t)
	if _, err := k.Kubectl(ctx, bytes.NewReader(dump), "exec", "-i", "-n", kindPostgresNS, "deploy/postgres", "--",
		"pg_restore", "-U", "postgres", "--exit-on-error", "-d", k.Database); err != nil {
		t.Fatalf("pg_restore: %v", err)
	}
}

// SchemaVersion returns the state database's schema version: the newest
// migration applied.
func (k *KubePlatform) SchemaVersion(ctx context.Context, t *testing.T) int {
	t.Helper()
	out, err := k.psql(ctx, k.Database, "SELECT MAX(version) FROM schema_migrations")
	if err != nil {
		t.Fatalf("reading the schema version: %v", err)
	}
	v, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		t.Fatalf("schema version %q: %v", out, err)
	}
	return v
}

// ScaleDownControlPlane scales the control plane to zero and waits until
// its pod is gone, as docs/disaster-recovery.md and docs/upgrading.md do
// before anything replaces the state database.
func (k *KubePlatform) ScaleDownControlPlane(ctx context.Context, t *testing.T) {
	t.Helper()
	if _, err := k.Kubectl(ctx, nil, "scale", "-n", k.Namespace, "deployment/control-plane", "--replicas=0"); err != nil {
		t.Fatal(err)
	}
	if _, err := k.Kubectl(ctx, nil, "wait", "-n", k.Namespace, "--for=delete", "pod",
		"-l", "app.kubernetes.io/component=control-plane", "--timeout=5m"); err != nil {
		t.Fatal(err)
	}
}

// init runs `pressfleet init --target kubernetes` into ValuesDir, and adds
// the test's settings to the config file it put in its Secret.
func (k *KubePlatform) init(ctx context.Context, t *testing.T, pebble *pebbleFiles) {
	t.Helper()
	dsnFile := filepath.Join(t.TempDir(), "dsn")
	if err := os.WriteFile(dsnFile, []byte(kindPostgresDSNBase+k.Database+"?sslmode=disable\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	args := []string{
		"init", "--target", "kubernetes", "--non-interactive", "--confirm-key-stored",
		"--output-dir", k.ValuesDir,
		"--namespace", k.Namespace,
		"--dashboard-domain", DashboardDomain,
		"--preview-base-domain", PreviewBaseDomain,
		"--public-ip", PublicIP,
		"--pod-cidrs", kindPodCIDR,
		"--storage-class-data", kindDataClass,
		"--postgres-dsn-file", dsnFile,
	}
	if pebble != nil {
		args = append(args, "--acme-ca-server", pebbleDirectory, "--acme-ca-certificates", pebble.caFile)
	} else {
		args = append(args, "--acme-ca-server", ACMECAServer)
	}
	var stdout, stderr bytes.Buffer
	code := cli.Run(ctx, args, cli.Options{
		Version: "e2e",
		Stdin:   strings.NewReader(""),
		Stdout:  &stdout,
		Stderr:  &stderr,
		Env:     func(string) (string, bool) { return "", false },
	})
	if code != 0 {
		t.Fatalf("init --target kubernetes exited %d: %s", code, stderr.String())
	}
	k.addSettings(t, map[string]any{
		"kubernetes": map[string]any{"insecure_registries": []string{k.Registry}},
		// Test platforms don't ask GitHub for releases (#140).
		"updates": map[string]any{"release_check": false},
	})
}

// addSettings merges settings into the config file in the
// pressfleet-config Secret that init wrote, as an operator edits it.
func (k *KubePlatform) addSettings(t *testing.T, settings map[string]any) {
	t.Helper()
	path := filepath.Join(k.ValuesDir, "secrets.yaml")
	data, err := os.ReadFile(path) //nolint:gosec // G304: the test's own temporary directory
	if err != nil {
		t.Fatal(err)
	}
	var docs []map[string]any
	dec := yaml.NewDecoder(bytes.NewReader(data))
	for {
		var doc map[string]any
		if err := dec.Decode(&doc); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatalf("parsing the Secrets init wrote: %v", err)
		}
		if doc == nil {
			continue
		}
		meta, _ := doc["metadata"].(map[string]any)
		if meta["name"] == "pressfleet-config" {
			sd, _ := doc["stringData"].(map[string]any)
			if sd == nil {
				sd = map[string]any{}
				doc["stringData"] = sd
			}
			cfg := map[string]any{}
			if s, ok := sd["pressfleet.yaml"].(string); ok {
				if err := yaml.Unmarshal([]byte(s), &cfg); err != nil {
					t.Fatalf("parsing the config file init wrote: %v", err)
				}
			}
			if cfg == nil {
				cfg = map[string]any{}
			}
			mergeSettings(cfg, settings)
			out, err := yaml.Marshal(cfg)
			if err != nil {
				t.Fatal(err)
			}
			sd["pressfleet.yaml"] = string(out)
		}
		docs = append(docs, doc)
	}
	var b bytes.Buffer
	enc := yaml.NewEncoder(&b)
	for _, doc := range docs {
		if err := enc.Encode(doc); err != nil {
			t.Fatal(err)
		}
	}
	if err := enc.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}

// InitSecret returns a value of a Secret that init wrote.
func (k *KubePlatform) InitSecret(t *testing.T, name, key string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(k.ValuesDir, "secrets.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	for {
		var doc struct {
			Metadata   struct{ Name string } `yaml:"metadata"`
			StringData map[string]string     `yaml:"stringData"`
		}
		if err := dec.Decode(&doc); err != nil {
			t.Fatalf("no key %s in the Secret %s that init wrote: %v", key, name, err)
		}
		if v, ok := doc.StringData[key]; ok && doc.Metadata.Name == name {
			return v
		}
	}
}

// mergeSettings copies src into dst, merging nested maps.
func mergeSettings(dst, src map[string]any) {
	for key, v := range src {
		if sub, ok := v.(map[string]any); ok {
			if d, ok := dst[key].(map[string]any); ok {
				mergeSettings(d, sub)
				continue
			}
		}
		dst[key] = v
	}
}

// chartDir returns the chart in the source tree.
func chartDir(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("can't find the source tree")
	}
	return filepath.Join(filepath.Dir(file), "..", "..", "..", "charts", "pressfleet")
}

// testValues are the chart values the harness adds to init's: the tree's
// control plane image, a namespace prefix of the platform's own, NodePorts
// instead of a LoadBalancer, and small volumes.
func (k *KubePlatform) testValues() map[string]any {
	small := func(size string) map[string]any { return map[string]any{"storage": map[string]any{"size": size}} }
	return map[string]any{
		"images":     map[string]any{"controlPlane": k.Image},
		"kubernetes": map[string]any{"siteNamespacePrefix": k.SitePrefix},
		"controlPlane": map[string]any{
			"data": map[string]any{"size": "2Gi"},
		},
		"mariadb":         small("2Gi"),
		"victoriaMetrics": small("1Gi"),
		"traefik": map[string]any{
			"service": map[string]any{"type": "NodePort"},
		},
	}
}

// InstallChart runs `helm install` or `helm upgrade` (verb) of the chart in
// the tree with init's values, the test's, and extra, and waits until the
// release's pods are ready.
func (k *KubePlatform) InstallChart(ctx context.Context, t *testing.T, verb string, extra map[string]any) {
	t.Helper()
	if out, err := k.RunChart(ctx, t, verb, "", extra); err != nil {
		t.Fatalf("helm %s: %v\n%s", verb, err, out)
	}
}

// RunChart runs `helm install` or `helm upgrade` (verb) of chart (a
// packaged chart, or empty for the tree's) with init's values, the test's,
// and extra, waits until the release's pods are ready, and returns helm's
// output. An upgrade runs the chart's hooks, so a refused upgrade is an
// error.
func (k *KubePlatform) RunChart(ctx context.Context, t *testing.T, verb, chart string, extra map[string]any) ([]byte, error) {
	t.Helper()
	if chart == "" {
		chart = chartDir(t)
	}
	values := k.testValues()
	if extra != nil {
		mergeSettings(values, extra)
	}
	data, err := yaml.Marshal(values)
	if err != nil {
		t.Fatal(err)
	}
	testValues := filepath.Join(k.ValuesDir, "test-values.yaml")
	if err := os.WriteFile(testValues, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return k.Helm(ctx, verb, "pressfleet", chart, "--namespace", k.Namespace,
		"-f", filepath.Join(k.ValuesDir, "values.yaml"), "-f", testValues,
		"--wait", "--timeout", strconv.Itoa(int(kubeBootTimeout.Seconds()))+"s")
}

// PackageChart packages the tree's chart stamped with version as both its
// version and its appVersion, as the release pipeline does (#227), and
// returns the archive's path.
func PackageChart(t *testing.T, version string) string {
	t.Helper()
	dir := t.TempDir()
	cmd := exec.CommandContext(t.Context(), "helm", "package", chartDir(t), //nolint:gosec // G204: helm, with the test's arguments
		"--version", version, "--app-version", version, "--destination", dir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("helm package --version %s: %v\n%s", version, err, out)
	}
	return filepath.Join(dir, "pressfleet-"+version+".tgz")
}

// useTreeImages points the control plane at the site images in the
// cluster's registry: a copy of the tree's version manifest with the
// registry's prefix, on the data volume (image_manifest_file). It restarts
// the control plane, as changing its configuration does.
func (k *KubePlatform) useTreeImages(ctx context.Context, t *testing.T) {
	t.Helper()
	manifest := kubeManifest(t, k.Registry+"/pressfleet")
	path := KubeTreeImagesEnv()["PRESSFLEET_IMAGE_MANIFEST_FILE"]
	if _, err := k.ExecControlPlane(ctx, bytes.NewReader(manifest), "sh", "-c", `mkdir -p "$(dirname "$1")" && cat > "$1"`, "sh", path); err != nil {
		t.Fatalf("writing the image manifest: %v", err)
	}
	k.SetEnv(ctx, t, KubeTreeImagesEnv())
}

// KubeTreeImagesEnv returns what StartKubernetes sets on the control plane's
// Deployment to point it at the tree's site images: a manifest in the
// data directory's e2e directory. A test that installs the chart afresh
// over a data volume that kept that directory sets it again with SetEnv.
func KubeTreeImagesEnv() map[string]string {
	return map[string]string{"PRESSFLEET_IMAGE_MANIFEST_FILE": KubeDataDir + "/e2e/manifest.json"}
}

// kubeManifest returns images/manifest.json with another registry.
func kubeManifest(t *testing.T, registry string) []byte {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	data, err := os.ReadFile(filepath.Join(filepath.Dir(file), "..", "..", "..", "images", "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	m["registry"] = registry
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// SetEnv sets environment variables of the control plane's Deployment and
// waits for the new pod, the way `kubectl set env` changes a setting.
func (k *KubePlatform) SetEnv(ctx context.Context, t *testing.T, env map[string]string) {
	t.Helper()
	args := []string{"set", "env", "-n", k.Namespace, "deployment/control-plane"}
	for _, key := range slices.Sorted(maps.Keys(env)) {
		args = append(args, key+"="+env[key])
	}
	if _, err := k.Kubectl(ctx, nil, args...); err != nil {
		t.Fatal(err)
	}
	k.WaitRollout(ctx, t, "deployment/control-plane")
}

// WaitRollout waits until a workload of the platform namespace has rolled
// out.
func (k *KubePlatform) WaitRollout(ctx context.Context, t *testing.T, workload string) {
	t.Helper()
	if _, err := k.Kubectl(ctx, nil, "rollout", "status", "-n", k.Namespace, workload, "--timeout", "5m"); err != nil {
		t.Fatal(err)
	}
}

// ExecControlPlane runs a command in the control plane's container with
// stdin, if not nil, and returns its standard output.
func (k *KubePlatform) ExecControlPlane(ctx context.Context, stdin io.Reader, argv ...string) ([]byte, error) {
	args := []string{"exec", "-n", k.Namespace, "deploy/control-plane", "-c", "control-plane"}
	if stdin != nil {
		args = append(args, "-i")
	}
	return k.Kubectl(ctx, stdin, append(append(args, "--"), argv...)...)
}

// ExecControlPlane runs a command in the control plane's container, with
// stdin if not nil, on either target.
func (p *Platform) ExecControlPlane(ctx context.Context, stdin io.Reader, argv ...string) ([]byte, error) {
	if p.Kube != nil {
		return p.Kube.ExecControlPlane(ctx, stdin, argv...)
	}
	if stdin != nil {
		return p.Project.ExecInput(ctx, stdin, Service, argv...)
	}
	return p.Project.Exec(ctx, Service, argv...)
}

// ExecVictoriaMetrics runs a command in VictoriaMetrics' container on
// either target. It has no published port or route, and only the control
// plane may connect to it, so tests read and write the metrics store from
// inside: its image has BusyBox's wget.
func (p *Platform) ExecVictoriaMetrics(ctx context.Context, argv ...string) ([]byte, error) {
	if p.Kube != nil {
		return p.Kube.Kubectl(ctx, nil, append([]string{"exec", "-n", p.Kube.Namespace, "statefulset/victoria-metrics", "--"}, argv...)...)
	}
	return p.Project.Exec(ctx, "victoria-metrics", argv...)
}

// DataFileSize returns the size of a file in the data directory (a path
// under p.DataDir), and whether it exists, on either target.
func (p *Platform) DataFileSize(ctx context.Context, t *testing.T, path string) (int64, bool) {
	t.Helper()
	if p.Kube == nil {
		info, err := os.Stat(path)
		if errors.Is(err, os.ErrNotExist) {
			return 0, false
		}
		if err != nil {
			t.Fatal(err)
		}
		return info.Size(), true
	}
	out, err := p.Kube.ExecControlPlane(ctx, nil, "sh", "-c", `if [ -e "$1" ]; then stat -c %s "$1"; else echo none; fi`, "sh", path)
	if err != nil {
		t.Fatal(err)
	}
	s := strings.TrimSpace(string(out))
	if s == "none" {
		return 0, false
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		t.Fatalf("size of %s: %q", path, s)
	}
	return n, true
}

// kubeConnect finds Traefik's NodePorts and node, and waits until the
// control plane answers ready through Traefik.
func (p *Platform) kubeConnect(ctx context.Context, t *testing.T) {
	t.Helper()
	k := p.Kube
	k.NodeIP = k.MustKubectl(t, "get", "pods", "-n", k.Namespace, "-l", "app.kubernetes.io/component=traefik",
		"-o", "jsonpath={.items[0].status.hostIP}")
	ports := map[string]string{}
	for _, name := range []string{"web", "websecure", "sftp"} {
		ports[name] = k.MustKubectl(t, "get", "service", "traefik", "-n", k.Namespace,
			"-o", `jsonpath={.spec.ports[?(@.name=="`+name+`")].nodePort}`)
		if ports[name] == "" {
			t.Fatalf("Traefik's Service has no NodePort %s", name)
		}
	}
	p.HTTPAddr = net.JoinHostPort(k.NodeIP, ports["web"])
	p.HTTPSAddr = net.JoinHostPort(k.NodeIP, ports["websecure"])
	p.SFTPAddr = net.JoinHostPort(k.NodeIP, ports["sftp"])
	p.BaseURL = "https://" + DashboardDomain
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	p.apiTransport = &http.Transport{
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, network, p.HTTPSAddr)
		},
		// Traefik serves its default certificate, or Pebble's, for the
		// dashboard domain.
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // G402: see above
	}
	t.Logf("Traefik at %s (HTTP), %s (HTTPS), %s (SFTP)", p.HTTPAddr, p.HTTPSAddr, p.SFTPAddr)
	p.waitReady(ctx, t)
}

// createAdminKube creates the admin user with the CLI in the control
// plane's container, as an operator does with kubectl exec.
func (p *Platform) createAdminKube(ctx context.Context, t *testing.T) {
	t.Helper()
	out, err := p.Kube.ExecControlPlane(ctx, strings.NewReader(p.AdminPassword.Reveal()+"\n"),
		"pressfleet", "user", "create", "--email", p.AdminEmail, "--role", "admin", "--password-stdin")
	if err != nil {
		t.Fatalf("creating admin user: %v", err)
	}
	t.Logf("%s", strings.TrimSpace(string(out)))
}

// kubeRestart applies env to the control plane's Deployment, which
// replaces its pod, then reconnects and logs in again.
func (p *Platform) kubeRestart(t *testing.T, env map[string]string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), kubeBootTimeout)
	defer cancel()
	if len(env) == 0 {
		if _, err := p.Kube.Kubectl(ctx, nil, "rollout", "restart", "-n", p.Kube.Namespace, "deployment/control-plane"); err != nil {
			t.Fatal(err)
		}
		p.Kube.WaitRollout(ctx, t, "deployment/control-plane")
	} else {
		p.Kube.SetEnv(ctx, t, env)
	}
	p.kubeConnect(ctx, t)
	p.Client = p.Login(t, p.AdminEmail, p.AdminPassword)
}

// kubeTeardown removes what StartKubernetes created: the release, the
// platform and site namespaces, their PersistentVolumes (the data class
// retains them), and the state database. It reports leftovers.
func (p *Platform) kubeTeardown(t *testing.T) {
	k := p.Kube
	ctx, cancel := context.WithTimeout(context.Background(), 2*cleanupTimeout)
	defer cancel()
	if t.Failed() {
		k.dumpState(ctx, t)
	}
	if _, err := k.Helm(ctx, "uninstall", "pressfleet", "--namespace", k.Namespace, "--wait", "--timeout", "3m"); err != nil &&
		!strings.Contains(err.Error(), "not found") {
		t.Logf("helm uninstall: %v", err)
	}
	for _, ns := range k.namespaces(ctx, t) {
		if _, err := k.Kubectl(ctx, nil, "delete", "namespace", ns, "--wait=true", "--timeout=3m", "--ignore-not-found"); err != nil {
			t.Errorf("deleting namespace %s: %v", ns, err)
		}
	}
	for _, pv := range k.persistentVolumes(ctx, t) {
		if _, err := k.Kubectl(ctx, nil, "delete", "pv", pv, "--wait=true", "--timeout=2m", "--ignore-not-found"); err != nil {
			t.Errorf("deleting PersistentVolume %s: %v", pv, err)
		}
	}
	if _, err := k.Kubectl(ctx, nil, "exec", "-n", kindPostgresNS, "deploy/postgres", "--",
		"psql", "-U", "postgres", "-c", "DROP DATABASE IF EXISTS "+k.Database+" WITH (FORCE)"); err != nil {
		t.Logf("dropping the state database: %v", err)
	}
	if left := k.namespaces(ctx, t); len(left) > 0 {
		t.Errorf("namespaces left behind: %v", left)
	}
}

// namespaces lists the platform namespace and its sites', and doctor's
// probe namespaces.
func (k *KubePlatform) namespaces(ctx context.Context, t *testing.T) []string {
	out, err := k.Kubectl(ctx, nil, "get", "namespaces", "-o", "name")
	if err != nil {
		t.Errorf("listing namespaces: %v", err)
		return nil
	}
	var found []string
	for line := range strings.Lines(string(out)) {
		name := strings.TrimPrefix(strings.TrimSpace(line), "namespace/")
		if name == k.Namespace || strings.HasPrefix(name, k.SitePrefix) || strings.HasPrefix(name, k.Namespace+"-doctor-") {
			found = append(found, name)
		}
	}
	return found
}

// persistentVolumes lists the PersistentVolumes of the platform's claims
// and the data volume's clones.
func (k *KubePlatform) persistentVolumes(ctx context.Context, t *testing.T) []string {
	out, err := k.Kubectl(ctx, nil, "get", "pv", "-o",
		`jsonpath={range .items[*]}{.metadata.name} {.spec.claimRef.namespace} {.metadata.labels.pressfleet\.io/data-clone}{"\n"}{end}`)
	if err != nil {
		t.Errorf("listing PersistentVolumes: %v", err)
		return nil
	}
	var found []string
	for line := range strings.Lines(string(out)) {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		if f[1] == k.Namespace || strings.HasPrefix(f[1], k.SitePrefix) || (len(f) > 2 && f[2] == k.Namespace) {
			found = append(found, f[0])
		}
	}
	return found
}

// dumpState logs what a failed test needs: the platform's objects and
// events, and the logs of its pods.
func (k *KubePlatform) dumpState(ctx context.Context, t *testing.T) {
	if out, err := k.ServedRoutes(ctx); err == nil {
		t.Logf("routes the control plane serves Traefik:\n%s", out)
	} else {
		t.Logf("reading the routes the control plane serves Traefik: %v", err)
	}
	for _, ns := range k.namespaces(ctx, t) {
		if out, err := k.Kubectl(ctx, nil, "get", "all,pvc,networkpolicies", "-n", ns, "-o", "wide"); err == nil {
			t.Logf("objects in %s:\n%s", ns, out)
		}
		if out, err := k.Kubectl(ctx, nil, "get", "events", "-n", ns, "--sort-by=.lastTimestamp"); err == nil {
			t.Logf("events in %s:\n%s", ns, out)
		}
		pods, err := k.Kubectl(ctx, nil, "get", "pods", "-n", ns, "-o", "name")
		if err != nil {
			continue
		}
		for pod := range strings.FieldsSeq(string(pods)) {
			if out, err := k.Kubectl(ctx, nil, "logs", "-n", ns, pod, "--all-containers", "--tail=300", "--prefix"); err == nil {
				t.Logf("logs of %s/%s:\n%s", ns, pod, out)
			}
		}
	}
}

// SiteExec runs commands in a site's containers ("php", "nginx") as their
// user: compose.Project on Compose, *KubeSite on Kubernetes. Tests written
// against it run on both targets.
type SiteExec interface {
	ExecService(ctx context.Context, service string, command ...string) ([]byte, error)
}

// KubeSite is a site's namespace on the test cluster.
type KubeSite struct {
	Kube      Kube
	Namespace string
}

// Site returns a site of the platform.
func (k *KubePlatform) Site(siteID string) *KubeSite {
	return &KubeSite{Kube: k.Kube, Namespace: k.SiteNamespace(siteID)}
}

// ExecService runs a command in a container of the site's pod, like
// `kubectl exec deploy/site -c <service>`, and returns its standard output.
func (s *KubeSite) ExecService(ctx context.Context, service string, command ...string) ([]byte, error) {
	return s.Kube.Kubectl(ctx, nil, append([]string{"exec", "-n", s.Namespace, "deploy/site", "-c", service, "--"}, command...)...)
}

// String names the site's namespace, for test messages.
func (s *KubeSite) String() string { return s.Namespace }

// ServedRoutes returns the dynamic configuration the control plane serves
// Traefik's HTTP provider (#322), fetched from Traefik's pod, the only one
// the NetworkPolicy lets in.
func (k *KubePlatform) ServedRoutes(ctx context.Context) ([]byte, error) {
	url := "http://control-plane." + k.Namespace + ".svc:8081/traefik/dynamic"
	return k.Kubectl(ctx, nil, "exec", "-n", k.Namespace, "deploy/traefik", "--", "wget", "-qO-", url)
}
