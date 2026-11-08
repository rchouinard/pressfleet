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

// labelSelector is a metav1.LabelSelector: the charts package can't import
// Kubernetes' (archtest), so selectorMatches evaluates it as the API does.
type labelSelector struct {
	MatchLabels      map[string]string `yaml:"matchLabels"`
	MatchExpressions []struct {
		Key      string   `yaml:"key"`
		Operator string   `yaml:"operator"`
		Values   []string `yaml:"values"`
	} `yaml:"matchExpressions"`
}

// selectorMatches reports whether s selects an object with labels. A nil
// selector selects nothing, as a missing namespaceSelector does.
func selectorMatches(t *testing.T, s *labelSelector, labels map[string]string) bool {
	t.Helper()
	if s == nil {
		return false
	}
	for k, v := range s.MatchLabels {
		if got, ok := labels[k]; !ok || got != v {
			return false
		}
	}
	for _, e := range s.MatchExpressions {
		v, ok := labels[e.Key]
		var match bool
		switch e.Operator {
		case "Exists":
			match = ok
		case "DoesNotExist":
			match = !ok
		case "In":
			match = ok && slices.Contains(e.Values, v)
		case "NotIn":
			match = !ok || !slices.Contains(e.Values, v)
		default:
			t.Fatalf("unknown selector operator %q", e.Operator)
		}
		if !match {
			return false
		}
	}
	return true
}

// networkPolicy is the part of a rendered NetworkPolicy that
// TestPlatformPoliciesAdmitOwnSites reads.
type networkPolicy struct {
	Kind     string `yaml:"kind"`
	Metadata struct {
		Name string `yaml:"name"`
	} `yaml:"metadata"`
	Spec struct {
		Ingress []struct {
			From []policyPeer `yaml:"from"`
		} `yaml:"ingress"`
	} `yaml:"spec"`
}

type policyPeer struct {
	NamespaceSelector *labelSelector `yaml:"namespaceSelector"`
	PodSelector       *labelSelector `yaml:"podSelector"`
}

// TestPlatformPoliciesAdmitOwnSites checks that the platform's
// NetworkPolicies admit pods from this install's site namespaces and not
// from another install's that share the prefix (security review K6, #326).
// Site namespaces from before the platform namespace label are admitted
// until the template rollout labels them.
func TestPlatformPoliciesAdmitOwnSites(t *testing.T) {
	site := func(platformNamespace string) map[string]string {
		l := map[string]string{
			"app.kubernetes.io/managed-by": "pressfleet",
			"pressfleet.io/site-id":        "01jabcdefghjkmnpqrstvwxyz0",
			"kubernetes.io/metadata.name":  "pf-01jabcdefghjkmnpqrstvwxyz0",
		}
		if platformNamespace != "" {
			l["pressfleet.io/platform-namespace"] = platformNamespace
		}
		return l
	}
	namespaces := []struct {
		name     string
		labels   map[string]string
		admitted bool
	}{
		{"this install's site", site(releaseNamespace), true},
		{"a site from before the label", site(""), true},
		{"another install's site", site("other"), false},
		{"a namespace that isn't a site", map[string]string{"kubernetes.io/metadata.name": "default"}, false},
		{"the platform namespace", map[string]string{"kubernetes.io/metadata.name": releaseNamespace}, false},
	}
	for _, values := range valuesFiles(t) {
		t.Run(filepath.Base(values), func(t *testing.T) {
			out, err := render(t, values)
			if err != nil {
				t.Fatalf("helm template: %v", err)
			}
			// Which policies admit each namespace's pods (any pod: a peer
			// with a podSelector of its own selects platform pods).
			admits := map[string][]string{}
			dec := yaml.NewDecoder(strings.NewReader(out))
			for {
				var np networkPolicy
				if err := dec.Decode(&np); errors.Is(err, io.EOF) {
					break
				} else if err != nil {
					t.Fatal(err)
				}
				if np.Kind != "NetworkPolicy" {
					continue
				}
				for _, ns := range namespaces {
					for _, rule := range np.Spec.Ingress {
						if slices.ContainsFunc(rule.From, func(peer policyPeer) bool {
							return peer.PodSelector == nil && selectorMatches(t, peer.NamespaceSelector, ns.labels)
						}) {
							admits[ns.name] = append(admits[ns.name], np.Metadata.Name)
							break
						}
					}
				}
			}
			for _, ns := range namespaces {
				got := admits[ns.name]
				switch {
				case ns.admitted && !slices.Contains(got, "redis"):
					t.Errorf("%s: admitted by %v, want redis among them", ns.name, got)
				case !ns.admitted && len(got) > 0:
					t.Errorf("%s: admitted by %v, want none", ns.name, got)
				}
			}
		})
	}
}
