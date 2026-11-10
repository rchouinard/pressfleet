package e2e_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/e2e"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner/compose"
)

// The chart's upgrade (design doc §4.7, §4.8; #224's carry-overs in #317).
// No release ships the chart yet, so there is no previous release to
// upgrade from: the test packages the tree's chart twice, stamped with two
// versions as the release pipeline stamps it, and builds a control plane
// image for each, reporting that version. The newer one is built with the
// e2emigration tag (store/migrate_e2e.go), so the upgrade migrates the
// state database, as an upgrade between releases usually does.

// The versions the test stamps its charts and control planes with.
const (
	k8sUpgradeFrom = "1.98.0"
	k8sUpgradeTo   = "1.99.0"
)

// TestKubernetesChartUpgrade installs one stamped chart and upgrades it to
// the next with helm upgrade: the upgrade-preflight hook passes, having
// looked up the running Deployment and StatefulSet, and the control plane
// migrates and comes back ready. A downgrade, and a change of MariaDB's
// release series, are refused before anything changes. Then the upgrade is
// undone as docs/upgrading.md says: the control plane scaled to zero, the
// PostgreSQL backup taken before the upgrade restored, and helm rollback.
func TestKubernetesChartUpgrade(t *testing.T) {
	if !e2e.KubernetesTests() {
		t.Skip("set " + e2e.EnvK8sTests + "=1 to run tests against a Kubernetes cluster (see make test-k8s-e2e)")
	}
	ctx := t.Context()
	fromChart, toChart := e2e.PackageChart(t, k8sUpgradeFrom), e2e.PackageChart(t, k8sUpgradeTo)
	fromImage := k8sControlPlaneImage(ctx, t, "v"+k8sUpgradeFrom, false)
	toImage := k8sControlPlaneImage(ctx, t, "v"+k8sUpgradeTo, true)
	controlPlane := func(image string) map[string]any {
		return map[string]any{"images": map[string]any{"controlPlane": image}}
	}

	p := e2e.StartKubernetes(t, e2e.KubeOptions{Chart: fromChart, Values: controlPlane(fromImage)})
	k := p.Kube
	checkK8sRelease(ctx, t, p, k8sUpgradeFrom, fromImage)
	mariadb := k.MustKubectl(t, "get", "statefulset", "mariadb", "-n", k.Namespace,
		"-o", `jsonpath={.spec.template.spec.containers[?(@.name=="mariadb")].image}`)

	// What the operator does first: a PostgreSQL backup.
	before := createGroup(t, p, "E2E Kubernetes Upgrade Before")
	schema := k.SchemaVersion(ctx, t)
	dump := k.DumpDatabase(ctx, t)

	t.Run("upgrade", func(t *testing.T) {
		if out, err := k.RunChart(ctx, t, "upgrade", toChart, controlPlane(toImage)); err != nil {
			t.Fatalf("helm upgrade from %s to %s: %v\n%s", k8sUpgradeFrom, k8sUpgradeTo, err, out)
		}
		// The hook passed (or helm upgrade would have failed), and it was
		// given what runs: Helm's lookup found the old release's
		// Deployment and StatefulSet. The release keeps the hook as
		// rendered.
		out, err := k.Helm(ctx, "get", "hooks", "pressfleet", "--namespace", k.Namespace)
		if err != nil {
			t.Fatal(err)
		}
		hooks := string(out)
		for _, want := range []string{
			"--from-version=" + k8sUpgradeFrom,
			"--from-mariadb-image=" + mariadb,
			"--to-mariadb-image=" + mariadb,
		} {
			if !strings.Contains(hooks, want) {
				t.Errorf("the upgrade-preflight hook wasn't given %s:\n%s", want, hooks)
			}
		}
		p.Connect(ctx, t)
		p.Client = p.Login(t, p.AdminEmail, p.AdminPassword)
		checkK8sRelease(ctx, t, p, k8sUpgradeTo, toImage)
		if got := k.SchemaVersion(ctx, t); got != schema+1 {
			t.Errorf("after the upgrade, the state database is at schema version %d, want %d", got, schema+1)
		}
		if !groupExists(t, p, before) {
			t.Error("the client group is gone after the upgrade")
		}
	})
	// Recorded after the upgrade: the rollback's restore loses it.
	after := createGroup(t, p, "E2E Kubernetes Upgrade After")

	t.Run("downgrade refused", func(t *testing.T) {
		checkK8sUpgradeRefused(ctx, t, p, fromChart, controlPlane(fromImage),
			"--from-version="+k8sUpgradeTo, "can't be downgraded")
	})
	t.Run("MariaDB series change refused", func(t *testing.T) {
		values := controlPlane(toImage)
		// Never pulled: the hook refuses before anything uses it.
		values["images"].(map[string]any)["mariadb"] = "docker.io/library/mariadb:11.8.3"
		checkK8sUpgradeRefused(ctx, t, p, toChart, values,
			"--from-mariadb-image="+mariadb, "release series")
	})

	t.Run("rollback", func(t *testing.T) {
		k.ScaleDownControlPlane(ctx, t)
		k.RestoreDatabase(ctx, t, dump)
		// Revision 1 is the install; the refused upgrades are revisions
		// of their own, so a bare rollback would go to the last of them.
		if out, err := k.Helm(ctx, "rollback", "pressfleet", "1", "--namespace", k.Namespace, "--wait", "--timeout", "10m"); err != nil {
			t.Fatalf("helm rollback: %v\n%s", err, out)
		}
		p.Connect(ctx, t)
		p.Client = p.Login(t, p.AdminEmail, p.AdminPassword)
		checkK8sRelease(ctx, t, p, k8sUpgradeFrom, fromImage)
		if got := k.SchemaVersion(ctx, t); got != schema {
			t.Errorf("after the rollback, the state database is at schema version %d, want %d", got, schema)
		}
		if !groupExists(t, p, before) {
			t.Error("the client group from before the upgrade is gone after the rollback")
		}
		if groupExists(t, p, after) {
			t.Error("the client group created after the upgrade survived restoring the backup from before it")
		}
	})
}

