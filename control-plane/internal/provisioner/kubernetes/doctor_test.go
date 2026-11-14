package kubernetes

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	authv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/version"
	fakediscovery "k8s.io/client-go/discovery/fake"
	kubefake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/rchouinard/pressfleet/control-plane/internal/config"
	"github.com/rchouinard/pressfleet/control-plane/internal/doctor"
)

func testProbe(t *testing.T, image string, objs ...runtime.Object) (*ClusterProbe, *kubefake.Clientset) {
	t.Helper()
	c, _ := fakeClient(t, objs...)
	p := newClusterProbe(c, ProbeConfig{Kubernetes: config.DefaultKubernetes(), Image: image})
	p.podTimeout, p.settle = 2*time.Second, 0
	return p, c.typed.(*kubefake.Clientset)
}

func node(name, arch string, ready bool, mem string) *corev1.Node {
	status := corev1.ConditionFalse
	if ready {
		status = corev1.ConditionTrue
	}
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: corev1.NodeStatus{
			NodeInfo:    corev1.NodeSystemInfo{Architecture: arch, OperatingSystem: "linux"},
			Conditions:  []corev1.NodeCondition{{Type: corev1.NodeReady, Status: status}},
			Allocatable: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse(mem)},
		},
	}
}

func TestProbeServerVersion(t *testing.T) {
	p, cs := testProbe(t, "")
	cs.Discovery().(*fakediscovery.FakeDiscovery).FakedServerVersion = &version.Info{Major: "1", Minor: "34+", GitVersion: "v1.34.2-eks-1"}
	v, err := p.ServerVersion(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if v.Major != 1 || v.Minor != 34 || v.Git != "v1.34.2-eks-1" {
		t.Errorf("version = %+v", v)
	}
}

func TestProbeNodes(t *testing.T) {
	cordoned := node("c", "amd64", true, "1Gi")
	cordoned.Spec.Unschedulable = true
	p, _ := testProbe(t, "", node("a", "amd64", true, "8Gi"), node("b", "arm64", false, "4Gi"), cordoned)
	nodes, err := p.Nodes(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	want := []doctor.ClusterNode{
		{Name: "a", OS: "linux", Arch: "amd64", Ready: true, AllocatableMemory: 8 << 30},
		{Name: "b", OS: "linux", Arch: "arm64", AllocatableMemory: 4 << 30},
		{Name: "c", OS: "linux", Arch: "amd64", Ready: true, Unschedulable: true, AllocatableMemory: 1 << 30},
	}
	if !slices.Equal(nodes, want) {
		t.Errorf("nodes = %+v, want %+v", nodes, want)
	}
}

// reviews answers SelfSubjectAccessReviews: allow decides each right.
func reviews(cs *kubefake.Clientset, allow func(*authv1.ResourceAttributes) bool) {
	cs.PrependReactor("create", "selfsubjectaccessreviews", func(a k8stesting.Action) (bool, runtime.Object, error) {
		r := a.(k8stesting.CreateAction).GetObject().(*authv1.SelfSubjectAccessReview)
		r.Status.Allowed = allow(r.Spec.ResourceAttributes)
		return true, r, nil
	})
}

func TestProbeRBAC(t *testing.T) {
	tests := []struct {
		name        string
		allow       func(*authv1.ResourceAttributes) bool
		wantMissing []string
		wantExcess  []string
	}{
		{
			name: "least privilege",
			allow: func(a *authv1.ResourceAttributes) bool {
				// Everything the driver needs, nothing cluster-wide beyond it.
				return !slices.ContainsFunc(forbidden(), func(r right) bool {
					return r.verb == a.Verb && r.resource == a.Resource && r.subresource == a.Subresource && r.group == a.Group && r.namespace == a.Namespace
				})
			},
		},
		{
			name:        "nothing allowed",
			allow:       func(*authv1.ResourceAttributes) bool { return false },
			wantMissing: []string{"create namespaces", "delete namespaces", "create pods in namespace pressfleet", "get pods/log in namespace pressfleet", "list pods.metrics.k8s.io in namespace pressfleet"},
		},
		{
			name:  "cluster-admin",
			allow: func(*authv1.ResourceAttributes) bool { return true },
			wantExcess: []string{
				"get secrets in all namespaces", "list secrets in all namespaces", "get nodes/proxy",
				"create pods/exec in all namespaces", "create clusterrolebindings.rbac.authorization.k8s.io",
				"escalate clusterroles.rbac.authorization.k8s.io",
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p, cs := testProbe(t, "")
			reviews(cs, tc.allow)
			rep, err := p.RBAC(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			for _, w := range tc.wantMissing {
				if !slices.Contains(rep.Missing, w) {
					t.Errorf("missing = %q, want it to contain %q", rep.Missing, w)
				}
			}
			if tc.wantMissing == nil && len(rep.Missing) > 0 {
				t.Errorf("missing = %q, want none", rep.Missing)
			}
			if !slices.Equal(rep.Excess, tc.wantExcess) {
				t.Errorf("excess = %q, want %q", rep.Excess, tc.wantExcess)
			}
		})
	}
}

func TestProbeRBACError(t *testing.T) {
	p, cs := testProbe(t, "")
	cs.PrependReactor("create", "selfsubjectaccessreviews", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("authorization is down")
	})
	if _, err := p.RBAC(t.Context()); err == nil {
		t.Error("RBAC succeeded without the authorization API")
	}
}

func TestProbeStorageClass(t *testing.T) {
	std := &storagev1.StorageClass{
		ObjectMeta:  metav1.ObjectMeta{Name: "standard", Annotations: map[string]string{defaultClassAnnotation: "true"}},
		Provisioner: "example.com/rwo",
	}
	nfs := &storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: "nfs"}, Provisioner: "nfs.csi.k8s.io"}
	p, _ := testProbe(t, "", std, nfs)
	for _, tc := range []struct {
		name, want string
		found      bool
	}{
		{"", "standard", true},
		{"nfs", "nfs", true},
		{"gone", "", false},
	} {
		info, found, err := p.StorageClass(t.Context(), tc.name)
		if err != nil || found != tc.found || info.Name != tc.want {
			t.Errorf("StorageClass(%q) = %+v, %v, %v; want %q, %v", tc.name, info, found, err, tc.want, tc.found)
		}
	}

	p, _ = testProbe(t, "", nfs)
	if _, found, err := p.StorageClass(t.Context(), ""); err != nil || found {
		t.Errorf("without a default class: found = %v, err = %v", found, err)
	}
}

