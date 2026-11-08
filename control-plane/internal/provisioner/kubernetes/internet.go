package kubernetes

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	metav1ac "k8s.io/client-go/applyconfigurations/meta/v1"
	networkingv1ac "k8s.io/client-go/applyconfigurations/networking/v1"
)

// This file keeps sites off the cluster's nodes and API server when those
// have addresses outside the ranges the internet rule leaves out anyway
// (nonInternet; security review K4). NetworkPolicy can only allow, so the
// addresses must be exceptions of the very rule that allows the internet.
// That rule is the policy InternetPolicy, which the driver writes in each
// site namespace instead of rendering it from templates/k8s: the addresses
// change as nodes come and go, and rewriting one policy neither re-renders
// the site nor touches its pod.

// InternetPolicy is the NetworkPolicy in each site namespace that lets the
// site's pod and its tasks reach the internet, less the non-internet
// ranges, the pod ranges, and the cluster's own addresses. The driver
// writes it; it doesn't carry RenderedLabel, so Prune leaves it alone.
const InternetPolicy = "site-internet"

// ErrClusterAddresses is returned when the driver can't read the nodes'
// or the API server's addresses, so it can't keep sites away from them.
var ErrClusterAddresses = errors.New("kubernetes: can't read the cluster's node and API server addresses")

// DefaultInternetResync is how often the driver reads the cluster's
// addresses to update the sites' internet policies when they change.
const DefaultInternetResync = 30 * time.Second

// apiServerNamespace and apiServerService are where the API server
// publishes its endpoints: the EndpointSlice of the kubernetes Service,
// which the API server names after the Service.
const (
	apiServerNamespace = "default"
	apiServerService   = "kubernetes"
)

// internetState is what the driver keeps to update the internet policies.
type internetState struct {
	// pods are the configured pod ranges (kubernetes.pod_cidrs).
	pods []netip.Prefix
	// resync is how often Run reads the addresses; zero means
	// DefaultInternetResync.
	resync time.Duration

	// mu makes reading the addresses and writing a policy one step, so
	// that a site's apply and the resync can't leave a policy with an
	// older set than the other wrote.
	mu sync.Mutex
	// synced is the set (clusterAddresses.key) every site namespace has,
	// "" until the first resync wrote them all.
	synced string
}

// ClusterAddresses are the addresses of the cluster itself that sites
// must not reach.
type ClusterAddresses struct {
	// Nodes are the nodes' InternalIP and ExternalIP addresses.
	Nodes []netip.Addr
	// APIServer are the API server's endpoints, which the kubernetes
	// Service's traffic reaches after its DNAT, where policy applies.
	APIServer []netip.Addr
}

// all returns every address, sorted, without duplicates.
func (a ClusterAddresses) all() []netip.Addr {
	out := slices.Concat(a.Nodes, a.APIServer)
	slices.SortFunc(out, func(x, y netip.Addr) int { return x.Compare(y) })
	return slices.Compact(out)
}

// key identifies the set of addresses.
func (a ClusterAddresses) key() string {
	var b strings.Builder
	for _, addr := range a.all() {
		b.WriteString(addr.String())
		b.WriteByte(' ')
	}
	return b.String()
}

// ClusterAddresses reads the nodes' InternalIP and ExternalIP addresses and
// the API server's endpoints. Either read failing is ErrClusterAddresses:
// the driver doesn't write a policy from half the addresses.
func (c *Client) ClusterAddresses(ctx context.Context) (ClusterAddresses, error) {
	var out ClusterAddresses
	nodes, err := c.typed.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return out, fmt.Errorf("%w: listing nodes: %w", ErrClusterAddresses, err)
	}
	for _, n := range nodes.Items {
		for _, a := range n.Status.Addresses {
			if a.Type != corev1.NodeInternalIP && a.Type != corev1.NodeExternalIP {
				continue
			}
			if ip, err := netip.ParseAddr(a.Address); err == nil {
				out.Nodes = append(out.Nodes, ip.Unmap())
			}
		}
	}
	slice, err := c.typed.DiscoveryV1().EndpointSlices(apiServerNamespace).Get(ctx, apiServerService, metav1.GetOptions{})
	if err != nil {
		return out, fmt.Errorf("%w: reading the EndpointSlice %s/%s: %w", ErrClusterAddresses, apiServerNamespace, apiServerService, err)
	}
	for _, e := range slice.Endpoints {
		for _, a := range e.Addresses {
			// FQDN slices hold names, which policy can't name either.
			if ip, err := netip.ParseAddr(a); err == nil {
				out.APIServer = append(out.APIServer, ip.Unmap())
			}
		}
	}
	return out, nil
}

