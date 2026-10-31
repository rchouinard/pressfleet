package e2e_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/api/client"
	"github.com/rchouinard/pressfleet/control-plane/internal/catalog"
	"github.com/rchouinard/pressfleet/control-plane/internal/cli"
	"github.com/rchouinard/pressfleet/control-plane/internal/e2e"
	"github.com/rchouinard/pressfleet/control-plane/internal/platform"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner/compose"
	"github.com/rchouinard/pressfleet/control-plane/internal/redact"
	"github.com/rchouinard/pressfleet/control-plane/internal/skew"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
)

// brokenControlPlane is the Dockerfile of a control plane image that
// starts, migrates its state database, and serves like the one it is built
// from, but whose healthcheck always fails: an upgrade to it fails the way
// a broken release would. The platform's Compose file sets the
// healthcheck, which overrides an image's HEALTHCHECK, so the image
// replaces the command the healthcheck runs instead.
const brokenControlPlane = `FROM %s
RUN mv /usr/local/bin/pressfleet /usr/local/bin/pressfleet-real \
 && printf '#!/bin/sh\n[ "$1" = healthcheck ] && exit 1\nexec /usr/local/bin/pressfleet-real "$@"\n' >/usr/local/bin/pressfleet \
 && chmod 0755 /usr/local/bin/pressfleet
`

