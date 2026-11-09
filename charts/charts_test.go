package charts_test

import (
	"bytes"
	"errors"
	"flag"
	"io"
	"iter"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	"github.com/rchouinard/pressfleet/charts"
)

var update = flag.Bool("update", false, "rewrite the golden files of TestTemplate")

// releaseNamespace is the namespace the chart is rendered for.
const releaseNamespace = "pressfleet"

// helm returns the helm binary, which mise installs (mise.toml). Without it
// the Helm tests are skipped, except in CI, where they must run.
func helm(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("helm")
	if err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("helm is not on the PATH; mise.toml pins it")
		}
		t.Skip("helm is not on the PATH; run `mise install`")
	}
	return path
}

// runHelm runs helm with args and returns its standard output, or its
// standard error as the error.
func runHelm(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(t.Context(), helm(t), args...)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return stdout.String(), errors.New(stderr.String())
	}
	return stdout.String(), nil
}

// render runs helm template with a values file.
func render(t *testing.T, values string) (string, error) {
	t.Helper()
	return runHelm(t, "template", "pressfleet", charts.ChartDir, "--namespace", releaseNamespace, "--values", values)
}

// valuesFiles are the values files the chart is rendered with: the least
// an install needs, and every option set.
func valuesFiles(t *testing.T) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join("testdata", "values", "*.yaml"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no values files in testdata/values: %v", err)
	}
	return files
}

func TestSharedFilesUpToDate(t *testing.T) {
	for _, f := range charts.SharedFiles {
		want, err := charts.Shared(f)
		if err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(filepath.Join(charts.ChartDir, filepath.FromSlash(f.Chart)))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s/%s differs from templates/platform/%s; run `make generate` (never edit it by hand)", charts.ChartDir, f.Chart, f.Platform)
		}
	}
}

func TestLint(t *testing.T) {
	for _, values := range valuesFiles(t) {
		t.Run(filepath.Base(values), func(t *testing.T) {
			if out, err := runHelm(t, "lint", "--strict", charts.ChartDir, "--namespace", releaseNamespace, "--values", values); err != nil {
				t.Errorf("helm lint: %v\n%s", err, out)
			}
		})
	}
}

