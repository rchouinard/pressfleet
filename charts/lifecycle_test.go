package charts_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

// lifecycleObject is the part of a rendered object the lifecycle tests
// read: hooks, labels, replicas, and the pod's identity, containers, and
// volumes.
type lifecycleObject struct {
	Kind     string `yaml:"kind"`
	Metadata struct {
		Name        string            `yaml:"name"`
		Labels      map[string]string `yaml:"labels"`
		Annotations map[string]string `yaml:"annotations"`
	} `yaml:"metadata"`
	Spec struct {
		// A Pod's spec is the pod itself.
		lifecyclePod `yaml:",inline"`
		Replicas     *int `yaml:"replicas"`
		Template     struct {
			Metadata struct {
				Labels map[string]string `yaml:"labels"`
			} `yaml:"metadata"`
			Spec lifecyclePod `yaml:"spec"`
		} `yaml:"template"`
	} `yaml:"spec"`
}

// lifecyclePod is the part of a pod spec the lifecycle tests read.
type lifecyclePod struct {
	ServiceAccountName string `yaml:"serviceAccountName"`
	Containers         []struct {
		Image   string   `yaml:"image"`
		Command []string `yaml:"command"`
		Args    []string `yaml:"args"`
		Env     []struct {
			Name string `yaml:"name"`
		} `yaml:"env"`
		VolumeMounts []struct {
			Name      string `yaml:"name"`
			MountPath string `yaml:"mountPath"`
		} `yaml:"volumeMounts"`
	} `yaml:"containers"`
	Volumes []struct {
		Name                  string `yaml:"name"`
		PersistentVolumeClaim struct {
			ClaimName string `yaml:"claimName"`
		} `yaml:"persistentVolumeClaim"`
	} `yaml:"volumes"`
}

// renderObjects renders the chart with values and returns its objects by
// kind/name.
func renderObjects(t *testing.T, values string) map[string]lifecycleObject {
	t.Helper()
	path := filepath.Join(t.TempDir(), "values.yaml")
	if err := os.WriteFile(path, []byte(values), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := render(t, path)
	if err != nil {
		t.Fatalf("helm template: %v", err)
	}
	objects := map[string]lifecycleObject{}
	for doc := range strings.SplitSeq(out, "\n---\n") {
		var o lifecycleObject
		if err := yaml.Unmarshal([]byte(doc), &o); err != nil {
			t.Fatalf("decoding helm template output: %v", err)
		}
		if o.Kind != "" {
			objects[o.Kind+"/"+o.Metadata.Name] = o
		}
	}
	return objects
}

const podCIDRs = "kubernetes:\n  podCIDRs: [10.244.0.0/16]\n"

func TestUpgradePreflightHook(t *testing.T) {
	objects := renderObjects(t, podCIDRs)
	job, ok := objects["Job/upgrade-preflight"]
	if !ok {
		t.Fatal("no upgrade-preflight Job")
	}
	if got := job.Metadata.Annotations["helm.sh/hook"]; got != "pre-upgrade" {
		t.Errorf("hook = %q, want pre-upgrade", got)
	}
	c := job.Spec.Template.Spec.Containers[0]
	if cp := objects["Deployment/control-plane"].Spec.Template.Spec.Containers[0]; c.Image != cp.Image {
		t.Errorf("preflight image %s, want the new control plane's %s", c.Image, cp.Image)
	}
	// helm template has no cluster to look up, so only the new MariaDB
	// image is known.
	if !slices.Equal(c.Args[:2], []string{"admin", "upgrade-preflight"}) ||
		!slices.Contains(c.Args, "--from-version=") || !slices.ContainsFunc(c.Args, func(a string) bool {
		return strings.HasPrefix(a, "--to-mariadb-image=") && strings.Contains(a, "mariadb:")
	}) {
		t.Errorf("args = %q", c.Args)
	}

	external := renderObjects(t, podCIDRs+"mariadb:\n  enabled: false\n  address: db.example:3306\n")
	if args := external["Job/upgrade-preflight"].Spec.Template.Spec.Containers[0].Args; !slices.Contains(args, "--to-mariadb-image=") {
		t.Errorf("with an external MariaDB, args = %q, want no MariaDB image to check", args)
	}
}

func TestRecovery(t *testing.T) {
	off := renderObjects(t, podCIDRs)
	if _, ok := off["Job/state-recover"]; ok {
		t.Error("state-recover Job rendered without recovery.enabled")
	}
	if r := off["Deployment/control-plane"].Spec.Replicas; r == nil || *r != 1 {
		t.Errorf("control plane replicas = %v, want 1", r)
	}

	on := renderObjects(t, podCIDRs+"recovery:\n  enabled: true\n  snapshot: 1a2b3c4d\n  force: true\n")
	if r := on["Deployment/control-plane"].Spec.Replicas; r == nil || *r != 0 {
		t.Errorf("control plane replicas during recovery = %v, want 0", r)
	}
	job, ok := on["Job/state-recover"]
	if !ok {
		t.Fatal("no state-recover Job with recovery.enabled")
	}
	if got := job.Metadata.Annotations["helm.sh/hook"]; got != "post-install,post-upgrade" {
		t.Errorf("hook = %q, want post-install,post-upgrade", got)
	}
	spec := job.Spec.Template.Spec
	if want := []string{"state", "recover", "--snapshot", "1a2b3c4d", "--force"}; !slices.Equal(spec.Containers[0].Args, want) {
		t.Errorf("args = %q, want %q", spec.Containers[0].Args, want)
	}
	// Its repository tasks are pods the driver starts as the control plane.
	if spec.ServiceAccountName != "control-plane" {
		t.Errorf("serviceAccountName = %q, want control-plane", spec.ServiceAccountName)
	}
	// The Service and the NetworkPolicies select the control plane by its
	// component; the Job must not be taken for it.
	if got := job.Spec.Template.Metadata.Labels["app.kubernetes.io/component"]; got == "control-plane" {
		t.Error("the Job's pod is labeled as the control plane")
	}
}

func TestRecoveryRejectsSnapshot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "values.yaml")
	if err := os.WriteFile(path, []byte(podCIDRs+"recovery:\n  enabled: true\n  snapshot: \"--force\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := render(t, path); err == nil || !strings.Contains(err.Error(), "snapshot") {
		t.Errorf("helm template = %v, want an error about the snapshot", err)
	}
}

