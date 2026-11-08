package cli_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	"github.com/rchouinard/pressfleet/control-plane/internal/cli"
	"github.com/rchouinard/pressfleet/control-plane/internal/config"
	"github.com/rchouinard/pressfleet/control-plane/internal/redact"
	"github.com/rchouinard/pressfleet/control-plane/internal/secrets"
)

// kubernetesEnv is a working directory for init --target kubernetes, with
// input files for the secrets it reads.
type kubernetesEnv struct {
	t        *testing.T
	out      string
	dsnFile  string
	smtpFile string
	dnsFile  string
	env      map[string]string
	// touched records any attempt to reach Docker, which init for
	// Kubernetes must never make.
	touched []string
}

func newKubernetesEnv(t *testing.T) *kubernetesEnv {
	t.Helper()
	root := t.TempDir()
	e := &kubernetesEnv{
		t:        t,
		out:      filepath.Join(root, "out"),
		dsnFile:  filepath.Join(root, "dsn"),
		smtpFile: filepath.Join(root, "smtp"),
		dnsFile:  filepath.Join(root, "dns.env"),
		env:      map[string]string{},
	}
	for path, content := range map[string]string{
		e.dsnFile:  "postgres://pressfleet:pg-secret@postgres.db.svc:5432/pressfleet\n",
		e.smtpFile: "smtp-secret\n",
		e.dnsFile:  "CF_DNS_API_TOKEN=cf-secret\n",
	} {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return e
}

// args answers every question with flags.
func (e *kubernetesEnv) args(extra ...string) []string {
	return append([]string{
		"init", "--target", "kubernetes", "--output-dir", e.out,
		"--non-interactive", "--confirm-key-stored",
		"--dashboard-domain", "fleet.example.com",
		"--preview-base-domain", "preview.example.com",
		"--namespace", "pf-platform",
		"--pod-cidrs", "10.244.0.0/16, fd00:10:244::/56",
		"--storage-class-data", "nfs",
		"--postgres-dsn-file", e.dsnFile,
	}, extra...)
}

func (e *kubernetesEnv) run(stdin string, args ...string) result {
	e.t.Helper()
	var stdout, stderr syncBuffer
	code := cli.Run(context.Background(), args, cli.Options{
		Version: "v1.2.3",
		Stdin:   strings.NewReader(stdin),
		Stdout:  &stdout,
		Stderr:  &stderr,
		Env: func(k string) (string, bool) {
			v, ok := e.env[k]
			return v, ok
		},
		ControlPlane: func(project string) cli.ControlPlaneProbe {
			e.touched = append(e.touched, "control plane "+project)
			return fakeProbe{&fakeControlPlane{}}
		},
		PlatformProject: func(name, _ string) cli.PlatformProject {
			e.touched = append(e.touched, "platform "+name)
			return nil
		},
	})
	return result{code, stdout.String(), stderr.String()}
}

// manifest is the part of a Secret manifest the tests read.
type manifest struct {
	Kind     string `yaml:"kind"`
	Metadata struct {
		Name      string `yaml:"name"`
		Namespace string `yaml:"namespace"`
	} `yaml:"metadata"`
	StringData map[string]string `yaml:"stringData"`
}

// secrets reads the written Secret manifests by name.
func (e *kubernetesEnv) secrets() map[string]manifest {
	e.t.Helper()
	data, err := os.ReadFile(filepath.Join(e.out, "secrets.yaml"))
	if err != nil {
		e.t.Fatal(err)
	}
	out := map[string]manifest{}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	for {
		var m manifest
		err := dec.Decode(&m)
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			e.t.Fatal(err)
		}
		out[m.Metadata.Name] = m
	}
}

// chartValues is the part of the values file the tests read.
type chartValues struct {
	DashboardDomain   string `yaml:"dashboardDomain"`
	PreviewBaseDomain string `yaml:"previewBaseDomain"`
	Kubernetes        struct {
		PodCIDRs       []string          `yaml:"podCIDRs"`
		StorageClasses map[string]string `yaml:"storageClasses"`
	} `yaml:"kubernetes"`
	ACME struct {
		DNSProvider          string `yaml:"dnsProvider"`
		DNSCredentialsSecret string `yaml:"dnsCredentialsSecret"`
	} `yaml:"acme"`
}