// checkK8sRelease checks that the control plane's Deployment is the
// chart's of version, on image, and that the control plane runs that
// version and is ready.
func checkK8sRelease(ctx context.Context, t *testing.T, p *e2e.Platform, version, image string) {
	t.Helper()
	k := p.Kube
	got := k.MustKubectl(t, "get", "deployment", "control-plane", "-n", k.Namespace, "-o",
		`jsonpath={.metadata.labels.app\.kubernetes\.io/version} {.spec.template.spec.containers[?(@.name=="control-plane")].image}`)
	if want := version + " " + image; got != want {
		t.Errorf("the control plane's Deployment is %q, want %q", got, want)
	}
	if info := kubeControlPlaneInfo(ctx, t, p); info.Version != "v"+version || !info.Ready {
		t.Errorf("the control plane runs %s (ready: %t), want v%s", info.Version, info.Ready, version)
	}
}

// checkK8sUpgradeRefused runs helm upgrade to chart with values and checks
// that the upgrade-preflight hook refused it, saying want, after being
// given arg, and that the release's workloads didn't change.
func checkK8sUpgradeRefused(ctx context.Context, t *testing.T, p *e2e.Platform, chart string, values map[string]any, arg, want string) {
	t.Helper()
	k := p.Kube
	workloads := func() string {
		return k.MustKubectl(t, "get", "deployments,statefulsets", "-n", k.Namespace, "-o",
			`jsonpath={range .items[*]}{.kind}/{.metadata.name} generation {.metadata.generation} `+
				`version {.metadata.labels.app\.kubernetes\.io/version} images {.spec.template.spec.containers[*].image}{"\n"}{end}`)
	}
	pods := func() string {
		return k.MustKubectl(t, "get", "pods", "-n", k.Namespace, "-l", "app.kubernetes.io/component=control-plane", "-o", "name")
	}
	workloadsBefore, podsBefore := workloads(), pods()
	out, err := k.RunChart(ctx, t, "upgrade", chart, values)
	if err == nil {
		t.Fatalf("helm upgrade wasn't refused:\n%s", out)
	}
	t.Logf("helm upgrade: %v", err)
	// The failed Job is kept, so that the operator can read why.
	logs := k.MustKubectl(t, "logs", "-n", k.Namespace, "job/upgrade-preflight")
	if !strings.Contains(logs, "upgrade refused") || !strings.Contains(logs, want) {
		t.Errorf("upgrade-preflight's log doesn't say the upgrade was refused (%q):\n%s", want, logs)
	}
	args := k.MustKubectl(t, "get", "job", "upgrade-preflight", "-n", k.Namespace, "-o", "jsonpath={.spec.template.spec.containers[0].args}")
	if !strings.Contains(args, arg) {
		t.Errorf("upgrade-preflight wasn't given %s: %s", arg, args)
	}
	if got := workloads(); got != workloadsBefore {
		t.Errorf("the refused upgrade changed the release's workloads:\n%s\nwas:\n%s", got, workloadsBefore)
	}
	if got := pods(); got != podsBefore {
		t.Errorf("the refused upgrade replaced the control plane's pod: %s, was %s", got, podsBefore)
	}
	if info := kubeControlPlaneInfo(ctx, t, p); !info.Ready {
		t.Errorf("after the refused upgrade, the control plane isn't ready: %+v", info)
	}
}

