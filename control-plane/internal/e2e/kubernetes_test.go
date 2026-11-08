package e2e_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/pkg/sftp"

	"github.com/rchouinard/pressfleet/control-plane/internal/api/client"
	"github.com/rchouinard/pressfleet/control-plane/internal/catalog"
	"github.com/rchouinard/pressfleet/control-plane/internal/e2e"
)

// The Kubernetes acceptance tests (#228): the Kubernetes driver milestone's
// done-when criteria, on a kind cluster (make test-k8s-e2e). Each installs
// the platform with the chart, as an operator does, and goes through the
// API, Traefik's NodePorts, and the SFTP gateway; checks inside the cluster
// go through kubectl.

// k8sPHP returns the PHP version new sites get, whose images
// scripts/kind/cluster.sh site-images pushed.
func k8sPHP(t *testing.T) string {
	t.Helper()
	cat, err := catalog.Load()
	if err != nil {
		t.Fatal(err)
	}
	php, ok := cat.DefaultPHPVersion(time.Now())
	if !ok {
		t.Fatal("no default PHP version")
	}
	return php
}

// k8sSite creates a site and waits until it runs.
func k8sSite(t *testing.T, p *e2e.Platform, groupID, name string) (client.Site, *e2e.KubeSite) {
	t.Helper()
	created := createSite(t, p, groupID, name, k8sPHP(t))
	waitJob(t, p, created.Job.ID)
	return getSite(t, p, created.Site.ID), p.Kube.Site(created.Site.ID)
}

