package e2e_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/e2e"
)

// TestKubernetesNetworkPolicyIgnored checks doctor's NetworkPolicy check
// (#225) on a cluster whose CNI ignores NetworkPolicy: the plain ptp plugin
// (kind's own kindnet enforces NetworkPolicy since kind 0.24), on a
// cluster of its own (make test-k8s-e2e-ptp); on any other
// cluster it is skipped. Isolation (§6) rests on NetworkPolicy, so the
// check is required: `pressfleet doctor` fails it, the chart's Helm test
// fails, and the control plane refuses to start until the operator skips
// the preflight checks (the harness does, to get this far).
func TestKubernetesNetworkPolicyIgnored(t *testing.T) {
	p := e2e.StartKubernetes(t, e2e.KubeOptions{IgnoredNetworkPolicy: true})
	k := p.Kube
	ctx := t.Context()
	const id = "k8s-networkpolicy"
	isFailure := func(r doctorResult) bool {
		return r.Status == "fail" && r.Severity == "required" && strings.Contains(r.Message, "ignores NetworkPolicy")
	}

	t.Run("pressfleet doctor", func(t *testing.T) {
		// It exits non-zero for the failure.
		out, _ := p.ExecControlPlane(ctx, nil, "pressfleet", "doctor", "--json")
		var rep struct {
			Results []doctorResult `json:"results"`
		}
		if err := json.Unmarshal(out, &rep); err != nil {
			t.Fatalf("pressfleet doctor --json: %v: %s", err, out)
		}
		found := false
		for _, r := range rep.Results {
			if r.ID == id {
				found = true
				t.Logf("%s (%s): %s: %s", r.ID, r.Severity, r.Status, r.Message)
				if !isFailure(r) {
					t.Errorf("%s is %s (%s): %s, want a required failure: the CNI ignores NetworkPolicy", id, r.Status, r.Severity, r.Message)
				}
			}
		}
		if !found {
			t.Errorf("no %s check in pressfleet doctor:\n%s", id, out)
		}
	})

	t.Run("Helm test", func(t *testing.T) {
		out, err := k.Helm(ctx, "test", "pressfleet", "--namespace", k.Namespace, "--timeout", "10m")
		if err == nil {
			t.Fatalf("helm test passed on a CNI that ignores NetworkPolicy:\n%s", out)
		}
		logs := k.MustKubectl(t, "logs", "-n", k.Namespace, "doctor-test")
		if !strings.Contains(logs, id) && !strings.Contains(logs, "NetworkPolicy enforcement") {
			t.Errorf("helm test failed (%v), but doctor-test's log doesn't name the NetworkPolicy check:\n%s", err, logs)
		}
	})

	// Without the variable the harness set, the control plane's preflight
	// checks fail, and it exits. The Deployment is Recreate, so the pod
	// that ran is gone and the new one keeps failing.
	t.Run("preflight refuses to start", func(t *testing.T) {
		if _, err := k.Kubectl(ctx, nil, "set", "env", "-n", k.Namespace, "deployment/control-plane", "PRESSFLEET_SKIP_PREFLIGHT-"); err != nil {
			t.Fatal(err)
		}
		const want = "required preflight checks failed"
		deadline := time.Now().Add(5 * time.Minute)
		for {
			pods := k.MustKubectl(t, "get", "pods", "-n", k.Namespace, "-l", "app.kubernetes.io/component=control-plane",
				"-o", "jsonpath={.items[*].metadata.name}")
			for _, pod := range strings.Fields(pods) {
				for _, prev := range []bool{false, true} {
					args := []string{"logs", "-n", k.Namespace, pod, "-c", "control-plane", "--tail=200"}
					if prev {
						args = append(args, "--previous")
					}
					out, err := k.Kubectl(ctx, nil, args...)
					if err != nil {
						continue
					}
					for line := range strings.Lines(string(out)) {
						if strings.Contains(line, want) && strings.Contains(line, id) {
							t.Logf("%s: %s", pod, strings.TrimSpace(line))
							return
						}
					}
				}
			}
			if time.Now().After(deadline) {
				t.Fatalf("no %q naming %s in the control plane's log after 5m; pods:\n%s", want, id,
					k.MustKubectl(t, "get", "pods", "-n", k.Namespace, "-o", "wide"))
			}
			time.Sleep(5 * time.Second)
		}
	})
}