// kubeControlPlaneInfo asks the control plane for its version and readiness
// through its admin interface.
func kubeControlPlaneInfo(ctx context.Context, t *testing.T, p *e2e.Platform) adminInfo {
	t.Helper()
	out, err := p.ExecControlPlane(ctx, nil, "pressfleet", "admin", "info")
	if err != nil {
		t.Fatalf("pressfleet admin info: %v", err)
	}
	var info adminInfo
	if err := json.Unmarshal(out, &info); err != nil {
		t.Fatalf("pressfleet admin info: %v: %s", err, out)
	}
	return info
}

// groupExists reports whether a client group exists.
func groupExists(t *testing.T, p *e2e.Platform, groupID string) bool {
	t.Helper()
	resp, err := p.Client.GetClientGroupWithResponse(t.Context(), groupID)
	if err != nil {
		t.Fatal(err)
	}
	switch resp.StatusCode() {
	case http.StatusOK:
		return true
	case http.StatusNotFound:
		return false
	}
	t.Fatalf("get client group %s: status %d: %s", groupID, resp.StatusCode(), resp.Body)
	return false
}

// k8sControlPlaneImage builds a control plane image from the tree's, with
// this tree's binary reporting version, and with migration the
// e2emigration tag's extra migration, and pushes it to the cluster's
// registry. It returns the image as the cluster pulls it.
func k8sControlPlaneImage(ctx context.Context, t *testing.T, version string, migration bool) string {
	t.Helper()
	registry, push, tag := os.Getenv(e2e.EnvK8sRegistry), os.Getenv(e2e.EnvK8sPushRegistry), os.Getenv(e2e.EnvK8sImageTag)
	if registry == "" || push == "" || tag == "" {
		t.Fatal(e2e.EnvK8sRegistry + ", " + e2e.EnvK8sPushRegistry + ", and " + e2e.EnvK8sImageTag +
			" must name the registry and the control plane image's tag (see make test-k8s-e2e)")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	base := push + "/pressfleet/control-plane:" + tag
	pulled, err := compose.EnsureImage(ctx, nil, base)
	if err != nil {
		t.Fatal(err)
	}
	if pulled {
		t.Cleanup(func() { _ = compose.RemoveImage(context.Background(), nil, base) })
	}
	name := "control-plane:" + tag + "-" + version
	if migration {
		name += "-migration"
	}
	local := push + "/pressfleet/" + name
	bin := goBuildPressfleet(ctx, t, version, true, migration)
	dir := filepath.Dir(bin)
	dockerfile := fmt.Sprintf("FROM %s\nCOPY %s /usr/local/bin/pressfleet\n", base, filepath.Base(bin))
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(dockerfile), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := compose.BuildImageDir(ctx, nil, local, dir); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = compose.RemoveImage(context.Background(), nil, local) }()
	if _, err := compose.DockerExec(ctx, nil, "image", "push", "--quiet", local); err != nil {
		t.Fatalf("pushing %s: %v", local, err)
	}
	t.Logf("pushed control plane %s as %s", version, registry+"/pressfleet/"+name)
	return registry + "/pressfleet/" + name
}
