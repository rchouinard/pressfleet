package kubernetes

import (
	"context"
	"errors"
	"net/netip"
	"slices"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	kubefake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// The fake cluster's own addresses: a node on a public address (k3s on a
// VPS) with a public IPv6 address too, a node on a private one, and an API
// server endpoint on a public address (a managed cluster's).
var (
	publicNode   = netip.MustParseAddr("203.0.113.10")
	publicNodeV6 = netip.MustParseAddr("2001:db8::10")
	privateNode  = netip.MustParseAddr("10.0.0.12")
	publicAPI    = netip.MustParseAddr("198.51.100.7")
)

func addressedNode(name string, addrs ...corev1.NodeAddress) *corev1.Node {
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}, Status: corev1.NodeStatus{Addresses: addrs}}
}

// testClusterObjects are the nodes and the API server's EndpointSlice of
// the fake cluster.
func testClusterObjects() []runtime.Object {
	return []runtime.Object{
		addressedNode("public",
			corev1.NodeAddress{Type: corev1.NodeInternalIP, Address: publicNode.String()},
			corev1.NodeAddress{Type: corev1.NodeExternalIP, Address: publicNodeV6.String()},
			corev1.NodeAddress{Type: corev1.NodeHostName, Address: "public"},
		),
		addressedNode("private", corev1.NodeAddress{Type: corev1.NodeInternalIP, Address: privateNode.String()}),
		&discoveryv1.EndpointSlice{
			ObjectMeta:  metav1.ObjectMeta{Name: apiServerService, Namespace: apiServerNamespace},
			AddressType: discoveryv1.AddressTypeIPv4,
			Endpoints:   []discoveryv1.Endpoint{{Addresses: []string{publicAPI.String()}}},
		},
	}
}

// openTo returns the policies' egress rules that let a pod with podLabels
// reach addr on every port: a rule with an ipBlock holding addr outside its
// exceptions and no ports. Rules limited to ports (DNS, the SMTP relay's)
// are left aside: they reach any address on their port by design.
func openTo(t *testing.T, policies []networkingv1.NetworkPolicy, podLabels map[string]string, addr netip.Addr) []string {
	t.Helper()
	var open []string
	for _, pol := range policies {
		sel, err := metav1.LabelSelectorAsSelector(&pol.Spec.PodSelector)
		if err != nil {
			t.Fatal(err)
		}
		if !sel.Matches(labels.Set(podLabels)) {
			continue
		}
		for _, rule := range pol.Spec.Egress {
			if len(rule.Ports) > 0 {
				continue
			}
			if len(rule.To) == 0 {
				open = append(open, pol.Name+": a rule to anywhere")
			}
			for _, peer := range rule.To {
				if peer.IPBlock == nil {
					continue
				}
				cidr := netip.MustParsePrefix(peer.IPBlock.CIDR)
				excepted := false
				for _, e := range peer.IPBlock.Except {
					ep := netip.MustParsePrefix(e)
					if !cidr.Contains(ep.Addr()) || ep.Bits() < cidr.Bits() {
						t.Errorf("%s: exception %s is outside %s, which the API server refuses", pol.Name, e, cidr)
					}
					excepted = excepted || ep.Contains(addr)
				}
				if cidr.Contains(addr) && !excepted {
					open = append(open, pol.Name+": "+cidr.String())
				}
			}
		}
	}
	return open
}