func (e *kubernetesEnv) values() chartValues {
	e.t.Helper()
	data, err := os.ReadFile(filepath.Join(e.out, "values.yaml"))
	if err != nil {
		e.t.Fatal(err)
	}
	var v chartValues
	if err := yaml.Unmarshal(data, &v); err != nil {
		e.t.Fatal(err)
	}
	return v
}

func TestInitKubernetes(t *testing.T) {
	e := newKubernetesEnv(t)
	r := e.run("", e.args(
		"--smtp-host", "smtp.example.com", "--smtp-username", "mailer", "--smtp-password-file", e.smtpFile,
		"--dns-provider", "cloudflare", "--dns-credentials-file", e.dnsFile,
	)...)
	if r.code != 0 {
		t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s", r.code, r.stdout, r.stderr)
	}
	if len(e.touched) > 0 {
		t.Errorf("init --target kubernetes reached Docker: %v", e.touched)
	}
	if !strings.Contains(r.stdout, "WARNING: "+cli.MasterKeyWarning+"\n") {
		t.Errorf("stdout lacks the verbatim master key warning:\n%s", r.stdout)
	}
	for _, want := range []string{
		"kubectl apply --server-side -f " + filepath.Join(e.out, "secrets.yaml"),
		"helm install pressfleet oci://ghcr.io/rchouinard/pressfleet/charts/pressfleet --version 1.2.3 --namespace pf-platform",
		"kubectl -n pf-platform exec deploy/control-plane -- pressfleet user create",
	} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("stdout lacks %q:\n%s", want, r.stdout)
		}
	}
	for _, secret := range []string{"pg-secret", "smtp-secret", "cf-secret"} {
		if strings.Contains(r.stdout+r.stderr, secret) {
			t.Errorf("output shows the secret %q", secret)
		}
	}

	info, err := os.Stat(filepath.Join(e.out, "secrets.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("secrets.yaml mode = %04o, want 0600", perm)
	}

	v := e.values()
	if v.DashboardDomain != "fleet.example.com" || v.PreviewBaseDomain != "preview.example.com" {
		t.Errorf("domains = %q, %q", v.DashboardDomain, v.PreviewBaseDomain)
	}
	if strings.Join(v.Kubernetes.PodCIDRs, ",") != "10.244.0.0/16,fd00:10:244::/56" || v.Kubernetes.StorageClasses["data"] != "nfs" {
		t.Errorf("kubernetes values = %+v", v.Kubernetes)
	}
	if v.ACME.DNSProvider != "cloudflare" || v.ACME.DNSCredentialsSecret != "pressfleet-dns-credentials" {
		t.Errorf("acme values = %+v", v.ACME)
	}
	data, err := os.ReadFile(filepath.Join(e.out, "values.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"pg-secret", "smtp-secret", "cf-secret", "pfmk1:"} {
		if strings.Contains(string(data), secret) {
			t.Errorf("values.yaml holds the secret %q", secret)
		}
	}

	// The Secrets the chart reads, with the keys it reads, in the namespace.
	s := e.secrets()
	want := map[string][]string{
		"pressfleet-master-key":      {"master.key"},
		"pressfleet-platform":        {"mariadb-root-password", "redis-admin-password", "ssh-host-key"},
		"pressfleet-postgres":        {"dsn"},
		"pressfleet-config":          {"pressfleet.yaml"},
		"pressfleet-dns-credentials": {"CF_DNS_API_TOKEN"},
	}
	for name, keys := range want {
		m, ok := s[name]
		if !ok {
			t.Errorf("no Secret %s", name)
			continue
		}
		if m.Kind != "Secret" || m.Metadata.Namespace != "pf-platform" {
			t.Errorf("%s: kind %q, namespace %q", name, m.Kind, m.Metadata.Namespace)
		}
		for _, k := range keys {
			if m.StringData[k] == "" {
				t.Errorf("%s has no %s", name, k)
			}
		}
	}
	key, err := secrets.ParseMasterKey(redact.Secret(s["pressfleet-master-key"].StringData["master.key"]))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.stdout, key.Encode().Reveal()) {
		t.Error("the stored master key isn't the one printed")
	}
	if got := s["pressfleet-postgres"].StringData["dsn"]; !strings.Contains(got, "pg-secret") {
		t.Errorf("dsn = %q", got)
	}
	if !strings.Contains(s["pressfleet-platform"].StringData["ssh-host-key"], "PRIVATE KEY-----") {
		t.Error("ssh-host-key is not a private key")
	}

	// The config file loads, with the chart's variables, into a valid
	// configuration whose SMTP password is sealed with the master key.
	cfgFile := filepath.Join(t.TempDir(), "pressfleet.yaml")
	if err := os.WriteFile(cfgFile, []byte(s["pressfleet-config"].StringData["pressfleet.yaml"]), 0o600); err != nil {
		t.Fatal(err)
	}
	chartEnv := map[string]string{
		"PRESSFLEET_DRIVER":                        "kubernetes",
		"PRESSFLEET_DATA_DIR":                      "/var/lib/pressfleet",
		"PRESSFLEET_DATABASE_DRIVER":               "postgres",
		"PRESSFLEET_DATABASE_POSTGRES_DSN":         s["pressfleet-postgres"].StringData["dsn"],
		"PRESSFLEET_KUBERNETES_POD_CIDRS":          "10.244.0.0/16",
		"PRESSFLEET_KUBERNETES_PLATFORM_NAMESPACE": "pf-platform",
	}
	cfg, err := config.Load(cfgFile, func(k string) (string, bool) { v, ok := chartEnv[k]; return v, ok })
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("the config Secret doesn't make a valid configuration: %v", err)
	}
	keys, err := secrets.NewKeyring(key)
	if err != nil {
		t.Fatal(err)
	}
	if pw, err := keys.OpenSetting(config.SMTPPasswordSetting, cfg.SMTP.Password); err != nil || pw.Reveal() != "smtp-secret" {
		t.Errorf("SMTP password doesn't open with the master key: %v", err)
	}
	if cfg.ACME.DNSProvider != "" || cfg.ACME.DNSCredentials != "" {
		t.Errorf("the config file has DNS settings, which are Traefik's on Kubernetes: %+v", cfg.ACME)
	}
}