// TestUpgradeRecovery installs the minimum upgradable-from release with its
// own CLI, upgrades it with this tree's CLI to a control plane that never
// becomes healthy, and recovers by running the commands the failed upgrade
// prints, verbatim (#142). The platform must come back at the previous
// release with its data, sites must serve throughout, and a later upgrade
// to a working control plane must succeed, which also tests upgrading from
// the previous release (#95).
func TestUpgradeRecovery(t *testing.T) {
	image := e2e.Image(t)
	releaseCLI := e2e.ReleaseCLI(t, skew.MinUpgradeFrom)
	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	configPath := filepath.Join(root, "etc", "pressfleet.yaml")
	project := compose.Project{
		Name: releaseProjectName(t, skew.MinUpgradeFrom),
		Dir:  filepath.Join(dataDir, "platform"),
	}
	var siteProject compose.Project
	// Registered before Adopt, so it runs after the platform teardown: see
	// TestSiteCreateDelete.
	t.Cleanup(func() {
		if siteProject.Name == "" {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if res, err := siteProject.Resources(ctx); err == nil && !res.Empty() {
			t.Logf("removing leftover site project %s", siteProject.Name)
			if err := siteProject.Down(ctx, compose.DownOptions{Volumes: true}); err != nil {
				t.Errorf("removing leftover site project %s: %v", siteProject.Name, err)
			}
		}
	})
	p := e2e.Adopt(t, project, dataDir)
	e2e.WriteOverride(t, project.Dir)
	ctx, cancel := context.WithTimeout(t.Context(), 40*time.Minute)
	defer cancel()

	const email = "admin@upgrade-recovery.test"
	password := redact.Secret(strings.ToLower(rand.Text()))
	// upgrade runs this tree's CLI.
	upgrade := func(args ...string) (int, string) {
		t.Helper()
		var stdout, stderr bytes.Buffer
		code := cli.Run(ctx, append([]string{"upgrade", "--config", configPath, "--yes", "--skip-preflight"}, args...), cli.Options{
			Version:             "e2e",
			Stdin:               strings.NewReader(""),
			Stdout:              &stdout,
			Stderr:              &stderr,
			Env:                 func(string) (string, bool) { return "", false },
			PlatformProjectName: project.Name,
		})
		t.Logf("upgrade output:\n%s%s", stdout.String(), stderr.String())
		return code, stderr.String()
	}

	// 1. Install the previous release with its own CLI, which installs its
	// own control plane image.
	t.Logf("installing %s with %s", skew.MinUpgradeFrom, releaseCLI)
	install := exec.CommandContext(ctx, releaseCLI, "init", "--config", configPath,
		"--non-interactive", "--confirm-key-stored", "--skip-preflight",
		"--dashboard-domain", "fleet.upgrade-recovery.test", "--preview-base-domain", "preview.upgrade-recovery.test",
		"--data-dir", dataDir, "--admin-email", email, "--admin-password-stdin",
		"--acme-ca-server", e2e.ACMECAServer,
	)
	install.Env = releaseEnv(project.Name)
	install.Stdin = strings.NewReader(password.Reveal() + "\n")
	var installOut, installErr bytes.Buffer
	install.Stdout, install.Stderr = &installOut, &installErr
	err := install.Run()
	t.Logf("%s init output:\n%s%s", skew.MinUpgradeFrom,
		strings.ReplaceAll(installOut.String(), keyLine(installOut.String()), "    pfmk1:<redacted>"), installErr.String())
	if err != nil {
		t.Fatalf("%s init: %v", skew.MinUpgradeFrom, err)
	}

	// 2. Record the state: the version, a client group, and a running site
	// and what it serves.
	p.Connect(ctx, t)
	p.Client = p.Login(t, email, password)
	before := controlPlaneInfo(ctx, t, project)
	if before.Version != skew.MinUpgradeFrom || !before.Ready {
		t.Fatalf("the installed control plane runs %s (ready: %t), want %s", before.Version, before.Ready, skew.MinUpgradeFrom)
	}
	// The site images of this tree's catalog. The previous release pins its
	// own; the provisioner pulls any of those missing, unless they are
	// private.
	cat, err := catalog.Load()
	if err != nil {
		t.Fatal(err)
	}
	php, ok := cat.DefaultPHPVersion(time.Now())
	if !ok {
		t.Fatal("no default PHP version")
	}
	images, err := cat.Images(php, cat.LatestWordPress().Version)
	if err != nil {
		t.Fatal(err)
	}
	e2e.EnsureImages(t, images.PHP, images.Nginx)
	groupID := createGroup(t, p, "E2E Upgrade Recovery Agency")
	created := createSite(t, p, groupID, "E2E Upgrade Recovery", php)
	siteProject = compose.Project{Name: compose.SiteProjectName(created.Site.ID), Dir: t.TempDir()}
	waitJob(t, p, created.Job.ID)
	site := getSite(t, p, created.Site.ID)
	siteURL := "https://" + site.PreviewDomain + "/"
	hc := p.IngressClient(t)
	title := pageTitle(t, hc, siteURL)

	// 3. Upgrade to a control plane that never becomes healthy, while a
	// visitor keeps requesting the site.
	// The control plane's container creates the state backups directory as
	// root, mode 0700, which the operator running upgrade can't read. The
	// release's init creates it as the test user instead, so make it so:
	// the recovery steps have to find the backup through Docker.
	backupsDir := filepath.Join(dataDir, "state", "backups")
	if _, err := project.ExecService(ctx, e2e.Service, "sh", "-c", `mkdir -p "$1" && chown 0:0 "$1" && chmod 0700 "$1"`, "sh", backupsDir); err != nil {
		t.Fatalf("making the state backups directory root's: %v", err)
	}
	if os.Geteuid() != 0 {
		if _, err := os.ReadDir(backupsDir); err == nil {
			t.Fatalf("%s is still readable by the test user", backupsDir)
		}
	}
	previousSchema := installedSchema(ctx, t, project)
	latestSchema, err := store.LatestVersion(store.SQLite)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("the state database is at schema version %d in %s and %d in this tree", previousSchema, skew.MinUpgradeFrom, latestSchema)
	broken := "pressfleet-control-plane:" + project.Name + "-broken"
	if err := compose.BuildImage(ctx, nil, broken, fmt.Sprintf(brokenControlPlane, image)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = compose.RemoveImage(context.Background(), nil, broken) })
	shared := sharedContainers(ctx, t, project)
	traffic := e2e.ProbeTraffic(t, hc, siteURL)
	code, stderr := upgrade("--control-plane-image", broken)

	// 4. The upgrade fails with the recovery steps, and the site served
	// throughout.
	if code == 0 {
		t.Fatal("the upgrade to a control plane that never becomes healthy succeeded")
	}
	checkTraffic(ctx, t, project, traffic, shared, "the failed upgrade")
	commands := recoveryCommands(stderr)
	if len(commands) == 0 {
		t.Fatalf("the failed upgrade printed no recovery commands")
	}
	previousDir := filepath.Join(dataDir, "upgrade", "previous-platform")
	for _, want := range []string{"To return to " + skew.MinUpgradeFrom, project.Name, project.Dir, previousDir} {
		if !strings.Contains(stderr, want) {
			t.Errorf("the recovery steps don't name %s", want)
		}
	}
	restore := slices.IndexFunc(commands, func(c string) bool { return strings.Contains(c, " state restore ") })
	switch {
	case restore >= 0:
		// The restore runs the previous control plane image, by digest.
		if digest := previousControlPlaneDigest(t, dataDir); !strings.Contains(stderr, digest) {
			t.Errorf("the recovery steps don't name the previous control plane image %s", digest)
		}
	case previousSchema != latestSchema:
		// The new control plane migrated the database, so without the
		// restore the previous release crash-loops on a schema it doesn't
		// know. The steps mustn't hedge either: they have to name the
		// backup (the operator running them isn't root, and the backups
		// directory is).
		t.Fatalf("the state database schema is %d in %s and %d in this tree, but the recovery steps have no state restore command", previousSchema, skew.MinUpgradeFrom, latestSchema)
	case !strings.Contains(stderr, "The state database needs no restore"):
		t.Error("the recovery steps neither restore the state database nor say it needs no restore")
	}

	// 5. Recover by running the printed commands as they are.
	for i, command := range commands {
		out, err := compose.RunShell(ctx, command)
		t.Logf("$ %s\n%s", command, out)
		if err != nil {
			t.Fatalf("recovery command %d failed: %v", i+1, err)
		}
		if i == restore {
			checkStateRestore(t, command, string(out))
		}
	}

	// 6. The platform is back at the previous release, with its data.
	p.Connect(ctx, t)
	if after := controlPlaneInfo(ctx, t, project); after.Version != before.Version || !after.Ready {
		t.Errorf("after the recovery, the control plane runs %s (ready: %t), want %s", after.Version, after.Ready, before.Version)
	}
	p.Client = p.Login(t, email, password)
	if resp, err := p.Client.GetClientGroupWithResponse(ctx, groupID); err != nil || resp.JSON200 == nil {
		t.Errorf("the client group is gone after the recovery: %v %v", err, resp)
	}
	if s := getSite(t, p, site.ID); s.Status != client.SiteStatusRunning {
		t.Errorf("after the recovery, the site is %s", s.Status)
	}
	if got := pageTitle(t, hc, siteURL); got != title {
		t.Errorf("after the recovery, the site serves %q, want %q", got, title)
	}
	backup, err := p.Client.CreateSiteBackupWithResponse(ctx, site.ID)
	if err != nil {
		t.Fatal(err)
	}
	if backup.JSON202 == nil {
		t.Fatalf("back up after the recovery: status %d: %s", backup.StatusCode(), backup.Body)
	}
	waitJob(t, p, backup.JSON202.Job.ID)

	// 7. Upgrading to a working control plane succeeds from the recovered
	// platform, without interrupting the site.
	shared = sharedContainers(ctx, t, project)
	traffic = e2e.ProbeTraffic(t, hc, siteURL)
	if code, stderr := upgrade("--control-plane-image", image); code != 0 {
		_, _ = traffic.Stop()
		t.Fatalf("the upgrade after the recovery exited %d: %s", code, stderr)
	}
	checkTraffic(ctx, t, project, traffic, shared, "the upgrade")
	if c, err := project.ServiceContainer(ctx, e2e.Service); err != nil || c.Image != image {
		t.Errorf("the control plane runs %q (%v), want %s", c.Image, err, image)
	}
	p.Connect(ctx, t)
	if info := controlPlaneInfo(ctx, t, project); !info.Ready {
		t.Error("the upgraded control plane isn't ready")
	}
	p.Client = p.Login(t, email, password)
	if s := getSite(t, p, site.ID); s.Status != client.SiteStatusRunning {
		t.Errorf("after the upgrade, the site is %s", s.Status)
	}
}

