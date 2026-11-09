package e2e_test

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/alerts"
	"github.com/rchouinard/pressfleet/control-plane/internal/api/client"
	"github.com/rchouinard/pressfleet/control-plane/internal/e2e"
)

// k8sNoSource are the catalog entries a cluster has no source for.
var k8sNoSource = map[string]string{
	"host_disk_usage": "a cluster's disks are for its own monitoring: the node collector writes no filesystem series (metrics.NodeCollector)",
	"host_disk_io":    "a cluster's disks are for its own monitoring: the node collector writes no disk I/O series (metrics.NodeCollector)",
}

// TestKubernetesMonitoring is TestM4Acceptance on a cluster, with the
// Kubernetes side of the metrics (#226, carried over in #317): on a
// platform whose Traefik gets its certificates from Pebble, it
//
//   - checks that every catalog entry but the host disk ones has data from
//     the real collectors (PHP-FPM, page cache, containers, storage,
//     backups, traffic, and the rest);
//   - checks the host series in VictoriaMetrics, one per node with its
//     allocatable memory, and the overview's summary of them;
//   - checks the capacity against the nodes' summed allocatable memory;
//   - checks that the uptime and certificate checks, which go through
//     Traefik's Service, find the site up with Pebble's certificate;
//   - fires and resolves each default alert rule, as on Compose;
//   - restarts the control plane, which starts its node CPU counters
//     again, and checks that the host CPU chart reads the reset as one.
//
// A removed node isn't tested: the kind cluster has a control plane node,
// which runs the NFS server and PostgreSQL, and one worker, which runs
// every platform's pods, so neither can leave while tests run. The unit
// tests of provisioner/kubernetes cover a node leaving (TestNodeStats).
func TestKubernetesMonitoring(t *testing.T) {
	p := e2e.StartKubernetes(t, e2e.KubeOptions{Pebble: true})
	k := p.Kube
	ctx := t.Context()

	// The uptime check verifies the site's certificate as a visitor does,
	// so the control plane must trust Pebble's. Go reads the file
	// SSL_CERT_FILE names instead of the system roots; the API server's CA
	// is configured apart.
	roots := e2e.KubeDataDir + "/e2e/pebble-roots.pem"
	if _, err := p.ExecControlPlane(ctx, bytes.NewReader(p.Pebble.RootsPEM), "sh", "-c", `mkdir -p "$(dirname "$1")" && cat > "$1"`, "sh", roots); err != nil {
		t.Fatalf("writing Pebble's roots: %v", err)
	}

	groupID := createGroup(t, p, "E2E Kubernetes Monitoring")
	site, _ := k8sSite(t, p, groupID, "E2E Kubernetes Monitoring")
	hc := p.VerifyingIngressClient(t)
	home := "https://" + site.PreviewDomain + "/"
	waitCertified(t, hc, home, nil, http.StatusOK, time.Now())
	keepTraffic(ctx, hc, home)

	resp, err := p.Client.CreateSiteBackupWithResponse(ctx, site.ID)
	if err != nil {
		t.Fatal(err)
	}
	if resp.JSON202 == nil {
		t.Fatalf("back up: status %d: %s", resp.StatusCode(), resp.Body)
	}
	waitJob(t, p, resp.JSON202.Job.ID)

	// As in TestM4Acceptance: restarting runs every collector at once, and
	// short intervals keep the waits short.
	p.Restart(t, map[string]string{
		"SSL_CERT_FILE":                                 roots,
		"PRESSFLEET_METRICS_INTERVAL_SECONDS":           strconv.Itoa(int(m4Interval.Seconds())),
		"PRESSFLEET_ALERTS_EVALUATION_INTERVAL_SECONDS": strconv.Itoa(int(m4Interval.Seconds())),
	})

	t.Run("every metric of the design is collected", func(t *testing.T) {
		checkMetrics(t, p, site.ID, groupID, k8sNoSource)
	})

	nodes := k8sNodes(t, k)
	t.Run("host series per node", func(t *testing.T) { checkK8sHostSeries(t, p, nodes) })
	t.Run("capacity against allocatable memory", func(t *testing.T) { checkK8sCapacity(t, p, nodes) })
	t.Run("uptime and certificate checks through Traefik's Service", func(t *testing.T) {
		checkK8sUptime(t, p, site)
	})
	t.Run("each default alert rule fires and resolves", func(t *testing.T) {
		// "Disk nearly full" has no real series on a cluster; the series
		// the test writes still take it through the engine.
		checkAlerts(t, p, site.ID, groupID)
	})
	t.Run("node CPU after a control plane restart", func(t *testing.T) { checkK8sNodeCPURestart(t, p, nodes) })
}