// TestKubernetesAcceptance provisions sites on a platform installed with
// the chart and checks the milestone's criteria on them: HTTPS on the
// preview domain, isolation (§6), maintenance mode, backup and restore with
// a rollback, SFTP and a database session, staging, and stop, start, and
// delete with nothing left behind. Along the way it checks what earlier
// issues could only check with Docker standing in for a cluster.
func TestKubernetesAcceptance(t *testing.T) {
	p := e2e.StartKubernetes(t, e2e.KubeOptions{})
	k := p.Kube
	ctx := t.Context()

	t.Run("doctor", func(t *testing.T) { checkK8sDoctor(t, p) })

	groupID := createGroup(t, p, "E2E Kubernetes Agency")
	site, one := k8sSite(t, p, groupID, "E2E Kubernetes One")
	hc := p.IngressClient(t)
	preview := "https://" + site.PreviewDomain + "/"

	t.Run("preview over HTTPS", func(t *testing.T) {
		_, body := waitIngress(t, hc, preview, nil, http.StatusOK)
		if !strings.Contains(body, "<title>E2E Kubernetes One") {
			t.Errorf("home page:\n%.300s", body)
		}
		resp, _ := ingressGet(t, hc, "http://"+site.PreviewDomain+"/a?b=c", nil)
		if resp.StatusCode/100 != 3 || resp.Header.Get("Location") != "https://"+site.PreviewDomain+"/a?b=c" {
			t.Errorf("HTTP: status %d, Location %q", resp.StatusCode, resp.Header.Get("Location"))
		}
	})

	t.Run("site namespace", func(t *testing.T) {
		ns := k.SiteNamespace(site.ID)
		if got := k.MustKubectl(t, "get", "namespace", ns, "-o", `jsonpath={.metadata.labels.pod-security\.kubernetes\.io/enforce}`); got != "restricted" {
			t.Errorf("namespace %s enforces Pod Security %q, want restricted", ns, got)
		}
		if got := k.MustKubectl(t, "get", "namespace", ns, "-o", `jsonpath={.metadata.labels.pressfleet\.io/platform-namespace}`); got != k.Namespace {
			t.Errorf("namespace %s belongs to install %q, want %q", ns, got, k.Namespace)
		}
		// The loopback probe trusts X-Forwarded-Proto from the pod
		// network: WordPress sees HTTPS (#218).
		if got := siteWP(t, one, "eval", "echo home_url();"); got != "https://"+site.PreviewDomain {
			t.Errorf("home_url() = %q", got)
		}
		for _, tool := range []string{"timeout", "find", "sh", "cgi-fcgi", "php"} {
			if _, err := one.ExecService(ctx, "php", "sh", "-c", "command -v "+tool); err != nil {
				t.Errorf("the php image has no %s: %v", tool, err)
			}
		}
		if _, err := k.Kubectl(ctx, nil, "exec", "-n", k.Namespace, "deploy/traefik", "--", "sh", "-c", "command -v wget"); err != nil {
			t.Errorf("the Traefik image has no wget: %v", err)
		}
	})

	t.Run("Traefik's services are attributed to the site", func(t *testing.T) {
		// The CRD provider names a route's service
		// <namespace>-<IngressRoute>-<hash>@kubernetescrd (#316); the
		// driver maps those back to the site, or the traffic charts
		// stay empty. A rate needs two scrapes, so keep the traffic up.
		deadline := time.Now().Add(4 * time.Minute)
		for ; ; time.Sleep(5 * time.Second) {
			ingressGet(t, hc, preview, nil)
			start := time.Now().Add(-15 * time.Minute)
			r, err := p.Client.QuerySiteMetricsWithResponse(ctx, site.ID, &client.QuerySiteMetricsParams{Metric: &[]string{"request_rate", "status_codes"}, Start: &start})
			if err != nil {
				t.Fatal(err)
			}
			if r.JSON200 == nil {
				t.Fatalf("site metrics: status %d: %s", r.StatusCode(), r.Body)
			}
			for _, m := range r.JSON200.Metrics {
				for _, s := range m.Series {
					if len(s.Points) > 0 {
						return
					}
				}
			}
			if time.Now().After(deadline) {
				out, _ := k.Kubectl(ctx, nil, "exec", "-n", k.Namespace, "deploy/traefik", "--", "wget", "-qO-", "http://127.0.0.1:8082/metrics")
				t.Fatalf("the site's traffic metrics have no data; Traefik's metrics:\n%s", out)
			}
		}
	})

	two, twoSite := k8sSite(t, p, groupID, "E2E Kubernetes Two")
	t.Run("isolation", func(t *testing.T) { checkK8sIsolation(t, p, site, one, two, twoSite) })
	t.Run("control plane's rights", func(t *testing.T) { checkK8sControlPlaneRights(t, p, site) })
	t.Run("another install's namespaces", func(t *testing.T) { checkK8sOtherInstall(t, p, site) })

	t.Run("maintenance", func(t *testing.T) {
		set := func(enabled bool) {
			resp, err := p.Client.SetSiteMaintenanceWithResponse(ctx, site.ID, client.SetSiteMaintenanceJSONRequestBody{Enabled: enabled})
			if err != nil || resp.JSON202 == nil {
				t.Fatalf("maintenance %v: %v %s", enabled, err, resp.Body)
			}
			waitJob(t, p, resp.JSON202.Job.ID)
		}
		set(true)
		resp, _ := waitIngress(t, hc, preview, nil, http.StatusServiceUnavailable)
		if resp.Header.Get("Retry-After") == "" {
			t.Errorf("maintenance page headers: %v", resp.Header)
		}
		// WP-CLI still works.
		if got := siteWP(t, one, "option", "get", "blogname"); got != "E2E Kubernetes One" {
			t.Errorf("blogname = %q", got)
		}
		set(false)
		waitIngress(t, hc, preview, nil, http.StatusOK)
	})

	t.Run("backup and restore", func(t *testing.T) {
		rc := restoreChecks{p: p, site: site, proj: one}
		snapshot := rc.setUp(t)
		t.Run("same site", func(t *testing.T) { rc.sameSite(t, snapshot) })
		t.Run("rollback", rc.rollback)
		// The rollback leaves the site in maintenance mode.
		resp, err := p.Client.SetSiteMaintenanceWithResponse(ctx, site.ID, client.SetSiteMaintenanceJSONRequestBody{Enabled: false})
		if err != nil || resp.JSON202 == nil {
			t.Fatalf("maintenance off: %v %s", err, resp.Body)
		}
		waitJob(t, p, resp.JSON202.Job.ID)
	})

	t.Run("data volume clones", func(t *testing.T) {
		// The backups mounted the data volume in the site's namespace
		// through a clone of its PersistentVolume (#219), and the driver
		// labeled the data volume's own.
		pv := k.MustKubectl(t, "get", "pv", "-l", "pressfleet.io/data-volume="+k.Namespace, "-o", "name")
		if pv == "" {
			t.Error("no PersistentVolume is labeled as the data volume")
		}
		clone := k.MustKubectl(t, "get", "pv", k.SiteNamespace(site.ID)+"-data", "-o",
			"jsonpath={.spec.persistentVolumeReclaimPolicy} {.spec.claimRef.namespace}/{.spec.claimRef.name}")
		if clone != "Retain "+k.SiteNamespace(site.ID)+"/pressfleet-data" {
			t.Errorf("clone: %q", clone)
		}
		// The admission policies refuse a PersistentVolume of the control
		// plane's that isn't a clone of the data volume.
		hostPath := `{"apiVersion":"v1","kind":"PersistentVolume","metadata":{"name":"` + k.SiteNamespace(two.ID) + `-data",` +
			`"labels":{"pressfleet.io/data-clone":"` + k.Namespace + `"}},"spec":{"capacity":{"storage":"1Gi"},` +
			`"accessModes":["ReadWriteMany"],"persistentVolumeReclaimPolicy":"Retain","hostPath":{"path":"/"},` +
			`"claimRef":{"namespace":"` + k.SiteNamespace(two.ID) + `","name":"pressfleet-data"}}}`
		out, err := k.Kubectl(ctx, strings.NewReader(hostPath), "create", "--dry-run=server", "-f", "-",
			"--as=system:serviceaccount:"+k.Namespace+":control-plane")
		if err == nil || !strings.Contains(err.Error(), "denied") {
			t.Errorf("a hostPath PersistentVolume as the control plane: %v %s", err, out)
		}
	})

	t.Run("SFTP", func(t *testing.T) { checkK8sSFTP(t, p, site, one) })
	t.Run("database session", func(t *testing.T) { checkK8sDatabaseSession(t, p, site, one) })
	t.Run("staging", func(t *testing.T) { checkK8sStaging(t, p, site, one) })

	t.Run("stop, start, and delete", func(t *testing.T) {
		stop, err := p.Client.StopSiteWithResponse(ctx, two.ID)
		if err != nil || stop.JSON202 == nil {
			t.Fatalf("stop: %v %s", err, stop.Body)
		}
		waitJob(t, p, stop.JSON202.Job.ID)
		twoPreview := "https://" + two.PreviewDomain + "/"
		waitIngress(t, hc, twoPreview, nil, http.StatusServiceUnavailable, http.StatusNotFound, http.StatusBadGateway)
		if pods := k.MustKubectl(t, "get", "pods", "-n", k.SiteNamespace(two.ID), "-l", "pressfleet.io/component=site", "-o", "name"); pods != "" {
			t.Errorf("a stopped site has pods: %s", pods)
		}
		start, err := p.Client.StartSiteWithResponse(ctx, two.ID)
		if err != nil || start.JSON202 == nil {
			t.Fatalf("start: %v %s", err, start.Body)
		}
		waitJob(t, p, start.JSON202.Job.ID)
		waitIngress(t, hc, twoPreview, nil, http.StatusOK)

		for _, s := range []client.Site{site, two} {
			deleteK8sSite(t, p, s.ID)
		}
		waitIngress(t, hc, preview, nil, http.StatusNotFound)
	})
}