// internetPolicy is the InternetPolicy of the site namespace ns for the
// cluster's addresses. It selects what the rendered site-egress policy
// does: the site's pod and its tasks.
func (p *Provisioner) internetPolicy(ns, siteID string, addrs ClusterAddresses) *networkingv1ac.NetworkPolicyApplyConfiguration {
	v4, v6 := internetExceptions(p.internet.pods, addrs.all())
	return networkingv1ac.NetworkPolicy(InternetPolicy, ns).
		WithLabels(map[string]string{managedByLabel: FieldManager, siteIDLabel: siteID}).
		WithSpec(networkingv1ac.NetworkPolicySpec().
			WithPodSelector(metav1ac.LabelSelector().WithMatchExpressions(
				metav1ac.LabelSelectorRequirement().
					WithKey(ComponentLabel).
					WithOperator(metav1.LabelSelectorOpIn).
					WithValues(SiteComponent, TaskComponent),
			)).
			WithPolicyTypes(networkingv1.PolicyTypeEgress).
			WithEgress(networkingv1ac.NetworkPolicyEgressRule().WithTo(
				networkingv1ac.NetworkPolicyPeer().WithIPBlock(networkingv1ac.IPBlock().WithCIDR("0.0.0.0/0").WithExcept(v4...)),
				networkingv1ac.NetworkPolicyPeer().WithIPBlock(networkingv1ac.IPBlock().WithCIDR("::/0").WithExcept(v6...)),
			)))
}

// writeInternetPolicy applies the site namespace's InternetPolicy.
func (p *Provisioner) writeInternetPolicy(ctx context.Context, ns, siteID string, addrs ClusterAddresses) error {
	_, err := p.typed.NetworkingV1().NetworkPolicies(ns).Apply(ctx, p.internetPolicy(ns, siteID, addrs),
		metav1.ApplyOptions{FieldManager: FieldManager, Force: true})
	if err != nil {
		return fmt.Errorf("applying NetworkPolicy %s/%s: %w", ns, InternetPolicy, err)
	}
	return nil
}

// applyInternetPolicy writes the site's InternetPolicy from the cluster's
// current addresses. Without them it fails rather than let the site reach
// what it shouldn't: the site has no internet until it succeeds.
func (p *Provisioner) applyInternetPolicy(ctx context.Context, siteID string) error {
	p.internet.mu.Lock()
	defer p.internet.mu.Unlock()
	addrs, err := p.ClusterAddresses(ctx)
	if err != nil {
		return err
	}
	ns := p.SiteNamespace(siteID)
	// The first call in a namespace whose RoleBinding may be seconds old.
	return p.whileForbidden(ctx, func() error { return p.writeInternetPolicy(ctx, ns, siteID, addrs) })
}

// RunBackground keeps the internet policies of every site current until
// ctx ends (driver.BackgroundWorker): it reads the cluster's addresses
// every resync interval and rewrites every site's policy when they
// changed, and once at the start, which also brings in sites rendered
// before the policy existed. A failed resync is logged when its error
// changes and tried again at the next interval; the policies keep the last
// set written meanwhile.
func (p *Provisioner) RunBackground(ctx context.Context) error {
	every := p.internet.resync
	if every <= 0 {
		every = DefaultInternetResync
	}
	t := time.NewTicker(every)
	defer t.Stop()
	lastErr := ""
	for {
		err := p.SyncInternetPolicies(ctx)
		if ctx.Err() != nil {
			return nil
		}
		msg := ""
		if err != nil {
			msg = err.Error()
		}
		switch {
		case msg != "" && msg != lastErr:
			p.log.WarnContext(ctx, "can't update the sites' internet policies; sites may reach nodes or API server endpoints that are new since the last update",
				"error", err)
		case msg == "" && lastErr != "":
			p.log.InfoContext(ctx, "the sites' internet policies are current again")
		}
		lastErr = msg
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

// SyncInternetPolicies writes the InternetPolicy of every site namespace if
// the cluster's addresses changed since it last did. A namespace it fails
// to write makes the next call try them all again.
func (p *Provisioner) SyncInternetPolicies(ctx context.Context) error {
	p.internet.mu.Lock()
	defer p.internet.mu.Unlock()
	addrs, err := p.ClusterAddresses(ctx)
	if err != nil {
		return err
	}
	key := addrs.key()
	if key == p.internet.synced {
		return nil
	}
	list, err := p.typed.CoreV1().Namespaces().List(ctx, metav1.ListOptions{
		LabelSelector: managedByLabel + "=" + FieldManager + "," + siteIDLabel,
	})
	if err != nil {
		return fmt.Errorf("listing site namespaces: %w", err)
	}
	var errs []error
	n := 0
	for _, ns := range list.Items {
		if !strings.HasPrefix(ns.Name, p.namespacePrefix) || ns.DeletionTimestamp != nil {
			continue
		}
		err := p.writeInternetPolicy(ctx, ns.Name, ns.Labels[siteIDLabel], addrs)
		switch {
		case apierrors.IsNotFound(err):
			// Deleted since the list.
		case err != nil:
			errs = append(errs, err)
		default:
			n++
		}
	}
	if err := errors.Join(errs...); err != nil {
		return err
	}
	if p.internet.synced != "" {
		p.log.InfoContext(ctx, "the cluster's addresses changed; updated the sites' internet policies",
			"sites", n, "addresses", len(addrs.all()))
	}
	p.internet.synced = key
	return nil
}
