package doctor_test

import (
	"context"
	"errors"
	"net/netip"
	"testing"

	"github.com/rchouinard/pressfleet/control-plane/internal/doctor"
	"github.com/rchouinard/pressfleet/control-plane/internal/doctor/doctortest"
)

var traefikIP = netip.MustParseAddr("198.51.100.20")

// k8sFixture is a healthy Kubernetes install that setup breaks.
type k8sFixture struct {
	*fixture
	cluster *doctortest.Cluster
}

// runK8sCases is runCases for a Kubernetes install: setup gets the cluster
// to break as well as the fixture.
func runK8sCases(t *testing.T, id string, cases []k8sCase) {
	t.Helper()
	var cc []checkCase
	for _, tc := range cases {
		cc = append(cc, checkCase{
			name: tc.name, wantStatus: tc.wantStatus, wantMessage: tc.wantMessage, wantFix: tc.wantFix,
			setup: func(f *fixture) {
				c := doctortest.HealthyCluster()
				f.opts.Kubernetes = true
				f.opts.Cluster = c
				f.opts.DashboardDomain = "fleet.example.com"
				f.opts.PreviewBaseDomain = "preview.example.com"
				f.opts.RandomLabel = func() string { return "probe" }
				f.host.DNS["fleet.example.com"] = []netip.Addr{traefikIP}
				f.host.DNS["*.preview.example.com"] = []netip.Addr{traefikIP}
				if tc.setup != nil {
					tc.setup(&k8sFixture{f, c})
				}
			},
		})
	}
	runCases(t, id, cc)
}

type k8sCase struct {
	name        string
	setup       func(*k8sFixture)
	wantStatus  doctor.Status
	wantMessage string
	wantFix     string
}

func TestK8sVersion(t *testing.T) {
	runK8sCases(t, "k8s-version", []k8sCase{
		{name: "supported", wantStatus: doctor.StatusPass, wantMessage: "v1.35.1"},
		{name: "newer", setup: func(f *k8sFixture) { f.cluster.Version.Minor = 40 }, wantStatus: doctor.StatusPass},
		{name: "too old", setup: func(f *k8sFixture) {
			f.cluster.Version = doctor.ClusterVersion{Major: 1, Minor: 29, Git: "v1.29.4"}
		}, wantStatus: doctor.StatusFail, wantMessage: "v1.29.4", wantFix: "Upgrade"},
		{name: "unreachable", setup: func(f *k8sFixture) { f.cluster.VersionErr = errors.New("connection refused") },
			wantStatus: doctor.StatusFail, wantMessage: "connection refused", wantFix: "kubeconfig"},
		{name: "no connection", setup: func(f *k8sFixture) { f.opts.Cluster, f.opts.ClusterErr = nil, errors.New("not in a pod") },
			wantStatus: doctor.StatusFail, wantMessage: "not in a pod", wantFix: "kubeconfig"},
	})
}

func TestK8sNodes(t *testing.T) {
	runK8sCases(t, "k8s-nodes", []k8sCase{
		{name: "amd64", wantStatus: doctor.StatusPass, wantMessage: "2 linux/amd64"},
		{name: "arm64 node", setup: func(f *k8sFixture) { f.cluster.NodeList[1].Arch = "arm64" },
			wantStatus: doctor.StatusFail, wantMessage: "n2 (linux/arm64)", wantFix: "Cordon"},
		{name: "cordoned arm64 node ignored", setup: func(f *k8sFixture) {
			f.cluster.NodeList[1].Arch, f.cluster.NodeList[1].Unschedulable = "arm64", true
		}, wantStatus: doctor.StatusPass, wantMessage: "1 linux/amd64"},
		{name: "no ready node", setup: func(f *k8sFixture) {
			f.cluster.NodeList[0].Ready, f.cluster.NodeList[1].Ready = false, false
		}, wantStatus: doctor.StatusFail, wantMessage: "no ready", wantFix: "Add"},
		{name: "list denied", setup: func(f *k8sFixture) { f.cluster.NodesErr = errors.New("forbidden") },
			wantStatus: doctor.StatusFail, wantMessage: "forbidden", wantFix: "list nodes"},
	})
}

