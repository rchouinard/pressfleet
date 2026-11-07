package doctor

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strings"
)

// Kubernetes support window (§4.8): the three newest minor releases when
// the PressFleet release ships. Raise it with each release.
const (
	MinKubernetesMajor = 1
	MinKubernetesMinor = 35
)

// Sentinel errors a ClusterProbe returns for conditions checks branch on.
var (
	// ErrNoProbeImage means probe pods can't run because no image is
	// configured (task_image).
	ErrNoProbeImage = errors.New("doctor: no image to run probe pods with (task_image is not set)")
	// ErrNoCluster means the doctor has no connection to the cluster.
	ErrNoCluster = errors.New("doctor: not connected to a Kubernetes cluster")
)

// ClusterVersion is the API server's version.
type ClusterVersion struct {
	Major, Minor int
	// Git is the full version, such as v1.35.1.
	Git string
}

// ClusterNode is a node's facts that checks use.
type ClusterNode struct {
	Name string
	// OS and Arch are in GOOS/GOARCH terms.
	OS, Arch string
	Ready    bool
	// Unschedulable is true for a cordoned node.
	Unschedulable bool
	// AllocatableMemory is what pods can request on the node, in bytes.
	AllocatableMemory uint64
}

// Schedulable says whether new pods are placed on the node.
func (n ClusterNode) Schedulable() bool { return n.Ready && !n.Unschedulable }

// NodeCgroup is the result of checking a node's cgroup version.
type NodeCgroup struct {
	Node string
	V2   bool
	// Err says why the node couldn't be checked.
	Err string
}

// RBACReport says how the control plane's rights differ from what the
// driver needs.
type RBACReport struct {
	// Missing lists the rights the driver needs and the control plane lacks,
	// such as "create namespaces".
	Missing []string
	// Excess lists the cluster-wide rights beyond the driver's needs that
	// the control plane has, such as "get secrets in all namespaces".
	Excess []string
}

// StorageClassInfo describes a storage class.
type StorageClassInfo struct {
	Name        string
	Provisioner string
}

// NetworkPolicyState is the outcome of the NetworkPolicy probe.
type NetworkPolicyState string

// NetworkPolicy probe outcomes.
const (
	// NetworkPolicyEnforced means the denied probe pod couldn't connect
	// while the allowed one could.
	NetworkPolicyEnforced NetworkPolicyState = "enforced"
	// NetworkPolicyIgnored means the pod a policy denied connected anyway.
	NetworkPolicyIgnored NetworkPolicyState = "ignored"
	// NetworkPolicyInconclusive means the probe couldn't run, or the pod
	// that should connect didn't.
	NetworkPolicyInconclusive NetworkPolicyState = "inconclusive"
)

// NetworkPolicyResult is what the NetworkPolicy probe found.
type NetworkPolicyResult struct {
	State  NetworkPolicyState
	Detail string
}

// ClusterProbe reads facts about the Kubernetes cluster (§4.8). The
// Kubernetes driver's package implements it, because client-go is confined
// there (§4.2).
type ClusterProbe interface {
	ServerVersion(ctx context.Context) (ClusterVersion, error)
	Nodes(ctx context.Context) ([]ClusterNode, error)
	// CgroupV2 runs a pod on each node to see which cgroup version it uses.
	CgroupV2(ctx context.Context, nodes []ClusterNode) ([]NodeCgroup, error)
	RBAC(ctx context.Context) (RBACReport, error)
	// StorageClass finds the named class, or the default class for "".
	// found is false when there is none.
	StorageClass(ctx context.Context, name string) (info StorageClassInfo, found bool, err error)
	// MetricsAPI returns nil when metrics.k8s.io answers.
	MetricsAPI(ctx context.Context) error
	// NetworkPolicy runs two pods, one denied by a NetworkPolicy.
	NetworkPolicy(ctx context.Context) (NetworkPolicyResult, error)
	// TraefikAddresses returns the external IP addresses and host names of
	// Traefik's Service.
	TraefikAddresses(ctx context.Context) ([]string, error)
	// PlatformMemoryRequests returns the memory the platform's pods request.
	PlatformMemoryRequests(ctx context.Context) (uint64, error)
}

