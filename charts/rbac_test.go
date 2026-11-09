package charts_test

import (
	"errors"
	"io"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

// rbacObject is the part of a rendered RBAC or admission object that
// TestRBACLeastPrivilege reads.
type rbacObject struct {
	Kind     string `yaml:"kind"`
	Metadata struct {
		Name      string `yaml:"name"`
		Namespace string `yaml:"namespace"`
	} `yaml:"metadata"`
	Rules []struct {
		APIGroups     []string `yaml:"apiGroups"`
		Resources     []string `yaml:"resources"`
		Verbs         []string `yaml:"verbs"`
		ResourceNames []string `yaml:"resourceNames"`
	} `yaml:"rules"`
	RoleRef struct {
		Kind string `yaml:"kind"`
		Name string `yaml:"name"`
	} `yaml:"roleRef"`
	Spec struct {
		PolicyName string `yaml:"policyName"`
	} `yaml:"spec"`
}

// TestRBACLeastPrivilege keeps the chart's RBAC within what the Kubernetes
// security review (docs/security-review.md, #229) found: no wildcards, no
// escalate or impersonate, bind on the site ClusterRole alone, a
// cluster-wide control plane role that reads no Secrets and reaches no
// node or pod, pods that are never changed in place, an admission policy
// for each cluster-wide right RBAC can't narrow, and outside the platform
// namespace only the read of the API server's EndpointSlice (#324).
func TestRBACLeastPrivilege(t *testing.T) {
	const (
		controlPlane = "pressfleet:" + releaseNamespace + ":control-plane"
		site         = "pressfleet:" + releaseNamespace + ":site"
		traefik      = "pressfleet:" + releaseNamespace + ":traefik"
		apiServer    = "pressfleet:" + releaseNamespace + ":api-server-endpoints"
	)
	// What the control plane's ClusterRole may name: group/resource, and
	// whether it may write it. Its writes are narrowed by the admission
	// policies below.
	clusterWide := map[string]bool{
		"/namespaces":                            true,
		"rbac.authorization.k8s.io/rolebindings": true,
		"rbac.authorization.k8s.io/clusterroles": true, // bind, checked below
		"/nodes":                                 false,
		"metrics.k8s.io/nodes":                   false,
		"storage.k8s.io/storageclasses":          false,
		"/persistentvolumes":                     true,
	}
	writes := func(verbs []string) bool {
		return slices.ContainsFunc(verbs, func(v string) bool {
			return !slices.Contains([]string{"get", "list", "watch"}, v)
		})
	}
	for _, values := range valuesFiles(t) {
		t.Run(filepath.Base(values), func(t *testing.T) {
			out, err := render(t, values)
			if err != nil {
				t.Fatalf("helm template: %v", err)
			}
			roles := map[string]bool{}
			var policies, bindings []string
			dec := yaml.NewDecoder(strings.NewReader(out))
			for {
				var o rbacObject
				err := dec.Decode(&o)
				if errors.Is(err, io.EOF) {
					break
				}
				if err != nil {
					t.Fatalf("decoding helm template output: %v", err)
				}
				switch o.Kind {
				case "ValidatingAdmissionPolicy":
					policies = append(policies, o.Metadata.Name)
				case "ValidatingAdmissionPolicyBinding":
					bindings = append(bindings, o.Spec.PolicyName)
				case "ClusterRoleBinding":
					if o.RoleRef.Name != controlPlane && o.RoleRef.Name != traefik {
						t.Errorf("ClusterRoleBinding %s binds %s cluster-wide", o.Metadata.Name, o.RoleRef.Name)
					}
				}
				if o.Kind != "ClusterRole" && o.Kind != "Role" {
					continue
				}
				name := o.Kind + "/" + o.Metadata.Name
				roles[name] = true
				// A Role elsewhere than the platform namespace, which the
				// chart's Roles leave to the release, may only read where
				// the API server publishes its addresses.
				if o.Kind == "Role" && o.Metadata.Namespace != "" && o.Metadata.Namespace != releaseNamespace {
					r := o.Rules
					if o.Metadata.Name != apiServer || o.Metadata.Namespace != "default" || len(r) != 1 ||
						!slices.Equal(r[0].APIGroups, []string{"discovery.k8s.io"}) || !slices.Equal(r[0].Resources, []string{"endpointslices"}) ||
						!slices.Equal(r[0].Verbs, []string{"get"}) || !slices.Equal(r[0].ResourceNames, []string{"kubernetes"}) {
						t.Errorf("Role %s/%s: rights outside the platform namespace beyond get on the EndpointSlice default/kubernetes: %+v",
							o.Metadata.Namespace, o.Metadata.Name, r)
					}
				}
				for _, r := range o.Rules {
					for _, field := range [][]string{r.APIGroups, r.Resources, r.Verbs} {
						if slices.Contains(field, "*") {
							t.Errorf("%s: a wildcard in %v", name, r)
						}
					}
					for _, v := range []string{"escalate", "impersonate"} {
						if slices.Contains(r.Verbs, v) {
							t.Errorf("%s: %s on %v", name, v, r.Resources)
						}
					}
					if slices.Contains(r.Verbs, "bind") &&
						(!slices.Equal(r.Resources, []string{"clusterroles"}) || !slices.Equal(r.ResourceNames, []string{site}) || len(r.Verbs) != 1) {
						t.Errorf("%s: bind on anything but the site ClusterRole: %v", name, r)
					}
					for _, res := range r.Resources {
						switch {
						case res == "pods" && (slices.Contains(r.Verbs, "update") || slices.Contains(r.Verbs, "patch")):
							t.Errorf("%s: changes pods in place (%v); a new image would run as the pod's ServiceAccount", name, r.Verbs)
						case strings.HasPrefix(res, "serviceaccounts") && writes(r.Verbs):
							t.Errorf("%s: writes %s", name, res)
						case res == "nodes/proxy":
							t.Errorf("%s: nodes/proxy", name)
						}
						if o.Metadata.Name == traefik && writes(r.Verbs) {
							t.Errorf("%s: writes %s (%v)", name, res, r.Verbs)
						}
						if o.Metadata.Name != controlPlane {
							continue
						}
						for _, g := range r.APIGroups {
							write, ok := clusterWide[g+"/"+res]
							switch {
							case !ok:
								t.Errorf("%s: cluster-wide access to %s/%s", name, g, res)
							case !write && writes(r.Verbs):
								t.Errorf("%s: writes %s/%s (%v)", name, g, res, r.Verbs)
							}
						}
					}
				}
			}
			for _, want := range []string{"ClusterRole/" + controlPlane, "ClusterRole/" + site, "ClusterRole/" + traefik, "Role/control-plane", "Role/" + apiServer} {
				if !roles[want] {
					t.Errorf("no %s rendered", want)
				}
			}
			// The cluster-wide writes RBAC can't narrow to PressFleet's
			// namespaces and volumes, and the pods and Services the
			// control plane creates, each have a policy, bound.
			for _, want := range []string{
				"control-plane." + releaseNamespace + ".pressfleet.io",
				"data-volume." + releaseNamespace + ".pressfleet.io",
				"data-volume-clones." + releaseNamespace + ".pressfleet.io",
				"workloads." + releaseNamespace + ".pressfleet.io",
			} {
				if !slices.Contains(policies, want) || !slices.Contains(bindings, want) {
					t.Errorf("admission policy %s: rendered %v, bound %v", want, slices.Contains(policies, want), slices.Contains(bindings, want))
				}
			}
		})
	}
}