// deleteK8sSite deletes a site through the API and checks that nothing of
// it is left in the cluster: its namespace, the data volume's clone, its
// database and user.
func deleteK8sSite(t *testing.T, p *e2e.Platform, siteID string) {
	t.Helper()
	k := p.Kube
	resp, err := p.Client.DeleteSiteWithResponse(t.Context(), siteID, nil)
	if err != nil || resp.JSON202 == nil {
		t.Fatalf("delete site %s: %v %s", siteID, err, resp.Body)
	}
	waitJob(t, p, resp.JSON202.Job.ID)
	ns := k.SiteNamespace(siteID)
	if out, err := k.Kubectl(t.Context(), nil, "get", "namespace", ns, "--ignore-not-found", "-o", "name"); err != nil || strings.TrimSpace(string(out)) != "" {
		t.Errorf("namespace %s is left: %v %s", ns, err, out)
	}
	if out, err := k.Kubectl(t.Context(), nil, "get", "pv", ns+"-data", "--ignore-not-found", "-o", "name"); err != nil || strings.TrimSpace(string(out)) != "" {
		t.Errorf("the data volume clone %s-data is left: %v %s", ns, err, out)
	}
	db := "pf_" + siteID
	out, err := p.Kube.Kubectl(t.Context(), nil, "exec", "-n", k.Namespace, "mariadb-0", "--", "sh", "-c",
		`mariadb -uroot -p"$(cat /run/secrets/mariadb_root_password)" -N -e "SELECT COUNT(*) FROM information_schema.schemata WHERE schema_name='`+db+
			`'; SELECT COUNT(*) FROM mysql.user WHERE user LIKE '%`+siteID+`%'"`)
	if err != nil {
		t.Errorf("checking MariaDB for site %s: %v", siteID, err)
	} else if got := strings.Fields(string(out)); !slices.Equal(got, []string{"0", "0"}) {
		t.Errorf("site %s left a database or user in MariaDB: %v", siteID, got)
	}
	got, err := p.Client.GetSiteWithResponse(t.Context(), siteID)
	if err != nil || got.StatusCode() != http.StatusNotFound {
		t.Errorf("get deleted site %s: %v %d", siteID, err, got.StatusCode())
	}
}

