package dashboard_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/e2e"
	"github.com/rchouinard/pressfleet/control-plane/internal/id"
	"github.com/rchouinard/pressfleet/control-plane/internal/ingress"
	"github.com/rchouinard/pressfleet/control-plane/internal/platform"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner/compose"
)

// The harness is a small HTTP server on a loopback port that answers the
// specs' questions about platform state the API doesn't show: what a
// deleted site left behind on Docker, the data directory, MariaDB, and the
// object cache. Specs reach it through e2e/fixtures/harness.ts. It goes
// through compose.Project like the rest of the e2e harness, so the specs
// never run the docker CLI (design doc §4.2).
//
// Add an endpoint here, and a method to harness.ts, when a spec needs to
// check something only the host can see.
type harness struct {
	p *e2e.Platform
	// projectDir is an empty directory for addressing site projects by
	// name (see TestSiteCreateDelete): their own directories belong to
	// root.
	projectDir string
	// ingress reaches Traefik's published ports (Platform.IngressClient).
	ingress *http.Client
}

// startHarness serves the harness until the test ends and returns its URL.
func startHarness(t *testing.T, p *e2e.Platform, projectDir string) string {
	t.Helper()
	h := &harness{p: p, projectDir: projectDir, ingress: p.IngressClient(t)}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /sites/{id}/leftovers", h.siteLeftovers)
	mux.HandleFunc("POST /client-groups/{id}/backup-repository", h.createRepository)
	mux.HandleFunc("GET /exists", h.exists)
	mux.HandleFunc("POST /sites/{id}/exec", h.siteExec)
	mux.HandleFunc("POST /ingress", h.ingressGet)
	mux.HandleFunc("POST /mariadb/query", h.mariadbQuery)
	addMonitoringEndpoints(t, mux, p)

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = srv.Close() })
	return "http://" + l.Addr().String()
}

// siteLeftovers lists what is left of a site, one human-readable entry per
// object: an empty list means the site left nothing behind.
func (h *harness) siteLeftovers(w http.ResponseWriter, r *http.Request) {
	siteID := r.PathValue("id")
	if !id.Valid(siteID) {
		http.Error(w, "invalid site ID", http.StatusBadRequest)
		return
	}
	left, err := h.leftovers(r.Context(), siteID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string][]string{"leftovers": left})
}

