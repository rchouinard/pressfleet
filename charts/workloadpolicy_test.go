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

// TestWorkloadPolicyConstrainsPods checks that the rendered admission policy
// for the control plane's pods keeps the rules of the Kubernetes security
// review (#359): every kind of container is hardened, ephemeral containers
// are reached, and the chart's component label is refused. The policy is CEL,
// which the tests can't evaluate, so this checks what each rule names; the
// driver's pod tests check that its pods carry what the rules require, and
// the Kubernetes end to end tests run them against a real API server.
func TestWorkloadPolicyConstrainsPods(t *testing.T) {
	type rule struct {
		Resources []string `yaml:"resources"`
	}
	type policy struct {
		Kind     string `yaml:"kind"`
		Metadata struct {
			Name string `yaml:"name"`
		} `yaml:"metadata"`
		Spec struct {
			MatchConstraints struct {
				ResourceRules []rule `yaml:"resourceRules"`
			} `yaml:"matchConstraints"`
			Validations []struct {
				Expression string `yaml:"expression"`
			} `yaml:"validations"`
		} `yaml:"spec"`
	}
	for _, values := range valuesFiles(t) {
		t.Run(filepath.Base(values), func(t *testing.T) {
			out, err := render(t, values)
			if err != nil {
				t.Fatalf("helm template: %v", err)
			}
			var found bool
			dec := yaml.NewDecoder(strings.NewReader(out))
			for {
				var p policy
				if err := dec.Decode(&p); errors.Is(err, io.EOF) {
					break
				} else if err != nil {
					t.Fatalf("decoding helm template output: %v", err)
				}
				if p.Kind != "ValidatingAdmissionPolicy" || !strings.HasPrefix(p.Metadata.Name, "workloads.") {
					continue
				}
				found = true
				var all []string
				for _, v := range p.Spec.Validations {
					all = append(all, strings.Join(strings.Fields(v.Expression), " "))
				}
				rules := strings.Join(all, "\n")
				if !slices.ContainsFunc(p.Spec.MatchConstraints.ResourceRules, func(r rule) bool {
					return slices.Contains(r.Resources, "pods/ephemeralcontainers")
				}) {
					t.Error("the policy doesn't match ephemeral container updates")
				}
				for _, list := range []string{"containers", "initContainers", "ephemeralContainers"} {
					for _, want := range []string{
						"object.spec." + list + ".all(c,",
						"c.securityContext.privileged",
						"c.securityContext.allowPrivilegeEscalation == false",
						"c.securityContext.capabilities.drop.exists(d, d == 'ALL')",
						"size(c.securityContext.capabilities.add) == 0",
						"c.securityContext.runAsUser != 0",
					} {
						if !strings.Contains(rules, want) {
							t.Errorf("no rule for %s has %q", list, want)
						}
					}
				}
				for _, want := range []string{
					"object.spec.securityContext.runAsNonRoot",
					"object.spec.securityContext.runAsUser != 0",
					"!('app.kubernetes.io/component' in object.metadata.labels)",
				} {
					if !strings.Contains(rules, want) {
						t.Errorf("no rule has %q", want)
					}
				}
			}
			if !found {
				t.Error("the chart renders no workloads policy")
			}
		})
	}
}
