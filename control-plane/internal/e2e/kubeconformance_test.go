package e2e_test

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/catalog"
	"github.com/rchouinard/pressfleet/control-plane/internal/e2e"
	"github.com/rchouinard/pressfleet/control-plane/internal/platform"
)

// conformanceRunner is the pod TestKubernetesConformance runs the suite in:
// the control plane's pod spec as far as the suite needs it (its
// ServiceAccount, user, groups, and data volume), idle until the test execs
// the suite's binary in it. It meets Pod Security restricted.
const conformanceRunner = `apiVersion: v1
kind: Pod
metadata:
  name: conformance
spec:
  serviceAccountName: control-plane
  restartPolicy: Never
  securityContext:
    runAsNonRoot: true
    runAsUser: 60001
    runAsGroup: 60001
    fsGroup: 60001
    supplementalGroups: [60000]
    seccompProfile:
      type: RuntimeDefault
  containers:
    - name: conformance
      image: IMAGE
      command: ["sleep", "infinity"]
      securityContext:
        allowPrivilegeEscalation: false
        readOnlyRootFilesystem: true
        capabilities:
          drop: [ALL]
      volumeMounts:
        - name: data
          mountPath: /var/lib/pressfleet
        - name: tmp
          mountPath: /tmp
  volumes:
    - name: data
      persistentVolumeClaim:
        claimName: control-plane-data
    - name: tmp
      emptyDir: {}
`