// TestInitKubernetesValuesPassSchema renders the chart with the values
// init writes: values.schema.json rejects unknown keys and bad values.
func TestInitKubernetesValuesPassSchema(t *testing.T) {
	helm, err := exec.LookPath("helm")
	if err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("helm is not on the PATH; mise.toml pins it")
		}
		t.Skip("helm is not on the PATH; run `mise install`")
	}
	chart, err := filepath.Abs(filepath.Join("..", "..", "..", "charts", "pressfleet"))
	if err != nil {
		t.Fatal(err)
	}
	for name, extra := range map[string][]string{
		"minimal": nil,
		"every option": {
			"--public-ip", "203.0.113.7", "--storage-class-sites", "fast",
			"--acme-email", "ops@example.com", "--acme-ca-server", "https://pebble.example.com/dir",
			"--dns-provider", "cloudflare", "--acme-dns-resolvers", "1.1.1.1:53,8.8.8.8:53",
			"--acme-ca-certificates", "CA",
		},
	} {
		t.Run(name, func(t *testing.T) {
			e := newKubernetesEnv(t)
			pem := filepath.Join(t.TempDir(), "ca.pem")
			if err := os.WriteFile(pem, []byte("-----BEGIN CERTIFICATE-----\n-----END CERTIFICATE-----\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			args := e.args()
			for _, a := range extra {
				if a == "CA" {
					a = pem
				}
				args = append(args, a)
			}
			if slices.Contains(extra, "--dns-provider") {
				args = append(args, "--dns-credentials-file", e.dnsFile)
			}
			if r := e.run("", args...); r.code != 0 {
				t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s", r.code, r.stdout, r.stderr)
			}
			var stderr bytes.Buffer
			cmd := exec.CommandContext(t.Context(), helm, "template", "pressfleet", chart, "--namespace", "pf-platform",
				"--values", filepath.Join(e.out, "values.yaml"))
			cmd.Stdout, cmd.Stderr = io.Discard, &stderr
			if err := cmd.Run(); err != nil {
				t.Errorf("helm template with init's values: %v\n%s", err, stderr.String())
			}
		})
	}
}

func TestInitKubernetesForceKeepsSecrets(t *testing.T) {
	e := newKubernetesEnv(t)
	if r := e.run("", e.args()...); r.code != 0 {
		t.Fatalf("first run: exit %d\n%s%s", r.code, r.stdout, r.stderr)
	}
	first := e.secrets()

	r := e.run("", e.args()...)
	if r.code == 0 || !strings.Contains(r.stderr, "--force") {
		t.Fatalf("second run without --force: exit %d, stderr:\n%s", r.code, r.stderr)
	}

	// --force reuses what the cluster already holds; the answers come from
	// the earlier files, so only the output directory needs giving.
	r = e.run("", "init", "--target", "kubernetes", "--output-dir", e.out, "--force",
		"--non-interactive", "--confirm-key-stored", "--dashboard-domain", "new.example.com")
	if r.code != 0 {
		t.Fatalf("--force: exit %d\n%s%s", r.code, r.stdout, r.stderr)
	}
	if !strings.Contains(r.stdout, "Reusing the existing master key") {
		t.Errorf("stdout doesn't say the key is reused:\n%s", r.stdout)
	}
	second := e.secrets()
	for _, k := range []struct{ secret, key string }{
		{"pressfleet-master-key", "master.key"},
		{"pressfleet-platform", "mariadb-root-password"},
		{"pressfleet-platform", "redis-admin-password"},
		{"pressfleet-platform", "ssh-host-key"},
		{"pressfleet-postgres", "dsn"},
	} {
		if first[k.secret].StringData[k.key] != second[k.secret].StringData[k.key] {
			t.Errorf("%s %s changed on --force", k.secret, k.key)
		}
	}
	if ns := second["pressfleet-platform"].Metadata.Namespace; ns != "pf-platform" {
		t.Errorf("namespace = %q, want the earlier run's", ns)
	}
	v := e.values()
	if v.DashboardDomain != "new.example.com" || v.PreviewBaseDomain != "preview.example.com" || len(v.Kubernetes.PodCIDRs) != 2 {
		t.Errorf("values after --force = %+v", v)
	}
}

func TestInitKubernetesRefuses(t *testing.T) {
	tests := []struct {
		name string
		args func(e *kubernetesEnv) []string
		want string
	}{
		{"a Compose flag", func(e *kubernetesEnv) []string { return e.args("--admin-email", "a@example.com") }, "--admin-email"},
		{"the data directory", func(e *kubernetesEnv) []string { return e.args("--data-dir", "/srv") }, "--data-dir"},
		{"a Kubernetes flag on Compose", func(*kubernetesEnv) []string {
			return []string{"init", "--non-interactive", "--confirm-key-stored", "--pod-cidrs", "10.0.0.0/16"}
		}, "--pod-cidrs"},
		{"an unknown target", func(*kubernetesEnv) []string { return []string{"init", "--target", "nomad"} }, "--target"},
		{"no pod network", func(e *kubernetesEnv) []string {
			return []string{"init", "--target", "kubernetes", "--output-dir", e.out, "--non-interactive", "--confirm-key-stored",
				"--dashboard-domain", "fleet.example.com", "--preview-base-domain", "preview.example.com", "--postgres-dsn-file", e.dsnFile}
		}, "--pod-cidrs"},
		{"no state database", func(e *kubernetesEnv) []string {
			return []string{"init", "--target", "kubernetes", "--output-dir", e.out, "--non-interactive", "--confirm-key-stored",
				"--dashboard-domain", "fleet.example.com", "--preview-base-domain", "preview.example.com", "--pod-cidrs", "10.0.0.0/16"}
		}, "--postgres-dsn-file"},
		{"a DNS provider without credentials", func(e *kubernetesEnv) []string { return e.args("--dns-provider", "cloudflare") }, "--dns-credentials-file"},
		{"a bad pod network", func(e *kubernetesEnv) []string { return e.args("--pod-cidrs", "10.0.0.1/16") }, "pod_cidrs"},
		{"no key confirmation", func(e *kubernetesEnv) []string {
			return []string{"init", "--target", "kubernetes", "--output-dir", e.out, "--non-interactive"}
		}, "--confirm-key-stored"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newKubernetesEnv(t)
			r := e.run("", tt.args(e)...)
			if r.code == 0 {
				t.Fatalf("exit 0, want an error\n%s", r.stdout)
			}
			if !strings.Contains(r.stderr, tt.want) {
				t.Errorf("stderr doesn't mention %q:\n%s", tt.want, r.stderr)
			}
			if _, err := os.Stat(e.out); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("wrote %s despite the error", e.out)
			}
		})
	}
}

