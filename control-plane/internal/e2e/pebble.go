package e2e

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/ingress"
	"github.com/rchouinard/pressfleet/control-plane/internal/platform"
)

// Images of Pebble, Let's Encrypt's ACME test server, and its companion
// challenge test server, whose DNS server answers every A query with one
// address. Pinned like the platform's images (templates/platform/images.yaml):
// <tag>@<linux/amd64 manifest digest>.
const (
	PebbleImage       = "ghcr.io/letsencrypt/pebble:2.10.1@sha256:5080492187bea12909c053d7a2b90a9e95104ef1c82381828e7823db71ce7e54"
	ChallTestSrvImage = "ghcr.io/letsencrypt/pebble-challtestsrv:2.10.1@sha256:1994a326ec60e0de5e8051aa192269c1aaeff0969a4b703d11fa51370f3dc1a8"
)

// Pebble's services in the platform project, and their ports.
const (
	pebbleService       = "pebble"
	challTestSrvService = "challtestsrv"
	// pebbleDirectory is the ACME directory Traefik uses. The host name is
	// in the certificate of Pebble's API (see pebbleTLS).
	pebbleDirectory      = "https://" + pebbleService + ":14000/dir"
	pebbleManagementPort = 15000
	challTestSrvMgmtPort = 8055
)

// pebbleWait bounds waiting for Pebble's and the DNS server's management
// APIs to answer after the containers started.
const pebbleWait = 30 * time.Second

// Pebble is the certificate authority of a platform started with
// StartWithPebble.
type Pebble struct {
	// Roots holds Pebble's root certificate, which every certificate it
	// issues chains to. Pebble generates it when it starts.
	Roots *x509.CertPool
	// RootsPEM is the same certificate in PEM form, for handing to
	// something that verifies certificates outside the test process, such
	// as the control plane's own checks of its sites (SSL_CERT_FILE).
	RootsPEM []byte
	// TraefikIP is Traefik's address on the platform network. The DNS
	// server resolves every name to it, and it is the platform's public_ip.
	TraefikIP string
}

// StartWithPebble is Start with a certificate authority: Traefik gets its
// certificates from Pebble with the HTTP-01 challenge, as it would from
// Let's Encrypt. The platform project gets two more services on the
// platform network:
//
//   - pebble, the ACME server (acme.ca_server), whose API certificate is
//     signed by a CA the harness generates and hands Traefik as
//     acme.ca_certificates. Its validation authority connects to port 80,
//     not Pebble's test default of 5002, and doesn't sleep before
//     validating.
//   - challtestsrv, the DNS server Pebble resolves names with. It answers
//     every A query with Traefik's address on the platform network (and no
//     AAAA query), so every domain a test uses "points at this host", and
//     Pebble validates challenges against Traefik for real.
//
// The control plane uses challtestsrv as its DNS server too, and its
// public_ip is Traefik's address, so the go-live DNS check (package domains)
// passes for any domain without skip_dns_check. Since both addresses are
// only known once the containers run, the control plane is recreated with
// them before the admin user is created.
//
// VerifyingIngressClient verifies certificates against Pebble's root.
func StartWithPebble(t *testing.T) *Platform {
	t.Helper()
	return start(t, true, 0)
}

// preparePebble writes Pebble's configuration and the certificate of its
// API into the platform directory, and returns the platform's ACME settings
// and the override that adds Pebble and its DNS server.
func preparePebble(t *testing.T, dir string) (platform.ACMEConfig, override) {
	t.Helper()
	pebbleDir := filepath.Join(dir, "pebble")
	if err := os.MkdirAll(pebbleDir, 0o755); err != nil { //nolint:gosec // G301: the Pebble container reads it
		t.Fatal(err)
	}
	caPEM, certPEM, keyPEM := pebbleTLS(t)
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
		t.Fatal(err)
	}
	// Traefik reads the CA file as root without capabilities, so it can't
	// read a file the test user owns unless everyone can. None of these are
	// secrets: they are throwaway test keys.
	for name, data := range map[string][]byte{"ca.pem": caPEM, "cert.pem": certPEM, "key.pem": keyPEM, "config.json": config} {
		if err := os.WriteFile(filepath.Join(pebbleDir, name), data, 0o644); err != nil { //nolint:gosec // G306: see above
			t.Fatal(err)
		}
	}
	acme := platform.ACMEConfig{CAServer: pebbleDirectory, CACertificates: filepath.Join(pebbleDir, "ca.pem")}
	o := override{
		// Traefik registers its ACME account when it starts.
		traefikDependsOn: []string{pebbleService},
		services: `  ` + pebbleService + `:
    image: ` + strconv.Quote(PebbleImage) + `
    command: ["-config", "/pebble/config.json", "-dnsserver", "` + challTestSrvService + `:53"]
    environment:
      # Validate at once instead of after a random delay of up to 15s.
      PEBBLE_VA_NOSLEEP: "1"
      # Don't reject 5% of valid nonces: lego retries them, but slowly.
      PEBBLE_WFE_NONCEREJECT: "0"
    volumes:
      - type: bind
        source: ./pebble
        target: /pebble
        read_only: true
    ports:
      - "127.0.0.1::` + strconv.Itoa(pebbleManagementPort) + `"
    networks:
      - platform
    depends_on:
      - ` + challTestSrvService + `
  ` + challTestSrvService + `:
    image: ` + strconv.Quote(ChallTestSrvImage) + `
    # DNS on port 53, the only port Docker's embedded DNS server forwards
    # to; the challenge servers are off, Traefik answers the challenges.
    command: ["-defaultIPv6", "", "-dnsserver", ":53", "-http01", "", "-https01", "", "-tlsalpn01", "", "-doh", ""]
    ports:
      - "127.0.0.1::` + strconv.Itoa(challTestSrvMgmtPort) + `"
    networks:
      - platform
`,
	}
	return acme, o
}

