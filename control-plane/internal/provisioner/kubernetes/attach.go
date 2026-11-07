package kubernetes

import (
	"context"
	"errors"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/remotecommand"
	"k8s.io/streaming/pkg/httpstream"
)

// podAttacher attaches to a running container's standard streams until
// the container exits, the streams fail, or ctx ends. A nil Stdin attaches
// to the output only.
type podAttacher func(ctx context.Context, namespace, pod, container string, streams remotecommand.StreamOptions) error

// attacher returns the attacher tasks use: the API server's, unless a test
// replaced it.
func (p *Provisioner) attacher() podAttacher {
	if p.attach != nil {
		return p.attach
	}
	return p.attachPod
}

// attachPod attaches through the pods/attach subresource, over WebSocket,
// or SPDY where something between the driver and the API server can't
// upgrade to WebSocket, as kubectl does. Attaching keeps standard output
// and standard error apart, which the pod logs API merges, and streams
// them without the size limits of the node's container logs.
func (c *Client) attachPod(ctx context.Context, namespace, pod, container string, streams remotecommand.StreamOptions) error {
	if c.rest == nil {
		return errors.New("the client has no REST connection to attach with")
	}
	req := c.typed.CoreV1().RESTClient().Post().
		Namespace(namespace).Resource("pods").Name(pod).SubResource("attach").
		VersionedParams(&corev1.PodAttachOptions{
			Container: container,
			Stdin:     streams.Stdin != nil,
			Stdout:    streams.Stdout != nil,
			Stderr:    streams.Stderr != nil,
		}, scheme.ParameterCodec)
	ws, err := remotecommand.NewWebSocketExecutor(c.rest, "GET", req.URL().String())
	if err != nil {
		return fmt.Errorf("attaching to pod %s/%s: %w", namespace, pod, err)
	}
	spdy, err := remotecommand.NewSPDYExecutor(c.rest, "POST", req.URL())
	if err != nil {
		return fmt.Errorf("attaching to pod %s/%s: %w", namespace, pod, err)
	}
	exec, err := remotecommand.NewFallbackExecutor(ws, spdy, func(err error) bool {
		return httpstream.IsUpgradeFailure(err) || httpstream.IsHTTPSProxyError(err)
	})
	if err != nil {
		return fmt.Errorf("attaching to pod %s/%s: %w", namespace, pod, err)
	}
	if err := exec.StreamWithContext(ctx, streams); err != nil {
		return fmt.Errorf("attaching to pod %s/%s: %w", namespace, pod, err)
	}
	return nil
}
