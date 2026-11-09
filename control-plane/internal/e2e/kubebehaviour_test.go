package e2e_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/e2e"
)

// kindS3 is the S3 service of TestKubernetesDriverBehaviour, in a
// namespace of its own: what a repository task with BackupNetwork may
// reach in the cluster (backup.s3.endpoint).
const kindS3 = `apiVersion: v1
kind: Namespace
metadata:
  name: NAMESPACE
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: s3
  namespace: NAMESPACE
spec:
  replicas: 1
  selector:
    matchLabels:
      app: s3
  template:
    metadata:
      labels:
        app: s3
    spec:
      containers:
        - name: s3
          image: IMAGE
          env:
            - name: RUSTFS_ACCESS_KEY
              value: S3ACCESSVALUE
            - name: RUSTFS_SECRET_KEY
              value: S3SECRETVALUE
          ports:
            - containerPort: 9000
          readinessProbe:
            tcpSocket:
              port: 9000
          volumeMounts:
            - name: data
              mountPath: /data
      volumes:
        - name: data
          emptyDir: {}
---
apiVersion: v1
kind: Service
metadata:
  name: s3
  namespace: NAMESPACE
spec:
  selector:
    app: s3
  ports:
    - port: 9000
      targetPort: 9000
`

// TestKubernetesDriverBehaviour runs the kubernetes package's
// TestKindBehaviour in the cluster, as TestKubernetesConformance runs the
// conformance suite: what the driver relies on the cluster for (#317),
// such as processes that end, mounts, limits, network policies, cleanup
// after a crash, metrics, and logs. It deploys an S3 service in a
// namespace of its own for the repository tasks' network. Then it scales
// metrics-server to zero and runs the MetricsUnavailable subtest alone,
// and scales it back up.
//
// PRESSFLEET_K8S_BEHAVIOUR_RUN narrows the tests (-test.run), e.g.
// 'TestKindBehaviour/Stats'; the MetricsUnavailable run happens anyway.
func TestKubernetesDriverBehaviour(t *testing.T) {
	p := e2e.StartKubernetes(t, e2e.KubeOptions{})
	k := p.Kube
	ctx := t.Context()
	s3NS := k.Namespace + "-s3"
	t.Cleanup(func() {
		_, _ = k.Kubectl(context.Background(), nil, "delete", "namespace", s3NS, "--ignore-not-found", "--wait=true", "--timeout=3m")
	})
	manifest := strings.NewReplacer("NAMESPACE", s3NS, "IMAGE", s3Image, "S3ACCESSVALUE", s3AccessKey, "S3SECRETVALUE", s3SecretKey).Replace(kindS3)
	if _, err := k.Kubectl(ctx, strings.NewReader(manifest), "apply", "-f", "-"); err != nil {
		t.Fatal(err)
	}
	if _, err := k.Kubectl(ctx, nil, "rollout", "status", "-n", s3NS, "deployment/s3", "--timeout=5m"); err != nil {
		t.Fatalf("starting the S3 service: %v", err)
	}

	r := startDriverTests(t, k)
	run := os.Getenv("PRESSFLEET_K8S_BEHAVIOUR_RUN")
	if run == "" {
		run = "^TestKindBehaviour$"
	}
	r.run(t, run, "60m", map[string]string{
		"PRESSFLEET_TEST_S3_ENDPOINT":   "http://s3." + s3NS + ".svc:9000",
		"PRESSFLEET_TEST_S3_ACCESS_KEY": s3AccessKey,
		"PRESSFLEET_TEST_S3_SECRET_KEY": s3SecretKey,
	})

	// Without metrics-server, last: the other tests need it, and it takes
	// a while to come back.
	replicas := strings.TrimSpace(k.MustKubectl(t, "get", "deployment", "metrics-server", "-n", "kube-system", "-o", "jsonpath={.spec.replicas}"))
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
		defer cancel()
		for _, args := range [][]string{
			{"scale", "deployment/metrics-server", "-n", "kube-system", "--replicas=" + replicas},
			{"rollout", "status", "deployment/metrics-server", "-n", "kube-system", "--timeout=3m"},
			{"wait", "apiservice/v1beta1.metrics.k8s.io", "--for=condition=Available", "--timeout=3m"},
		} {
			if _, err := k.Kubectl(ctx, nil, args...); err != nil {
				t.Errorf("restoring metrics-server: %v", err)
				return
			}
		}
	})
	k.MustKubectl(t, "scale", "deployment/metrics-server", "-n", "kube-system", "--replicas=0")
	r.run(t, "^TestKindBehaviour$/^MetricsUnavailable$", "20m", map[string]string{"PRESSFLEET_TEST_METRICS_DOWN": "1"})
}
