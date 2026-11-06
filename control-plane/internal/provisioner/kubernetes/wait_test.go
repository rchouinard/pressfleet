package kubernetes

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	kubefake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func deployment(replicas int32, status appsv1.DeploymentStatus) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "site", Namespace: "pf-site1", Generation: 2},
		Spec:       appsv1.DeploymentSpec{Replicas: &replicas},
		Status:     status,
	}
}

func TestRolloutStatus(t *testing.T) {
	tests := []struct {
		name    string
		d       *appsv1.Deployment
		done    bool
		msg     string
		wantErr error
	}{
		{"running", deployment(1, appsv1.DeploymentStatus{ObservedGeneration: 2, Replicas: 1, UpdatedReplicas: 1, AvailableReplicas: 1}), true, "", nil},
		{"stopped", deployment(0, appsv1.DeploymentStatus{ObservedGeneration: 2}), true, "", nil},
		{"spec not seen", deployment(1, appsv1.DeploymentStatus{ObservedGeneration: 1, Replicas: 1, UpdatedReplicas: 1, AvailableReplicas: 1}), false, "hasn't seen", nil},
		{"new pod pending", deployment(1, appsv1.DeploymentStatus{ObservedGeneration: 2, Replicas: 1}), false, "0 of 1 pods updated", nil},
		{"old pod left", deployment(1, appsv1.DeploymentStatus{ObservedGeneration: 2, Replicas: 2, UpdatedReplicas: 1, AvailableReplicas: 1}), false, "1 old pods", nil},
		{"stopping", deployment(0, appsv1.DeploymentStatus{ObservedGeneration: 2, Replicas: 1}), false, "1 old pods", nil},
		{"not ready", deployment(1, appsv1.DeploymentStatus{ObservedGeneration: 2, Replicas: 1, UpdatedReplicas: 1}), false, "0 of 1 pods available", nil},
		{"deadline", deployment(1, appsv1.DeploymentStatus{ObservedGeneration: 2, Replicas: 1, Conditions: []appsv1.DeploymentCondition{{
			Type: appsv1.DeploymentProgressing, Status: corev1.ConditionFalse, Reason: "ProgressDeadlineExceeded",
			Message: `ReplicaSet "site-1" has timed out progressing.`,
		}}}), false, "", ErrRolloutFailed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			done, msg, err := rolloutStatus(tt.d)
			if done != tt.done || !strings.Contains(msg, tt.msg) || !errors.Is(err, tt.wantErr) {
				t.Errorf("rolloutStatus = %v, %q, %v; want %v, %q, %v", done, msg, err, tt.done, tt.msg, tt.wantErr)
			}
		})
	}
}

func TestWaitForRollout(t *testing.T) {
	// The fake starts with a rollout in progress, and finishes it on the
	// third look.
	d := deployment(1, appsv1.DeploymentStatus{ObservedGeneration: 2, Replicas: 1})
	c, _ := fakeClient(t, d)
	var gets atomic.Int32
	c.typed.(*kubefake.Clientset).PrependReactor("get", "deployments", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if a.(k8stesting.GetAction).GetName() != "site" || gets.Add(1) < 3 {
			return false, nil, nil
		}
		done := d.DeepCopy()
		done.Status = appsv1.DeploymentStatus{ObservedGeneration: 2, Replicas: 1, UpdatedReplicas: 1, AvailableReplicas: 1}
		return true, done, nil
	})
	if err := c.WaitForRollout(t.Context(), "pf-site1", "site"); err != nil {
		t.Fatal(err)
	}
	if n := gets.Load(); n != 3 {
		t.Errorf("looked %d times, want 3", n)
	}

	err := c.WaitForRollout(t.Context(), "pf-site1", "missing")
	if !apierrors.IsNotFound(err) {
		t.Errorf("missing deployment: err = %v, want not found", err)
	}
}

func TestWaitForRolloutTimesOut(t *testing.T) {
	c, _ := fakeClient(t, deployment(1, appsv1.DeploymentStatus{ObservedGeneration: 2, Replicas: 1, UpdatedReplicas: 1}))
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	err := c.WaitForRollout(ctx, "pf-site1", "site")
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "0 of 1 pods available") {
		t.Errorf("err = %v, want the deadline, with where the rollout stopped", err)
	}
}

func TestWaitForNamespaceDeleted(t *testing.T) {
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: "pf-site1"},
		Status:     corev1.NamespaceStatus{Phase: corev1.NamespaceTerminating},
	}
	c, _ := fakeClient(t, ns)
	typed := c.typed.(*kubefake.Clientset)

	if err := c.WaitForNamespaceDeleted(t.Context(), "pf-other"); err != nil {
		t.Errorf("a namespace that doesn't exist: %v", err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	if err := c.WaitForNamespaceDeleted(ctx, "pf-site1"); !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "Terminating") {
		t.Errorf("a terminating namespace: err = %v, want the deadline", err)
	}

	// The API server removes it a little later.
	go func() {
		time.Sleep(20 * time.Millisecond)
		_ = typed.Tracker().Delete(corev1.SchemeGroupVersion.WithResource("namespaces"), "", "pf-site1")
	}()
	if err := c.WaitForNamespaceDeleted(t.Context(), "pf-site1"); err != nil {
		t.Errorf("a namespace that goes: %v", err)
	}
}
