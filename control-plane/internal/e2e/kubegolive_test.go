package e2e_test

import (
	"encoding/base64"
	"net/http"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/api/client"
	"github.com/rchouinard/pressfleet/control-plane/internal/e2e"
)

// TestKubernetesGoLive takes a site live on a platform whose Traefik gets
// its certificates from Pebble, as it would from Let's Encrypt: the
// carry-overs of #217 on a real cluster. The CRD provider picks up the
// site's IngressRoutes and Middlewares; the primary domain gets an HTTP-01
// certificate and HSTS, the preview domain and the alias redirect with
// path and query (HSTS on the alias, a production domain, only); the
// preview's basicAuth Middleware, from a Secret, answers 401 with noindex;
// maintenance mode takes the primary domain over from the platform
// namespace and gives it back; and none of it restarts the site's pod.
func TestKubernetesGoLive(t *testing.T) {
	p := e2e.StartKubernetes(t, e2e.KubeOptions{Pebble: true})
	k := p.Kube
	ctx := t.Context()
	site, _ := k8sSite(t, p, createGroup(t, p, "E2E Kubernetes Go-Live"), "E2E Kubernetes Live")
	hc := p.IngressClient(t)
	preview := site.PreviewDomain
	const primary, alias = "live.k8s.e2e.test", "www.live.k8s.e2e.test"
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
		resp, err := p.Client.GoLiveSiteWithResponse(ctx, site.ID, client.GoLiveSiteJSONRequestBody{
			PrimaryDomain: primary, Aliases: &[]string{alias}, SkipDNSCheck: &skip,
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
		}{{preview, false}, {alias, true}} {
			res, _ := waitIngress(t, hc, "https://"+tc.host+"/shop/item?id=7&x=y", nil, http.StatusMovedPermanently)
			if res.Header.Get("Location") != "https://"+primary+"/shop/item?id=7&x=y" {
				t.Errorf("%s: Location %q", tc.host, res.Header.Get("Location"))
			}
			if got := res.Header.Get("Strict-Transport-Security") != ""; got != tc.hsts {
				t.Errorf("%s: Strict-Transport-Security %q", tc.host, res.Header.Get("Strict-Transport-Security"))
			}
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
		// Its routes are in the platform namespace, at priority 100000.
		if routes := k.MustKubectl(t, "get", "ingressroutes", "-n", k.Namespace, "-l", "pressfleet.io/maintenance-for="+site.ID, "-o", "name"); routes == "" {
			t.Error("no maintenance routes in the platform namespace")
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
