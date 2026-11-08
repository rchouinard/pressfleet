package kubernetes

import (
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	kubefake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
)

// Under the chart's RBAC the control plane may only read a site's
// namespace through the RoleBinding in it, so for a site whose namespace
// doesn't exist the API server answers forbidden, not not found (#228).
// That is still a missing site; a refusal in a namespace that exists isn't.
func TestMissingSiteUnderRBAC(t *testing.T) {
	p := testProvisioner(t)
	cs := p.typed.(*kubefake.Clientset)
	cs.PrependReactor("get", "deployments", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Group: "apps", Resource: "deployments"}, "site", errors.New("no RoleBinding"))
	})
	if _, err := p.ExecWPCLI(t.Context(), testSiteID, []string{"option", "get", "home"}); !errors.Is(err, provisioner.ErrSiteNotFound) {
		t.Errorf("ExecWPCLI without the namespace = %v, want ErrSiteNotFound", err)
	}
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: p.SiteNamespace(testSiteID)}}
	if _, err := p.typed.CoreV1().Namespaces().Create(t.Context(), ns, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := p.ExecWPCLI(t.Context(), testSiteID, []string{"option", "get", "home"}); !apierrors.IsForbidden(err) {
		t.Errorf("ExecWPCLI refused in an existing namespace = %v, want the refusal", err)
	}
}

// A new RoleBinding takes a moment to reach the authorizer, so the first
// calls in a site's new namespace can be refused; the driver waits for
// them rather than failing the site.
func TestCreateSiteWaitsForRoleBinding(t *testing.T) {
	p := cluster(t, nil)
	cs := p.typed.(*kubefake.Clientset)
	refusals := 2
	cs.PrependReactor("list", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
		if refusals == 0 {
			return false, nil, nil
		}
		refusals--
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "secrets"}, "", errors.New("no RoleBinding yet"))
	})
	if _, err := p.CreateSite(t.Context(), testSite()); err != nil {
		t.Fatalf("CreateSite while the RoleBinding settles: %v", err)
	}
	if refusals != 0 {
		t.Errorf("%d refusals left; the driver didn't list the namespace's secrets", refusals)
	}
}