// TestDoctorHelmTest checks the Helm test (§4.8): a pod that runs
// `pressfleet doctor` as the control plane would, kept until the next run.
func TestDoctorHelmTest(t *testing.T) {
	objects := renderObjects(t, podCIDRs+"config:\n  secretName: pressfleet-config\n")
	test, ok := objects["Pod/doctor-test"]
	if !ok {
		t.Fatal("no doctor-test Pod")
	}
	if got := test.Metadata.Annotations["helm.sh/hook"]; got != "test" {
		t.Errorf("hook = %q, want test", got)
	}
	// Creating the pod fails while the last run's is still there.
	if got := test.Metadata.Annotations["helm.sh/hook-delete-policy"]; !strings.Contains(got, "before-hook-creation") {
		t.Errorf("hook-delete-policy = %q, want before-hook-creation", got)
	}
	// The Service and the NetworkPolicies select the control plane by its
	// component; the test must not be taken for it.
	if got := test.Metadata.Labels["app.kubernetes.io/component"]; got == "control-plane" {
		t.Error("the test pod is labeled as the control plane")
	}

	pod, cp := test.Spec.lifecyclePod, objects["Deployment/control-plane"].Spec.Template.Spec
	// Doctor checks the control plane's RBAC, and starts its probe pods,
	// as the control plane.
	if pod.ServiceAccountName != cp.ServiceAccountName {
		t.Errorf("serviceAccountName = %q, want the control plane's %q", pod.ServiceAccountName, cp.ServiceAccountName)
	}
	c, want := pod.Containers[0], cp.Containers[0]
	if c.Image != want.Image {
		t.Errorf("image %s, want the control plane's %s", c.Image, want.Image)
	}
	if script := strings.Join(append(c.Command, c.Args...), " "); !strings.HasPrefix(script, "sh -c pressfleet doctor;") {
		t.Errorf("command %q, want pressfleet doctor", script)
	}
	if !slices.Equal(c.Env, want.Env) {
		t.Errorf("env %v, want the control plane's %v", c.Env, want.Env)
	}
	if !slices.Equal(c.VolumeMounts, want.VolumeMounts) || !slices.Equal(pod.Volumes, cp.Volumes) {
		t.Errorf("volumes %v mounted at %v, want the control plane's %v at %v", pod.Volumes, c.VolumeMounts, cp.Volumes, want.VolumeMounts)
	}
}