func TestK8sCgroup(t *testing.T) {
	runK8sCases(t, "k8s-cgroup", []k8sCase{
		{name: "v2", wantStatus: doctor.StatusPass, wantMessage: "2 nodes use cgroup v2"},
		{name: "v1 node", setup: func(f *k8sFixture) { f.cluster.Cgroups[1].V2 = false },
			wantStatus: doctor.StatusFail, wantMessage: "cgroup v1: n2", wantFix: "unified_cgroup_hierarchy"},
		{name: "node not checked", setup: func(f *k8sFixture) { f.cluster.Cgroups[0] = doctor.NodeCgroup{Node: "n1", Err: "ImagePullBackOff"} },
			wantStatus: doctor.StatusWarn, wantMessage: "n1: ImagePullBackOff", wantFix: "pull"},
		{name: "no image", setup: func(f *k8sFixture) { f.cluster.CgroupErr = doctor.ErrNoProbeImage },
			wantStatus: doctor.StatusWarn, wantMessage: "task_image", wantFix: "TASK_IMAGE"},
		{name: "pods can't run", setup: func(f *k8sFixture) { f.cluster.CgroupErr = errors.New("forbidden") },
			wantStatus: doctor.StatusWarn, wantMessage: "forbidden", wantFix: "namespaces"},
	})
}

func TestK8sRBAC(t *testing.T) {
	runK8sCases(t, "k8s-rbac", []k8sCase{
		{name: "least privilege", wantStatus: doctor.StatusPass},
		{name: "missing", setup: func(f *k8sFixture) { f.cluster.Rights.Missing = []string{"create namespaces"} },
			wantStatus: doctor.StatusFail, wantMessage: "create namespaces", wantFix: "Grant"},
		{name: "cluster-wide excess", setup: func(f *k8sFixture) {
			f.cluster.Rights.Excess = []string{"get secrets in all namespaces"}
		}, wantStatus: doctor.StatusFail, wantMessage: "get secrets in all namespaces", wantFix: "cluster-admin"},
		{name: "review failed", setup: func(f *k8sFixture) { f.cluster.RBACErr = errors.New("unavailable") },
			wantStatus: doctor.StatusWarn, wantMessage: "unavailable"},
	})
}

func TestK8sStorage(t *testing.T) {
	runK8sCases(t, "k8s-storage-sites", []k8sCase{
		{name: "default class", wantStatus: doctor.StatusPass, wantMessage: `"standard"`},
		{name: "no default", setup: func(f *k8sFixture) { delete(f.cluster.Classes, "") },
			wantStatus: doctor.StatusFail, wantMessage: "no default", wantFix: "storage_classes.sites"},
		{name: "named class", setup: func(f *k8sFixture) { f.opts.StorageClassSites = "rwx" },
			wantStatus: doctor.StatusPass, wantMessage: `"rwx"`},
		{name: "missing class", setup: func(f *k8sFixture) { f.opts.StorageClassSites = "gone" },
			wantStatus: doctor.StatusFail, wantMessage: `"gone" doesn't exist`, wantFix: "storage_classes.sites"},
		{name: "read denied", setup: func(f *k8sFixture) { f.cluster.ClassErr = errors.New("forbidden") },
			wantStatus: doctor.StatusWarn, wantMessage: "forbidden", wantFix: "storage classes"},
	})
	runK8sCases(t, "k8s-storage-data", []k8sCase{
		{name: "rwo-only default", wantStatus: doctor.StatusPass, wantMessage: "ReadWriteMany"},
		{name: "rwx class", setup: func(f *k8sFixture) { f.opts.StorageClassData = "rwx" },
			wantStatus: doctor.StatusPass, wantMessage: `"rwx"`},
		{name: "node-local class", setup: func(f *k8sFixture) { f.opts.StorageClassData = "local" },
			wantStatus: doctor.StatusFail, wantMessage: "ReadWriteOnce volumes only", wantFix: "storage_classes.data"},
	})
}