// rwxIncapable lists provisioners of block or node-local storage that
// can't serve ReadWriteMany volumes. A class cannot say which access modes
// it supports, so this catches the common mistakes and nothing more.
var rwxIncapable = []string{
	"rancher.io/local-path", "kubernetes.io/no-provisioner", "ebs.csi.aws.com", "kubernetes.io/aws-ebs",
	"pd.csi.storage.gke.io", "kubernetes.io/gce-pd", "disk.csi.azure.com", "kubernetes.io/azure-disk",
	"csi.hetzner.cloud", "block.csi.vultr.com", "dobs.csi.digitalocean.com", "do.csi.digitalocean.com",
	"cinder.csi.openstack.org",
}

// unshareable lists ReadWriteMany provisioners whose volumes are known not
// to take several PersistentVolumes pointing at one volume, which is how
// site tasks mount the data volume (#219): Azure Files wants a volume
// handle of its own for each, and Longhorn ties a volume to one
// PersistentVolume.
var unshareable = []string{"file.csi.azure.com", "kubernetes.io/azure-file", "driver.longhorn.io"}

// cluster returns the probe, or the reason there is none.
func (e *env) cluster() (ClusterProbe, error) {
	if e.Cluster != nil {
		return e.Cluster, nil
	}
	if e.ClusterErr != nil {
		return nil, e.ClusterErr
	}
	return nil, ErrNoCluster
}

func clusterUnreachable() outcome {
	return skip("the cluster is not reachable (see the Kubernetes version check)")
}

const clusterFix = "Run PressFleet in the cluster, or set kubernetes.kubeconfig (PRESSFLEET_KUBERNETES_KUBECONFIG) " +
	"and make sure the API server is reachable."

func checkK8sVersion(ctx context.Context, e *env) outcome {
	c, err := e.cluster()
	if err != nil {
		return fail(clusterFix, "can't connect to the cluster: %v", err)
	}
	v, err := c.ServerVersion(ctx)
	if err != nil {
		return fail(clusterFix, "can't read the cluster's version: %v", err)
	}
	if v.Major < MinKubernetesMajor || (v.Major == MinKubernetesMajor && v.Minor < MinKubernetesMinor) {
		return fail(fmt.Sprintf("Upgrade the cluster to Kubernetes %d.%d or newer.", MinKubernetesMajor, MinKubernetesMinor),
			"Kubernetes %s is older than the oldest supported release, %d.%d", v.Git, MinKubernetesMajor, MinKubernetesMinor)
	}
	return pass("Kubernetes %s", v.Git)
}

func checkK8sNodes(ctx context.Context, e *env) outcome {
	c, err := e.cluster()
	if err != nil {
		return clusterUnreachable()
	}
	nodes, err := c.Nodes(ctx)
	if err != nil {
		return fail("Give the control plane's service account the right to list nodes.", "can't list nodes: %v", err)
	}
	var amd64, bad []string
	for _, n := range nodes {
		if !n.Schedulable() {
			continue
		}
		if n.OS == "linux" && n.Arch == "amd64" {
			amd64 = append(amd64, n.Name)
		} else {
			bad = append(bad, fmt.Sprintf("%s (%s/%s)", n.Name, n.OS, n.Arch))
		}
	}
	switch {
	case len(bad) > 0:
		return fail("Cordon or remove the other nodes; PressFleet's images are linux/amd64 only.",
			"nodes that aren't linux/amd64: %s", strings.Join(bad, ", "))
	case len(amd64) == 0:
		return fail("Add a ready linux/amd64 node.", "no ready, schedulable nodes")
	}
	return pass("%d linux/amd64 nodes", len(amd64))
}