// checkK8sDoctor checks the control plane's doctor on the cluster (#225):
// the API's report, and `pressfleet doctor` in the control plane's
// container, which also runs the checks that start pods: kind with Calico
// enforces NetworkPolicy, nodes use cgroup v2, and the chart's RBAC is what
// the driver needs. The test domains resolve nowhere, so the DNS checks
// fail, and Traefik's NodePort Service has no external address.
func checkK8sDoctor(t *testing.T, p *e2e.Platform) {
	expected := map[string]bool{"k8s-dns-dashboard": true, "k8s-dns-preview": true, "k8s-traefik": true}
	check := func(source string, results []doctorResult, want ...string) {
		t.Helper()
		var lines []string
		byID := map[string]doctorResult{}
		for _, r := range results {
			byID[r.ID] = r
			lines = append(lines, fmt.Sprintf("%s (%s): %s: %s", r.ID, r.Severity, r.Status, r.Message))
			if (r.Status == "fail" || r.Status == "warn") && !expected[r.ID] && strings.HasPrefix(r.ID, "k8s-") {
				t.Errorf("%s: %s %s: %s", source, r.ID, r.Status, r.Message)
			}
		}
		t.Logf("%s:\n%s", source, strings.Join(lines, "\n"))
		for _, id := range want {
			if r := byID[id]; r.Status != "pass" {
				t.Errorf("%s: %s is %q, want pass: %s", source, id, r.Status, r.Message)
			}
		}
	}

	resp, err := p.Client.GetPlatformDoctorWithResponse(t.Context())
	if err != nil || resp.JSON200 == nil {
		t.Fatalf("doctor: %v %s", err, resp.Body)
	}
	var api []doctorResult
	for _, r := range resp.JSON200.Results {
		api = append(api, doctorResult{ID: r.ID, Severity: string(r.Severity), Status: string(r.Status), Message: r.Message})
	}
	check("the API's doctor", api, "k8s-version", "k8s-nodes", "k8s-rbac", "k8s-metrics", "k8s-storage-sites", "k8s-storage-data")

	// It exits non-zero for the expected failures.
	out, _ := p.ExecControlPlane(t.Context(), nil, "pressfleet", "doctor", "--json")
	var cli struct {
		Results []doctorResult `json:"results"`
	}
	if err := json.Unmarshal(out, &cli); err != nil {
		t.Fatalf("pressfleet doctor --json: %v: %s", err, out)
	}
	check("pressfleet doctor", cli.Results, "k8s-rbac", "k8s-cgroup", "k8s-networkpolicy")
}

// doctorResult is one check of a doctor report.
type doctorResult struct {
	ID       string `json:"id"`
	Severity string `json:"severity"`
	Status   string `json:"status"`
	Message  string `json:"message"`
}

// k8sProbe runs probeScript in a site's php container.
func k8sProbe(t *testing.T, s *e2e.KubeSite, targets []string) map[string]probeResult {
	t.Helper()
	out, err := s.ExecService(t.Context(), "php", append([]string{"php", "-r", probeScript, "--"}, targets...)...)
	if err != nil {
		t.Fatalf("probing from %s: %v", s, err)
	}
	results := map[string]probeResult{}
	for line := range strings.Lines(strings.TrimSpace(string(out))) {
		f := strings.SplitN(strings.TrimSpace(line), " ", 4)
		if len(f) < 3 {
			t.Fatalf("probe output line %q", line)
		}
		r := probeResult{target: f[0], open: f[1] == "open"}
		if len(f) == 4 {
			r.detail = f[3]
		}
		results[f[0]] = r
	}
	return results
}

// checkK8sIsolation checks §6 on the cluster: a site reaches its own
// database and object cache, but not another site, the control plane, the
// API server, the metadata address, or the platform's other services.
func checkK8sIsolation(t *testing.T, p *e2e.Platform, site client.Site, one *e2e.KubeSite, two client.Site, twoSite *e2e.KubeSite) {
	k := p.Kube
	podIP := func(ns string) string {
		return k.MustKubectl(t, "get", "pods", "-n", ns, "-l", "pressfleet.io/component=site", "-o", "jsonpath={.items[0].status.podIP}")
	}
	svcIP := func(ns, name string) string {
		return k.MustKubectl(t, "get", "service", name, "-n", ns, "-o", "jsonpath={.spec.clusterIP}")
	}
	apiServer := svcIP("default", "kubernetes")
	twoNS := k.SiteNamespace(two.ID)
	pn := k.Namespace
	traefik := k.MustKubectl(t, "get", "pods", "-n", pn, "-l", "app.kubernetes.io/component=traefik", "-o", "jsonpath={.items[0].status.podIP}")
	node := k.MustKubectl(t, "get", "nodes", "-o", `jsonpath={.items[0].status.addresses[?(@.type=="InternalIP")].address}`)
	open := []string{
		"mariadb." + pn + ".svc:3306",
		"redis." + pn + ".svc:6379",
	}
	closed := []string{
		"nginx." + twoNS + ".svc:8080",
		podIP(twoNS) + ":8080",
		podIP(twoNS) + ":9000",
		"control-plane." + pn + ".svc:8080",
		"control-plane." + pn + ".svc:2222",
		"victoria-metrics." + pn + ".svc:8428",
		"maintenance." + pn + ".svc:8080",
		"kubernetes.default.svc:443",
		apiServer + ":443",
		"169.254.169.254:80",
		"postgres.pfk8s-postgres.svc:5432",
		// Traefik's metrics and ping stay on its loopback (F5), and the
		// node's kubelet is out of reach (#229).
		net.JoinHostPort(traefik, "8082"),
		net.JoinHostPort(node, "10250"),
	}
	got := k8sProbe(t, one, append(slices.Clone(open), closed...))
	for _, target := range open {
		if !got[target].open {
			t.Errorf("site one can't reach %s: %s", target, got[target].detail)
		}
	}
	for _, target := range closed {
		if got[target].open {
			t.Errorf("site one reaches %s", target)
		}
	}
	// And the other way round, its own pod's address excepted.
	back := k8sProbe(t, twoSite, []string{podIP(k.SiteNamespace(site.ID)) + ":8080", "nginx." + k.SiteNamespace(site.ID) + ".svc:8080"})
	for target, r := range back {
		if r.open {
			t.Errorf("site two reaches %s", target)
		}
	}
}