// k8sNodes returns the cluster's nodes and their allocatable memory in
// bytes.
func k8sNodes(t *testing.T, k *e2e.KubePlatform) map[string]float64 {
	t.Helper()
	out := k.MustKubectl(t, "get", "nodes", "-o", `jsonpath={range .items[*]}{.metadata.name}{" "}{.status.allocatable.memory}{"\n"}{end}`)
	nodes := map[string]float64{}
	for line := range strings.Lines(out) {
		name, mem, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok {
			continue
		}
		b, err := parseQuantity(mem)
		if err != nil {
			t.Fatalf("node %s: allocatable memory %q: %v", name, mem, err)
		}
		nodes[name] = b
	}
	if len(nodes) < 2 {
		t.Fatalf("the cluster has %d nodes, want at least 2 (scripts/kind/kind.yaml): %s", len(nodes), out)
	}
	return nodes
}

// parseQuantity parses a Kubernetes quantity of bytes, such as "16303180Ki".
// The tests don't import the Kubernetes SDK (§4.2), and nodes report
// memory as a plain number or with one of these suffixes.
func parseQuantity(s string) (float64, error) {
	for _, u := range []struct {
		suffix string
		mult   float64
	}{
		{"Ki", 1 << 10}, {"Mi", 1 << 20}, {"Gi", 1 << 30}, {"Ti", 1 << 40},
		{"k", 1e3}, {"M", 1e6}, {"G", 1e9}, {"T", 1e12},
	} {
		if n, ok := strings.CutSuffix(s, u.suffix); ok {
			v, err := strconv.ParseFloat(n, 64)
			return v * u.mult, err
		}
	}
	return strconv.ParseFloat(s, 64)
}

// vmSeries is one series of a VictoriaMetrics instant query.
type vmSeries struct {
	Metric map[string]string `json:"metric"`
	Value  [2]any            `json:"value"`
}

// value returns the series' value.
func (s vmSeries) value() float64 {
	v, _ := s.Value[1].(string)
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return math.NaN()
	}
	return f
}