// TestInitKubernetesInteractiveAbort answers the questions and then refuses
// to confirm the master key: nothing is written.
func TestInitKubernetesInteractiveAbort(t *testing.T) {
	e := newKubernetesEnv(t)
	answers := strings.Join([]string{
		"fleet.example.com",   // dashboard domain
		"preview.example.com", // preview base domain
		"",                    // public IP
		"",                    // namespace: pressfleet
		"10.42.0.0/16",        // pod CIDRs
		"",                    // site storage class
		"",                    // data storage class
		e.dsnFile,             // DSN file
		"",                    // SMTP host
		"",                    // backups: local
		"",                    // ACME email
		"",                    // DNS provider
		"no",                  // master key not stored
	}, "\n") + "\n"
	r := e.run(answers, "init", "--target", "kubernetes", "--output-dir", e.out)
	if r.code == 0 || !strings.Contains(r.stderr, "master key must be stored") {
		t.Fatalf("exit %d, stderr:\n%s\nstdout:\n%s", r.code, r.stderr, r.stdout)
	}
	if !strings.Contains(r.stdout, "Pod network ranges") || !strings.Contains(r.stdout, "Traefik's Service") {
		t.Errorf("the Kubernetes questions weren't asked:\n%s", r.stdout)
	}
	if strings.Contains(r.stdout, "Data directory") || strings.Contains(r.stdout, "admin user") {
		t.Errorf("a Compose-only question was asked:\n%s", r.stdout)
	}
	if _, err := os.Stat(e.out); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("wrote %s after the abort", e.out)
	}
}