// TestTemplate compares helm template's output with golden files. Run it
// with -update to rewrite them after a deliberate change, and review the
// diff.
func TestTemplate(t *testing.T) {
	for _, values := range valuesFiles(t) {
		name := filepath.Base(values)
		t.Run(name, func(t *testing.T) {
			got, err := render(t, values)
			if err != nil {
				t.Fatalf("helm template: %v", err)
			}
			golden := filepath.Join("testdata", "golden", name)
			if *update {
				if err := os.WriteFile(golden, []byte(got), 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatalf("%v (run go test ./charts/ -run TestTemplate -update)", err)
			}
			if got != string(want) {
				t.Errorf("helm template output differs from %s; if the change is deliberate, run go test ./charts/ -run TestTemplate -update and review the diff\n%s",
					golden, firstDifference(string(want), got))
			}
		})
	}
}

// firstDifference shows the first line where two renders differ.
func firstDifference(want, got string) string {
	w, g := strings.Split(want, "\n"), strings.Split(got, "\n")
	for i := range max(len(w), len(g)) {
		var wl, gl string
		if i < len(w) {
			wl = w[i]
		}
		if i < len(g) {
			gl = g[i]
		}
		if wl != gl {
			return "line " + strconv.Itoa(i+1) + ":\n  want: " + wl + "\n  got:  " + gl
		}
	}
	return ""
}

func TestTemplateRejects(t *testing.T) {
	const pods = "kubernetes:\n  podCIDRs: [10.244.0.0/16]\n"
	tests := []struct {
		name, values, want string
	}{
		{"no pod CIDRs", "{}\n", "podCIDRs"},
		{"external MariaDB without an address", pods + "mariadb:\n  enabled: false\n", "address"},
		{"chart MariaDB with an address", pods + "mariadb:\n  address: db.example:3306\n", "address"},
		{"DNS provider without credentials", pods + "acme:\n  dnsProvider: cloudflare\n", "dnsCredentialsSecret"},
		{"unknown key", pods + "traefik:\n  replicas: 2\n", "replicas"},
		{"SFTP on 443", pods + "sftp:\n  port: 443\n", "port"},
		{"site prefix too long", "kubernetes:\n  podCIDRs: [10.244.0.0/16]\n  siteNamespacePrefix: " + strings.Repeat("a", 38) + "\n", "siteNamespacePrefix"},
		{"ClusterIP Traefik", pods + "traefik:\n  service:\n    type: ClusterIP\n", "type"},
		{"plain HTTP ACME server", pods + "acme:\n  caServer: http://ca.example/dir\n", "caServer"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			values := filepath.Join(t.TempDir(), "values.yaml")
			if err := os.WriteFile(values, []byte(tt.values), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := render(t, values)
			if err == nil {
				t.Fatal("helm template succeeded, want an error")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error doesn't mention %q:\n%v", tt.want, err)
			}
		})
	}
}

// TestPodsMeetRestricted checks that every pod the chart runs meets Pod
// Security "restricted", so the platform namespace can enforce it.
func TestPodsMeetRestricted(t *testing.T) {
	for _, values := range valuesFiles(t) {
		t.Run(filepath.Base(values), func(t *testing.T) {
			out, err := render(t, values)
			if err != nil {
				t.Fatalf("helm template: %v", err)
			}
			pods := 0
			for doc := range documents(t, out) {
				spec, ok := podSpec(doc)
				if !ok {
					continue
				}
				pods++
				name := doc.Kind + "/" + doc.Metadata.Name
				checkRestricted(t, name, spec)
			}
			if pods == 0 {
				t.Fatal("no pods rendered")
			}
		})
	}
}

// object is the part of a rendered object the tests read.
type object struct {
	Kind     string `yaml:"kind"`
	Metadata struct {
		Name string `yaml:"name"`
	} `yaml:"metadata"`
	Spec struct {
		// A Pod's spec is the pod itself.
		pod      `yaml:",inline"`
		Template struct {
			Spec *pod `yaml:"spec"`
		} `yaml:"template"`
	} `yaml:"spec"`
}

type pod struct {
	SecurityContext struct {
		RunAsNonRoot   *bool  `yaml:"runAsNonRoot"`
		RunAsUser      *int64 `yaml:"runAsUser"`
		SeccompProfile struct {
			Type string `yaml:"type"`
		} `yaml:"seccompProfile"`
	} `yaml:"securityContext"`
	InitContainers []container `yaml:"initContainers"`
	Containers     []container `yaml:"containers"`
	Volumes        []struct {
		Name     string `yaml:"name"`
		HostPath any    `yaml:"hostPath"`
	} `yaml:"volumes"`
	HostNetwork bool `yaml:"hostNetwork"`
	HostPID     bool `yaml:"hostPID"`
	HostIPC     bool `yaml:"hostIPC"`
}

type container struct {
	Name            string `yaml:"name"`
	SecurityContext struct {
		AllowPrivilegeEscalation *bool `yaml:"allowPrivilegeEscalation"`
		Capabilities             struct {
			Drop []string `yaml:"drop"`
			Add  []string `yaml:"add"`
		} `yaml:"capabilities"`
	} `yaml:"securityContext"`
}

func documents(t *testing.T, out string) iter.Seq[object] {
	return func(yield func(object) bool) {
		dec := yaml.NewDecoder(strings.NewReader(out))
		for {
			var o object
			err := dec.Decode(&o)
			if errors.Is(err, io.EOF) {
				return
			}
			if err != nil {
				t.Fatalf("decoding helm template output: %v", err)
			}
			if !yield(o) {
				return
			}
		}
	}
}

func podSpec(o object) (*pod, bool) {
	switch o.Kind {
	case "Deployment", "StatefulSet", "DaemonSet", "Job":
		return o.Spec.Template.Spec, o.Spec.Template.Spec != nil
	case "Pod":
		return &o.Spec.pod, true
	}
	return nil, false
}

func checkRestricted(t *testing.T, name string, p *pod) {
	t.Helper()
	sc := p.SecurityContext
	if sc.RunAsNonRoot == nil || !*sc.RunAsNonRoot || sc.RunAsUser == nil || *sc.RunAsUser == 0 {
		t.Errorf("%s: runAsNonRoot must be true, with a non-zero runAsUser", name)
	}
	if sc.SeccompProfile.Type != "RuntimeDefault" {
		t.Errorf("%s: seccompProfile must be RuntimeDefault, got %q", name, sc.SeccompProfile.Type)
	}
	if p.HostNetwork || p.HostPID || p.HostIPC {
		t.Errorf("%s: shares a host namespace", name)
	}
	for _, v := range p.Volumes {
		if v.HostPath != nil {
			t.Errorf("%s: volume %s is a hostPath", name, v.Name)
		}
	}
	for _, c := range append(p.InitContainers, p.Containers...) {
		csc := c.SecurityContext
		if csc.AllowPrivilegeEscalation == nil || *csc.AllowPrivilegeEscalation {
			t.Errorf("%s: container %s must set allowPrivilegeEscalation false", name, c.Name)
		}
		if len(csc.Capabilities.Drop) != 1 || csc.Capabilities.Drop[0] != "ALL" {
			t.Errorf("%s: container %s must drop ALL capabilities", name, c.Name)
		}
		if len(csc.Capabilities.Add) > 0 {
			t.Errorf("%s: container %s adds capabilities %v", name, c.Name, csc.Capabilities.Add)
		}
	}
}

// TestKubeVersionMatchesDoctor keeps the chart's minimum Kubernetes version,
// which the release notes advertise, equal to the one pressfleet doctor
// enforces. Bump both together.
func TestKubeVersionMatchesDoctor(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(charts.ChartDir, "Chart.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var chart struct {
		KubeVersion string `yaml:"kubeVersion"`
	}
	if err := yaml.Unmarshal(data, &chart); err != nil {
		t.Fatal(err)
	}
	// Read the constants from the source: the doctor package is internal to
	// control-plane/, which this package can't import.
	src, err := os.ReadFile(filepath.Join("..", "control-plane", "internal", "doctor", "cluster.go"))
	if err != nil {
		t.Fatal(err)
	}
	var parts [2]string
	for i, name := range []string{"MinKubernetesMajor", "MinKubernetesMinor"} {
		m := regexp.MustCompile(`\b` + name + `\s*=\s*(\d+)`).FindSubmatch(src)
		if m == nil {
			t.Fatalf("doctor/cluster.go has no %s constant", name)
		}
		parts[i] = string(m[1])
	}
	want := ">=" + parts[0] + "." + parts[1] + ".0-0"
	if chart.KubeVersion != want {
		t.Errorf("Chart.yaml kubeVersion is %q, but doctor's minimum makes it %q", chart.KubeVersion, want)
	}
}