// releaseProjectName returns the Compose project name to install a release
// under. The init of releases up to v0.1.0 ignores
// PRESSFLEET_PLATFORM_PROJECT and installs under the default name, which
// on a workstation may be the developer's own platform: then the test is
// skipped (in CI, where the runner has no platform, it fails instead).
func releaseProjectName(t *testing.T, version string) string {
	t.Helper()
	v, err := skew.Parse(version)
	if err != nil {
		t.Fatal(err)
	}
	last, err := skew.Parse("v0.1.0")
	if err != nil {
		t.Fatal(err)
	}
	if v.Compare(last) > 0 {
		return "pfupg-" + strings.ToLower(rand.Text())[:10]
	}
	name := platform.ProjectName
	res, err := compose.Project{Name: name, Dir: t.TempDir()}.Resources(t.Context())
	switch {
	case err != nil:
		t.Fatal(err)
	case !res.Empty() && os.Getenv("CI") == "true":
		t.Fatalf("the Compose project %s exists on this Docker host, and %s can only install under that name", name, version)
	case !res.Empty():
		t.Skipf("the Compose project %s exists on this Docker host (a development platform?), and %s can only install under that name. "+
			"Remove it to run this test, or run it in CI", name, version)
	}
	return name
}

// releaseEnv is the environment of a release's CLI: enough to find docker,
// and the platform's project name, but none of the test's PRESSFLEET_*
// settings.
func releaseEnv(project string) []string {
	env := []string{"PRESSFLEET_PLATFORM_PROJECT=" + project}
	for _, name := range []string{"PATH", "HOME", "DOCKER_HOST", "DOCKER_CONFIG", "DOCKER_CONTEXT"} {
		if v, ok := os.LookupEnv(name); ok {
			env = append(env, name+"="+v)
		}
	}
	return env
}