func TestUpgradePreflight(t *testing.T) {
	const (
		mariadb12 = "docker.io/library/mariadb:12.3.3@sha256:0bc7"
		mariadb11 = "docker.io/library/mariadb:11.8.2@sha256:aaaa"
	)
	tests := []struct {
		name     string
		version  string
		args     []string
		wantCode int
		want     string
	}{
		{"first install", "v1.2.0", nil, 0, "nothing to check"},
		{"upgrade", "v1.2.0", []string{"--from-version", "1.1.0", "--from-mariadb-image", mariadb12, "--to-mariadb-image", mariadb12}, 0, "from v1.1.0 to v1.2.0"},
		{"same release", "v1.2.0", []string{"--from-version", "1.2.0"}, 0, "already runs v1.2.0"},
		{"downgrade", "v1.1.0", []string{"--from-version", "1.2.0"}, 1, "can't be downgraded"},
		{"too old", "v1.2.0", []string{"--from-version", "0.0.9"}, 1, "intermediate release"},
		{"development build", "v1.2.0-dev", []string{"--from-version", "0.0.0-dev"}, 0, "development build"},
		{"MariaDB series change", "v1.2.0", []string{"--from-version", "1.1.0", "--from-mariadb-image", mariadb11, "--to-mariadb-image", mariadb12}, 1, "from the 11.8 to the 12.3 release series"},
		{"unknown MariaDB series", "v1.2.0", []string{"--from-mariadb-image", "registry.example/mariadb@sha256:aaaa", "--to-mariadb-image", mariadb12}, 1, "can't tell"},
		{"external MariaDB", "v1.2.0", []string{"--from-version", "1.1.0", "--from-mariadb-image", mariadb11}, 0, "Upgrading"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := cli.Run(context.Background(), append([]string{"admin", "upgrade-preflight"}, tt.args...), cli.Options{
				Version: tt.version, Stdout: &stdout, Stderr: &stderr,
				// No config, Secrets, or Docker: the hook has none.
				Env: func(string) (string, bool) { return "", false },
				ControlPlane: func(string) cli.ControlPlaneProbe {
					t.Error("the preflight looked for a Compose control plane")
					return fakeProbe{&fakeControlPlane{}}
				},
			})
			if code != tt.wantCode {
				t.Errorf("exit %d, want %d\nstdout:\n%s\nstderr:\n%s", code, tt.wantCode, stdout.String(), stderr.String())
			}
			if out := stdout.String() + stderr.String(); !strings.Contains(out, tt.want) {
				t.Errorf("output doesn't mention %q:\n%s", tt.want, out)
			}
		})
	}
}