// checkK8sControlPlaneRights checks the control plane's rights as its
// ServiceAccount (security review of the Kubernetes target, #229): RBAC
// gives it no Secrets, exec, or kubelet outside its namespaces and never
// lets it change a pod in place, and the chart's admission policies refuse
// what RBAC can't: a RoleBinding to another subject, a namespace at an old
// Pod Security version, a Service past Traefik or with external IPs, and a
// pod with a ServiceAccount token or another ServiceAccount. Each refused
// object has an admitted twin, so a refusal is the policy's.
func checkK8sControlPlaneRights(t *testing.T, p *e2e.Platform, site client.Site) {
	k := p.Kube
	ctx := t.Context()
	as := "--as=system:serviceaccount:" + k.Namespace + ":control-plane"
	siteNS := k.SiteNamespace(site.ID)
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"get", "secrets", "-n", "kube-system"}, "no"},
		{[]string{"list", "secrets", "--all-namespaces"}, "no"},
		{[]string{"create", "pods", "--subresource=exec", "-n", "kube-system"}, "no"},
		{[]string{"get", "nodes", "--subresource=proxy"}, "no"},
		{[]string{"patch", "pods", "-n", k.Namespace}, "no"},
		{[]string{"update", "pods", "-n", siteNS}, "no"},
		{[]string{"create", "serviceaccounts", "--subresource=token", "-n", k.Namespace}, "no"},
		{[]string{"create", "clusterrolebindings"}, "no"},
		{[]string{"create", "pods", "--subresource=exec", "-n", siteNS}, "yes"},
		{[]string{"get", "secrets", "-n", siteNS}, "yes"},
	} {
		// A subresource goes in --subresource: can-i reads "pods/exec" as the
		// pod named exec. can-i exits 1 for no; its answer is on standard
		// output either way.
		out, _ := k.Kubectl(ctx, nil, append([]string{"auth", "can-i", as}, c.args...)...)
		if got := strings.TrimSpace(string(out)); got != c.want {
			t.Errorf("can the control plane %s? %q, want %q", strings.Join(c.args, " "), got, c.want)
		}
	}

	dryRun := func(obj map[string]any) error {
		body, err := json.Marshal(obj)
		if err != nil {
			t.Fatal(err)
		}
		_, err = k.Kubectl(ctx, bytes.NewReader(body), "create", "--dry-run=server", "-f", "-", as)
		return err
	}
	meta := func(name, ns string) map[string]any {
		m := map[string]any{"name": name}
		if ns != "" {
			m["namespace"] = ns
		}
		return m
	}
	roleBinding := func(subjectNS string) map[string]any {
		return map[string]any{
			"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "RoleBinding", "metadata": meta("pf-review", siteNS),
			"roleRef":  map[string]any{"apiGroup": "rbac.authorization.k8s.io", "kind": "ClusterRole", "name": "pressfleet:" + k.Namespace + ":site"},
			"subjects": []any{map[string]any{"kind": "ServiceAccount", "name": "control-plane", "namespace": subjectNS}},
		}
	}
	namespace := func(version, owner string) map[string]any {
		m := meta(k.SitePrefix+"review", "")
		m["labels"] = map[string]any{
			"app.kubernetes.io/managed-by":               "pressfleet",
			"pressfleet.io/site-id":                      "review",
			"pressfleet.io/platform-namespace":           owner,
			"pod-security.kubernetes.io/enforce":         "restricted",
			"pod-security.kubernetes.io/enforce-version": version,
		}
		return map[string]any{"apiVersion": "v1", "kind": "Namespace", "metadata": m}
	}
	service := func(spec map[string]any) map[string]any {
		spec["selector"] = map[string]any{"pressfleet.io/component": "site"}
		spec["ports"] = []any{map[string]any{"port": 80, "targetPort": 8080}}
		return map[string]any{"apiVersion": "v1", "kind": "Service", "metadata": meta("pf-review", siteNS), "spec": spec}
	}
	pod := func(serviceAccount string, token bool) map[string]any {
		return map[string]any{
			"apiVersion": "v1", "kind": "Pod", "metadata": meta("pf-review", k.Namespace),
			"spec": map[string]any{
				"serviceAccountName":           serviceAccount,
				"automountServiceAccountToken": token,
				"securityContext": map[string]any{
					"runAsNonRoot": true, "runAsUser": 65534, "seccompProfile": map[string]any{"type": "RuntimeDefault"},
				},
				"containers": []any{map[string]any{
					"name": "c", "image": k.Image, "command": []any{"true"},
					"securityContext": map[string]any{"allowPrivilegeEscalation": false, "capabilities": map[string]any{"drop": []any{"ALL"}}},
				}},
			},
		}
	}
	for _, c := range []struct {
		name              string
		admitted, refused map[string]any
	}{
		{"a RoleBinding of the site ClusterRole to another subject", roleBinding(k.Namespace), roleBinding(siteNS)},
		{"a namespace at an old Pod Security version", namespace("latest", k.Namespace), namespace("v1.25", k.Namespace)},
		{"a namespace labeled for another install", namespace("latest", k.Namespace), namespace("latest", "other-install")},
		{"a NodePort Service", service(map[string]any{"type": "ClusterIP"}), service(map[string]any{"type": "NodePort"})},
		{"a Service with external IPs", service(map[string]any{}), service(map[string]any{"externalIPs": []any{"203.0.113.10"}})},
		{"a pod with a ServiceAccount token", pod("default", false), pod("default", true)},
		{"a pod as Traefik's ServiceAccount", pod("default", false), pod("traefik", false)},
	} {
		if err := dryRun(c.admitted); err != nil {
			t.Errorf("%s: its admitted twin was refused: %v", c.name, err)
		}
		if err := dryRun(c.refused); err == nil || !strings.Contains(err.Error(), "denied request") {
			t.Errorf("%s: want the admission policy's refusal, got %v", c.name, err)
		}
	}
}

