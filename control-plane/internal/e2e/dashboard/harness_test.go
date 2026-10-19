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
	"os"
	"path/filepath"
	"strings"
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
}

// startHarness serves the harness until the test ends and returns its URL.
func startHarness(t *testing.T, p *e2e.Platform, projectDir string) string {
	t.Helper()
	h := &harness{p: p, projectDir: projectDir}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /sites/{id}/leftovers", h.siteLeftovers)
	mux.HandleFunc("POST /client-groups/{id}/backup-repository", h.createRepository)
	mux.HandleFunc("GET /exists", h.exists)
	mux.HandleFunc("POST /sites/{id}/exec", h.siteExec)
	mux.HandleFunc("POST /mariadb/query", h.mariadbQuery)

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

// siteExec runs a command in a site's php container, as the site's user,
// and returns its standard output: WP-CLI to read the site's content, sh
// to read its files, or curl to fetch a page from its nginx on the site
// network. The request body is {"command": [...]}.
func (h *harness) siteExec(w http.ResponseWriter, r *http.Request) {
	siteID := r.PathValue("id")
	if !id.Valid(siteID) {
		http.Error(w, "invalid site ID", http.StatusBadRequest)
		return
	}
	var req struct {
		Command []string `json:"command"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.Command) == 0 {
		http.Error(w, "the body must be {\"command\": [...]}", http.StatusBadRequest)
		return
	}
	sp := compose.Project{Name: compose.SiteProjectName(siteID), Dir: h.projectDir}
	out, err := sp.ExecService(r.Context(), "php", req.Command...)
	if err != nil {
		http.Error(w, fmt.Sprintf("%v: %s", err, out), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"output": string(out)})
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