// vmQuery runs an instant query in VictoriaMetrics' container.
func vmQuery(t *testing.T, p *e2e.Platform, query string) []vmSeries {
	t.Helper()
	out, err := p.ExecVictoriaMetrics(t.Context(), "wget", "-q", "-O", "-", m4VictoriaMetrics+"/api/v1/query?query="+url.QueryEscape(query))
	if err != nil {
		t.Fatalf("query %s: %v", query, err)
	}
	var res struct {
		Status string `json:"status"`
		Data   struct {
			Result []vmSeries `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(out, &res); err != nil || res.Status != "success" {
		t.Fatalf("query %s: %v: %s", query, err, out)
	}
	return res.Data.Result
}

// vmSamples are the raw samples of one series, from VictoriaMetrics'
// export API.
type vmSamples struct {
	Metric     map[string]string `json:"metric"`
	Values     []float64         `json:"values"`
	Timestamps []int64           `json:"timestamps"`
}

// vmExport returns the raw samples of the series a selector matches since
// start.
func vmExport(t *testing.T, p *e2e.Platform, selector string, start time.Time) []vmSamples {
	t.Helper()
	q := url.Values{"match[]": {selector}, "start": {strconv.FormatInt(start.Unix(), 10)}}
	out, err := p.ExecVictoriaMetrics(t.Context(), "wget", "-q", "-O", "-", m4VictoriaMetrics+"/api/v1/export?"+q.Encode())
	if err != nil {
		t.Fatalf("export %s: %v", selector, err)
	}
	var series []vmSamples
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(nil, 16<<20)
	for sc.Scan() {
		var s vmSamples
		if err := json.Unmarshal(sc.Bytes(), &s); err != nil {
			t.Fatalf("export %s: %v: %s", selector, err, sc.Bytes())
		}
		series = append(series, s)
	}
	return series
}

// eventually calls check every few seconds until it returns "" or wait is
// up, then fails with its last answer.
func eventually(t *testing.T, wait time.Duration, check func() string) {
	t.Helper()
	deadline := time.Now().Add(wait)
	for {
		why := check()
		if why == "" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("after %s: %s", wait, why)
		}
		select {
		case <-t.Context().Done():
			t.Fatal(t.Context().Err())
		case <-time.After(5 * time.Second):
		}
	}
}

// checkK8sHostSeries checks the node collector's series in VictoriaMetrics
// and the platform summary the overview shows: a series per node, with the
// node's allocatable memory as its total.
func checkK8sHostSeries(t *testing.T, p *e2e.Platform, nodes map[string]float64) {
	ctx := t.Context()
	eventually(t, 2*time.Minute, func() string {
		total := map[string]float64{}
		for _, s := range vmQuery(t, p, `pressfleet_host_memory_total_bytes`) {
			total[s.Metric["node"]] = s.value()
		}
		avail := map[string]float64{}
		for _, s := range vmQuery(t, p, `pressfleet_host_memory_available_bytes`) {
			avail[s.Metric["node"]] = s.value()
		}
		cpu := map[string][]string{}
		for _, s := range vmQuery(t, p, `pressfleet_host_cpu_seconds_total`) {
			cpu[s.Metric["node"]] = append(cpu[s.Metric["node"]], s.Metric["mode"])
		}
		for node, alloc := range nodes {
			if total[node] != alloc {
				return fmt.Sprintf("node %s: memory total %v, want its allocatable memory %v (all: %v)", node, total[node], alloc, total)
			}
			if a, ok := avail[node]; !ok || a <= 0 || a >= alloc {
				return fmt.Sprintf("node %s: available memory %v of %v", node, a, alloc)
			}
			slices.Sort(cpu[node])
			if !slices.Equal(cpu[node], []string{"idle", "used"}) {
				return fmt.Sprintf("node %s: CPU series of modes %v, want idle and used", node, cpu[node])
			}
		}
		if len(total) != len(nodes) || len(cpu) != len(nodes) {
			return fmt.Sprintf("host series of nodes %v and %v, want %d nodes", total, cpu, len(nodes))
		}
		return ""
	})

	t.Run("overview", func(t *testing.T) {
		eventually(t, 2*time.Minute, func() string {
			r, err := p.Client.GetPlatformMetricsSummaryWithResponse(ctx, &client.GetPlatformMetricsSummaryParams{Metric: &[]string{"host_cpu", "host_memory"}})
			if err != nil {
				t.Fatal(err)
			}
			if r.JSON200 == nil {
				t.Fatalf("platform summary: status %d: %s", r.StatusCode(), r.Body)
			}
			got := map[string]map[string]float64{} // metric/type -> node -> value
			for _, m := range r.JSON200.Metrics {
				for _, v := range m.Values {
					key := m.ID + "/" + v.Labels["type"]
					if got[key] == nil {
						got[key] = map[string]float64{}
					}
					got[key][v.Labels["node"]] = v.Value
				}
			}
			for node, alloc := range nodes {
				if c, ok := got["host_cpu/"][node]; !ok || c < 0 || c > 1 {
					return fmt.Sprintf("node %s: host CPU %v (%v)", node, c, got)
				}
				if v := got["host_memory/total"][node]; v != alloc {
					return fmt.Sprintf("node %s: total memory %v, want %v (%v)", node, v, alloc, got)
				}
				if v := got["host_memory/used"][node]; v <= 0 || v >= alloc {
					return fmt.Sprintf("node %s: used memory %v of %v", node, v, alloc)
				}
			}
			return ""
		})
	})
}

// checkK8sCapacity checks that the capacity compares the sites'
// reservations with the nodes' allocatable memory, summed.
func checkK8sCapacity(t *testing.T, p *e2e.Platform, nodes map[string]float64) {
	var sum float64
	for _, b := range nodes {
		sum += b
	}
	r, err := p.Client.GetPlatformCapacityWithResponse(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	c := r.JSON200
	if c == nil {
		t.Fatalf("capacity: status %d: %s", r.StatusCode(), r.Body)
	}
	if c.HostMemoryMb == nil || c.Ratio == nil {
		t.Fatalf("capacity without the cluster's memory: %+v", c)
	}
	if want := sum / (1 << 20); math.Abs(*c.HostMemoryMb-want) > 0.01 {
		t.Errorf("capacity compares with %.2f MiB, want the nodes' allocatable memory, %.2f MiB", *c.HostMemoryMb, want)
	}
	if c.CountedSites < 1 || c.ReservedMemoryMb <= 0 {
		t.Errorf("capacity counts %d sites reserving %d MiB", c.CountedSites, c.ReservedMemoryMb)
	}
	if want := float64(c.ReservedMemoryMb) / *c.HostMemoryMb; math.Abs(*c.Ratio-want) > 1e-9 {
		t.Errorf("ratio %v, want %v", *c.Ratio, want)
	}
	if want := client.PlatformCapacityLevel("ok"); *c.Ratio < 0.9 && c.Level != want {
		t.Errorf("level %q at ratio %v, want %q", c.Level, *c.Ratio, want)
	}
}

// checkK8sUptime checks the uptime and certificate checks. The control
// plane connects to Traefik's Service (traefik.<namespace>.svc:443) with
// the domain as the TLS server name: the test domains resolve nowhere in
// the cluster, so a site that is up with a verified certificate was
// reached there.
func checkK8sUptime(t *testing.T, p *e2e.Platform, site client.Site) {
	sel := `{site_id="` + site.ID + `"}`
	eventually(t, 3*time.Minute, func() string {
		up := vmQuery(t, p, alerts.MetricSiteUp+sel)
		if len(up) != 1 || up[0].value() != 1 {
			return fmt.Sprintf("site_up %+v, want 1", up)
		}
		code := vmQuery(t, p, `pressfleet_site_http_check_status_code`+sel)
		if len(code) != 1 || code[0].value() != http.StatusOK {
			return fmt.Sprintf("check status %+v, want 200", code)
		}
		cert := vmQuery(t, p, alerts.MetricSiteCertExpiry+`{site_id="`+site.ID+`",domain="`+site.PreviewDomain+`"}`)
		// Pebble's certificates are valid for 90 days.
		if len(cert) != 1 || cert[0].value() < float64(time.Now().Add(60*24*time.Hour).Unix()) {
			return fmt.Sprintf("certificate expiry of %s %+v, want Pebble's certificate's, about 90 days away", site.PreviewDomain, cert)
		}
		return ""
	})
}

// checkK8sNodeCPURestart restarts the control plane, which keeps the
// nodes' CPU counters in memory (metrics.k8s.io reports rates), so they
// start again, and checks that the host CPU chart reads the drop as a
// counter reset: every point stays a share between 0 and 1, and points
// continue after the restart.
func checkK8sNodeCPURestart(t *testing.T, p *e2e.Platform, nodes map[string]float64) {
	ctx := t.Context()
	const used = `pressfleet_host_cpu_seconds_total{mode="used"}`
	before := map[string]float64{}
	for _, s := range vmQuery(t, p, used) {
		before[s.Metric["node"]] = s.value()
	}
	restart := time.Now()
	p.Restart(t, nil)

	// The old pod may write a few more samples while the new one starts,
	// but only the new one writes less than the old one had.
	eventually(t, 3*time.Minute, func() string {
		reset := map[string]bool{}
		for _, s := range vmExport(t, p, used, restart) {
			node := s.Metric["node"]
			for i, v := range s.Values {
				if s.Timestamps[i] >= restart.UnixMilli() && v < before[node] {
					reset[node] = true
				}
			}
		}
		for node := range nodes {
			if !reset[node] {
				return fmt.Sprintf("node %s: the CPU counter (%v before the restart) hasn't started again", node, before[node])
			}
		}
		return ""
	})

	start := restart.Add(-5 * time.Minute)
	eventually(t, 4*time.Minute, func() string {
		r, err := p.Client.QueryPlatformMetricsWithResponse(ctx, &client.QueryPlatformMetricsParams{Metric: &[]string{"host_cpu"}, Start: &start})
		if err != nil {
			t.Fatal(err)
		}
		if r.JSON200 == nil {
			t.Fatalf("host CPU: status %d: %s", r.StatusCode(), r.Body)
		}
		after := map[string]bool{}
		for _, m := range r.JSON200.Metrics {
			for _, s := range m.Series {
				for _, pt := range s.Points {
					if pt.Value < 0 || pt.Value > 1 || math.IsNaN(pt.Value) {
						t.Fatalf("node %s: host CPU %v at %s (the restart was at %s)", s.Labels["node"], pt.Value, pt.Time, restart)
					}
					// A point a minute after the restart has the reset in
					// its rate window.
					if pt.Time.After(restart.Add(time.Minute)) {
						after[s.Labels["node"]] = true
					}
				}
			}
		}
		for node := range nodes {
			if !after[node] {
				return fmt.Sprintf("node %s: no host CPU a minute after the restart (%s)", node, restart)
			}
		}
		return ""
	})
}