func checkK8sCgroup(ctx context.Context, e *env) outcome {
	c, err := e.cluster()
	if err != nil {
		return clusterUnreachable()
	}
	if e.SkipClusterPods {
		return outcome{status: StatusUnavailable, message: "runs a pod on every node, so it runs at startup, in `pressfleet doctor`, and in the Helm test"}
	}
	nodes, err := c.Nodes(ctx)
	if err != nil {
		return clusterUnreachable()
	}
	nodes = slices.DeleteFunc(nodes, func(n ClusterNode) bool { return !n.Schedulable() })
	res, err := c.CgroupV2(ctx, nodes)
	if errors.Is(err, ErrNoProbeImage) {
		return warn("Set task_image (PRESSFLEET_TASK_IMAGE) to the control plane image.", "can't check: %v", err)
	}
	if err != nil {
		return warn("Check that the control plane may create namespaces and pods, and that nodes can pull the task image.",
			"couldn't run the probe pods: %v", err)
	}
	var v1, unknown []string
	for _, r := range res {
		switch {
		case r.Err != "":
			unknown = append(unknown, r.Node+": "+r.Err)
		case !r.V2:
			v1 = append(v1, r.Node)
		}
	}
	switch {
	case len(v1) > 0:
		return fail("Boot the nodes with cgroup v2 (systemd.unified_cgroup_hierarchy=1), or use a distribution that defaults to it.",
			"nodes on cgroup v1: %s", strings.Join(v1, ", "))
	case len(unknown) > 0:
		return warn("Check that the nodes can pull the task image and run pods.", "couldn't check %s", strings.Join(unknown, "; "))
	}
	return pass("%d nodes use cgroup v2", len(res))
}

func checkK8sRBAC(ctx context.Context, e *env) outcome {
	c, err := e.cluster()
	if err != nil {
		return clusterUnreachable()
	}
	r, err := c.RBAC(ctx)
	if err != nil {
		return warn("Check that the API server's authorization API is available.", "couldn't check the control plane's rights: %v", err)
	}
	switch {
	case len(r.Missing) > 0:
		return fail("Grant the control plane's service account these rights; the Helm chart's RBAC does.",
			"missing rights: %s", strings.Join(r.Missing, ", "))
	case len(r.Excess) > 0:
		return fail("Remove these cluster-wide rights from the control plane's service account (a binding to cluster-admin is the usual cause). "+
			"Sites' namespaces get their rights from the driver's own bindings.",
			"cluster-wide rights beyond the driver's needs: %s", strings.Join(r.Excess, ", "))
	}
	return pass("the control plane has the rights it needs and no cluster-wide ones beyond them")
}

// checkK8sStorage checks the storage class named by class(e). A class
// can't say which access modes it supports, so the check can only catch
// provisioners known to lack ReadWriteMany.
func checkK8sStorage(setting string, class func(*env) string, rwx bool) func(context.Context, *env) outcome {
	return func(ctx context.Context, e *env) outcome {
		c, err := e.cluster()
		if err != nil {
			return clusterUnreachable()
		}
		name := class(e)
		info, found, err := c.StorageClass(ctx, name)
		if err != nil {
			return warn("Give the control plane's service account the right to get storage classes.", "can't read storage classes: %v", err)
		}
		setting := "kubernetes.storage_classes." + setting
		if !found {
			if name == "" {
				return fail("Mark a storage class as the cluster's default, or set "+setting+".", "the cluster has no default storage class")
			}
			return fail("Create the storage class, or correct "+setting+".", "storage class %q doesn't exist", name)
		}
		if !rwx {
			return pass("storage class %q (%s)", info.Name, info.Provisioner)
		}
		if slices.Contains(rwxIncapable, info.Provisioner) {
			return fail("Use a ReadWriteMany class (NFS, CephFS, Longhorn, EFS, Filestore, Azure Files) and set "+setting+".",
				"storage class %q (%s) provides ReadWriteOnce volumes only; the data directory needs ReadWriteMany", info.Name, info.Provisioner)
		}
		if slices.Contains(unshareable, info.Provisioner) {
			return warn("Use NFS (nfs.csi.k8s.io), CephFS, or EFS for the data directory and set "+setting+".",
				"storage class %q (%s) is known not to accept several PersistentVolumes on one volume, which site tasks mount the data directory through",
				info.Name, info.Provisioner)
		}
		return pass("storage class %q (%s); whether it supports ReadWriteMany shows when the data volume binds", info.Name, info.Provisioner)
	}
}

func checkK8sMetrics(ctx context.Context, e *env) outcome {
	c, err := e.cluster()
	if err != nil {
		return clusterUnreachable()
	}
	if err := c.MetricsAPI(ctx); err != nil {
		return fail("Install metrics-server (https://github.com/kubernetes-sigs/metrics-server) and check that its APIService is available.",
			"metrics.k8s.io doesn't answer: %v", err)
	}
	return pass("metrics.k8s.io answers")
}

