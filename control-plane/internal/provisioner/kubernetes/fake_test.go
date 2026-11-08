package kubernetes

import (
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	kubefake "k8s.io/client-go/kubernetes/fake"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	k8stesting "k8s.io/client-go/testing"
)

// fakeClient is a client on fake clientsets whose actions show what the
// driver did. The typed and the dynamic client share one object store for
// the built-in kinds, as they share the API server, and that store applies
// server-side apply patches with field ownership, as the API server does:
// the driver applies through the dynamic client and reads through the typed
// one.
func fakeClient(t *testing.T, typed ...runtime.Object) (*Client, *dynamicfake.FakeDynamicClient) {
	t.Helper()
	cs := kubefake.NewClientset(typed...)
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	listKinds := map[schema.GroupVersionResource]string{}
	for gvk, k := range kinds {
		listKinds[k.resource] = gvk.Kind + "List"
	}
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, listKinds)
	dyn.PrependReactor("*", "*", k8stesting.ObjectReaction(cs.Tracker()))
	c := newClient(cs, dyn, nil)
	c.poll = 5e6 // 5ms
	return c, dyn
}

// rollOut makes the fake cluster's Deployments roll out at once: every
// Deployment the typed client reads reports that its desired pods are up
// to date and available, as the controllers would make it once its pods
// are ready.
func rollOut(c *Client) {
	cs := c.typed.(*kubefake.Clientset)
	cs.PrependReactor("get", "deployments", func(a k8stesting.Action) (bool, runtime.Object, error) {
		ga := a.(k8stesting.GetAction)
		obj, err := cs.Tracker().Get(ga.GetResource(), ga.GetNamespace(), ga.GetName(), metav1.GetOptions{})
		if err != nil {
			return true, nil, err
		}
		d := obj.(*appsv1.Deployment).DeepCopy()
		n := replicas(d)
		d.Status = appsv1.DeploymentStatus{
			ObservedGeneration: d.Generation,
			Replicas:           n, UpdatedReplicas: n, ReadyReplicas: n, AvailableReplicas: n,
		}
		return true, d, nil
	})
}
