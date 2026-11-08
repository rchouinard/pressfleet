package kubernetes

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
)

// pullSecret is the install's image pull Secret in the platform namespace.
func pullSecret() *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "ghcr", Namespace: "pressfleet"},
		Type:       corev1.SecretTypeDockerConfigJson,
		Data:       map[string][]byte{corev1.DockerConfigJsonKey: []byte(`{"auths":{"ghcr.io":{"auth":"dXNlcjpwYXNz"}}}`)},
	}
}

// cluster is the driver on a fake cluster whose Deployments roll out at
// once.
func cluster(t *testing.T, mutate func(*Config), objs ...runtime.Object) *Provisioner {
	t.Helper()
	c, _ := fakeClient(t, append([]runtime.Object{pullSecret()}, objs...)...)
	rollOut(c)
	cfg := testConfig(t)
	if mutate != nil {
		mutate(&cfg)
	}
	return newWithClient(cfg, c)
}

func (p *Provisioner) testDeployment(t *testing.T) *appsv1.Deployment {
	t.Helper()
	d, err := p.typed.AppsV1().Deployments(p.SiteNamespace(testSiteID)).Get(t.Context(), SiteDeployment, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func wantPhase(t *testing.T, p *Provisioner, want provisioner.SitePhase) {
	t.Helper()
	st, err := p.GetSite(t.Context(), testSiteID)
	if err != nil {
		t.Fatal(err)
	}
	if st.Phase != want || st.ID != testSiteID {
		t.Errorf("GetSite = %+v, want phase %s", st, want)
	}
}

func TestSiteLifecycle(t *testing.T) {
	p := cluster(t, nil)
	ctx := t.Context()
	cfg := testSite()
	ns := p.SiteNamespace(testSiteID)

	st, err := p.CreateSite(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if st.Phase != provisioner.SiteRunning {
		t.Errorf("CreateSite = %+v, want running", st)
	}
	// Everything the Compose project has, in the site's namespace.
	core, apps := p.typed.CoreV1(), p.typed.AppsV1()
	namespace, err := core.Namespaces().Get(ctx, ns, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if namespace.Labels["pod-security.kubernetes.io/enforce"] != "restricted" {
		t.Errorf("namespace labels %v: Pod Security restricted isn't enforced", namespace.Labels)
	}
	if got := namespace.Labels[PlatformNamespaceLabel]; got != "pressfleet" {
		t.Errorf("namespace label %s = %q, want the platform namespace", PlatformNamespaceLabel, got)
	}
	checks := map[string]func() error{
		"service": func() error { _, err := core.Services(ns).Get(ctx, "nginx", metav1.GetOptions{}); return err },
		"volume": func() error {
			_, err := core.PersistentVolumeClaims(ns).Get(ctx, "wp-content", metav1.GetOptions{})
			return err
		},
		"wp-config":   func() error { _, err := core.Secrets(ns).Get(ctx, "wp-config", metav1.GetOptions{}); return err },
		"pull secret": func() error { _, err := core.Secrets(ns).Get(ctx, "ghcr", metav1.GetOptions{}); return err },
		"role binding": func() error {
			_, err := p.typed.RbacV1().RoleBindings(ns).Get(ctx, "pressfleet-control-plane", metav1.GetOptions{})
			return err
		},
	}
	for name, check := range checks {
		if err := check(); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	policies, err := p.typed.NetworkingV1().NetworkPolicies(ns).List(ctx, metav1.ListOptions{})
	if err != nil || len(policies.Items) != 3 {
		t.Errorf("network policies: %d, %v; want 3", len(policies.Items), err)
	}
	if d := p.testDeployment(t); replicas(d) != 1 {
		t.Errorf("a new site has %d pods, want 1", replicas(d))
	}

	// Creating it again converges.
	if _, err := p.CreateSite(ctx, cfg); err != nil {
		t.Errorf("CreateSite again: %v", err)
	}

	if err := p.StopSite(ctx, testSiteID); err != nil {
		t.Fatal(err)
	}
	wantPhase(t, p, provisioner.SiteStopped)
	if err := p.StopSite(ctx, testSiteID); err != nil {
		t.Errorf("StopSite again: %v", err)
	}

	// Updating a stopped site keeps it stopped: the manifests leave the
	// number of pods alone.
	cfg.PHPVersion = "8.3"
	if st, err := p.UpdateSite(ctx, cfg); err != nil || st.Phase != provisioner.SiteStopped {
		t.Fatalf("UpdateSite = %+v, %v; want stopped", st, err)
	}
	d := p.testDeployment(t)
	if replicas(d) != 0 {
		t.Errorf("UpdateSite started a stopped site: %d pods", replicas(d))
	}
	if img := d.Spec.Template.Spec.Containers[0].Image; !strings.Contains(img, "php8.3") {
		t.Errorf("php image %s after the update to PHP 8.3", img)
	}

	if err := p.StartSite(ctx, testSiteID); err != nil {
		t.Fatal(err)
	}
	wantPhase(t, p, provisioner.SiteRunning)

	// CreateSite starts a stopped site.
	if err := p.StopSite(ctx, testSiteID); err != nil {
		t.Fatal(err)
	}
	if st, err := p.CreateSite(ctx, cfg); err != nil || st.Phase != provisioner.SiteRunning {
		t.Errorf("CreateSite of a stopped site = %+v, %v; want running", st, err)
	}

	siteDir, _ := p.layout.SiteDir(testSiteID)
	if err := p.DeleteSite(ctx, testSiteID); err != nil {
		t.Fatal(err)
	}
	if _, err := core.Namespaces().Get(ctx, ns, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("the namespace is still there: %v", err)
	}
	if _, err := os.Stat(siteDir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the site's directory is still there: %v", err)
	}
	if err := p.DeleteSite(ctx, testSiteID); err != nil {
		t.Errorf("DeleteSite again: %v", err)
	}

	// The fake doesn't delete what was in the namespace, as the API
	// server does; the site is gone with its Deployment.
	if err := apps.Deployments(ns).Delete(ctx, SiteDeployment, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	for name, op := range map[string]func() error{
		"GetSite":    func() error { _, err := p.GetSite(ctx, testSiteID); return err },
		"UpdateSite": func() error { _, err := p.UpdateSite(ctx, cfg); return err },
		"StartSite":  func() error { return p.StartSite(ctx, testSiteID) },
		"StopSite":   func() error { return p.StopSite(ctx, testSiteID) },
	} {
		if err := op(); !errors.Is(err, provisioner.ErrSiteNotFound) {
			t.Errorf("%s of a deleted site = %v, want ErrSiteNotFound", name, err)
		}
	}
}

// A change to wp-config.php rolls the pod: the Secret is mounted as one
// file, which a running container never sees change.
func TestUpdateSiteRollsWPConfig(t *testing.T) {
	p := cluster(t, nil)
	cfg := testSite()
	if _, err := p.CreateSite(t.Context(), cfg); err != nil {
		t.Fatal(err)
	}
	const key = "pressfleet.io/wp-config-checksum"
	before := p.testDeployment(t).Spec.Template.Annotations[key]
	if _, err := p.UpdateSite(t.Context(), cfg); err != nil {
		t.Fatal(err)
	}
	if again := p.testDeployment(t).Spec.Template.Annotations[key]; again != before {
		t.Errorf("an update without changes changed the checksum: %s, then %s", before, again)
	}
	cfg.Environment, cfg.DisableMail = provisioner.EnvironmentStaging, true
	if _, err := p.UpdateSite(t.Context(), cfg); err != nil {
		t.Fatal(err)
	}
	if after := p.testDeployment(t).Spec.Template.Annotations[key]; after == before {
		t.Error("the staging environment didn't change the pod template")
	}
	secret, err := p.typed.CoreV1().Secrets(p.SiteNamespace(testSiteID)).Get(t.Context(), "wp-config", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(secret.Data["wp-config.php"]), "'WP_ENVIRONMENT_TYPE', 'staging'") {
		t.Error("the Secret doesn't have the staging wp-config.php")
	}
}

// Objects the manifests no longer render are deleted; objects the driver
// didn't render are left alone.
func TestUpdateSitePrunes(t *testing.T) {
	p := cluster(t, nil)
	ctx := t.Context()
	if _, err := p.CreateSite(ctx, testSite()); err != nil {
		t.Fatal(err)
	}
	ns := p.SiteNamespace(testSiteID)
	core := p.typed.CoreV1()
	rendered := map[string]string{RenderedLabel: "true", managedByLabel: FieldManager}
	for _, s := range []*corev1.Secret{
		{ObjectMeta: metav1.ObjectMeta{Name: "dropped", Namespace: ns, Labels: rendered}},
		{ObjectMeta: metav1.ObjectMeta{Name: "task-secrets", Namespace: ns, Labels: map[string]string{managedByLabel: FieldManager}}},
	} {
		if _, err := core.Secrets(ns).Create(ctx, s, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := p.UpdateSite(ctx, testSite()); err != nil {
		t.Fatal(err)
	}
	if _, err := core.Secrets(ns).Get(ctx, "dropped", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("an object no template renders is still there: %v", err)
	}
	for _, name := range []string{"task-secrets", "wp-config", "ghcr"} {
		if _, err := core.Secrets(ns).Get(ctx, name, metav1.GetOptions{}); err != nil {
			t.Errorf("secret %s was deleted: %v", name, err)
		}
	}
}

func TestPullSecrets(t *testing.T) {
	ctx := t.Context()
	p := cluster(t, nil)
	if _, err := p.CreateSite(ctx, testSite()); err != nil {
		t.Fatal(err)
	}
	ns := p.SiteNamespace(testSiteID)
	cp, err := p.typed.CoreV1().Secrets(ns).Get(ctx, "ghcr", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if cp.Type != corev1.SecretTypeDockerConfigJson || string(cp.Data[corev1.DockerConfigJsonKey]) != string(pullSecret().Data[corev1.DockerConfigJsonKey]) {
		t.Errorf("the copy is %s %q", cp.Type, cp.Data)
	}

	// Once the install no longer names it, the copy goes.
	p.pullSecrets = nil
	if _, err := p.UpdateSite(ctx, testSite()); err != nil {
		t.Fatal(err)
	}
	if _, err := p.typed.CoreV1().Secrets(ns).Get(ctx, "ghcr", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("the copy of a pull secret no longer configured is still there: %v", err)
	}

	// A pull secret that doesn't exist fails the site.
	p.pullSecrets = []string{"missing"}
	if _, err := p.UpdateSite(ctx, testSite()); !apierrors.IsNotFound(err) {
		t.Errorf("UpdateSite with a missing pull secret = %v, want not found", err)
	}
}

func TestDeleteSiteRefusesForeignNamespace(t *testing.T) {
	tests := map[string]map[string]string{
		"not PressFleet's": nil,
		// Another install with the same site namespace prefix (K6).
		"another install's": {managedByLabel: FieldManager, siteIDLabel: testSiteID, PlatformNamespaceLabel: "other"},
	}
	for name, labels := range tests {
		t.Run(name, func(t *testing.T) {
			ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "pf-" + testSiteID, Labels: labels}}
			p := cluster(t, nil, ns)
			if err := p.DeleteSite(t.Context(), testSiteID); !errors.Is(err, ErrInvalidSite) {
				t.Errorf("DeleteSite = %v, want ErrInvalidSite", err)
			}
			if _, err := p.typed.CoreV1().Namespaces().Get(t.Context(), ns.Name, metav1.GetOptions{}); err != nil {
				t.Errorf("the namespace was deleted: %v", err)
			}
		})
	}
}

// Manifests rendered before the platform namespace label existed, such as
// a render snapshot, get it when applied: taking it off is refused by the
// chart's admission policy.
func TestLabelNamespaces(t *testing.T) {
	p := cluster(t, nil)
	objs, err := DecodeManifests([]byte(`apiVersion: v1
kind: Namespace
metadata:
  name: pf-site
  labels:
    app.kubernetes.io/managed-by: pressfleet
---
apiVersion: v1
kind: Namespace
metadata:
  name: pf-other
  labels:
    pressfleet.io/platform-namespace: other
---
apiVersion: v1
kind: Service
metadata:
  name: nginx
  namespace: pf-site
`))
	if err != nil {
		t.Fatal(err)
	}
	p.labelNamespaces(objs)
	for _, obj := range objs[:2] {
		if got := obj.GetLabels()[PlatformNamespaceLabel]; got != "pressfleet" {
			t.Errorf("namespace %s: label %q, want pressfleet", obj.GetName(), got)
		}
	}
	if objs[0].GetLabels()[managedByLabel] != FieldManager {
		t.Errorf("labelNamespaces dropped a label: %v", objs[0].GetLabels())
	}
	if _, ok := objs[2].GetLabels()[PlatformNamespaceLabel]; ok {
		t.Errorf("the Service got the namespace label: %v", objs[2].GetLabels())
	}
}

// Waiting gives up after the wait timeout, and says what the pod is
// waiting for.
func TestCreateSiteTimesOut(t *testing.T) {
	c, _ := fakeClient(t, pullSecret(), &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "site-1", Namespace: "pf-" + testSiteID, Labels: map[string]string{ComponentLabel: SiteComponent}},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
			Name:  "php",
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff", Message: "manifest unknown"}},
		}}},
	})
	cfg := testConfig(t)
	cfg.WaitTimeout = 50 * time.Millisecond
	p := newWithClient(cfg, c)
	_, err := p.CreateSite(t.Context(), testSite())
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "php: ImagePullBackOff: manifest unknown") {
		t.Errorf("CreateSite = %v, want a timeout naming the pull failure", err)
	}
}

func TestSitePhase(t *testing.T) {
	running := appsv1.DeploymentStatus{Replicas: 1, UpdatedReplicas: 1, AvailableReplicas: 1}
	dep := func(n int32, s appsv1.DeploymentStatus) *appsv1.Deployment {
		return &appsv1.Deployment{Spec: appsv1.DeploymentSpec{Replicas: &n}, Status: s}
	}
	pod := func(statuses ...corev1.ContainerStatus) corev1.Pod {
		return corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: statuses}}
	}
	ready := corev1.ContainerStatus{Name: "nginx", Ready: true, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}
	tests := []struct {
		name  string
		d     *appsv1.Deployment
		pods  []corev1.Pod
		phase provisioner.SitePhase
		msg   string
	}{
		{"running", dep(1, running), []corev1.Pod{pod(ready)}, provisioner.SiteRunning, ""},
		{"stopped", dep(0, appsv1.DeploymentStatus{}), nil, provisioner.SiteStopped, "site is stopped"},
		{"stopping", dep(0, appsv1.DeploymentStatus{}), []corev1.Pod{pod(ready)}, provisioner.SitePending, "stopping"},
		{"starting", dep(1, appsv1.DeploymentStatus{Replicas: 1, UpdatedReplicas: 1}), []corev1.Pod{pod(ready,
			corev1.ContainerStatus{Name: "php", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}})}, provisioner.SitePending, "php is starting"},
		{"pulling", dep(1, appsv1.DeploymentStatus{Replicas: 1, UpdatedReplicas: 1}), []corev1.Pod{pod(
			corev1.ContainerStatus{Name: "php", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ContainerCreating"}}})}, provisioner.SitePending, "php is ContainerCreating"},
		{"no pod yet", dep(1, appsv1.DeploymentStatus{}), nil, provisioner.SitePending, "0 of 1 pods updated"},
		{"crashing", dep(1, appsv1.DeploymentStatus{Replicas: 1, UpdatedReplicas: 1}), []corev1.Pod{pod(ready,
			corev1.ContainerStatus{Name: "php", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff", Message: "back-off 5m0s"}}})},
			provisioner.SiteFailed, "php: CrashLoopBackOff: back-off 5m0s"},
		{"unschedulable", dep(1, appsv1.DeploymentStatus{Replicas: 1, UpdatedReplicas: 1}), []corev1.Pod{{Status: corev1.PodStatus{
			Phase:      corev1.PodPending,
			Conditions: []corev1.PodCondition{{Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Message: "0/3 nodes are available: insufficient memory"}},
		}}}, provisioner.SitePending, "insufficient memory"},
		{"deadline", dep(1, appsv1.DeploymentStatus{Conditions: []appsv1.DeploymentCondition{{
			Type: appsv1.DeploymentProgressing, Reason: "ProgressDeadlineExceeded", Message: "timed out",
		}}}), nil, provisioner.SiteFailed, "timed out"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			phase, msg := sitePhase(tt.d, tt.pods)
			if phase != tt.phase || !strings.Contains(msg, tt.msg) || (tt.msg == "" && msg != "") {
				t.Errorf("sitePhase = %s, %q; want %s, %q", phase, msg, tt.phase, tt.msg)
			}
		})
	}
}

func TestRenderSnapshots(t *testing.T) {
	p := cluster(t, nil)
	ctx := t.Context()
	if err := p.SnapshotRender(ctx, testSiteID); !errors.Is(err, provisioner.ErrSiteNotFound) {
		t.Errorf("SnapshotRender of a site never rendered = %v, want ErrSiteNotFound", err)
	}
	cfg := testSite()
	if _, err := p.CreateSite(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := p.RestoreRender(ctx, testSiteID); !errors.Is(err, provisioner.ErrNoRenderSnapshot) {
		t.Errorf("RestoreRender without a snapshot = %v, want ErrNoRenderSnapshot", err)
	}
	if err := p.SnapshotRender(ctx, testSiteID); err != nil {
		t.Fatal(err)
	}
	old := p.testDeployment(t).Spec.Template.Spec.Containers[0].Image

	// A render the snapshot doesn't have: a new image, and an object only
	// it renders.
	cfg.PHPVersion = "8.3"
	if _, err := p.UpdateSite(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	dir, _ := p.manifestDir(testSiteID)
	extra := "apiVersion: v1\nkind: Service\nmetadata:\n  name: extra\n  namespace: pf-" + testSiteID + "\n  labels:\n    pressfleet.io/rendered: \"true\"\n"
	if err := os.WriteFile(filepath.Join(dir, "extra.yaml"), []byte(extra), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := p.ApplyDir(ctx, dir); err != nil {
		t.Fatal(err)
	}

	st, err := p.RestoreRender(ctx, testSiteID)
	if err != nil {
		t.Fatal(err)
	}
	if st.Phase != provisioner.SiteRunning {
		t.Errorf("RestoreRender = %+v, want running", st)
	}
	if img := p.testDeployment(t).Spec.Template.Spec.Containers[0].Image; img != old {
		t.Errorf("php image %s after the restore, want %s", img, old)
	}
	if _, err := os.Stat(filepath.Join(dir, "extra.yaml")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a manifest the snapshot doesn't have is still there: %v", err)
	}
	if _, err := p.typed.CoreV1().Services(p.SiteNamespace(testSiteID)).Get(ctx, "extra", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("an object the snapshot doesn't have is still there: %v", err)
	}
	// The snapshot stays, so restoring again works; a stopped site stays
	// stopped.
	if err := p.StopSite(ctx, testSiteID); err != nil {
		t.Fatal(err)
	}
	if st, err := p.RestoreRender(ctx, testSiteID); err != nil || st.Phase != provisioner.SiteStopped {
		t.Errorf("RestoreRender again = %+v, %v; want stopped", st, err)
	}
}