func TestProbeMetricsAPI(t *testing.T) {
	p, cs := testProbe(t, "")
	if err := p.MetricsAPI(t.Context()); err == nil {
		t.Error("MetricsAPI succeeded without metrics.k8s.io")
	}
	cs.Discovery().(*fakediscovery.FakeDiscovery).Resources = []*metav1.APIResourceList{{
		GroupVersion: "metrics.k8s.io/v1beta1",
		APIResources: []metav1.APIResource{{Name: "nodes"}, {Name: "pods"}},
	}}
	if err := p.MetricsAPI(t.Context()); err != nil {
		t.Errorf("MetricsAPI = %v", err)
	}
}

func TestProbeTraefikAddresses(t *testing.T) {
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "traefik", Namespace: "pressfleet"},
		Spec:       corev1.ServiceSpec{ExternalIPs: []string{"192.0.2.7"}},
		Status: corev1.ServiceStatus{LoadBalancer: corev1.LoadBalancerStatus{Ingress: []corev1.LoadBalancerIngress{
			{IP: "198.51.100.20"}, {Hostname: "lb.example.net"},
		}}},
	}
	p, _ := testProbe(t, "", svc)
	got, err := p.TraefikAddresses(t.Context())
	if err != nil || !slices.Equal(got, []string{"198.51.100.20", "lb.example.net", "192.0.2.7"}) {
		t.Errorf("addresses = %q, %v", got, err)
	}
	pending := svc.DeepCopy()
	pending.Status = corev1.ServiceStatus{}
	pending.Spec.ExternalIPs = nil
	p, _ = testProbe(t, "", pending)
	if got, err := p.TraefikAddresses(t.Context()); err != nil || len(got) != 0 {
		t.Errorf("pending service: addresses = %q, %v", got, err)
	}
	p, _ = testProbe(t, "")
	if _, err := p.TraefikAddresses(t.Context()); err == nil {
		t.Error("no Service is no error")
	}
}

func TestProbePlatformMemoryRequests(t *testing.T) {
	pod := func(name string, phase corev1.PodPhase, mem ...string) *corev1.Pod {
		p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "pressfleet"}, Status: corev1.PodStatus{Phase: phase}}
		for _, m := range mem {
			p.Spec.Containers = append(p.Spec.Containers, corev1.Container{Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse(m)},
			}})
		}
		return p
	}
	other := pod("elsewhere", corev1.PodRunning, "1Gi")
	other.Namespace = "other"
	p, _ := testProbe(t, "",
		pod("a", corev1.PodRunning, "256Mi", "64Mi"),
		pod("b", corev1.PodPending, "128Mi"),
		pod("done", corev1.PodSucceeded, "1Gi"),
		other,
	)
	got, err := p.PlatformMemoryRequests(t.Context())
	if err != nil || got != 448<<20 {
		t.Errorf("requests = %d, %v; want %d", got, err, 448<<20)
	}
}

