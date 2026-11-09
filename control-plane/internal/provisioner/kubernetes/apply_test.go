package kubernetes

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"
)

const siteManifests = `
apiVersion: v1
kind: Service
metadata:
  name: nginx
  namespace: pf-site1
spec:
  ports: [{port: 80}]
---
# A comment-only document is skipped.
---
apiVersion: v1
kind: Namespace
metadata:
  name: pf-site1
---
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: default-deny
  namespace: pf-site1
`

func TestDecodeManifests(t *testing.T) {
	objs, err := DecodeManifests([]byte(siteManifests))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, o := range objs {
		got = append(got, o.GetKind()+" "+objectName(o))
	}
	want := []string{"Service pf-site1/nginx", "Namespace pf-site1", "NetworkPolicy pf-site1/default-deny"}
	if !slices.Equal(got, want) {
		t.Errorf("decoded %q, want %q", got, want)
	}
	for name, doc := range map[string]string{
		"malformed":                 "kind: [",
		"unknown kind":              "apiVersion: v1\nkind: Pod\nmetadata: {name: x, namespace: ns}\n",
		"unknown version":           "apiVersion: apps/v1beta1\nkind: Deployment\nmetadata: {name: x, namespace: ns}\n",
		"no name":                   "apiVersion: v1\nkind: Secret\nmetadata: {namespace: ns}\n",
		"no namespace":              "apiVersion: v1\nkind: Secret\nmetadata: {name: x}\n",
		"cluster-wide in namespace": "apiVersion: v1\nkind: Namespace\nmetadata: {name: x, namespace: ns}\n",
	} {
		if _, err := DecodeManifests([]byte(doc)); !errors.Is(err, ErrInvalidManifest) {
			t.Errorf("%s: err = %v, want ErrInvalidManifest", name, err)
		}
	}
}

// applied lists the server-side applies the fake saw, as
// "resource namespace/name", with their field managers.
func applied(t *testing.T, dyn *dynamicfake.FakeDynamicClient) []string {
	t.Helper()
	var out []string
	for _, a := range dyn.Actions() {
		pa, ok := a.(k8stesting.PatchActionImpl)
		if !ok {
			continue
		}
		if pa.GetPatchType() != types.ApplyPatchType {
			t.Errorf("%s %s: patch type %s, want server-side apply", pa.GetResource().Resource, pa.GetName(), pa.GetPatchType())
		}
		out = append(out, pa.GetResource().Resource+" "+pa.GetNamespace()+"/"+pa.GetName())
	}
	return out
}

func TestApply(t *testing.T) {
	c, dyn := fakeClient(t)
	objs, err := DecodeManifests([]byte(siteManifests))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Apply(t.Context(), objs); err != nil {
		t.Fatal(err)
	}
	// The namespace first, then the others in order.
	want := []string{"namespaces /pf-site1", "services pf-site1/nginx", "networkpolicies pf-site1/default-deny"}
	if got := applied(t, dyn); !slices.Equal(got, want) {
		t.Errorf("applied %q, want %q", got, want)
	}
	for _, a := range dyn.Actions() {
		if pa, ok := a.(k8stesting.PatchActionImpl); ok {
			if opts := pa.PatchOptions; opts.FieldManager != FieldManager || opts.Force == nil || !*opts.Force {
				t.Errorf("%s: patch options %+v, want field manager %q, forced", pa.GetName(), opts, FieldManager)
			}
		}
	}
	svc, err := dyn.Resource(kinds[schema.GroupVersionKind{Version: "v1", Kind: "Service"}].resource).
		Namespace("pf-site1").Get(t.Context(), "nginx", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("the service wasn't created: %v", err)
	}
	if ports, _, _ := unstructured.NestedSlice(svc.Object, "spec", "ports"); len(ports) != 1 {
		t.Errorf("service ports = %v", ports)
	}

	// Applying again is fine.
	if err := c.Apply(t.Context(), objs); err != nil {
		t.Errorf("second apply: %v", err)
	}
}

func TestApplyDir(t *testing.T) {
	c, dyn := fakeClient(t)
	dir := t.TempDir()
	if err := c.ApplyDir(t.Context(), dir); !errors.Is(err, ErrInvalidManifest) {
		t.Errorf("empty directory: err = %v, want ErrInvalidManifest", err)
	}
	for name, content := range map[string]string{
		"b-service.yaml":   "apiVersion: v1\nkind: Service\nmetadata: {name: nginx, namespace: pf-site1}\n",
		"a-namespace.yaml": "apiVersion: v1\nkind: Namespace\nmetadata: {name: pf-site1}\n",
		"notes.txt":        "not a manifest",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.ApplyDir(t.Context(), dir); err != nil {
		t.Fatal(err)
	}
	want := []string{"namespaces /pf-site1", "services pf-site1/nginx"}
	if got := applied(t, dyn); !slices.Equal(got, want) {
		t.Errorf("applied %q, want %q", got, want)
	}

	if err := os.WriteFile(filepath.Join(dir, "c-bad.yaml"), []byte("apiVersion: v1\nkind: Pod\nmetadata: {name: x, namespace: ns}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := c.ApplyDir(t.Context(), dir); !errors.Is(err, ErrInvalidManifest) {
		t.Errorf("a bad manifest: err = %v, want ErrInvalidManifest", err)
	}
}

func TestApplyError(t *testing.T) {
	c, dyn := fakeClient(t)
	boom := errors.New("admission webhook denied the request")
	dyn.PrependReactor("patch", "services", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, boom
	})
	objs, err := DecodeManifests([]byte(siteManifests))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Apply(t.Context(), objs); !errors.Is(err, boom) {
		t.Errorf("err = %v, want the API server's", err)
	}
}