func checkK8sNetworkPolicy(ctx context.Context, e *env) outcome {
	c, err := e.cluster()
	if err != nil {
		return clusterUnreachable()
	}
	if e.SkipClusterPods {
		return outcome{status: StatusUnavailable, message: "runs probe pods, so it runs at startup, in `pressfleet doctor`, and in the Helm test"}
	}
	res, err := c.NetworkPolicy(ctx)
	if errors.Is(err, ErrNoProbeImage) {
		return warn("Set task_image (PRESSFLEET_TASK_IMAGE) to the control plane image.", "can't check: %v", err)
	}
	if err != nil {
		return warn("Check that the control plane may create namespaces, pods, and NetworkPolicies, and that nodes can pull the task image.",
			"couldn't run the probe pods: %v", err)
	}
	switch res.State {
	case NetworkPolicyEnforced:
		return pass("the CNI enforces NetworkPolicy: a denied pod couldn't connect")
	case NetworkPolicyIgnored:
		return fail("Install a CNI that enforces NetworkPolicy, such as Calico or Cilium. Without it, sites can reach each other and the platform.",
			"the CNI ignores NetworkPolicy: a pod it denied connected anyway")
	}
	return warn("Check that nodes can pull the task image and that pods can reach the API server.",
		"couldn't prove that NetworkPolicy is enforced: %s", res.Detail)
}

func checkK8sTraefik(ctx context.Context, e *env) outcome {
	c, err := e.cluster()
	if err != nil {
		return clusterUnreachable()
	}
	addrs, err := c.TraefikAddresses(ctx)
	if err != nil {
		return warn("Check that Traefik is installed and kubernetes.traefik_service names its Service.", "can't read Traefik's Service: %v", err)
	}
	if len(addrs) == 0 {
		return warn("Give Traefik's Service an external address: a LoadBalancer (MetalLB, or the cloud's controller) or external IPs.",
			"Traefik's Service has no external address yet")
	}
	return pass("Traefik is reachable at %s", strings.Join(addrs, ", "))
}

func checkK8sDashboardDNS(ctx context.Context, e *env) outcome {
	if e.DashboardDomain == "" {
		return skip("dashboard_domain is not set")
	}
	return checkK8sDNS(ctx, e, e.DashboardDomain, e.DashboardDomain)
}

func checkK8sPreviewDNS(ctx context.Context, e *env) outcome {
	if e.PreviewBaseDomain == "" {
		return skip("preview_base_domain is not set")
	}
	label := e.RandomLabel
	if label == nil {
		label = randomLabel
	}
	return checkK8sDNS(ctx, e, "*."+e.PreviewBaseDomain, label()+"."+e.PreviewBaseDomain)
}

// checkK8sDNS verifies that name (probed by resolving lookup) points at
// Traefik's external address, or at public_ip when that is set.
func checkK8sDNS(ctx context.Context, e *env, name, lookup string) outcome {
	c, err := e.cluster()
	if err != nil {
		return clusterUnreachable()
	}
	want := map[netip.Addr]bool{}
	if e.PublicIP.IsValid() {
		want[e.PublicIP.Unmap()] = true
	}
	addrs, err := c.TraefikAddresses(ctx)
	if err != nil || (len(addrs) == 0 && len(want) == 0) {
		return skip("Traefik has no external address to compare with (see the Traefik check)")
	}
	for _, a := range addrs {
		if ip, err := netip.ParseAddr(a); err == nil {
			want[ip.Unmap()] = true
			continue
		}
		// A load balancer given as a host name (AWS): compare addresses.
		if ips, err := e.Host.LookupHost(ctx, a); err == nil {
			for _, ip := range ips {
				want[ip.Unmap()] = true
			}
		}
	}
	fix := fmt.Sprintf("Create a DNS record for %s pointing at Traefik's external address.", name)
	ctx, cancel := context.WithTimeout(ctx, dnsTimeout)
	defer cancel()
	got, err := e.Host.LookupHost(ctx, lookup)
	if err != nil || len(got) == 0 {
		if err == nil {
			err = errors.New("no addresses")
		}
		return fail(fix, "%s does not resolve: %v", name, err)
	}
	for _, a := range got {
		if want[a.Unmap()] {
			return pass("%s resolves to %s, Traefik's address", name, a.Unmap())
		}
	}
	return fail(fix, "%s resolves to %s, which isn't Traefik's address", name, joinAddrs(got))
}