// sitePolicies lists the NetworkPolicies of the test site's namespace.
func sitePolicies(t *testing.T, p *Provisioner) []networkingv1.NetworkPolicy {
	t.Helper()
	list, err := p.typed.NetworkingV1().NetworkPolicies(p.SiteNamespace(testSiteID)).List(t.Context(), metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return list.Items
}

// The acceptance criterion of #324: whatever their range, neither a site's
// pod nor its tasks reach the nodes or the API server, and still reach the
// internet.
func TestSiteCantReachClusterAddresses(t *testing.T) {
	p := cluster(t, nil)
	if _, err := p.CreateSite(t.Context(), testSite()); err != nil {
		t.Fatal(err)
	}
	policies := sitePolicies(t, p)
	for _, component := range []string{SiteComponent, TaskComponent} {
		pod := map[string]string{ComponentLabel: component}
		for _, addr := range []netip.Addr{publicNode, publicNodeV6, privateNode, publicAPI} {
			if open := openTo(t, policies, pod, addr); len(open) > 0 {
				t.Errorf("a %s pod reaches %s through %v", component, addr, open)
			}
		}
		for _, addr := range []string{"192.0.2.80", "2001:db8:1::80"} {
			if open := openTo(t, policies, pod, netip.MustParseAddr(addr)); len(open) == 0 {
				t.Errorf("a %s pod doesn't reach the internet address %s", component, addr)
			}
		}
	}
	pol, err := p.typed.NetworkingV1().NetworkPolicies(p.SiteNamespace(testSiteID)).Get(t.Context(), InternetPolicy, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if pol.Labels[RenderedLabel] != "" || pol.Labels[managedByLabel] != FieldManager {
		t.Errorf("internet policy labels %v: want the driver's, not the rendered label Prune deletes by", pol.Labels)
	}
	// A private node is covered by 10.0.0.0/8 already.
	if except := pol.Spec.Egress[0].To[0].IPBlock.Except; slices.Contains(except, privateNode.String()+"/32") {
		t.Errorf("IPv4 exceptions %v: the private node needs none of its own", except)
	}
	// Updating the site keeps the policy, which Prune must not take for a
	// rendered object the templates dropped.
	if _, err := p.UpdateSite(t.Context(), testSite()); err != nil {
		t.Fatal(err)
	}
	if _, err := p.typed.NetworkingV1().NetworkPolicies(p.SiteNamespace(testSiteID)).Get(t.Context(), InternetPolicy, metav1.GetOptions{}); err != nil {
		t.Errorf("internet policy after UpdateSite: %v", err)
	}
}

// policyApplies counts the writes of internet policies.
func policyApplies(cs *kubefake.Clientset) int {
	n := 0
	for _, a := range cs.Actions() {
		if a.GetVerb() == "patch" && a.GetResource().Resource == "networkpolicies" {
			if pa, ok := a.(k8stesting.PatchAction); ok && pa.GetName() == InternetPolicy {
				n++
			}
		}
	}
	return n
}

func TestSyncInternetPoliciesFollowsNodes(t *testing.T) {
	p := cluster(t, nil)
	ctx := t.Context()
	cs := p.typed.(*kubefake.Clientset)
	if _, err := p.CreateSite(ctx, testSite()); err != nil {
		t.Fatal(err)
	}
	// A namespace of another install's prefix, which isn't this driver's.
	other := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "other-x", Labels: map[string]string{managedByLabel: FieldManager, siteIDLabel: "x"}}}
	// And one with this prefix, of another install (K6).
	foreign := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "pf-foreign", Labels: map[string]string{
		managedByLabel: FieldManager, siteIDLabel: "foreign", PlatformNamespaceLabel: "elsewhere",
	}}}
	for _, ns := range []*corev1.Namespace{other, foreign} {
		if _, err := cs.CoreV1().Namespaces().Create(ctx, ns, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	pod := map[string]string{ComponentLabel: SiteComponent}

	// The first sync writes every site, whatever was there.
	before := policyApplies(cs)
	if err := p.SyncInternetPolicies(ctx); err != nil {
		t.Fatal(err)
	}
	if got := policyApplies(cs) - before; got != 1 {
		t.Errorf("first sync wrote %d policies, want the one site's", got)
	}
	// Nothing changed: nothing is written.
	before = policyApplies(cs)
	if err := p.SyncInternetPolicies(ctx); err != nil {
		t.Fatal(err)
	}
	if got := policyApplies(cs) - before; got != 0 {
		t.Errorf("a sync without changes wrote %d policies", got)
	}

	// A node joins on a public address.
	joined := netip.MustParseAddr("192.0.2.50")
	if open := openTo(t, sitePolicies(t, p), pod, joined); len(open) == 0 {
		t.Fatalf("the site doesn't reach %s before it is a node; the test proves nothing", joined)
	}
	node := addressedNode("joined", corev1.NodeAddress{Type: corev1.NodeInternalIP, Address: joined.String()})
	if _, err := cs.CoreV1().Nodes().Create(ctx, node, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := p.SyncInternetPolicies(ctx); err != nil {
		t.Fatal(err)
	}
	if open := openTo(t, sitePolicies(t, p), pod, joined); len(open) > 0 {
		t.Errorf("the site reaches the new node %s through %v", joined, open)
	}
	for _, ns := range []string{other.Name, foreign.Name} {
		if _, err := cs.NetworkingV1().NetworkPolicies(ns).Get(ctx, InternetPolicy, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
			t.Errorf("namespace %s, not this install's, got an internet policy: %v", ns, err)
		}
	}

	// It leaves: its address is the internet again.
	if err := cs.CoreV1().Nodes().Delete(ctx, node.Name, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := p.SyncInternetPolicies(ctx); err != nil {
		t.Fatal(err)
	}
	if open := openTo(t, sitePolicies(t, p), pod, joined); len(open) == 0 {
		t.Errorf("the site still can't reach %s after the node left", joined)
	}
}

// Without the cluster's addresses the driver writes no internet policy:
// the site can't be created, and the sync leaves the policies as they are.
func TestInternetPolicyNeedsClusterAddresses(t *testing.T) {
	p := cluster(t, nil)
	ctx := t.Context()
	cs := p.typed.(*kubefake.Clientset)
	if _, err := p.CreateSite(ctx, testSite()); err != nil {
		t.Fatal(err)
	}
	cs.PrependReactor("get", "endpointslices", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Group: "discovery.k8s.io", Resource: "endpointslices"}, apiServerService, errors.New("no Role"))
	})
	before := policyApplies(cs)
	if err := p.SyncInternetPolicies(ctx); !errors.Is(err, ErrClusterAddresses) {
		t.Errorf("SyncInternetPolicies = %v, want ErrClusterAddresses", err)
	}
	if _, err := p.UpdateSite(ctx, testSite()); !errors.Is(err, ErrClusterAddresses) {
		t.Errorf("UpdateSite = %v, want ErrClusterAddresses", err)
	}
	if got := policyApplies(cs) - before; got != 0 {
		t.Errorf("%d internet policies written without the addresses", got)
	}
}