// kubernetesInstallEnv is the environment of the control plane pod.
func kubernetesInstallEnv() map[string]string {
	return map[string]string{
		"PRESSFLEET_DRIVER":                "kubernetes",
		"PRESSFLEET_DATABASE_DRIVER":       "postgres",
		"PRESSFLEET_DATABASE_POSTGRES_DSN": "postgres://pressfleet@localhost/pressfleet",
		"PRESSFLEET_KUBERNETES_POD_CIDRS":  "10.244.0.0/16",
	}
}

func TestComposeCommandsRefusedOnKubernetes(t *testing.T) {
	for _, tt := range []struct {
		args []string
		want string
	}{
		{[]string{"upgrade", "--yes"}, "helm upgrade"},
		{[]string{"recover"}, "recovery values"},
		{[]string{"metrics-access", "rotate"}, "metrics API"},
	} {
		t.Run(tt.args[0], func(t *testing.T) {
			cp := &fakeControlPlane{}
			r := runProbed(t, t.Context(), tt.args, kubernetesInstallEnv(), nil, nil, &syncBuffer{}, cp)
			if r.code == 0 {
				t.Fatal("exit 0, want the command refused")
			}
			if !strings.Contains(r.stderr, "driver is kubernetes") || !strings.Contains(r.stderr, tt.want) {
				t.Errorf("stderr doesn't explain the refusal:\n%s", r.stderr)
			}
			if projects, _ := cp.calls(); len(projects) > 0 {
				t.Errorf("looked for a Compose control plane: %v", projects)
			}
		})
	}
}

func TestVersionOnKubernetes(t *testing.T) {
	// In the control plane pod (CLI v1.2.3-test), version reports what its
	// release check recorded, with Helm's upgrade (#319).
	tests := []struct {
		name string
		// latest is the recorded release; empty means no record.
		latest string
		// want is the notice; empty means none.
		want string
	}{
		{"no record", "", ""},
		{"newer release", "v1.3.0",
			"A newer PressFleet release is available: v1.3.0 (https://example.com/v1.3.0).\n" +
				"To upgrade, read its release notes and run `helm upgrade` " +
				"(docs/kubernetes.md, \"Upgrading\").\n"},
		{"up to date", "v1.2.2", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dataDir := t.TempDir()
			if tt.latest != "" {
				file := config.Layout{Root: dataDir}.ReleaseCheckFile()
				if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
					t.Fatal(err)
				}
				rec := `{"latest_version":"` + tt.latest + `","release_url":"https://example.com/` + tt.latest +
					`","checked_at":"2026-10-20T12:00:00Z"}`
				if err := os.WriteFile(file, []byte(rec), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			env := kubernetesInstallEnv()
			env["PRESSFLEET_DATA_DIR"] = dataDir
			cp := &fakeControlPlane{}
			r := runProbed(t, t.Context(), []string{"version"}, env, nil, nil, &syncBuffer{}, cp)
			if r.code != 0 {
				t.Fatalf("exit %d\n%s", r.code, r.stderr)
			}
			if !strings.Contains(r.stdout, "skew rules don't apply on Kubernetes") {
				t.Errorf("stdout:\n%s", r.stdout)
			}
			if tt.want == "" {
				if strings.Contains(r.stdout, "newer PressFleet release") {
					t.Errorf("unexpected notice:\n%s", r.stdout)
				}
			} else if !strings.HasSuffix(r.stdout, "\n\n"+tt.want) {
				t.Errorf("stdout lacks the notice %q:\n%s", tt.want, r.stdout)
			}
			if strings.Contains(r.stdout, "pressfleet upgrade") {
				t.Errorf("stdout points to pressfleet upgrade:\n%s", r.stdout)
			}
			if projects, _ := cp.calls(); len(projects) > 0 {
				t.Errorf("looked for a Compose control plane: %v", projects)
			}
		})
	}
}