func TestK8sMetrics(t *testing.T) {
	runK8sCases(t, "k8s-metrics", []k8sCase{
		{name: "answers", wantStatus: doctor.StatusPass},
		{name: "missing", setup: func(f *k8sFixture) { f.cluster.MetricsErr = errors.New("the server is currently unable") },
			wantStatus: doctor.StatusFail, wantMessage: "unable", wantFix: "metrics-server"},
	})
}

func TestK8sNetworkPolicy(t *testing.T) {
	runK8sCases(t, "k8s-networkpolicy", []k8sCase{
		{name: "enforced", wantStatus: doctor.StatusPass, wantMessage: "enforces"},
		{name: "ignored", setup: func(f *k8sFixture) {
			f.cluster.NetPol = doctor.NetworkPolicyResult{State: doctor.NetworkPolicyIgnored}
		}, wantStatus: doctor.StatusFail, wantMessage: "ignores NetworkPolicy", wantFix: "Calico or Cilium"},
		{name: "inconclusive", setup: func(f *k8sFixture) {
			f.cluster.NetPol = doctor.NetworkPolicyResult{State: doctor.NetworkPolicyInconclusive, Detail: "ErrImagePull"}
		}, wantStatus: doctor.StatusWarn, wantMessage: "ErrImagePull", wantFix: "pull"},
		{name: "no image", setup: func(f *k8sFixture) { f.cluster.NetPolErr = doctor.ErrNoProbeImage },
			wantStatus: doctor.StatusWarn, wantMessage: "task_image", wantFix: "TASK_IMAGE"},
		{name: "pods can't run", setup: func(f *k8sFixture) { f.cluster.NetPolErr = errors.New("forbidden") },
			wantStatus: doctor.StatusWarn, wantMessage: "forbidden", wantFix: "NetworkPolicies"},
	})
}

func TestK8sTraefik(t *testing.T) {
	runK8sCases(t, "k8s-traefik", []k8sCase{
		{name: "load balancer", wantStatus: doctor.StatusPass, wantMessage: "198.51.100.20"},
		{name: "pending", setup: func(f *k8sFixture) { f.cluster.Traefik = nil },
			wantStatus: doctor.StatusWarn, wantMessage: "no external address", wantFix: "LoadBalancer"},
		{name: "no service", setup: func(f *k8sFixture) { f.cluster.TraefikErr = errors.New("not found") },
			wantStatus: doctor.StatusWarn, wantMessage: "not found", wantFix: "traefik_service"},
	})
}

func TestK8sDNS(t *testing.T) {
	runK8sCases(t, "k8s-dns-dashboard", []k8sCase{
		{name: "points at Traefik", wantStatus: doctor.StatusPass, wantMessage: "Traefik's address"},
		{name: "points elsewhere", setup: func(f *k8sFixture) { f.host.DNS["fleet.example.com"] = []netip.Addr{otherIP} },
			wantStatus: doctor.StatusFail, wantMessage: "192.0.2.99", wantFix: "Traefik's external address"},
		{name: "doesn't resolve", setup: func(f *k8sFixture) { delete(f.host.DNS, "fleet.example.com") },
			wantStatus: doctor.StatusFail, wantMessage: "does not resolve"},
		{name: "public ip", setup: func(f *k8sFixture) {
			f.cluster.Traefik = []string{"10.0.0.5"}
			f.opts.PublicIP = traefikIP
		}, wantStatus: doctor.StatusPass},
		{name: "load balancer host name", setup: func(f *k8sFixture) {
			f.cluster.Traefik = []string{"lb.example.net"}
			f.host.DNS["lb.example.net"] = []netip.Addr{traefikIP}
		}, wantStatus: doctor.StatusPass},
		{name: "no address yet", setup: func(f *k8sFixture) { f.cluster.Traefik = nil },
			wantStatus: doctor.StatusSkip, wantMessage: "no external address"},
		{name: "no domain", setup: func(f *k8sFixture) { f.opts.DashboardDomain = "" },
			wantStatus: doctor.StatusSkip, wantMessage: "dashboard_domain"},
	})
	runK8sCases(t, "k8s-dns-preview", []k8sCase{
		{name: "wildcard", wantStatus: doctor.StatusPass, wantMessage: "*.preview.example.com"},
		{name: "no wildcard", setup: func(f *k8sFixture) { delete(f.host.DNS, "*.preview.example.com") },
			wantStatus: doctor.StatusFail, wantMessage: "does not resolve", wantFix: "*.preview.example.com"},
	})
}