// checkK8sOtherInstall checks that the control plane can't manage the site
// namespaces of another install that shares its site namespace prefix
// (security review K6, #326). As the operator, it creates two namespaces
// that look like sites: one labeled for another install, and one without
// pressfleet.io/platform-namespace, as sites created before the label are.
// As the control plane, with server-side dry runs, it may label the second
// as its own (how the template rollout migrates its sites) and delete it,
// but may not change or delete the first, or bind its site ClusterRole in
// either before it is labeled as its own.
func checkK8sOtherInstall(t *testing.T, p *e2e.Platform, site client.Site) {
	k := p.Kube
	ctx := t.Context()
	as := "--as=system:serviceaccount:" + k.Namespace + ":control-plane"
	foreign, legacy := k.SitePrefix+"k6-foreign", k.SitePrefix+"k6-legacy"
	for name, owner := range map[string]string{foreign: "other-install", legacy: ""} {
		labels := map[string]any{
			"app.kubernetes.io/managed-by":               "pressfleet",
			"pressfleet.io/site-id":                      name,
			"pod-security.kubernetes.io/enforce":         "restricted",
			"pod-security.kubernetes.io/enforce-version": "latest",
		}
		if owner != "" {
			labels["pressfleet.io/platform-namespace"] = owner
		}
		body, err := json.Marshal(map[string]any{
			"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]any{"name": name, "labels": labels},
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := k.Kubectl(ctx, bytes.NewReader(body), "create", "-f", "-"); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_, _ = k.Kubectl(context.WithoutCancel(ctx), nil, "delete", "namespace", name, "--wait=false", "--ignore-not-found")
		})
	}

	roleBinding := func(ns string) error {
		body, err := json.Marshal(map[string]any{
			"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "RoleBinding",
			"metadata": map[string]any{"name": "pf-review", "namespace": ns},
			"roleRef":  map[string]any{"apiGroup": "rbac.authorization.k8s.io", "kind": "ClusterRole", "name": "pressfleet:" + k.Namespace + ":site"},
			"subjects": []any{map[string]any{"kind": "ServiceAccount", "name": "control-plane", "namespace": k.Namespace}},
		})
		if err != nil {
			t.Fatal(err)
		}
		_, err = k.Kubectl(ctx, bytes.NewReader(body), "create", "--dry-run=server", "-f", "-", as)
		return err
	}
	label := func(ns, l string) error {
		_, err := k.Kubectl(ctx, nil, "label", "--dry-run=server", "--overwrite", "namespace", ns, l, as)
		return err
	}
	own := "pressfleet.io/platform-namespace=" + k.Namespace
	del := func(ns string) error {
		_, err := k.Kubectl(ctx, nil, "delete", "--dry-run=server", "namespace", ns, as)
		return err
	}
	siteNS := k.SiteNamespace(site.ID)
	for _, c := range []struct {
		name     string
		admitted bool
		err      error
	}{
		{"change its own site namespace", true, label(siteNS, "pressfleet.io/review=true")},
		{"label a site namespace from before the label as its own", true, label(legacy, own)},
		{"label another install's namespace as its own", false, label(foreign, own)},
		{"change a site namespace from before the label but leave it unlabeled", false, label(legacy, "pressfleet.io/review=true")},
		{"change another install's namespace", false, label(foreign, "pressfleet.io/review=true")},
		{"delete a site namespace from before the label", true, del(legacy)},
		{"delete another install's namespace", false, del(foreign)},
		{"bind its ClusterRole in its own site namespace", true, roleBinding(siteNS)},
		{"bind its ClusterRole in a namespace not yet labeled as its own", false, roleBinding(legacy)},
		{"bind its ClusterRole in another install's namespace", false, roleBinding(foreign)},
	} {
		switch {
		case c.admitted && c.err != nil:
			t.Errorf("%s: refused: %v", c.name, c.err)
		case !c.admitted && (c.err == nil || !strings.Contains(c.err.Error(), "denied request")):
			t.Errorf("%s: want the admission policy's refusal, got %v", c.name, c.err)
		}
	}
}