// podResults ends each probe pod the moment it is created, as the kubelet
// would after running its script: phases maps a pod's name to its phase.
func podResults(cs *kubefake.Clientset, phases map[string]corev1.PodPhase) {
	cs.PrependReactor("create", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
		pod := a.(k8stesting.CreateAction).GetObject().(*corev1.Pod)
		if phase, ok := phases[pod.Name]; ok {
			pod.Status.Phase = phase
		}
		return false, nil, nil // the tracker stores it
	})
}

// recordNamespaces remembers the namespaces created, and the policies and
// pods in them, because the probe deletes its namespace when it is done.
func recordCreated(cs *kubefake.Clientset) *[]string {
	var created []string
	cs.PrependReactor("create", "*", func(a k8stesting.Action) (bool, runtime.Object, error) {
		obj := a.(k8stesting.CreateAction).GetObject()
		if m, ok := obj.(metav1.Object); ok {
			created = append(created, a.GetResource().Resource+"/"+m.GetName())
		}
		return false, nil, nil
	})
	return &created
}

func TestProbeNetworkPolicy(t *testing.T) {
	tests := []struct {
		name   string
		phases map[string]corev1.PodPhase
		want   doctor.NetworkPolicyState
	}{
		{"denied pod blocked", map[string]corev1.PodPhase{"allowed": corev1.PodSucceeded, "denied": corev1.PodSucceeded}, doctor.NetworkPolicyEnforced},
		{"denied pod connected", map[string]corev1.PodPhase{"allowed": corev1.PodSucceeded, "denied": corev1.PodFailed}, doctor.NetworkPolicyIgnored},
		{"control pod can't connect", map[string]corev1.PodPhase{"allowed": corev1.PodFailed, "denied": corev1.PodSucceeded}, doctor.NetworkPolicyInconclusive},
		{"nothing finishes", nil, doctor.NetworkPolicyInconclusive},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p, cs := testProbe(t, "ghcr.io/example/control-plane:v1")
			p.podTimeout = 100 * time.Millisecond
			podResults(cs, tc.phases)
			created := recordCreated(cs)
			res, err := p.NetworkPolicy(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if res.State != tc.want {
				t.Errorf("state = %s (%s), want %s", res.State, res.Detail, tc.want)
			}
			if tc.want == doctor.NetworkPolicyInconclusive && res.Detail == "" {
				t.Error("an inconclusive result says nothing about why")
			}
			var ns, policy string
			for _, c := range *created {
				switch {
				case strings.HasPrefix(c, "namespaces/"):
					ns = strings.TrimPrefix(c, "namespaces/")
				case strings.HasPrefix(c, "networkpolicies/"):
					policy = c
				}
			}
			if !strings.HasPrefix(ns, "pressfleet-doctor-") || policy != "networkpolicies/deny-egress" {
				t.Errorf("created %q", *created)
			}
			// The probe cleans up whatever the result.
			if list, _ := cs.CoreV1().Namespaces().List(t.Context(), metav1.ListOptions{}); len(list.Items) != 0 {
				t.Errorf("probe namespaces left: %v", list.Items)
			}
		})
	}
}

func TestProbeNetworkPolicyDeniesTheDeniedPodOnly(t *testing.T) {
	p, cs := testProbe(t, "img")
	podResults(cs, map[string]corev1.PodPhase{"allowed": corev1.PodSucceeded, "denied": corev1.PodSucceeded})
	var policy *networkingv1.NetworkPolicy
	var pods []*corev1.Pod
	cs.PrependReactor("create", "*", func(a k8stesting.Action) (bool, runtime.Object, error) {
		switch o := a.(k8stesting.CreateAction).GetObject().(type) {
		case *networkingv1.NetworkPolicy:
			policy = o.DeepCopy()
		case *corev1.Pod:
			pods = append(pods, o.DeepCopy())
		}
		return false, nil, nil
	})
	if _, err := p.NetworkPolicy(t.Context()); err != nil {
		t.Fatal(err)
	}
	if policy == nil || len(pods) != 2 {
		t.Fatalf("policy = %v, pods = %d", policy, len(pods))
	}
	if policy.Spec.PodSelector.MatchLabels["role"] != "denied" || len(policy.Spec.Egress) != 0 ||
		!slices.Equal(policy.Spec.PolicyTypes, []networkingv1.PolicyType{networkingv1.PolicyTypeEgress}) {
		t.Errorf("policy = %+v, want deny-all egress for role=denied", policy.Spec)
	}
	for _, pod := range pods {
		// Pod Security restricted, so the probe runs in a hardened namespace.
		c := pod.Spec.Containers[0]
		if pod.Spec.SecurityContext.RunAsNonRoot == nil || !*pod.Spec.SecurityContext.RunAsNonRoot ||
			*c.SecurityContext.AllowPrivilegeEscalation || c.Image != "img" || *pod.Spec.AutomountServiceAccountToken {
			t.Errorf("pod %s is not restricted: %+v", pod.Name, pod.Spec)
		}
	}
}

