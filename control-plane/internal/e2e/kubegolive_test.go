package e2e_test

import (
	"encoding/base64"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/api/client"
	"github.com/rchouinard/pressfleet/control-plane/internal/e2e"
)

// TestKubernetesGoLive takes a site live on a platform whose Traefik gets
// its certificates from Pebble, as it would from Let's Encrypt: the
// carry-overs of #217 on a real cluster. Traefik's HTTP provider picks up
// the routes the control plane serves (#322); the primary domain gets an
// HTTP-01 certificate and HSTS, the preview domain and the alias redirect
// with path and query (HSTS on the alias, a production domain, only); the
// preview's basicAuth, with the password's hash, answers 401 with noindex;
// maintenance mode takes the primary domain over and gives it back; and
// none of it restarts the site's pod. An alias whose certificate isn't
// issued (its domain points elsewhere, so HTTP-01 fails) is routed all the
// same, with Traefik's default certificate.
//
// Traefik has the chart's LoadBalancer Service here, as in production
// (cloud-provider-kind gives it an address; the other tests use NodePorts):
// everything goes through it, doctor finds its address, and SFTP works
// through it. The dashboard domain gets its certificate from Pebble too
// (the chart's own route, #223).
func TestKubernetesGoLive(t *testing.T) {
	p := e2e.StartKubernetes(t, e2e.KubeOptions{Pebble: true, LoadBalancer: true})
	k := p.Kube
	ctx := t.Context()
	site, one := k8sSite(t, p, createGroup(t, p, "E2E Kubernetes Go-Live"), "E2E Kubernetes Live")
	hc := p.IngressClient(t)
	preview := site.PreviewDomain
	const primary, alias = "live.k8s.e2e.test", "www.live.k8s.e2e.test"
	// Resolves to Pebble's own loopback, where nothing answers HTTP-01.
	const pending = "pending.live.k8s.e2e.test"

	t.Run("dashboard certificate", func(t *testing.T) {
		waitCertified(t, p.VerifyingIngressClient(t), "https://"+e2e.DashboardDomain+"/readyz", nil, http.StatusOK, time.Now())
	})

	t.Run("LoadBalancer", func(t *testing.T) {
		lb, _, err := net.SplitHostPort(p.HTTPSAddr)
		if err != nil {
			t.Fatal(err)
		}
		if lb == k.NodeIP {
			t.Fatalf("the test reaches Traefik at its node's address %s, not a load balancer's", lb)
		}
		resp, err := p.Client.GetPlatformDoctorWithResponse(ctx)
		if err != nil || resp.JSON200 == nil {
			t.Fatalf("doctor: %v %s", err, resp.Body)
		}
		for _, r := range resp.JSON200.Results {
			if r.ID == "k8s-traefik" && (r.Status != "pass" || !strings.Contains(r.Message, lb)) {
				t.Errorf("doctor's k8s-traefik: %s: %s, want pass with %s", r.Status, r.Message, lb)
			}
		}
		checkK8sSFTP(t, p, site, one)
	})
	podUID := func() string {
		return k.MustKubectl(t, "get", "pods", "-n", k.SiteNamespace(site.ID), "-l", "pressfleet.io/component=site",
			"-o", "jsonpath={.items[*].metadata.uid}")
	}
	before := podUID()

	t.Run("preview protection", func(t *testing.T) {
		on := true
		resp, err := p.Client.UpdateSiteWithResponse(ctx, site.ID, client.UpdateSiteJSONRequestBody{
			PreviewProtection: &client.PreviewProtectionUpdate{Enabled: &on},
		})
		if err != nil || resp.JSON202 == nil || resp.JSON202.PreviewCredentials == nil {
			t.Fatalf("protect: %v %s", err, resp.Body)
		}
		waitJob(t, p, resp.JSON202.Job.ID)
		creds := resp.JSON202.PreviewCredentials
		res, _ := waitIngress(t, hc, "https://"+preview+"/", nil, http.StatusUnauthorized)
		if res.Header.Get("X-Robots-Tag") != "noindex" {
			t.Errorf("401 headers: %v", res.Header)
		}
		auth := http.Header{"Authorization": {"Basic " + base64.StdEncoding.EncodeToString([]byte(creds.Username+":"+creds.Password))}}
		if res, _ := ingressGet(t, hc, "https://"+preview+"/", auth); res.StatusCode != http.StatusOK {
			t.Errorf("with the password: status %d", res.StatusCode)
		}
		off := false
		resp, err = p.Client.UpdateSiteWithResponse(ctx, site.ID, client.UpdateSiteJSONRequestBody{
			PreviewProtection: &client.PreviewProtectionUpdate{Enabled: &off},
		})
		if err != nil || resp.JSON202 == nil {
			t.Fatalf("unprotect: %v %s", err, resp.Body)
		}
		waitJob(t, p, resp.JSON202.Job.ID)
		waitIngress(t, hc, "https://"+preview+"/", nil, http.StatusOK)
	})

	t.Run("go live", func(t *testing.T) {
		skip := true
		p.Pebble.ResolveTo(ctx, t, pending, "127.0.0.1")
		resp, err := p.Client.GoLiveSiteWithResponse(ctx, site.ID, client.GoLiveSiteJSONRequestBody{
			PrimaryDomain: primary, Aliases: &[]string{alias, pending}, SkipDNSCheck: &skip,
		})
		if err != nil || resp.JSON202 == nil {
			t.Fatalf("go-live: %v %s", err, resp.Body)
		}
		waitJob(t, p, resp.JSON202.Job.ID)
		res, _ := waitCertified(t, p.VerifyingIngressClient(t), "https://"+primary+"/", nil, http.StatusOK, time.Now())
		if got := res.Header.Get("Strict-Transport-Security"); got != "max-age=31536000" {
			t.Errorf("Strict-Transport-Security = %q", got)
		}
		for _, tc := range []struct {
			host string
			hsts bool
		}{{preview, false}, {alias, true}, {pending, true}} {
			res, _ := waitIngress(t, hc, "https://"+tc.host+"/shop/item?id=7&x=y", nil, http.StatusMovedPermanently)
			if res.Header.Get("Location") != "https://"+primary+"/shop/item?id=7&x=y" {
				t.Errorf("%s: Location %q", tc.host, res.Header.Get("Location"))
			}
			if got := res.Header.Get("Strict-Transport-Security") != ""; got != tc.hsts {
				t.Errorf("%s: Strict-Transport-Security %q", tc.host, res.Header.Get("Strict-Transport-Security"))
			}
		}
	})

	// The primary domain and the first alias have their certificates by
	// now, ordered at the same time; the pending alias's validation
	// failed. It redirects all the same (above), with Traefik's own
	// certificate, which a browser rejects.
	t.Run("certificate not issued", func(t *testing.T) {
		waitCertified(t, p.VerifyingIngressClient(t), "https://"+alias+"/", nil, http.StatusMovedPermanently, time.Now())
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+pending+"/", nil)
		if err != nil {
			t.Fatal(err)
		}
		if res, err := p.VerifyingIngressClient(t).Do(req); err == nil {
			_ = res.Body.Close()
			t.Errorf("GET https://%s/: a certificate that verifies, for %v", pending, res.TLS.PeerCertificates[0].DNSNames)
		} else if !strings.Contains(err.Error(), "certificate") {
			t.Errorf("GET https://%s/: want a certificate error, got %v", pending, err)
		}
		res, _ := ingressGet(t, hc, "https://"+pending+"/", nil)
		if res.TLS == nil || len(res.TLS.PeerCertificates) == 0 {
			t.Fatalf("GET https://%s/: no TLS", pending)
		}
		if leaf := res.TLS.PeerCertificates[0]; leaf.Subject.CommonName != "TRAEFIK DEFAULT CERT" {
			t.Errorf("GET https://%s/: a certificate for %q (%v) issued by %q, want Traefik's default", pending,
				leaf.Subject.CommonName, leaf.DNSNames, leaf.Issuer.CommonName)
		}
		if res.StatusCode != http.StatusMovedPermanently || res.Header.Get("Location") != "https://"+primary+"/" {
			t.Errorf("GET https://%s/: %d to %q, want a redirect to the primary domain", pending, res.StatusCode, res.Header.Get("Location"))
		}
	})

	t.Run("maintenance takes the domain over", func(t *testing.T) {
		set := func(enabled bool) {
			resp, err := p.Client.SetSiteMaintenanceWithResponse(ctx, site.ID, client.SetSiteMaintenanceJSONRequestBody{Enabled: enabled})
			if err != nil || resp.JSON202 == nil {
				t.Fatalf("maintenance %v: %v %s", enabled, err, resp.Body)
			}
			waitJob(t, p, resp.JSON202.Job.ID)
		}
		set(true)
		// The control plane serves Traefik its twins, at priority 100000.
		routes, err := k.ServedRoutes(ctx)
		if err != nil {
			t.Fatalf("reading the served routes from Traefik's pod: %v", err)
		}
		if !strings.Contains(string(routes), "pf-"+site.ID+"-primary-maintenance") {
			t.Errorf("no maintenance routes among those served:\n%s", routes)
		}
		for _, host := range []string{primary, alias, preview} {
			waitIngress(t, hc, "https://"+host+"/", nil, http.StatusServiceUnavailable)
		}
		set(false)
		waitIngress(t, hc, "https://"+primary+"/", nil, http.StatusOK)
	})

	if after := podUID(); after != before {
		t.Errorf("the site's pod changed from %s to %s: routing changes must not restart it", before, after)
	}
	deleteK8sSite(t, p, site.ID)
}