// checkK8sSFTP checks SFTP through Traefik's sftp entrypoint and the
// gateway: files written belong to the site's user, the session pod is
// gone when the client closes, and the kubelet's podPidsLimit bounds its
// processes (#220).
func checkK8sSFTP(t *testing.T, p *e2e.Platform, site client.Site, one *e2e.KubeSite) {
	k := p.Kube
	signer, _ := registerKey(t, p.Client)
	conn, err := dialSFTP(p, site.Slug, signer)
	if err != nil {
		t.Fatalf("connecting to the gateway: %v", err)
	}
	c, err := sftp.NewClient(conn)
	if err != nil {
		t.Fatalf("starting SFTP: %v", err)
	}
	f, err := c.Create("/from-sftp.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte("via sftp\n")); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if got := readSFTP(t, c, "/from-sftp.txt"); got != "via sftp\n" {
		t.Errorf("read back %q", got)
	}
	ns := k.SiteNamespace(site.ID)
	const sessionPods = "pressfleet.io/component=session"
	session := k.MustKubectl(t, "get", "pods", "-n", ns, "-l", sessionPods, "-o", "name")
	if session == "" {
		t.Errorf("no session pod while the SFTP client is connected; pods:\n%s", k.MustKubectl(t, "get", "pods", "-n", ns, "--show-labels"))
	} else {
		// Pod Security restricted admitted it; the kubelet's podPidsLimit
		// bounds its processes (#220), a pod spec can't.
		if got := k.MustKubectl(t, "get", "-n", ns, session, "-o", "jsonpath={.spec.containers[0].resources.limits.memory}"); got == "" {
			t.Errorf("the session pod has no memory limit")
		}
	}
	_ = c.Close()
	_ = conn.Close()
	if got := siteSh(t, one, "cat "+wpContent+"/from-sftp.txt; stat -c %u "+wpContent+"/from-sftp.txt; id -u"); true {
		lines := strings.Fields(got)
		if len(lines) != 4 || lines[0]+" "+lines[1] != "via sftp" || lines[2] != lines[3] {
			t.Errorf("the file written over SFTP, its owner, and the site's UID: %q", got)
		}
	}
	// The session pod goes away with the client.
	deadline := time.Now().Add(time.Minute)
	for {
		left := k.MustKubectl(t, "get", "pods", "-n", ns, "-l", sessionPods, "-o", "name")
		if left == "" {
			break
		}
		if time.Now().After(deadline) {
			t.Errorf("session pods left after the client closed:\n%s", left)
			break
		}
		time.Sleep(2 * time.Second)
	}
}

