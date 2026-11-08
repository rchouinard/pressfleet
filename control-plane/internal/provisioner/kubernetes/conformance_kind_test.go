package kubernetes

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/rchouinard/pressfleet/control-plane/internal/catalog"
	"github.com/rchouinard/pressfleet/control-plane/internal/config"
	"github.com/rchouinard/pressfleet/control-plane/internal/objectcache"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner/conformance"
	"github.com/rchouinard/pressfleet/control-plane/internal/redact"
	"github.com/rchouinard/pressfleet/control-plane/internal/sitedb"
)

// TestConformance runs the driver conformance suite (package conformance,
// docs/testing.md) against the Kubernetes driver on a real cluster. It runs
// inside the cluster, in a pod of a platform the chart installed, as the
// control plane's ServiceAccount and with the data volume at the data
// directory, because tasks mount what the suite puts there: the
// Kubernetes end-to-end tests build this test binary and start it there
// (TestKubernetesConformance in package e2e, make test-k8s-e2e).
//
// It is skipped unless PRESSFLEET_K8S_CONFORMANCE=1; the PRESSFLEET_TEST_*
// variables below describe the platform.
func TestConformance(t *testing.T) {
	if os.Getenv("PRESSFLEET_K8S_CONFORMANCE") != "1" {
		t.Skip("set PRESSFLEET_K8S_CONFORMANCE=1, in a pod of the cluster, to run the conformance suite (see make test-k8s-e2e)")
	}
	env := func(name string) string {
		v := os.Getenv("PRESSFLEET_TEST_" + name)
		if v == "" {
			t.Fatalf("PRESSFLEET_TEST_%s is not set", name)
		}
		return v
	}
	php, altPHP := env("PHP_VERSION"), env("ALT_PHP_VERSION")
	images := map[string]catalog.Images{
		php:    {PHP: env("SITE_PHP_IMAGE"), Nginx: env("SITE_NGINX_IMAGE")},
		altPHP: {PHP: env("SITE_ALT_PHP_IMAGE"), Nginx: env("SITE_NGINX_IMAGE")},
	}
	k := config.DefaultKubernetes()
	k.PlatformNamespace = env("NAMESPACE")
	k.SiteNamespacePrefix = env("SITE_PREFIX")
	k.PodCIDRs = strings.Split(env("POD_CIDRS"), ",")
	k.InsecureRegistries = strings.Split(env("INSECURE_REGISTRIES"), ",")
	k.SiteVolumeSize = "1Gi"
	dataDir := env("DATA_DIR")
	cache := objectcache.Credentials{Username: env("CACHE_USER"), Password: redact.Secret(env("CACHE_PASSWORD")), Database: 1}

	conformance.Run(t, conformance.Env{
		Images:        images,
		PHPVersion:    php,
		AltPHPVersion: altPHP,
		WPCoreVersion: env("WP_VERSION"),
		Database: sitedb.Credentials{
			Database: env("DB_NAME"), Username: env("DB_USER"), Password: redact.Secret(env("DB_PASSWORD")),
		},
		ObjectCache: &cache,
		New: func(t *testing.T, s conformance.Setup) conformance.Instance {
			p, err := New(Config{
				// The data volume's root, which task mounts are relative
				// to; the suite's own directories (Setup.Layout) are below
				// it, as TMPDIR is.
				Layout:         config.Layout{Root: dataDir},
				Kubernetes:     k,
				Secrets:        s.Secrets,
				Images:         s.Images,
				MariaDBAddress: env("MARIADB_ADDRESS"),
				RedisAddress:   env("REDIS_ADDRESS"),
				WaitTimeout:    3 * time.Minute,
				Tasks:          TaskConfig{Image: env("TASK_IMAGE")},
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := p.Connect(t.Context()); err != nil {
				t.Fatal(err)
			}
			return kindInstance(p)
		},
	})
}

// kindInstance is the driver with the ways of looking inside its sites
// that the suite needs, straight through the API server.
func kindInstance(p *Provisioner) conformance.Instance {
	run := func(ctx context.Context, target execTarget, argv ...string) (string, error) {
		var stdout, stderr bytes.Buffer
		if err := p.exec.Exec(ctx, target, argv, nil, &stdout, &stderr); err != nil {
			return stdout.String(), fmt.Errorf("%v: %w: %s", argv, err, stderr.String())
		}
		return stdout.String(), nil
	}
	return conformance.Instance{
		Driver: p,

		Exec: func(ctx context.Context, siteID string, c provisioner.Component, argv ...string) (string, error) {
			target, err := p.sitePod(ctx, siteID)
			if err != nil {
				return "", err
			}
			target.Container = string(c)
			return run(ctx, target, argv...)
		},

		// From the php container to nginx in the same pod, as the
		// driver's own probe does: the site's NetworkPolicy lets only
		// Traefik in from elsewhere.
		Get: func(ctx context.Context, siteID, host, path string) (*conformance.HTTPResponse, error) {
			target, err := p.sitePod(ctx, siteID)
			if err != nil {
				return nil, err
			}
			out, err := run(ctx, target, "php", "-r", kindHTTPGetScript, host, path)
			if err != nil {
				return nil, err
			}
			var r struct {
				Status  int
				Headers []string
				Body    string
			}
			if err := json.Unmarshal([]byte(out), &r); err != nil {
				return nil, fmt.Errorf("response %q: %w", out, err)
			}
			resp := &conformance.HTTPResponse{StatusCode: r.Status, Header: http.Header{}, Body: r.Body}
			for _, h := range r.Headers {
				if k, v, ok := strings.Cut(h, ":"); ok {
					resp.Header.Add(strings.TrimSpace(k), strings.TrimSpace(v))
				}
			}
			return resp, nil
		},

		// The site pod's address: its own PHP reaches it (the same network
		// namespace), and the NetworkPolicies keep everyone else but
		// Traefik out.
		Address: func(ctx context.Context, siteID string) (string, error) {
			target, err := p.sitePod(ctx, siteID)
			if err != nil {
				return "", err
			}
			pod, err := p.typed.CoreV1().Pods(target.Namespace).Get(ctx, target.Pod, metav1.GetOptions{})
			if err != nil {
				return "", err
			}
			return pod.Status.PodIP + ":8080", nil
		},
		EdgeOnlyIngress: true,
		// php and nginx share the site's pod, which a new wp-config.php
		// replaces.
		OnePod: true,

		Leftovers: func(ctx context.Context, owner string, deleted bool) ([]string, error) {
			var left []string
			namespaces := []string{p.platformNamespace}
			// A deleted site's namespace is gone, and with it the
			// control plane's rights to list anything in it.
			if _, err := p.typed.CoreV1().Namespaces().Get(ctx, p.SiteNamespace(owner), metav1.GetOptions{}); err == nil {
				namespaces = append(namespaces, p.SiteNamespace(owner))
			} else if !apierrors.IsNotFound(err) {
				return nil, err
			}
			for _, ns := range namespaces {
				for _, sel := range []string{TaskLabel, ComponentLabel + "=" + SessionComponent} {
					pods, err := p.typed.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: sel})
					if err != nil && !apierrors.IsNotFound(err) {
						return nil, err
					}
					for _, pod := range podsOf(pods) {
						if ns != p.platformNamespace || pod.Labels[repositoryLabel] == owner || strings.Contains(pod.Name, strings.ToLower(owner)) {
							left = append(left, "pod "+ns+"/"+pod.Name)
						}
					}
				}
				secrets, err := p.typed.CoreV1().Secrets(ns).List(ctx, metav1.ListOptions{LabelSelector: TaskLabel})
				if err != nil && !apierrors.IsNotFound(err) {
					return nil, err
				}
				if secrets != nil {
					for _, s := range secrets.Items {
						if ns != p.platformNamespace || s.Labels[repositoryLabel] == owner {
							left = append(left, "secret "+ns+"/"+s.Name)
						}
					}
				}
			}
			if !deleted {
				return left, nil
			}
			ns := p.SiteNamespace(owner)
			if _, err := p.typed.CoreV1().Namespaces().Get(ctx, ns, metav1.GetOptions{}); err == nil {
				left = append(left, "namespace "+ns)
			} else if !apierrors.IsNotFound(err) {
				return nil, err
			}
			if _, err := p.typed.CoreV1().PersistentVolumes().Get(ctx, ns+"-data", metav1.GetOptions{}); err == nil {
				left = append(left, "persistent volume "+ns+"-data")
			} else if !apierrors.IsNotFound(err) {
				return nil, err
			}
			return left, nil
		},

		// Whether the site's maintenance routes are among those served to
		// Traefik.
		InMaintenance: func(_ context.Context, siteID string) (bool, error) {
			return p.inMaintenance(siteID)
		},
	}
}

func podsOf(list *corev1.PodList) []corev1.Pod {
	if list == nil {
		return nil
	}
	return list.Items
}

// kindHTTPGetScript sends a GET with the host and path in argv to nginx on
// the pod's loopback interface and prints the response as JSON. HTTP/1.0
// keeps nginx from chunking the body.
const kindHTTPGetScript = `error_reporting(0);
[, $host, $path] = $argv;
$f = fsockopen('127.0.0.1', 8080, $errno, $errstr, 10);
if (!$f) { fwrite(STDERR, "connecting to nginx: $errstr\n"); exit(1); }
stream_set_timeout($f, 30);
fwrite($f, "GET $path HTTP/1.0\r\nHost: $host\r\nX-Forwarded-Proto: https\r\nConnection: close\r\n\r\n");
$raw = stream_get_contents($f);
[$head, $body] = array_pad(explode("\r\n\r\n", $raw, 2), 2, '');
$lines = explode("\r\n", $head);
if (!preg_match('#^HTTP/[0-9.]+ ([0-9]{3})#', $lines[0], $m)) { fwrite(STDERR, "no HTTP response\n"); exit(1); }
echo json_encode(array('status' => (int) $m[1], 'headers' => array_slice($lines, 1), 'body' => $body));`