// RunBackground picks up a node that joins within its resync interval.
func TestRunBackgroundFollowsNodes(t *testing.T) {
	p := cluster(t, nil)
	p.internet.resync = 10 * time.Millisecond
	ctx := t.Context()
	if _, err := p.CreateSite(ctx, testSite()); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	runCtx, stop := context.WithCancel(ctx)
	go func() { done <- p.RunBackground(runCtx) }()
	defer func() {
		stop()
		if err := <-done; err != nil {
			t.Errorf("RunBackground = %v", err)
		}
	}()
	joined := netip.MustParseAddr("192.0.2.51")
	node := addressedNode("joined", corev1.NodeAddress{Type: corev1.NodeExternalIP, Address: joined.String()})
	if _, err := p.typed.CoreV1().Nodes().Create(ctx, node, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	pod := map[string]string{ComponentLabel: SiteComponent}
	for deadline := time.Now().Add(5 * time.Second); len(openTo(t, sitePolicies(t, p), pod, joined)) > 0; {
		if time.Now().After(deadline) {
			t.Fatalf("the site still reaches the new node %s", joined)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestClusterProbeAddresses(t *testing.T) {
	c, _ := fakeClient(t, testClusterObjects()...)
	got, err := newClusterProbe(c, ProbeConfig{}).ClusterAddresses(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got.Nodes, []netip.Addr{publicNode, publicNodeV6, privateNode}) && !slices.Equal(got.Nodes, []netip.Addr{privateNode, publicNode, publicNodeV6}) {
		t.Errorf("nodes %v", got.Nodes)
	}
	if !slices.Equal(got.APIServer, []netip.Addr{publicAPI}) {
		t.Errorf("API server %v", got.APIServer)
	}
}