// checkK8sDatabaseSession opens a read-write database session and writes
// through an `ssh -L` tunnel to the MariaDB Service, then reads it back in
// the site.
func checkK8sDatabaseSession(t *testing.T, p *e2e.Platform, site client.Site, one *e2e.KubeSite) {
	ctx := t.Context()
	signer, _ := registerKey(t, p.Client)
	rw := client.DatabaseSessionAccessReadWrite
	resp, err := p.Client.CreateSiteDatabaseSessionWithResponse(ctx, site.ID, client.DatabaseSessionCreate{Access: &rw})
	if err != nil || resp.JSON201 == nil {
		t.Fatalf("opening a database session: %v, %d %s", err, resp.StatusCode(), resp.Body)
	}
	session := resp.JSON201
	addr := tunnelTarget(t, session.Tunnel.SSHCommand, site.Slug)
	conn, err := dialSFTP(p, site.Slug, signer)
	if err != nil {
		t.Fatalf("connecting to the gateway: %v", err)
	}
	defer func() { _ = conn.Close() }()
	db := tunnelDB(t, conn, addr, session.Credentials)
	const marker = "pf-e2e-k8s-db-session"
	if _, err := db.ExecContext(ctx, "INSERT INTO wp_options (option_name, option_value, autoload) VALUES ('pf_e2e_k8s_marker', ?, 'no')", marker); err != nil {
		t.Fatalf("writing through the tunnel to %s: %v", addr, err)
	}
	if got := siteWP(t, one, "option", "get", "pf_e2e_k8s_marker"); got != marker {
		t.Errorf("the site reads %q", got)
	}
	if end, err := p.Client.EndSiteDatabaseSessionWithResponse(ctx, site.ID, session.Session.ID); err != nil || end.StatusCode() != http.StatusNoContent {
		t.Errorf("ending the session: %v %s", err, end.Body)
	}
}

// checkK8sStaging clones the site into a staging site, refreshes it, and
// pushes it back: the staging site runs as WP_ENVIRONMENT_TYPE staging with
// its mail off (#101, #216).
func checkK8sStaging(t *testing.T, p *e2e.Platform, site client.Site, one *e2e.KubeSite) {
	ctx := t.Context()
	resp, err := p.Client.CreateStagingSiteWithResponse(ctx, site.ID, client.CreateStagingSiteJSONRequestBody{})
	if err != nil || resp.JSON202 == nil {
		t.Fatalf("create staging site: %v %s", err, resp.Body)
	}
	stagID := resp.JSON202.Site.ID
	// Production can't be deleted while it has a staging site.
	defer deleteK8sSite(t, p, stagID)
	waitJob(t, p, resp.JSON202.Job.ID)
	stag := p.Kube.Site(stagID)
	if got := siteWP(t, stag, "eval", "echo wp_get_environment_type();"); got != "staging" {
		t.Errorf("staging site: wp_get_environment_type() = %q", got)
	}
	if got := siteWP(t, one, "eval", "echo wp_get_environment_type();"); got != "production" {
		t.Errorf("production: wp_get_environment_type() = %q", got)
	}
	out, err := stag.ExecService(ctx, "php", "sh", "-c", `wp eval "$1" 2>&1`, "sh", mailProbe)
	if err != nil || !strings.Contains(string(out), `"sent":true`) || !strings.Contains(string(out), `"reached_phpmailer":false`) {
		t.Errorf("wp_mail() on the staging site: %v\n%s", err, out)
	}

	// A change on production reaches staging with a refresh, and a change
	// on staging reaches production with a push.
	siteWP(t, one, "option", "update", "pf_k8s_staging", "from-production")
	refresh, err := p.Client.RefreshStagingSiteWithResponse(ctx, stagID, client.RefreshStagingSiteJSONRequestBody{Files: client.StagingCopyFilesAll, Database: true})
	if err != nil || refresh.JSON202 == nil {
		t.Fatalf("refresh: %v %s", err, refresh.Body)
	}
	waitJob(t, p, refresh.JSON202.Job.ID)
	if got := siteWP(t, stag, "option", "get", "pf_k8s_staging"); got != "from-production" {
		t.Errorf("staging after the refresh: %q", got)
	}
	siteWP(t, stag, "option", "update", "pf_k8s_staging", "from-staging")
	push, err := p.Client.PushStagingSiteWithResponse(ctx, stagID, client.PushStagingSiteJSONRequestBody{Files: client.StagingCopyFilesAll, Database: true})
	if err != nil || push.JSON202 == nil {
		t.Fatalf("push: %v %s", err, push.Body)
	}
	waitJob(t, p, push.JSON202.Job.ID)
	if got := siteWP(t, one, "option", "get", "pf_k8s_staging"); got != "from-staging" {
		t.Errorf("production after the push: %q", got)
	}
	if got := siteWP(t, one, "eval", "echo wp_get_environment_type();"); got != "production" {
		t.Errorf("production after the push: wp_get_environment_type() = %q", got)
	}
}
