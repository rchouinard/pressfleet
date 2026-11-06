package doctortest

import (
	"context"

	"github.com/rchouinard/pressfleet/control-plane/internal/doctor"
)

// Cluster is a fake doctor.ClusterProbe. HealthyCluster returns one that
// passes every check; tests change individual fields.
type Cluster struct {
	Version    doctor.ClusterVersion
	VersionErr error
	NodeList   []doctor.ClusterNode
	NodesErr   error
	Cgroups    []doctor.NodeCgroup
	CgroupErr  error
	Rights     doctor.RBACReport
	RBACErr    error
	// Classes maps names to classes; the key "" is the default class.
	Classes    map[string]doctor.StorageClassInfo
	ClassErr   error
	MetricsErr error
	NetPol     doctor.NetworkPolicyResult
	NetPolErr  error
	Traefik    []string
	TraefikErr error
	Requests   uint64
	RequestErr error
	// PodProbes counts the calls that would run pods.
	PodProbes int
}

var _ doctor.ClusterProbe = (*Cluster)(nil)

// HealthyCluster returns a Cluster that satisfies every check: Kubernetes
// 1.35 with two amd64 nodes of 8 GiB, cgroup v2, enforced NetworkPolicy,
// and Traefik at 198.51.100.20.
func HealthyCluster() *Cluster {
	return &Cluster{
		Version: doctor.ClusterVersion{Major: 1, Minor: 35, Git: "v1.35.1"},
		NodeList: []doctor.ClusterNode{
			{Name: "n1", OS: "linux", Arch: "amd64", Ready: true, AllocatableMemory: 8 << 30},
			{Name: "n2", OS: "linux", Arch: "amd64", Ready: true, AllocatableMemory: 8 << 30},
		},
		Cgroups: []doctor.NodeCgroup{{Node: "n1", V2: true}, {Node: "n2", V2: true}},
		Classes: map[string]doctor.StorageClassInfo{
			"":      {Name: "standard", Provisioner: "example.com/rwo"},
			"rwx":   {Name: "rwx", Provisioner: "nfs.csi.k8s.io"},
			"local": {Name: "local", Provisioner: "rancher.io/local-path"},
		},
		NetPol:  doctor.NetworkPolicyResult{State: doctor.NetworkPolicyEnforced},
		Traefik: []string{"198.51.100.20"},
	}
}

// ServerVersion implements doctor.ClusterProbe.
func (c *Cluster) ServerVersion(context.Context) (doctor.ClusterVersion, error) {
	return c.Version, c.VersionErr
}

// Nodes implements doctor.ClusterProbe.
func (c *Cluster) Nodes(context.Context) ([]doctor.ClusterNode, error) { return c.NodeList, c.NodesErr }

// CgroupV2 implements doctor.ClusterProbe.
func (c *Cluster) CgroupV2(context.Context, []doctor.ClusterNode) ([]doctor.NodeCgroup, error) {
	c.PodProbes++
	return c.Cgroups, c.CgroupErr
}

// RBAC implements doctor.ClusterProbe.
func (c *Cluster) RBAC(context.Context) (doctor.RBACReport, error) { return c.Rights, c.RBACErr }

// StorageClass implements doctor.ClusterProbe.
func (c *Cluster) StorageClass(_ context.Context, name string) (doctor.StorageClassInfo, bool, error) {
	info, ok := c.Classes[name]
	return info, ok, c.ClassErr
}

// MetricsAPI implements doctor.ClusterProbe.
func (c *Cluster) MetricsAPI(context.Context) error { return c.MetricsErr }

// NetworkPolicy implements doctor.ClusterProbe.
func (c *Cluster) NetworkPolicy(context.Context) (doctor.NetworkPolicyResult, error) {
	c.PodProbes++
	return c.NetPol, c.NetPolErr
}

// TraefikAddresses implements doctor.ClusterProbe.
func (c *Cluster) TraefikAddresses(context.Context) ([]string, error) { return c.Traefik, c.TraefikErr }

// PlatformMemoryRequests implements doctor.ClusterProbe.
func (c *Cluster) PlatformMemoryRequests(context.Context) (uint64, error) {
	return c.Requests, c.RequestErr
}
