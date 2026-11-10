package e2e

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Pebble on Kubernetes: the same certificate authority as StartWithPebble's,
// in the platform namespace, where Traefik reaches it as pebble:14000 (the
// name its API certificate is for). The DNS server answers every A query with
// Traefik Service's cluster address, so Pebble validates HTTP-01 challenges
// against Traefik's Service as Let's Encrypt would against a load balancer.
// The control plane keeps the cluster's DNS: go-live tests skip the DNS
// check.

// pebbleFiles are Pebble's API certificate and its CA, generated before the
// install so that init can put the CA in the chart's acme.caCertificates
// Secret.
type pebbleFiles struct {
	caPEM, certPEM, keyPEM []byte
	caFile                 string
}

func newPebbleFiles(t *testing.T) *pebbleFiles {
	t.Helper()
	caPEM, certPEM, keyPEM := pebbleTLS(t)
	f := &pebbleFiles{caPEM: caPEM, certPEM: certPEM, keyPEM: keyPEM, caFile: filepath.Join(t.TempDir(), "pebble-ca.pem")}
	if err := os.WriteFile(f.caFile, caPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	return f
}

// pebbleManifests returns Pebble, its DNS server, their Services, and a
// NetworkPolicy that lets Traefik, the test, and Pebble's validation reach
// them, for the platform namespace. Their pods meet Pod Security
// restricted.
func pebbleManifests(f *pebbleFiles, traefikIP string) (string, error) {
	config, err := json.Marshal(map[string]any{"pebble": map[string]any{
		"listenAddress":                  "0.0.0.0:14000",
		"managementListenAddress":        "0.0.0.0:" + strconv.Itoa(pebbleManagementPort),
		"certificate":                    "/pebble/cert.pem",
		"privateKey":                     "/pebble/key.pem",
		"httpPort":                       80,
		"tlsPort":                        443,
		"ocspResponderURL":               "",
		"externalAccountBindingRequired": false,
		"retryAfter":                     map[string]int{"authz": 1, "order": 1},
		"keyAlgorithm":                   "ecdsa",
		"profiles": map[string]any{
			"default": map[string]any{"description": "PressFleet end-to-end tests", "validityPeriod": 7776000},
		},
	}})
	if err != nil {
		return "", err
	}
	indent := func(b []byte) string {
		return "    " + strings.ReplaceAll(strings.TrimSpace(string(b)), "\n", "\n    ")
	}
	const security = `
      securityContext:
        runAsNonRoot: true
        runAsUser: 65534
        runAsGroup: 65534
        seccompProfile:
          type: RuntimeDefault`
	const container = `
          securityContext:
            allowPrivilegeEscalation: false
            readOnlyRootFilesystem: true
            capabilities:
              drop: [ALL]`
	return `apiVersion: v1
kind: Secret
metadata:
  name: pebble
  labels:
    app.kubernetes.io/part-of: pebble-e2e
stringData:
  config.json: ` + strconv.Quote(string(config)) + `
  cert.pem: |
` + indent(f.certPEM) + `
  key.pem: |
` + indent(f.keyPEM) + `
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: pebble
  labels:
    app.kubernetes.io/part-of: pebble-e2e
spec:
  selector:
    matchLabels:
      app: pebble
  template:
    metadata:
      labels:
        app: pebble
        app.kubernetes.io/part-of: pebble-e2e
    spec:` + security + `
      containers:
        - name: pebble
          image: ` + strconv.Quote(PebbleImage) + `
          args: ["-config", "/pebble/config.json", "-dnsserver", "challtestsrv:8053"]
          env:
            - name: PEBBLE_VA_NOSLEEP
              value: "1"
            - name: PEBBLE_WFE_NONCEREJECT
              value: "0"
          volumeMounts:
            - name: config
              mountPath: /pebble
              readOnly: true` + container + `
      volumes:
        - name: config
          secret:
            secretName: pebble
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: challtestsrv
  labels:
    app.kubernetes.io/part-of: pebble-e2e
spec:
  selector:
    matchLabels:
      app: challtestsrv
  template:
    metadata:
      labels:
        app: challtestsrv
        app.kubernetes.io/part-of: pebble-e2e
    spec:` + security + `
      containers:
        - name: challtestsrv
          image: ` + strconv.Quote(ChallTestSrvImage) + `
          args: ["-defaultIPv4", ` + strconv.Quote(traefikIP) + `, "-defaultIPv6", "", "-dnsserver", ":8053",
                 "-management", ":` + strconv.Itoa(challTestSrvMgmtPort) + `",
                 "-http01", "", "-https01", "", "-tlsalpn01", "", "-doh", ""]` + container + `
---
apiVersion: v1
kind: Service
metadata:
  name: pebble
  labels:
    app.kubernetes.io/part-of: pebble-e2e
spec:
  selector:
    app: pebble
  ports:
    - name: acme
      port: 14000
---
apiVersion: v1
kind: Service
metadata:
  name: pebble-management
  labels:
    app.kubernetes.io/part-of: pebble-e2e
spec:
  type: NodePort
  selector:
    app: pebble
  ports:
    - name: management
      port: ` + strconv.Itoa(pebbleManagementPort) + `
---
apiVersion: v1
kind: Service
metadata:
  name: challtestsrv
  labels:
    app.kubernetes.io/part-of: pebble-e2e
spec:
  selector:
    app: challtestsrv
  ports:
    - name: dns
      port: 8053
      protocol: UDP
    - name: dns-tcp
      port: 8053
      protocol: TCP
---
apiVersion: v1
kind: Service
metadata:
  name: challtestsrv-management
  labels:
    app.kubernetes.io/part-of: pebble-e2e
spec:
  type: NodePort
  selector:
    app: challtestsrv
  ports:
    - name: management
      port: ` + strconv.Itoa(challTestSrvMgmtPort) + `
---
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: pebble-e2e
  labels:
    app.kubernetes.io/part-of: pebble-e2e
spec:
  podSelector:
    matchLabels:
      app.kubernetes.io/part-of: pebble-e2e
  policyTypes: [Ingress]
  ingress:
    - {}
`, nil
}

// startPebble deploys Pebble into the platform namespace, restarts Traefik
// so that it registers its ACME account with it, and fetches Pebble's root.
func (k *KubePlatform) startPebble(ctx context.Context, t *testing.T, f *pebbleFiles) *Pebble {
	t.Helper()
	traefikIP := k.MustKubectl(t, "get", "service", "traefik", "-n", k.Namespace, "-o", "jsonpath={.spec.clusterIP}")
	manifests, err := pebbleManifests(f, traefikIP)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := k.Kubectl(ctx, strings.NewReader(manifests), "apply", "-n", k.Namespace, "-f", "-"); err != nil {
		t.Fatalf("deploying Pebble: %v", err)
	}
	k.WaitRollout(ctx, t, "deployment/pebble")
	k.WaitRollout(ctx, t, "deployment/challtestsrv")
	// Traefik registered its account at startup, when Pebble wasn't there.
	if _, err := k.Kubectl(ctx, nil, "rollout", "restart", "-n", k.Namespace, "deployment/traefik"); err != nil {
		t.Fatal(err)
	}
	k.WaitRollout(ctx, t, "deployment/traefik")

	node := k.MustKubectl(t, "get", "nodes", "-o", `jsonpath={.items[0].status.addresses[?(@.type=="InternalIP")].address}`)
	port := k.MustKubectl(t, "get", "service", "pebble-management", "-n", k.Namespace, "-o", "jsonpath={.spec.ports[0].nodePort}")
	apiRoots := x509.NewCertPool()
	apiRoots.AppendCertsFromPEM(f.caPEM)
	mgmt := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: apiRoots, ServerName: pebbleService, MinVersion: tls.VersionTLS12},
	}}
	var rootPEM bytes.Buffer
	url := "https://" + net.JoinHostPort(node, port) + "/roots/0"
	retry(ctx, t, "fetching Pebble's root certificate", func() error {
		rootPEM.Reset()
		return pebbleCall(ctx, mgmt, http.MethodGet, url, nil, &rootPEM)
	})
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(rootPEM.Bytes()) {
		t.Fatalf("Pebble's root certificate isn't PEM: %q", rootPEM.String())
	}
	dnsPort := k.MustKubectl(t, "get", "service", "challtestsrv-management", "-n", k.Namespace, "-o", "jsonpath={.spec.ports[0].nodePort}")
	t.Logf("Pebble issues certificates; every name resolves to Traefik's Service at %s", traefikIP)
	return &Pebble{Roots: roots, RootsPEM: rootPEM.Bytes(), TraefikIP: traefikIP, dnsAPI: "http://" + net.JoinHostPort(node, dnsPort)}
}