// createRepository creates a client group's backup repository the way an
// operator does before the group's first backup, with the CLI inside the
// control plane container, so offboarding can be tested without backing up
// a site first.
func (h *harness) createRepository(w http.ResponseWriter, r *http.Request) {
	groupID := r.PathValue("id")
	if !id.Valid(groupID) {
		http.Error(w, "invalid client group ID", http.StatusBadRequest)
		return
	}
	out, err := h.p.Project.Exec(r.Context(), e2e.Service, "pressfleet", "repository", "create", groupID)
	if err != nil {
		http.Error(w, fmt.Sprintf("%v: %s", err, out), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// exists reports whether a path under the data directory exists, as the
// control plane (root in its container, which mounts the data directory at
// the same path) sees it: the harness can't look into directories it
// creates with mode 0700.
func (h *harness) exists(w http.ResponseWriter, r *http.Request) {
	path := filepath.Clean(r.URL.Query().Get("path"))
	if rel, err := filepath.Rel(h.p.DataDir, path); err != nil || !filepath.IsAbs(path) || rel == ".." || strings.HasPrefix(rel, "../") {
		http.Error(w, "the path must be under the data directory", http.StatusBadRequest)
		return
	}
	out, err := h.p.Project.Exec(r.Context(), e2e.Service, "sh", "-c", `if test -e "$1"; then echo yes; else echo no; fi`, "sh", path)
	if err != nil {
		http.Error(w, fmt.Sprintf("%v: %s", err, out), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]bool{"exists": strings.TrimSpace(string(out)) == "yes"})
}

func (h *harness) leftovers(ctx context.Context, siteID string) ([]string, error) {
	left := []string{}
	sp := compose.Project{Name: compose.SiteProjectName(siteID), Dir: h.projectDir}

	res, err := sp.Resources(ctx)
	if err != nil {
		return nil, err
	}
	for _, c := range res.Containers {
		left = append(left, "container "+c)
	}
	for _, n := range res.Networks {
		left = append(left, "network "+n)
	}
	for _, v := range res.Volumes {
		left = append(left, "volume "+v)
	}

	// The harness created the sites directory, so it can see into it.
	dir := filepath.Join(h.p.DataDir, "sites", siteID)
	if _, err := os.Lstat(dir); err == nil {
		left = append(left, "directory "+dir)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}

	for _, service := range []string{"mariadb", "redis", ingress.TraefikService} {
		c, err := h.p.Project.ServiceContainer(ctx, service)
		if err != nil {
			return nil, err
		}
		if _, ok := c.Networks[sp.Name]; ok {
			left = append(left, service+" on network "+sp.Name)
		}
	}

	db, err := h.mariadb(ctx, siteID)
	if err != nil {
		return nil, err
	}
	left = append(left, db...)

	users, err := h.cacheUsers(ctx)
	if err != nil {
		return nil, err
	}
	for _, u := range users {
		if strings.Contains(u, siteID) {
			left = append(left, "object cache user "+u)
		}
	}
	return left, nil
}

// mariadb lists the site's database, user, and grants on the platform
// MariaDB, as root. The password is read inside the container, so it never
// reaches a command line.
func (h *harness) mariadb(ctx context.Context, siteID string) ([]string, error) {
	// siteID is validated, so it is safe to quote.
	name := "pf_" + siteID
	query := fmt.Sprintf("SELECT CONCAT('database ', SCHEMA_NAME) FROM information_schema.SCHEMATA WHERE SCHEMA_NAME = '%[1]s' "+
		"UNION ALL SELECT CONCAT('database user ', User, '@', Host) FROM mysql.user WHERE User = '%[1]s' "+
		"UNION ALL SELECT CONCAT('database grant on ', Db, ' to ', User) FROM mysql.db WHERE Db = '%[1]s' OR User = '%[1]s';", name)
	out, err := h.p.Project.Exec(ctx, "mariadb", "sh", "-c",
		`exec mariadb --user=root --password="$(cat /run/secrets/mariadb_root_password)" --batch --skip-column-names -e "$1"`, "sh", query)
	if err != nil {
		return nil, fmt.Errorf("querying MariaDB: %w", err)
	}
	var left []string
	for line := range strings.Lines(string(out)) {
		if line = strings.TrimSpace(line); line != "" {
			left = append(left, line)
		}
	}
	return left, nil
}

// cacheUsers lists the object cache's ACL users, as the admin. The
// password goes to valkey-cli over stdin.
func (h *harness) cacheUsers(ctx context.Context) ([]string, error) {
	pw, err := os.ReadFile(filepath.Join(h.p.DataDir, "platform", platform.RedisAdminPasswordSecret))
	if err != nil {
		return nil, err
	}
	out, err := h.p.Project.ExecInput(ctx, bytes.NewReader(append(pw, '\n')), "redis",
		"valkey-cli", "--no-auth-warning", "--user", platform.RedisAdminUser, "--askpass", "-e", "ACL", "USERS")
	if err != nil {
		return nil, fmt.Errorf("listing object cache users: %w", err)
	}
	return strings.Fields(string(out)), nil
}

// siteServices are the site containers siteExec runs commands in.
var siteServices = []string{"php", "nginx"}

// siteExec runs a command in one of a site's running containers, as the
// container's user: WP-CLI in php, or a look at the container's own view
// of its limits and caches. It answers 200 whether or not the command
// succeeded, with its standard output and, if it failed, the error, so a
// spec can check that a command fails (a purged cache key, say).
func (h *harness) siteExec(w http.ResponseWriter, r *http.Request) {
	siteID := r.PathValue("id")
	if !id.Valid(siteID) {
		http.Error(w, "invalid site ID", http.StatusBadRequest)
		return
	}
	var req struct {
		Service string   `json:"service"`
		Command []string `json:"command"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.Command) == 0 || !slices.Contains(siteServices, req.Service) {
		http.Error(w, `want {"service": "php" or "nginx", "command": [...]}`, http.StatusBadRequest)
		return
	}
	sp := compose.Project{Name: compose.SiteProjectName(siteID), Dir: h.projectDir}
	out, err := sp.ExecService(r.Context(), req.Service, req.Command...)
	res := map[string]string{"output": string(out)}
	if err != nil {
		res["error"] = err.Error()
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(res)
}

// ingressGet sends a GET through Traefik, as a browser would once DNS
// points a domain at the host, and returns the response without following
// redirects: its status, headers, and body. The browser only reaches the
// dashboard domain this way; answers that it can't read, such as a 401
// that asks for the preview password, come from here. Optional basic auth
// credentials come in the request body, so they stay out of URLs.
func (h *harness) ingressGet(w http.ResponseWriter, r *http.Request) {
	var req struct {
		URL      string `json:"url"`
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || !strings.HasPrefix(req.URL, "https://") && !strings.HasPrefix(req.URL, "http://") {
		http.Error(w, `want {"url": "https://..."}`, http.StatusBadRequest)
		return
	}
	out, err := http.NewRequestWithContext(r.Context(), http.MethodGet, req.URL, nil)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if req.Username != "" || req.Password != "" {
		out.SetBasicAuth(req.Username, req.Password)
	}
	// No cookie jar: each request is a fresh visitor.
	c := *h.ingress
	c.Jar = nil
	resp, err := c.Do(out)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	headers := map[string]string{}
	for k := range resp.Header {
		headers[strings.ToLower(k)] = strings.Join(resp.Header.Values(k), ", ")
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"status": resp.StatusCode, "headers": headers, "body": string(body)})
}

// Monitoring endpoints (#177): series written into the metrics store, so
// alert rules fire and resolve on cue; a webhook receiver for alert
// channels; and requests to a site through the ingress, so it writes log
// lines.
//
//	POST /metrics/series                 writes exposition lines into VictoriaMetrics
//	POST /metrics/delete?match=          deletes the series a selector matches
//	GET  /webhook                        the receiver's URL, as the control plane reaches it
//	GET  /webhook/requests?path=         what the receiver got on a path
//	POST /sites/{id}/visit?path=         a GET to the site's preview domain through Traefik
func addMonitoringEndpoints(t *testing.T, mux *http.ServeMux, p *e2e.Platform) {
	t.Helper()
	m := &monitoring{p: p, ingress: p.IngressClient(t), hooks: map[string][]webhookRequest{}}
	m.webhookURL = m.startReceiver(t)
	mux.HandleFunc("POST /metrics/series", m.writeSeries)
	mux.HandleFunc("POST /metrics/delete", m.deleteSeries)
	mux.HandleFunc("GET /webhook", m.webhook)
	mux.HandleFunc("GET /webhook/requests", m.webhookRequests)
	mux.HandleFunc("POST /sites/{id}/visit", m.visit)
}

type monitoring struct {
	p          *e2e.Platform
	ingress    *http.Client
	webhookURL string

	mu    sync.Mutex
	hooks map[string][]webhookRequest
}

// webhookRequest is one request the receiver got.
type webhookRequest struct {
	Method  string            `json:"method"`
	Headers map[string]string `json:"headers"`
	Body    string            `json:"body"`
}

// VictoriaMetrics' address inside its container, which has no published
// port; its image has BusyBox's wget (see m4_test.go).
const victoriaMetrics = "http://127.0.0.1:8428"

// seriesAge is how far in the past written samples are stamped.
// VictoriaMetrics hides samples younger than its -search.latencyOffset (30
// seconds) from queries, so a sample stamped now would take that long to
// count; one stamped a minute ago counts at once, and stays the latest
// value of its series for the query lookback (minutes).
const seriesAge = time.Minute

// writeSeries writes the body's exposition lines (`name{labels} value`,
// no timestamp) into the metrics store, stamped seriesAge ago.
func (m *monitoring) writeSeries(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	ts := time.Now().Add(-seriesAge).UnixMilli()
	var b strings.Builder
	for line := range strings.Lines(string(body)) {
		if line = strings.TrimSpace(line); line != "" {
			fmt.Fprintf(&b, "%s %d\n", line, ts)
		}
	}
	if b.Len() == 0 {
		http.Error(w, "no series", http.StatusBadRequest)
		return
	}
	if err := m.postVM(r.Context(), "/api/v1/import/prometheus", b.String()); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// deleteSeries removes the series a selector matches, so a condition ends
// at once instead of when its samples go stale.
func (m *monitoring) deleteSeries(w http.ResponseWriter, r *http.Request) {
	match := r.URL.Query().Get("match")
	if match == "" {
		http.Error(w, "match is required", http.StatusBadRequest)
		return
	}
	if err := m.postVM(r.Context(), "/api/v1/admin/tsdb/delete_series", url.Values{"match[]": {match}}.Encode()); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (m *monitoring) postVM(ctx context.Context, path, body string) error {
	out, err := m.p.Project.Exec(ctx, "victoria-metrics", "wget", "-q", "-O", "-", "--post-data", body, victoriaMetrics+path)
	if err != nil {
		return fmt.Errorf("posting to VictoriaMetrics %s: %w: %s", path, err, out)
	}
	return nil
}

// startReceiver serves the webhook receiver on the address of the control
// plane's default gateway, the host's side of the platform network, so
// alert channels can deliver to it like to any service outside the
// platform. It records every request by path and answers 204. This needs
// Linux, where the host owns the bridge's address; on Docker Desktop the
// specs that use it fail.
func (m *monitoring) startReceiver(t *testing.T) string {
	t.Helper()
	gw, err := m.gateway(t.Context())
	if err != nil {
		t.Fatalf("finding the control plane's gateway for the webhook receiver: %v", err)
	}
	l, err := net.Listen("tcp", net.JoinHostPort(gw, "0"))
	if err != nil {
		t.Fatalf("webhook receiver: %v", err)
	}
	srv := &http.Server{ReadHeaderTimeout: 10 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		req := webhookRequest{Method: r.Method, Headers: map[string]string{}, Body: string(body)}
		for k := range r.Header {
			req.Headers[strings.ToLower(k)] = r.Header.Get(k)
		}
		m.mu.Lock()
		m.hooks[r.URL.Path] = append(m.hooks[r.URL.Path], req)
		m.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})}
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = srv.Close() })
	return "http://" + l.Addr().String()
}

// gateway reads the control plane container's default route from
// /proc/net/route, where addresses are little-endian hex.
func (m *monitoring) gateway(ctx context.Context) (string, error) {
	out, err := m.p.Project.Exec(ctx, e2e.Service, "cat", "/proc/net/route")
	if err != nil {
		return "", fmt.Errorf("%w: %s", err, out)
	}
	for line := range strings.Lines(string(out)) {
		f := strings.Fields(line)
		if len(f) < 3 || f[1] != "00000000" {
			continue
		}
		v, err := strconv.ParseUint(f[2], 16, 32)
		if err != nil {
			return "", fmt.Errorf("default route %q: %w", line, err)
		}
		return netip.AddrFrom4([4]byte{byte(v), byte(v >> 8), byte(v >> 16), byte(v >> 24)}).String(), nil
	}
	return "", errors.New("no default route")
}

func (m *monitoring) webhook(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"url": m.webhookURL})
}

func (m *monitoring) webhookRequests(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	got := append([]webhookRequest{}, m.hooks[r.URL.Query().Get("path")]...)
	m.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string][]webhookRequest{"requests": got})
}

// visit sends a GET for the path to the site's preview domain, as a visitor
// would, and reports the status, so the site writes an access log line.
func (m *monitoring) visit(w http.ResponseWriter, r *http.Request) {
	siteID := r.PathValue("id")
	path := r.URL.Query().Get("path")
	if !id.Valid(siteID) || !strings.HasPrefix(path, "/") {
		http.Error(w, "a site ID and a path starting with / are required", http.StatusBadRequest)
		return
	}
	site, err := m.p.Client.GetSiteWithResponse(r.Context(), siteID)
	if err != nil || site.JSON200 == nil {
		http.Error(w, fmt.Sprintf("getting site %s: %v", siteID, err), http.StatusBadGateway)
		return
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, "https://"+site.JSON200.PreviewDomain+path, nil)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	resp, err := m.ingress.Do(req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]int{"status": resp.StatusCode})
}

// mariadbQuery runs the SQL in the request body on the platform MariaDB as
// root and returns its output in batch format, without column names: what
// a database session created or left behind.
func (h *harness) mariadbQuery(w http.ResponseWriter, r *http.Request) {
	query, err := io.ReadAll(r.Body)
	if err != nil || len(query) == 0 {
		http.Error(w, "the body must be the SQL to run", http.StatusBadRequest)
		return
	}
	out, err := h.p.Project.ExecInput(r.Context(), bytes.NewReader(query), "mariadb", "sh", "-c",
		`exec mariadb --user=root --password="$(cat /run/secrets/mariadb_root_password)" --batch --skip-column-names`)
	if err != nil {
		http.Error(w, fmt.Sprintf("%v: %s", err, out), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"output": string(out)})
}