func TestProbeNetworkPolicyNeedsAnImage(t *testing.T) {
	p, cs := testProbe(t, "")
	created := recordCreated(cs)
	if _, err := p.NetworkPolicy(t.Context()); !errors.Is(err, doctor.ErrNoProbeImage) {
		t.Errorf("err = %v, want ErrNoProbeImage", err)
	}
	if len(*created) != 0 {
		t.Errorf("created %q without an image", *created)
	}
}

func TestProbeStuckPodEndsEarly(t *testing.T) {
	p, cs := testProbe(t, "img")
	p.podTimeout = time.Minute
	cs.PrependReactor("create", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
		pod := a.(k8stesting.CreateAction).GetObject().(*corev1.Pod)
		pod.Status.Phase = corev1.PodPending
		pod.Status.ContainerStatuses = []corev1.ContainerStatus{{State: corev1.ContainerState{
			Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff", Message: "denied"},
		}}}
		return false, nil, nil
	})
	start := time.Now()
	res, err := p.NetworkPolicy(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if res.State != doctor.NetworkPolicyInconclusive || !strings.Contains(res.Detail, "ImagePullBackOff") {
		t.Errorf("result = %+v", res)
	}
	if time.Since(start) > 10*time.Second {
		t.Error("waited out the timeout for a pod that can't start")
	}
}

func TestProbeCleansUpWhenPodCreationFails(t *testing.T) {
	p, cs := testProbe(t, "img")
	cs.PrependReactor("create", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("pods is forbidden")
	})
	if _, err := p.NetworkPolicy(t.Context()); err == nil || !strings.Contains(err.Error(), "forbidden") {
		t.Errorf("err = %v", err)
	}
	if list, _ := cs.CoreV1().Namespaces().List(t.Context(), metav1.ListOptions{}); len(list.Items) != 0 {
		t.Errorf("probe namespaces left: %v", list.Items)
	}
}

func TestProbeCopiesImagePullSecrets(t *testing.T) {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "registry", Namespace: "pressfleet"},
		Type:       corev1.SecretTypeDockerConfigJson,
		Data:       map[string][]byte{".dockerconfigjson": []byte("{}")},
	}
	p, cs := testProbe(t, "img", secret)
	p.cfg.Kubernetes.ImagePullSecrets = []string{"registry"}
	podResults(cs, map[string]corev1.PodPhase{"allowed": corev1.PodSucceeded, "denied": corev1.PodSucceeded})
	var pulls []corev1.LocalObjectReference
	cs.PrependReactor("create", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
		pulls = a.(k8stesting.CreateAction).GetObject().(*corev1.Pod).Spec.ImagePullSecrets
		return false, nil, nil
	})
	created := recordCreated(cs)
	if _, err := p.NetworkPolicy(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(*created, "secrets/registry") || len(pulls) != 1 || pulls[0].Name != "registry" {
		t.Errorf("created %q, pods pull with %v", *created, pulls)
	}
}

// Copying a pull secret into the probe namespace is retried while the
// authorizer still refuses it, like the creations after it (#353).
func TestProbeRetriesPullSecretCopyWhileForbidden(t *testing.T) {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "registry", Namespace: "pressfleet"},
		Type:       corev1.SecretTypeDockerConfigJson,
		Data:       map[string][]byte{".dockerconfigjson": []byte("{}")},
	}
	p, cs := testProbe(t, "img", secret)
	p.c.poll = time.Millisecond
	p.cfg.Kubernetes.ImagePullSecrets = []string{"registry"}
	podResults(cs, map[string]corev1.PodPhase{"allowed": corev1.PodSucceeded, "denied": corev1.PodSucceeded})
	refusals := 2
	cs.PrependReactor("create", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
		if refusals > 0 {
			refusals--
			return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "secrets"}, "registry", errors.New("not yet"))
		}
		return false, nil, nil
	})
	created := recordCreated(cs)
	if _, err := p.NetworkPolicy(t.Context()); err != nil {
		t.Fatal(err)
	}
	if refusals != 0 || !slices.Contains(*created, "secrets/registry") {
		t.Errorf("created %q, %d refusals left", *created, refusals)
	}
}

