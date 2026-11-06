package kubernetes

import (
	"context"
	"errors"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
)

// ErrRolloutFailed is returned when a Deployment's rollout can't finish:
// it went past its progress deadline.
var ErrRolloutFailed = errors.New("kubernetes: rollout failed")

// WaitForRollout waits until the Deployment's current revision runs its
// desired replicas, all available, and no pod of an older revision is
// left: what Recreate leaves once a site's new pod is up, or once a site
// scaled to zero has stopped. It gives up with ErrRolloutFailed when the
// Deployment reports that its progress deadline passed, and otherwise
// waits as long as ctx allows. A Deployment that doesn't exist is an error
// for which apierrors.IsNotFound holds.
func (c *Client) WaitForRollout(ctx context.Context, namespace, name string) error {
	var last string
	err := wait.PollUntilContextCancel(ctx, c.poll, true, func(ctx context.Context) (bool, error) {
		d, err := c.typed.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		done, msg, err := rolloutStatus(d)
		last = msg
		return done, err
	})
	if err != nil && last != "" {
		return fmt.Errorf("waiting for deployment %s/%s (%s): %w", namespace, name, last, err)
	} else if err != nil {
		return fmt.Errorf("waiting for deployment %s/%s: %w", namespace, name, err)
	}
	return nil
}

// rolloutStatus reports whether d's rollout is complete, and if not, why,
// as kubectl rollout status does.
func rolloutStatus(d *appsv1.Deployment) (done bool, msg string, err error) {
	if d.Generation > d.Status.ObservedGeneration {
		return false, "the controller hasn't seen the newest spec", nil
	}
	for _, cond := range d.Status.Conditions {
		if cond.Type == appsv1.DeploymentProgressing && cond.Reason == "ProgressDeadlineExceeded" {
			return false, "", fmt.Errorf("%w: %s", ErrRolloutFailed, cond.Message)
		}
	}
	want := int32(1)
	if d.Spec.Replicas != nil {
		want = *d.Spec.Replicas
	}
	s := d.Status
	switch {
	case s.UpdatedReplicas < want:
		return false, fmt.Sprintf("%d of %d pods updated", s.UpdatedReplicas, want), nil
	case s.Replicas > s.UpdatedReplicas:
		return false, fmt.Sprintf("%d old pods still running", s.Replicas-s.UpdatedReplicas), nil
	case s.AvailableReplicas < want:
		return false, fmt.Sprintf("%d of %d pods available", s.AvailableReplicas, want), nil
	}
	return true, "", nil
}

// WaitForNamespaceDeleted waits until the namespace is gone: the API
// server removes it once every object in it has been deleted, volumes
// included. A namespace that doesn't exist is gone already.
func (c *Client) WaitForNamespaceDeleted(ctx context.Context, name string) error {
	var phase corev1.NamespacePhase
	err := wait.PollUntilContextCancel(ctx, c.poll, true, func(ctx context.Context) (bool, error) {
		ns, err := c.typed.CoreV1().Namespaces().Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		if err != nil {
			return false, err
		}
		phase = ns.Status.Phase
		return false, nil
	})
	if err != nil {
		return fmt.Errorf("waiting for namespace %s to be deleted (phase %q): %w", name, phase, err)
	}
	return nil
}