// connectPebble points the DNS server at Traefik, fetches Pebble's root, and
// recreates the control plane with the DNS server and public_ip; see
// StartWithPebble.
func (p *Platform) connectPebble(ctx context.Context, t *testing.T) {
	t.Helper()
	ip := func(service string) string {
		t.Helper()
		c, err := p.Project.ServiceContainer(ctx, service)
		if err != nil {
			t.Fatal(err)
		}
		addr := c.Networks[p.Project.Name+"_platform"]
		if addr == "" {
			t.Fatalf("%s isn't on the platform network: %v", service, c.Networks)
		}
		return addr
	}
	port := func(service string, port int) string {
		t.Helper()
		addr, err := p.Project.Port(ctx, service, port)
		if err != nil {
			t.Fatal(err)
		}
		return addr
	}
	traefikIP, dnsIP := ip(ingress.TraefikService), ip(challTestSrvService)

	body, err := json.Marshal(map[string]string{"ip": traefikIP})
	if err != nil {
		t.Fatal(err)
	}
	dnsAPI := "http://" + port(challTestSrvService, challTestSrvMgmtPort)
	retry(ctx, t, "setting the DNS server's default address", func() error {
		return pebbleCall(ctx, &http.Client{Timeout: 10 * time.Second}, http.MethodPost, dnsAPI+"/set-default-ipv4", body, nil)
	})

	// The management API has the same certificate as the ACME API.
	caPEM, err := os.ReadFile(filepath.Join(p.Project.Dir, "pebble", "ca.pem"))
	if err != nil {
		t.Fatal(err)
	}
	apiRoots := x509.NewCertPool()
	apiRoots.AppendCertsFromPEM(caPEM)
	mgmt := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: apiRoots, MinVersion: tls.VersionTLS12},
	}}
	var rootPEM bytes.Buffer
	mgmtURL := "https://" + port(pebbleService, pebbleManagementPort)
	retry(ctx, t, "fetching Pebble's root certificate", func() error {
		rootPEM.Reset()
		return pebbleCall(ctx, mgmt, http.MethodGet, mgmtURL+"/roots/0", nil, &rootPEM)
	})
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(rootPEM.Bytes()) {
		t.Fatalf("Pebble's root certificate isn't PEM: %q", rootPEM.String())
	}
	p.Pebble = &Pebble{Roots: roots, RootsPEM: rootPEM.Bytes(), TraefikIP: traefikIP}
	t.Logf("Pebble issues certificates; every name resolves to Traefik at %s", traefikIP)

	p.override.dns = []string{dnsIP}
	p.override.env = map[string]string{"PRESSFLEET_PUBLIC_IP": traefikIP}
	p.override.write(t, p.Project.Dir)
	p.up(ctx, t)
	p.Connect(ctx, t)
}

// pebbleCall sends a request to Pebble's or the DNS server's management
// API and copies the response body to out, if given.
func pebbleCall(ctx context.Context, c *http.Client, method, url string, body []byte, out io.Writer) error {
	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("%s %s: %s: %s", method, url, resp.Status, msg)
	}
	if out != nil {
		_, err = io.Copy(out, resp.Body)
	}
	return err
}

// retry calls f until it succeeds, failing the test after pebbleWait.
func retry(ctx context.Context, t *testing.T, what string, f func() error) {
	t.Helper()
	deadline := time.Now().Add(pebbleWait)
	for {
		err := f()
		if err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: %v", what, err)
		}
		select {
		case <-ctx.Done():
			t.Fatalf("%s: %v", what, ctx.Err())
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// pebbleTLS generates a CA and, signed by it, the certificate of Pebble's
// API for the host names Traefik and the test reach it by, as PEM.
func pebbleTLS(t *testing.T) (caPEM, certPEM, keyPEM []byte) {
	t.Helper()
	now := time.Now()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ca := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "PressFleet e2e Pebble API CA"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	ca, err = x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	leaf := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: pebbleService},
		DNSNames:     []string{pebbleService, "localhost"},
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1)},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leaf, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}),
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
}