func checkK8sCapacity(ctx context.Context, e *env) outcome {
	if e.Reservations == nil {
		return skip("site reservations are not available yet")
	}
	c, err := e.cluster()
	if err != nil {
		return clusterUnreachable()
	}
	reserved, sites, err := e.Reservations.MemoryReservations(ctx)
	if err != nil {
		return warn("Check the control plane logs.", "could not read site reservations: %v", err)
	}
	nodes, err := c.Nodes(ctx)
	if err != nil {
		return warn("Give the control plane's service account the right to list nodes.", "can't list nodes: %v", err)
	}
	var allocatable uint64
	for _, n := range nodes {
		if n.Schedulable() {
			allocatable += n.AllocatableMemory
		}
	}
	platformReq, err := c.PlatformMemoryRequests(ctx)
	if err != nil {
		return warn("Give the control plane's service account the right to list pods in the platform namespace.", "can't read the platform's requests: %v", err)
	}
	if reserved+platformReq > allocatable {
		return warn("Add nodes or memory, or reduce site reservations or the number of sites.",
			"%d sites request %s and the platform %s, which exceeds the cluster's allocatable memory (%s)",
			sites, formatGiB(reserved), formatGiB(platformReq), formatGiB(allocatable))
	}
	return pass("%d sites request %s and the platform %s of %s allocatable", sites, formatGiB(reserved), formatGiB(platformReq), formatGiB(allocatable))
}

// clusterChecks are the checks of a Kubernetes install (§4.8).
func clusterChecks() []Check {
	sites := func(e *env) string { return e.StorageClassSites }
	data := func(e *env) string { return e.StorageClassData }
	return []Check{
		{ID: "k8s-version", Name: "Kubernetes version", Severity: Required, Scopes: both, ClusterOnly: true, run: checkK8sVersion},
		{ID: "k8s-nodes", Name: "Nodes are linux/amd64", Severity: Required, Scopes: both, ClusterOnly: true, run: checkK8sNodes},
		{ID: "k8s-cgroup", Name: "Nodes use cgroup v2", Severity: Required, Scopes: both, ClusterOnly: true, run: checkK8sCgroup},
		{ID: "k8s-rbac", Name: "Control plane RBAC", Severity: Required, Scopes: both, ClusterOnly: true, run: checkK8sRBAC},
		{ID: "k8s-storage-sites", Name: "Site storage class", Severity: Required, Scopes: both, ClusterOnly: true, run: checkK8sStorage("sites", sites, false)},
		{ID: "k8s-storage-data", Name: "Data storage class", Severity: Required, Scopes: both, ClusterOnly: true, run: checkK8sStorage("data", data, true)},
		{ID: "k8s-metrics", Name: "metrics.k8s.io", Severity: Required, Scopes: both, ClusterOnly: true, run: checkK8sMetrics},
		{ID: "k8s-networkpolicy", Name: "NetworkPolicy enforcement", Severity: Required, Scopes: both, ClusterOnly: true, run: checkK8sNetworkPolicy},
		{ID: "k8s-traefik", Name: "Traefik external address", Severity: Recommended, Scopes: both, ClusterOnly: true, run: checkK8sTraefik},
		{ID: "k8s-dns-dashboard", Name: "Dashboard DNS", Severity: Recommended, Scopes: both, ClusterOnly: true, run: checkK8sDashboardDNS},
		{ID: "k8s-dns-preview", Name: "Wildcard preview DNS", Severity: Recommended, Scopes: both, ClusterOnly: true, run: checkK8sPreviewDNS},
		{ID: "k8s-capacity", Name: "Cluster memory capacity", Severity: Recommended, Scopes: runtimeOnly, ClusterOnly: true, run: checkK8sCapacity},
	}
}