// adminInfo is the part of `pressfleet admin info` the test reads.
type adminInfo struct {
	Version string `json:"version"`
	Ready   bool   `json:"ready"`
}

// controlPlaneInfo asks the control plane for its version and readiness
// through its admin interface, as the host CLI does.
func controlPlaneInfo(ctx context.Context, t *testing.T, project compose.Project) adminInfo {
	t.Helper()
	out, err := project.ExecService(ctx, e2e.Service, "pressfleet", "admin", "info")
	if err != nil {
		t.Fatalf("pressfleet admin info: %v", err)
	}
	var info adminInfo
	if err := json.Unmarshal(out, &info); err != nil {
		t.Fatalf("pressfleet admin info: %v: %s", err, out)
	}
	return info
}

// titleTag finds a page's title.
var titleTag = regexp.MustCompile(`(?s)<title>(.*?)</title>`)

// pageTitle gets a page through Traefik and returns its title, failing the
// test unless the page is served.
func pageTitle(t *testing.T, hc *http.Client, url string) string {
	t.Helper()
	resp, err := hc.Get(url) //nolint:noctx // the client has a timeout
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	m := titleTag.FindSubmatch(body)
	if resp.StatusCode != http.StatusOK || m == nil {
		t.Fatalf("GET %s: %s: %.500s", url, resp.Status, body)
	}
	return strings.TrimSpace(string(m[1]))
}

// sharedContainers returns the containers of the services every site
// depends on, by service.
func sharedContainers(ctx context.Context, t *testing.T, project compose.Project) map[string]string {
	t.Helper()
	ids := map[string]string{}
	for _, s := range []string{"traefik", "mariadb", "redis"} {
		c, err := project.ServiceContainer(ctx, s)
		if err != nil {
			t.Fatal(err)
		}
		ids[s] = c.ID
	}
	return ids
}