func TestProbeCgroupV2(t *testing.T) {
	p, cs := testProbe(t, "img")
	var onNodes []string
	cs.PrependReactor("create", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
		pod := a.(k8stesting.CreateAction).GetObject().(*corev1.Pod)
		onNodes = append(onNodes, pod.Spec.NodeName)
		// n2 is on cgroup v1: its script fails.
		pod.Status.Phase = corev1.PodSucceeded
		if pod.Spec.NodeName == "n2" {
			pod.Status.Phase = corev1.PodFailed
		}
		if pod.Spec.NodeName == "n3" {
			pod.Status.Phase = corev1.PodPending
			pod.Status.ContainerStatuses = []corev1.ContainerStatus{{State: corev1.ContainerState{
				Waiting: &corev1.ContainerStateWaiting{Reason: "ErrImagePull"},
			}}}
		}
		return false, nil, nil
	})
	nodes := []doctor.ClusterNode{{Name: "n1"}, {Name: "n2"}, {Name: "n3"}}
	got, err := p.CgroupV2(t.Context(), nodes)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(onNodes, []string{"n1", "n2", "n3"}) {
		t.Errorf("pods ran on %v", onNodes)
	}
	if len(got) != 3 || !got[0].V2 || got[0].Err != "" || got[1].V2 || got[1].Err != "" || got[2].V2 || !strings.HasPrefix(got[2].Err, "ErrImagePull") {
		t.Errorf("results = %+v", got)
	}
	if list, _ := cs.CoreV1().Namespaces().List(t.Context(), metav1.ListOptions{}); len(list.Items) != 0 {
		t.Errorf("probe namespaces left: %v", list.Items)
	}

	if res, err := p.CgroupV2(t.Context(), nil); err != nil || res != nil {
		t.Errorf("no nodes: %v, %v", res, err)
	}
	p.cfg.Image = ""
	if _, err := p.CgroupV2(t.Context(), nodes); !errors.Is(err, doctor.ErrNoProbeImage) {
		t.Errorf("err = %v, want ErrNoProbeImage", err)
	}
}

func TestLeadingInt(t *testing.T) {
	for in, want := range map[string]int{"35": 35, "34+": 34, "": 0, "x": 0} {
		if got := leadingInt(in); got != want {
			t.Errorf("leadingInt(%q) = %d, want %d", in, got, want)
		}
	}
}

// The probe namespace gets the site ClusterRole's RoleBinding before
// anything else is created in it, as a site's namespace does (#228), and
// creations the authorizer refuses while the binding is new are retried.
func TestProbeBindsItsNamespace(t *testing.T) {
	p, cs := testProbe(t, "img")
	p.c.poll = time.Millisecond
	podResults(cs, map[string]corev1.PodPhase{"allowed": corev1.PodSucceeded, "denied": corev1.PodSucceeded})
	var binding *rbacv1.RoleBinding
	cs.PrependReactor("create", "rolebindings", func(a k8stesting.Action) (bool, runtime.Object, error) {
		binding = a.(k8stesting.CreateAction).GetObject().(*rbacv1.RoleBinding)
		return false, nil, nil
	})
	refusals := 2
	cs.PrependReactor("create", "networkpolicies", func(k8stesting.Action) (bool, runtime.Object, error) {
		if refusals > 0 {
			refusals--
			return true, nil, apierrors.NewForbidden(schema.GroupResource{Group: "networking.k8s.io", Resource: "networkpolicies"}, "", errors.New("not yet"))
		}
		return false, nil, nil
	})
	created := recordCreated(cs)
	if _, err := p.NetworkPolicy(t.Context()); err != nil {
		t.Fatal(err)
	}
	if binding == nil || !strings.HasPrefix(binding.Namespace, "pressfleet-doctor-") ||
		binding.RoleRef.Name != SiteClusterRole("pressfleet") || len(binding.Subjects) != 1 ||
		binding.Subjects[0].Name != ControlPlaneServiceAccount || binding.Subjects[0].Namespace != "pressfleet" {
		t.Fatalf("RoleBinding = %+v", binding)
	}
	if refusals != 0 || len(*created) < 3 || !strings.HasPrefix((*created)[1], "rolebindings/") {
		t.Errorf("created %q, %d refusals left", *created, refusals)
	}
}
