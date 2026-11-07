package kubernetes

import (
	"context"
	"io"
	"sync"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// observers holds what the log and stats readers ask the cluster, behind
// functions so that tests can stand in for the pod logs API and
// metrics.k8s.io, which the fake clientset can't serve. The zero value
// talks to the real cluster.
type observers struct {
	// podLogs opens a container's log stream; nil means the Pod logs API.
	podLogs func(ctx context.Context, ns, pod string, opts *corev1.PodLogOptions) (io.ReadCloser, error)
	// podMetrics lists the metrics of the site's pods; nil means
	// metrics.k8s.io.
	podMetrics func(ctx context.Context, ns string) ([]podMetrics, error)

	mu  sync.Mutex
	cpu map[string]cpuState // by namespace/pod/container
}

// podMetrics is one item of metrics.k8s.io's PodMetricsList, which the
// driver decodes itself: k8s.io/metrics would be a module for three types.
type podMetrics struct {
	Metadata   metav1.ObjectMeta `json:"metadata"`
	Timestamp  metav1.Time       `json:"timestamp"`
	Window     metav1.Duration   `json:"window"`
	Containers []struct {
		Name  string              `json:"name"`
		Usage corev1.ResourceList `json:"usage"`
	} `json:"containers"`
}