// checkTraffic stops a traffic probe and fails the test if a request
// failed while the shared services kept their containers: an upgrade that
// recreates only the control plane mustn't interrupt sites. One that
// recreated Traefik, MariaDB, or the object cache interrupts them by
// design (upgrade warns about it), so then failures are only logged.
func checkTraffic(ctx context.Context, t *testing.T, project compose.Project, traffic *e2e.TrafficProbe, before map[string]string, during string) {
	t.Helper()
	ok, failures := traffic.Stop()
	var recreated []string
	for s, id := range sharedContainers(ctx, t, project) {
		if before[s] != id {
			recreated = append(recreated, s)
		}
	}
	t.Logf("%d site requests succeeded and %d failed during %s", ok, len(failures), during)
	switch {
	case ok == 0:
		t.Errorf("no site request succeeded during %s", during)
	case len(failures) == 0:
	case len(recreated) > 0:
		t.Logf("%s recreated %s, which interrupts sites; failed requests:\n%s", during, strings.Join(recreated, ", "), strings.Join(failures, "\n"))
	default:
		t.Errorf("site requests failed during %s, which recreated only the control plane:\n%s", during, strings.Join(failures, "\n"))
	}
}

// recoveryCommands extracts the commands from the recovery steps a failed
// upgrade prints: the indented lines between the introduction and the
// closing note.
func recoveryCommands(out string) []string {
	_, steps, ok := strings.Cut(out, "run these commands on this host as root:\n")
	if !ok {
		return nil
	}
	steps, _, _ = strings.Cut(steps, "\nThen use the pressfleet CLI")
	var commands []string
	for line := range strings.Lines(steps) {
		if command, ok := strings.CutPrefix(strings.TrimRight(line, "\n"), "    "); ok && command != "" {
			commands = append(commands, command)
		}
	}
	return commands
}

// previousControlPlaneDigest returns the image the control plane ran before
// the upgrade, by digest, from the upgrade's record.
func previousControlPlaneDigest(t *testing.T, dataDir string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dataDir, "upgrade", "upgrade.json"))
	if err != nil {
		t.Fatal(err)
	}
	var record struct {
		PreviousImages map[string]struct {
			Digests []string `json:"digests"`
		} `json:"previous_images"`
	}
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatal(err)
	}
	digests := record.PreviousImages[e2e.Service].Digests
	if len(digests) == 0 {
		t.Fatalf("the upgrade recorded no digest of the previous control plane image: %s", data)
	}
	return digests[0]
}

// schemaReady finds the schema version in the "state database ready" log
// line a control plane writes after migrating its state database.
var schemaReady = regexp.MustCompile(`state database ready.*?schema_version\W+(\d+)`)

// installedSchema returns the schema version the running control plane
// reports it migrated its state database to, from its log: the release's
// own answer, not an assumption about it.
func installedSchema(ctx context.Context, t *testing.T, project compose.Project) int {
	t.Helper()
	out, err := project.Logs(ctx)
	if err != nil {
		t.Fatalf("reading the control plane's log: %v", err)
	}
	matches := schemaReady.FindAllSubmatch(out, -1)
	if len(matches) == 0 {
		t.Fatalf("the control plane's log has no \"state database ready\" line:\n%.2000s", out)
	}
	schema, err := strconv.Atoi(string(matches[len(matches)-1][1]))
	if err != nil {
		t.Fatal(err)
	}
	return schema
}

// backupSchema finds the schema version in a pre-migration backup's name.
var backupSchema = regexp.MustCompile(`-schema(\d{4})\.db\b`)

// checkStateRestore checks the output of the recovery's state restore: the
// state database is back at the schema version of the backup taken before
// the migration, and the replaced database was kept.
func checkStateRestore(t *testing.T, command, out string) {
	t.Helper()
	m := backupSchema.FindStringSubmatch(command)
	if m == nil {
		t.Fatalf("the restore command names no pre-migration backup: %s", command)
	}
	schema, err := strconv.Atoi(m[1])
	if err != nil {
		t.Fatal(err)
	}
	if want := fmt.Sprintf("(schema version %d)", schema); !strings.Contains(out, want) {
		t.Errorf("the state restore didn't restore schema version %d", schema)
	}
	if !strings.Contains(out, "The previous database was saved as ") {
		t.Error("the state restore didn't keep the replaced database")
	}
	t.Logf("the recovery restored the state database to schema version %d", schema)
}