func TestK8sCapacity(t *testing.T) {
	reserve := func(f *k8sFixture, bytes uint64, sites int) {
		f.opts.Reservations = doctortest.Reservations{Total: bytes, Sites: sites}
	}
	runK8sCases(t, "k8s-capacity", []k8sCase{
		{name: "fits", setup: func(f *k8sFixture) { reserve(f, 4*gib, 30); f.cluster.Requests = 2 * gib },
			wantStatus: doctor.StatusPass, wantMessage: "30 sites request 4.0 GiB"},
		{name: "exceeds", setup: func(f *k8sFixture) { reserve(f, 15*gib, 100); f.cluster.Requests = 2 * gib },
			wantStatus: doctor.StatusWarn, wantMessage: "exceeds the cluster's allocatable memory (16.0 GiB)", wantFix: "Add nodes"},
		{name: "cordoned node doesn't count", setup: func(f *k8sFixture) {
			reserve(f, 9*gib, 70)
			f.cluster.NodeList[1].Unschedulable = true
		}, wantStatus: doctor.StatusWarn, wantMessage: "8.0 GiB"},
		{name: "no provider", wantStatus: doctor.StatusSkip, wantMessage: "not available"},
		{name: "requests unreadable", setup: func(f *k8sFixture) { reserve(f, gib, 1); f.cluster.RequestErr = errors.New("forbidden") },
			wantStatus: doctor.StatusWarn, wantMessage: "forbidden", wantFix: "list pods"},
	})
}

func TestKubernetesRunsClusterChecksInPlaceOfHostChecks(t *testing.T) {
	f := newFixture()
	f.opts.Kubernetes = true
	f.opts.Cluster = doctortest.HealthyCluster()
	f.opts.Host, f.opts.Docker = f.host, f.docker
	rep := doctor.Run(context.Background(), f.opts, doctor.ScopeRuntime)
	got := map[string]doctor.Status{}
	for _, r := range rep.Results {
		got[r.ID] = r.Status
	}
	for _, id := range []string{"k8s-version", "k8s-nodes", "k8s-cgroup", "k8s-rbac", "k8s-storage-sites", "k8s-storage-data", "k8s-metrics", "k8s-networkpolicy"} {
		if got[id] != doctor.StatusPass {
			t.Errorf("%s = %q, want pass", id, got[id])
		}
	}
	for _, id := range []string{"docker", "compose", "memory"} {
		if got[id] != doctor.StatusUnavailable {
			t.Errorf("%s = %q, want unavailable on Kubernetes", id, got[id])
		}
	}
	if got := rep.Outcome(); got != doctor.OK {
		t.Errorf("outcome = %v, want ok: %+v", got, rep.Results)
	}
}

func TestComposeHasNoClusterChecks(t *testing.T) {
	f := newFixture()
	f.opts.Host, f.opts.Docker = f.host, f.docker
	for _, r := range doctor.Run(context.Background(), f.opts, doctor.ScopeInstall, doctor.ScopeRuntime).Results {
		if len(r.ID) > 4 && r.ID[:4] == "k8s-" {
			t.Errorf("Compose install ran %s", r.ID)
		}
	}
}

func TestSkipClusterPods(t *testing.T) {
	f := newFixture()
	c := doctortest.HealthyCluster()
	f.opts.Kubernetes, f.opts.Cluster, f.opts.SkipClusterPods = true, c, true
	f.opts.Host, f.opts.Docker = f.host, f.docker
	rep := doctor.Run(context.Background(), f.opts, doctor.ScopeRuntime)
	for _, r := range rep.Results {
		if (r.ID == "k8s-cgroup" || r.ID == "k8s-networkpolicy") && r.Status != doctor.StatusUnavailable {
			t.Errorf("%s = %q, want unavailable", r.ID, r.Status)
		}
	}
	if c.PodProbes != 0 {
		t.Errorf("%d probes ran pods", c.PodProbes)
	}
}