// TestKubernetesConformance runs the driver conformance suite (#214)
// against the Kubernetes driver: it builds the kubernetes package's test
// binary, and runs its TestConformance in a pod of a platform the chart
// installed, with the control plane's ServiceAccount and data volume, so
// that the suite works under the chart's RBAC and admission policies. The
// control plane is scaled to zero meanwhile: its Startup would remove the
// suite's task pods.
//
// PRESSFLEET_K8S_CONFORMANCE_RUN narrows the suite (-test.run), e.g.
// 'TestConformance/Tasks'.
func TestKubernetesConformance(t *testing.T) {
	p := e2e.StartKubernetes(t, e2e.KubeOptions{})
	k := p.Kube
	ctx := t.Context()

	bin := filepath.Join(t.TempDir(), "kubernetes.test")
	build := exec.CommandContext(ctx, "go", "test", "-c", "-o", bin, "github.com/rchouinard/pressfleet/control-plane/internal/provisioner/kubernetes")
	build.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH=amd64")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building the conformance test binary: %v\n%s", err, out)
	}

	if _, err := k.Kubectl(ctx, nil, "scale", "-n", k.Namespace, "deployment/control-plane", "--replicas=0"); err != nil {
		t.Fatal(err)
	}
	if _, err := k.Kubectl(ctx, nil, "wait", "-n", k.Namespace, "--for=delete", "pod", "-l", "app.kubernetes.io/component=control-plane", "--timeout=3m"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// The control plane's Startup cleans up after a suite that was
		// stopped; the teardown removes the rest.
		_, _ = k.Kubectl(context.Background(), nil, "delete", "pod", "conformance", "-n", k.Namespace, "--wait=false")
	})

	// A database and user that every site of the suite shares, as on
	// Compose; the platform's admin user of the object cache.
	const dbPassword = "pf-conformance-db-pw"
	if out, err := k.Kubectl(ctx, nil, "exec", "-n", k.Namespace, "mariadb-0", "--", "sh", "-c",
		`mariadb -uroot -p"$(cat /run/secrets/mariadb_root_password)" -e "CREATE DATABASE wordpress; CREATE USER 'wordpress'@'%' IDENTIFIED BY '`+dbPassword+`'; GRANT ALL ON wordpress.* TO 'wordpress'@'%'"`); err != nil {
		t.Fatalf("creating the suite's database: %v %s", err, out)
	}
	cachePassword := k.InitSecret(t, "pressfleet-platform", "redis-admin-password")

	manifest := strings.Replace(conformanceRunner, "IMAGE", k.Image, 1)
	if _, err := k.Kubectl(ctx, strings.NewReader(manifest), "apply", "-n", k.Namespace, "-f", "-"); err != nil {
		t.Fatal(err)
	}
	if _, err := k.Kubectl(ctx, nil, "wait", "-n", k.Namespace, "--for=condition=Ready", "pod/conformance", "--timeout=3m"); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(bin)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if _, err := k.Kubectl(ctx, f, "exec", "-i", "-n", k.Namespace, "conformance", "--",
		"sh", "-c", "cat > /tmp/kubernetes.test && chmod +x /tmp/kubernetes.test && mkdir -p /var/lib/pressfleet/tmp/conformance"); err != nil {
		t.Fatalf("copying the test binary: %v", err)
	}

	cat, err := catalog.Load()
	if err != nil {
		t.Fatal(err)
	}
	php := k8sPHP(t)
	alt := ""
	for _, v := range []string{"8.5", "8.4"} {
		if v != php {
			alt = v
			break
		}
	}
	wp := cat.LatestWordPress().Version
	image := func(name string) string { return k.Registry + "/pressfleet/" + name }
	run := os.Getenv("PRESSFLEET_K8S_CONFORMANCE_RUN")
	if run == "" {
		run = "TestConformance"
	}
	vars := map[string]string{
		"PRESSFLEET_K8S_CONFORMANCE":          "1",
		"TMPDIR":                              "/var/lib/pressfleet/tmp/conformance",
		"HOME":                                "/tmp",
		"PRESSFLEET_TEST_NAMESPACE":           k.Namespace,
		"PRESSFLEET_TEST_SITE_PREFIX":         k.SitePrefix,
		"PRESSFLEET_TEST_POD_CIDRS":           "10.244.0.0/16",
		"PRESSFLEET_TEST_INSECURE_REGISTRIES": k.Registry,
		"PRESSFLEET_TEST_DATA_DIR":            e2e.KubeDataDir,
		"PRESSFLEET_TEST_MARIADB_ADDRESS":     "mariadb." + k.Namespace + ".svc:3306",
		"PRESSFLEET_TEST_REDIS_ADDRESS":       "redis." + k.Namespace + ".svc:6379",
		"PRESSFLEET_TEST_DB_NAME":             "wordpress",
		"PRESSFLEET_TEST_DB_USER":             "wordpress",
		"PRESSFLEET_TEST_DB_PASSWORD":         dbPassword,
		"PRESSFLEET_TEST_CACHE_USER":          platform.RedisAdminUser,
		"PRESSFLEET_TEST_CACHE_PASSWORD":      cachePassword,
		"PRESSFLEET_TEST_PHP_VERSION":         php,
		"PRESSFLEET_TEST_ALT_PHP_VERSION":     alt,
		"PRESSFLEET_TEST_WP_VERSION":          wp,
		"PRESSFLEET_TEST_SITE_PHP_IMAGE":      image("site-php:php" + php + "-wp" + wp),
		"PRESSFLEET_TEST_SITE_ALT_PHP_IMAGE":  image("site-php:php" + alt + "-wp" + wp),
		"PRESSFLEET_TEST_SITE_NGINX_IMAGE":    image("site-nginx:wp" + wp),
		"PRESSFLEET_TEST_TASK_IMAGE":          k.Image,
	}
	args := []string{"exec", "-n", k.Namespace, "conformance", "--", "env"}
	for name, v := range vars {
		args = append(args, name+"="+v)
	}
	args = append(args, "/tmp/kubernetes.test", "-test.v", "-test.timeout", "70m", "-test.run", run)
	start := time.Now()
	var out bytes.Buffer
	cmd := exec.CommandContext(ctx, "kubectl", append(kubectlGlobal(k), args...)...)
	cmd.Stdout = io.MultiWriter(&out, t.Output())
	cmd.Stderr = t.Output()
	err = cmd.Run()
	t.Logf("the suite ran for %s", time.Since(start).Round(time.Second))
	if err != nil {
		t.Errorf("the conformance suite failed: %v", err)
	}
	if !strings.Contains(out.String(), "\nPASS") && err == nil {
		t.Error("the conformance suite didn't report PASS")
	}
}

// kubectlGlobal returns kubectl's flags that select the test cluster.
func kubectlGlobal(k *e2e.KubePlatform) []string {
	args := []string{"--context", k.Context}
	if k.Kubeconfig != "" {
		args = append(args, "--kubeconfig", k.Kubeconfig)
	}
	return args
}
