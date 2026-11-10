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
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/api/client"
	"github.com/rchouinard/pressfleet/control-plane/internal/catalog"
	"github.com/rchouinard/pressfleet/control-plane/internal/cli"
	"github.com/rchouinard/pressfleet/control-plane/internal/config"
	"github.com/rchouinard/pressfleet/control-plane/internal/e2e"
	"github.com/rchouinard/pressfleet/control-plane/internal/ingress"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner/compose"
	"github.com/rchouinard/pressfleet/control-plane/internal/redact"
	"github.com/rchouinard/pressfleet/control-plane/internal/skew"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
)

// The upgrade rollback (design doc §4.7, #251): a failed upgrade rolls back
// on its own, an interrupted one too, `pressfleet upgrade --rollback`
// returns a succeeded one to the release before it, and a rollback that
// can't finish prints manual steps that do. TestUpgradeRecovery covers the
// manual steps of an upgrade run with --no-rollback.
//
// The rollback restores the state database only if the upgrade migrated
// it, and that is the path worth testing. An upgrade from a published
// release migrates as long as the tree has a migration since; an upgrade
// from this tree's own image never does. Those tests upgrade to a control
// plane built with the e2emigration tag instead, which adds a no-op
// migration (store/migrate_e2e.go).

// TestUpgradeRollback installs the minimum upgradable-from release with its
// own CLI, and upgrades it with this tree's CLI to a control plane whose
// healthcheck always fails: the upgrade rolls back on its own and exits 2,
// with the platform back on the release, its data, and its state database
// restored, sites serving throughout, and the failed control plane's
// rollouts held while it ran. Then an upgrade interrupted with SIGTERM
// while it applies the release rolls back the same way.
func TestUpgradeRollback(t *testing.T) {
	image := e2e.Image(t)
	releaseCLI := e2e.ReleaseCLI(t, skew.MinUpgradeFrom)
	ctx, cancel := context.WithTimeout(t.Context(), 40*time.Minute)
	defer cancel()
	u := newUpgradePlatform(ctx, t, releaseProjectName(t, skew.MinUpgradeFrom), "upgrade-rollback.test")
	u.installRelease(releaseCLI, skew.MinUpgradeFrom)

	// 1. A client group and a running site, and what it serves.
	before := controlPlaneInfo(ctx, t, u.Project)
	if before.Version != skew.MinUpgradeFrom || !before.Ready {
		t.Fatalf("the installed control plane runs %s (ready: %t), want %s", before.Version, before.Ready, skew.MinUpgradeFrom)
	}
	previousImage := u.controlPlaneImage()
	php := defaultPHP(t)
	u.ensureSiteImages(php)
	groupID := createGroup(t, u.Platform, "E2E Upgrade Rollback Agency")
	site := u.createSite(groupID, "E2E Upgrade Rollback", php)
	siteURL := "https://" + site.PreviewDomain + "/"
	hc := u.IngressClient(t)
	title := pageTitle(t, hc, siteURL)
	wordpress := wordPressVersion(t, hc, siteURL)
	// The group's repository has to exist, with its password in the state
	// database, before the upgrade backs the database up: the release
	// predates the fix for a repository created after a state backup
	// (#277), so a repository the upgraded control plane created would be
	// unusable after the rollback. The rollout hold keeps the failed control
	// plane from backing up before a core update, but a scheduled backup
	// could still fall into the upgrade. TestUpgradeRollbackRepository
	// tests that case against a control plane with the fix.
	backupSite(t, u.Platform, site.ID)

	previousSchema := installedSchema(ctx, t, u.Project)
	latestSchema, err := store.LatestVersion(store.SQLite)
	if err != nil {
		t.Fatal(err)
	}
	// Without a migration since the release, the upgrade wouldn't migrate
	// the state database, and the rollback wouldn't restore it.
	migrate := previousSchema == latestSchema
	t.Logf("the state database is at schema version %d in %s and %d in this tree; adding a migration: %t",
		previousSchema, skew.MinUpgradeFrom, latestSchema, migrate)
	broken := u.brokenImage(image, migrate)

	// 2. Upgrade to a control plane that never becomes healthy, while a
	// visitor keeps requesting the site. While it runs, its rollouts are
	// held: the upgrade isn't confirmed. The site runs an older WordPress
	// than this tree's catalog, so the core rollout would update it at once.
	shared := sharedContainers(ctx, t, u.Project)
	traffic := e2e.ProbeTraffic(t, hc, siteURL)
	run := u.startUpgrade("--skip-preflight", "--control-plane-image", broken)
	u.waitApplying(run.done, broken)
	checkRollouts(t, u.Client, true)
	res := run.wait()
	checkTraffic(ctx, t, u.Project, traffic, shared, "the failed upgrade and its rollback")

	// 3. It rolled back on its own: exit code 2, and the state database
	// restored from the backup taken before the migration.
	if res.code != 2 {
		t.Fatalf("the upgrade to a control plane that never becomes healthy exited %d, want 2 (rolled back)", res.code)
	}
	for _, want := range []string{"failed and was rolled back", "The state database was restored from "} {
		if !strings.Contains(res.stdout, want) {
			t.Errorf("the rollback's output doesn't say %q", want)
		}
	}
	if status := upgradeStatus(t, u.dataDir); status != "rolled-back" {
		t.Errorf("the upgrade record's status is %q, want rolled-back", status)
	}

	// 4. The platform is back on the release, with its data, and the site
	// kept its WordPress release.
	u.checkBackOn(before.Version, previousImage)
	if resp, err := u.Client.GetClientGroupWithResponse(ctx, groupID); err != nil || resp.JSON200 == nil {
		t.Errorf("the client group is gone after the rollback: %v %v", err, resp)
	}
	if s := getSite(t, u.Platform, site.ID); s.Status != client.SiteStatusRunning {
		t.Errorf("after the rollback, the site is %s", s.Status)
	}
	if got := pageTitle(t, hc, siteURL); got != title {
		t.Errorf("after the rollback, the site serves %q, want %q", got, title)
	}
	if got := wordPressVersion(t, hc, siteURL); got != wordpress {
		t.Errorf("after the rollback, the site runs WordPress %s, want %s: the failed control plane's core rollout wasn't held", got, wordpress)
	}
	backupSite(t, u.Platform, site.ID)

	// 5. An upgrade interrupted with SIGTERM while it applies the release
	// rolls back too. The CLI has to run as a process of its own to get
	// the signal; it is interrupted once the new control plane has migrated
	// the state database and answers ready, waiting for its healthcheck.
	bin := buildPressfleet(ctx, t, "e2e", false)
	shared = sharedContainers(ctx, t, u.Project)
	traffic = e2e.ProbeTraffic(t, hc, siteURL)
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, bin, "upgrade", "--config", u.configPath, "--yes", "--skip-preflight",
		"--control-plane-image", broken)
	cmd.Env = releaseEnv(u.Project.Name)
	cmd.Stdin = strings.NewReader("")
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(exited)
	}()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		<-exited
	})
	u.waitApplying(exited, broken)
	if status := upgradeStatus(t, u.dataDir); status != "in-progress" {
		t.Errorf("before the interrupt, the upgrade record's status is %q, want in-progress", status)
	}
	t.Log("interrupting the upgrade with SIGTERM")
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case <-exited:
	case <-ctx.Done():
		t.Fatal("the interrupted upgrade didn't exit")
	}
	t.Logf("interrupted upgrade output:\n%s%s", stdout.String(), stderr.String())
	checkTraffic(ctx, t, u.Project, traffic, shared, "the interrupted upgrade and its rollback")
	if code := cmd.ProcessState.ExitCode(); code != 2 {
		t.Fatalf("the interrupted upgrade exited %d, want 2 (rolled back)", code)
	}
	if !strings.Contains(stderr.String(), "failed: interrupted") {
		t.Error("the interrupted upgrade doesn't say it was interrupted")
	}
	if !strings.Contains(stdout.String(), "failed and was rolled back") {
		t.Error("the interrupted upgrade doesn't say it was rolled back")
	}
	if status := upgradeStatus(t, u.dataDir); status != "rolled-back" {
		t.Errorf("after the interrupt, the upgrade record's status is %q, want rolled-back", status)
	}
	u.checkBackOn(before.Version, previousImage)
	if got := pageTitle(t, hc, siteURL); got != title {
		t.Errorf("after the interrupted upgrade, the site serves %q, want %q", got, title)
	}
}

// TestUpgradeRollbackRepository tests a client group whose first backup
// repository is created while an upgrade that fails is running (#277). The
// rollback restores the state database from before the repository was
// created, which drops its password but leaves its files. The control
// plane it rolls back to has to set those files aside and create a new
// repository, so the group's backups keep working. It installs this tree,
// since published releases up to v1.1.0 predate the fix.
func TestUpgradeRollbackRepository(t *testing.T) {
	image := e2e.Image(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Minute)
	defer cancel()
	u := newUpgradePlatform(ctx, t, "pfrbr-"+strings.ToLower(rand.Text())[:10], "upgrade-rollback-repository.test")
	u.installTree(image)
	from := controlPlaneInfo(ctx, t, u.Project).Version
	php := defaultPHP(t)
	u.ensureSiteImages(php)
	groupID := createGroup(t, u.Platform, "E2E Rollback Repository Agency")
	site := u.createSite(groupID, "E2E Rollback Repository", php)

	// The upgrade to a control plane that never becomes healthy runs for
	// about two minutes (the healthcheck's start period and three failed
	// probes, 30s apart): long enough to back the site up, which creates
	// the group's first repository.
	broken := u.brokenImage(image, true)
	run := u.startUpgrade("--skip-preflight", "--control-plane-image", broken)
	u.waitApplying(run.done, broken)
	t.Log("backing the site up while the upgrade is waiting for the control plane")
	backupSite(t, u.Platform, site.ID)
	res := run.wait()
	if res.code != 2 {
		t.Fatalf("the upgrade exited %d, want 2 (rolled back)", res.code)
	}
	if !strings.Contains(res.stdout, "The state database was restored from ") {
		t.Fatal("the rollback didn't restore the state database, so the repository's record wasn't dropped")
	}

	// The group backs up again, from a new repository, and the old one is
	// set aside, not deleted.
	u.checkBackOn(from, image)
	backupSite(t, u.Platform, site.ID)
	backupsDir := filepath.Dir(config.Layout{Root: u.dataDir}.BackupRepositoriesDir())
	out, err := u.Project.ExecService(ctx, e2e.Service, "ls", "-1", backupsDir)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "unopenable-"+groupID+"-") {
		t.Errorf("the repository created during the upgrade wasn't set aside; %s holds:\n%s", backupsDir, out)
	}
}

// TestUpgradeRollbackOnDemand installs this tree and upgrades it to a
// control plane that migrates the state database. `pressfleet upgrade
// --rollback` returns it to the release before, restoring the state
// database. After another upgrade, a site created since makes it refuse,
// unless --force is given. Last, a rollback that can't finish, because the
// previous control plane image is gone and can't be pulled, prints the
// manual steps, and running them verbatim recovers the platform.
func TestUpgradeRollbackOnDemand(t *testing.T) {
	image := e2e.Image(t)
	ctx, cancel := context.WithTimeout(t.Context(), 40*time.Minute)
	defer cancel()
	name := "pfrbd-" + strings.ToLower(rand.Text())[:10]
	u := newUpgradePlatform(ctx, t, name, "upgrade-rollback-on-demand.test")
	// The platform runs its own tag of the tested image, which the last
	// part removes.
	previous := "pressfleet-control-plane:" + name + "-previous"
	if err := compose.TagImage(ctx, nil, image, previous); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = compose.RemoveImage(context.Background(), nil, previous) })
	u.installTree(previous)
	from := controlPlaneInfo(ctx, t, u.Project).Version
	groupID := createGroup(t, u.Platform, "E2E Rollback On Demand Agency")
	next := u.migratingImage(image)

	// 1. Upgrade, record something, and roll back: the record is gone with
	// the restored state database.
	if res := u.upgrade("--skip-preflight", "--control-plane-image", next); res.code != 0 {
		t.Fatalf("the upgrade exited %d", res.code)
	}
	u.checkBackOn("", next)
	lost := createGroup(t, u.Platform, "E2E Created After The Upgrade")
	res := u.upgrade("--rollback")
	if res.code != 0 {
		t.Fatalf("upgrade --rollback exited %d, want 0", res.code)
	}
	for _, want := range []string{"was rolled back: the platform is back on " + from, "The state database was restored from "} {
		if !strings.Contains(res.stdout, want) {
			t.Errorf("the rollback's output doesn't say %q", want)
		}
	}
	if !strings.Contains(res.stdout, "client group E2E Created After The Upgrade") {
		t.Error("the rollback didn't list the client group created after the upgrade")
	}
	if status := upgradeStatus(t, u.dataDir); status != "rolled-back" {
		t.Errorf("the upgrade record's status is %q, want rolled-back", status)
	}
	u.checkBackOn(from, previous)
	if resp, err := u.Client.GetClientGroupWithResponse(ctx, groupID); err != nil || resp.JSON200 == nil {
		t.Errorf("the client group from before the upgrade is gone after the rollback: %v %v", err, resp)
	}
	if resp, err := u.Client.GetClientGroupWithResponse(ctx, lost); err != nil || resp.StatusCode() != http.StatusNotFound {
		t.Errorf("the client group created after the upgrade is still there after the rollback: %v %v", err, resp)
	}
	// Only once.
	if res := u.upgrade("--rollback"); res.code != 1 || !strings.Contains(res.stderr, "rolled back already") {
		t.Errorf("a second upgrade --rollback exited %d: %s", res.code, res.stderr)
	}

	// 2. Upgrade again and create a site: the rollback would orphan it, so
	// it refuses without --force, and changes nothing.
	if res := u.upgrade("--skip-preflight", "--control-plane-image", next); res.code != 0 {
		t.Fatalf("the second upgrade exited %d", res.code)
	}
	u.checkBackOn("", next)
	php := defaultPHP(t)
	u.ensureSiteImages(php)
	site := u.createSite(groupID, "E2E Created After The Upgrade", php)
	res = u.upgrade("--rollback")
	if res.code != 1 {
		t.Fatalf("upgrade --rollback with a site created since the upgrade exited %d, want 1 (refused)", res.code)
	}
	if !strings.Contains(res.stderr, "would leave behind") || !strings.Contains(res.stderr, "was created after the upgrade") {
		t.Errorf("the refusal doesn't name the site created after the upgrade: %s", res.stderr)
	}
	if status := upgradeStatus(t, u.dataDir); status != "succeeded" {
		t.Errorf("after the refusal, the upgrade record's status is %q, want succeeded", status)
	}
	if c := u.controlPlaneImage(); c != next {
		t.Errorf("after the refusal, the control plane runs %s, want %s", c, next)
	}
	// With --force it rolls back, leaving the site's containers behind
	// with no record (the cleanup newUpgradePlatform registered removes
	// them).
	res = u.upgrade("--rollback", "--force")
	if res.code != 0 {
		t.Fatalf("upgrade --rollback --force exited %d, want 0", res.code)
	}
	if !strings.Contains(res.stdout, "rolling back anyway (--force)") {
		t.Error("the forced rollback doesn't warn about what it leaves behind")
	}
	u.checkBackOn(from, previous)
	if resp, err := u.Client.GetSiteWithResponse(ctx, site.ID); err != nil || resp.StatusCode() != http.StatusNotFound {
		t.Errorf("the site created after the upgrade still has a record after the forced rollback: %v %v", err, resp)
	}

	// 3. A rollback that can't finish: the previous control plane image is
	// removed while the upgrade runs, and can't be pulled (it was never
	// pushed anywhere), so restoring the state database with it fails.
	broken := u.brokenImage(image, true)
	run := u.startUpgrade("--skip-preflight", "--control-plane-image", broken)
	u.waitApplying(run.done, broken)
	if err := compose.RemoveImage(ctx, nil, previous); err != nil {
		t.Fatal(err)
	}
	res = run.wait()
	if res.code != 1 {
		t.Fatalf("the upgrade whose rollback can't finish exited %d, want 1", res.code)
	}
	if !strings.Contains(res.stderr, "The rollback stopped at step 2") {
		t.Errorf("the output doesn't say the rollback stopped at the state restore")
	}
	if status := upgradeStatus(t, u.dataDir); status != "rollback-failed" {
		t.Errorf("the upgrade record's status is %q, want rollback-failed", status)
	}
	commands := recoveryCommands(res.stderr)
	restore := slices.IndexFunc(commands, func(c string) bool { return strings.Contains(c, " state restore ") })
	if restore < 0 {
		t.Fatalf("the manual steps of the stopped rollback don't restore the state database: %q", commands)
	}

	// The operator makes the image available again, and runs the steps
	// as printed.
	if err := compose.TagImage(ctx, nil, image, previous); err != nil {
		t.Fatal(err)
	}
	if last := commands[len(commands)-1]; !strings.HasPrefix(last, "pressfleet upgrade --rollback") {
		t.Errorf("the manual steps don't end by marking the upgrade rolled back: %q", last)
	}
	for i, command := range commands {
		out, err := runRecoveryCommand(ctx, u.Project.Name, command)
		t.Logf("$ %s\n%s", command, out)
		if err != nil {
			t.Fatalf("recovery command %d failed: %v", i+1, err)
		}
		if i == restore {
			checkStateRestore(t, command, string(out))
		}
	}
	// The last step found the platform back on the release and marked the
	// upgrade rolled back, so the control plane runs its rollouts again
	// (#310).
	if status := upgradeStatus(t, u.dataDir); status != "rolled-back" {
		t.Errorf("after the manual steps, the upgrade record's status is %q, want rolled-back", status)
	}
	u.checkBackOn(from, previous)
	checkRollouts(t, u.Client, false)
	if resp, err := u.Client.GetClientGroupWithResponse(ctx, groupID); err != nil || resp.JSON200 == nil {
		t.Errorf("the client group is gone after the manual recovery: %v %v", err, resp)
	}
}

// upgradePlatform is a platform an upgrade test installs, upgrades, and
// rolls back.
type upgradePlatform struct {
	*e2e.Platform
	t          *testing.T
	ctx        context.Context //nolint:containedctx // the test's, for its helpers
	domain     string
	dataDir    string
	configPath string
	email      string
	password   redact.Secret
	// sites are the projects of the sites created, which a cleanup removes
	// if they are left: a forced rollback leaves a site with no record.
	sites []compose.Project
}

// newUpgradePlatform prepares a platform under the Compose project name,
// with test domains under domain, and its teardown.
func newUpgradePlatform(ctx context.Context, t *testing.T, name, domain string) *upgradePlatform {
	t.Helper()
	root := t.TempDir()
	u := &upgradePlatform{
		t:          t,
		ctx:        ctx,
		domain:     domain,
		dataDir:    filepath.Join(root, "data"),
		configPath: filepath.Join(root, "etc", "pressfleet.yaml"),
		email:      "admin@" + domain,
		password:   redact.Secret(strings.ToLower(rand.Text())),
	}
	// Registered before Adopt, so it runs after the platform teardown: see
	// TestSiteCreateDelete.
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		for _, sp := range u.sites {
			if res, err := sp.Resources(ctx); err == nil && !res.Empty() {
				t.Logf("removing leftover site project %s", sp.Name)
				if err := sp.Down(ctx, compose.DownOptions{Volumes: true}); err != nil {
					t.Errorf("removing leftover site project %s: %v", sp.Name, err)
				}
			}
		}
	})
	u.Platform = e2e.Adopt(t, compose.Project{Name: name, Dir: filepath.Join(u.dataDir, "platform")}, u.dataDir)
	return u
}

// installRelease installs a published release with its own CLI, which
// installs its own control plane image, and logs in.
func (u *upgradePlatform) installRelease(releaseCLI, version string) {
	t := u.t
	t.Helper()
	e2e.WriteReleaseOverride(t, u.Project.Dir)
	t.Logf("installing %s with %s", version, releaseCLI)
	install := exec.CommandContext(u.ctx, releaseCLI, "init", "--config", u.configPath,
		"--non-interactive", "--confirm-key-stored", "--skip-preflight",
		"--dashboard-domain", "fleet."+u.domain, "--preview-base-domain", "preview."+u.domain,
		"--data-dir", u.dataDir, "--admin-email", u.email, "--admin-password-stdin",
		"--acme-ca-server", e2e.ACMECAServer,
	)
	install.Env = releaseEnv(u.Project.Name)
	install.Stdin = strings.NewReader(u.password.Reveal() + "\n")
	var stdout, stderr bytes.Buffer
	install.Stdout, install.Stderr = &stdout, &stderr
	err := install.Run()
	t.Logf("%s init output:\n%s%s", version,
		strings.ReplaceAll(stdout.String(), keyLine(stdout.String()), "    pfmk1:<redacted>"), stderr.String())
	if err != nil {
		t.Fatalf("%s init: %v", version, err)
	}
	u.Connect(u.ctx, t)
	u.Client = u.Login(t, u.email, u.password)
}

// installTree installs the platform with this tree's CLI and the control
// plane image, and logs in.
func (u *upgradePlatform) installTree(image string) {
	t := u.t
	t.Helper()
	e2e.WriteOverride(t, u.Project.Dir)
	res := u.run(u.password.Reveal()+"\n", "init",
		"--non-interactive", "--confirm-key-stored", "--skip-preflight",
		"--dashboard-domain", "fleet."+u.domain, "--preview-base-domain", "preview."+u.domain,
		"--data-dir", u.dataDir, "--admin-email", u.email, "--admin-password-stdin",
		"--acme-ca-server", e2e.ACMECAServer, "--control-plane-image", image,
	)
	if res.code != 0 {
		t.Fatalf("init exited %d", res.code)
	}
	u.Connect(u.ctx, t)
	u.Client = u.Login(t, u.email, u.password)
}

// upgradeResult is how a run of this tree's CLI ended.
type upgradeResult struct {
	code           int
	stdout, stderr string
}

// run runs this tree's CLI in the test's process.
func (u *upgradePlatform) run(stdin string, args ...string) upgradeResult {
	var stdout, stderr bytes.Buffer
	code := cli.Run(u.ctx, append(args[:1:1], append([]string{"--config", u.configPath}, args[1:]...)...), cli.Options{
		Version:             "e2e",
		Stdin:               strings.NewReader(stdin),
		Stdout:              &stdout,
		Stderr:              &stderr,
		Env:                 func(string) (string, bool) { return "", false },
		PlatformProjectName: u.Project.Name,
	})
	out := strings.ReplaceAll(stdout.String(), keyLine(stdout.String()), "    pfmk1:<redacted>")
	u.t.Logf("pressfleet %s exited %d:\n%s%s", strings.Join(args, " "), code, out, stderr.String())
	return upgradeResult{code: code, stdout: out, stderr: stderr.String()}
}

// upgrade runs `pressfleet upgrade --yes` with args.
func (u *upgradePlatform) upgrade(args ...string) upgradeResult {
	return u.run("", append([]string{"upgrade", "--yes"}, args...)...)
}

// upgradeRun is an upgrade running in the background.
type upgradeRun struct {
	done chan struct{}
	res  upgradeResult
}

// wait waits for the upgrade to end.
func (r *upgradeRun) wait() upgradeResult {
	<-r.done
	return r.res
}

// startUpgrade starts `pressfleet upgrade --yes` with args in the
// background, so the test can look at the platform while it runs. The
// test waits for it to end before the platform is torn down.
func (u *upgradePlatform) startUpgrade(args ...string) *upgradeRun {
	r := &upgradeRun{done: make(chan struct{})}
	go func() {
		defer close(r.done)
		r.res = u.upgrade(args...)
	}()
	// A rollback, once started, runs on a context the test's doesn't
	// cancel; this runs before the teardown (cleanups run last first).
	u.t.Cleanup(func() { <-r.done })
	return r
}

// waitApplying waits until an upgrade, which ends by closing done, has
// started the control plane image and it answers ready, having migrated
// the state database, then logs in to it. The upgrade is still waiting for
// its healthcheck then.
func (u *upgradePlatform) waitApplying(done <-chan struct{}, image string) {
	t := u.t
	t.Helper()
	deadline := time.Now().Add(5 * time.Minute)
	for {
		// Connect needs Traefik's ports too, and an upgrade from an older
		// release may recreate Traefik.
		c, err := u.Project.ServiceContainer(u.ctx, e2e.Service)
		if err == nil && c.Running && c.Image == image {
			var traefik compose.ContainerDetails
			if traefik, err = u.Project.ServiceContainer(u.ctx, ingress.TraefikService); err == nil && traefik.Running {
				break
			}
		}
		select {
		case <-done:
			t.Fatalf("the upgrade ended before the control plane ran %s", image)
		case <-time.After(time.Second):
		}
		if time.Now().After(deadline) {
			t.Fatalf("the control plane doesn't run %s: %v", image, err)
		}
	}
	ctx, cancel := context.WithTimeout(u.ctx, 90*time.Second)
	defer cancel()
	u.Connect(ctx, t)
	u.Client = u.Login(t, u.email, u.password)
	t.Logf("the control plane runs %s and is ready", image)
}

// checkBackOn checks that the control plane runs image, reports ready at
// version (any if empty), and lets the admin log in.
func (u *upgradePlatform) checkBackOn(version, image string) {
	t := u.t
	t.Helper()
	u.Connect(u.ctx, t)
	info := controlPlaneInfo(u.ctx, t, u.Project)
	if !info.Ready || (version != "" && info.Version != version) {
		t.Errorf("the control plane runs %s (ready: %t), want %s", info.Version, info.Ready, version)
	}
	if got := u.controlPlaneImage(); got != image {
		t.Errorf("the control plane runs the image %s, want %s", got, image)
	}
	u.Client = u.Login(t, u.email, u.password)
}

// controlPlaneImage returns the image the control plane's container runs.
func (u *upgradePlatform) controlPlaneImage() string {
	u.t.Helper()
	c, err := u.Project.ServiceContainer(u.ctx, e2e.Service)
	if err != nil {
		u.t.Fatal(err)
	}
	return c.Image
}

// createSite creates a site, waits until it runs, and returns it.
func (u *upgradePlatform) createSite(groupID, name, php string) client.Site {
	t := u.t
	t.Helper()
	created := createSite(t, u.Platform, groupID, name, php)
	u.sites = append(u.sites, compose.Project{Name: compose.SiteProjectName(created.Site.ID), Dir: t.TempDir()})
	waitJob(t, u.Platform, created.Job.ID)
	return getSite(t, u.Platform, created.Site.ID)
}

// defaultPHP returns the PHP version sites are created with.
func defaultPHP(t *testing.T) string {
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

// ensureSiteImages pulls the site images the control plane and this tree's
// may run, before a site is created: a control plane has no registry
// credentials and the site images are private, so it can only use what is
// already on the host. They are the images of the newest WordPress release
// the running control plane has images for (its core rollout lists them,
// newest first, in v0.1.0 already) and of this tree's newest, which an
// upgraded control plane rolls out.
func (u *upgradePlatform) ensureSiteImages(php string) {
	t := u.t
	t.Helper()
	cat, err := catalog.Load()
	if err != nil {
		t.Fatal(err)
	}
	rollout, err := u.Client.GetCoreRolloutWithResponse(u.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rollout.JSON200 == nil || len(rollout.JSON200.Releases) == 0 {
		t.Fatalf("core rollout: status %d: %s", rollout.StatusCode(), rollout.Body)
	}
	var wanted []string
	for _, wp := range []string{rollout.JSON200.Releases[0].Version, cat.LatestWordPress().Version} {
		// Images doesn't require the version to be in this tree's catalog.
		images, err := cat.Images(php, wp)
		if err != nil {
			t.Fatal(err)
		}
		for _, image := range []string{images.PHP, images.Nginx} {
			if !slices.Contains(wanted, image) {
				wanted = append(wanted, image)
			}
		}
	}
	t.Logf("the control plane provisions WordPress %s; ensuring %s", rollout.JSON200.Releases[0].Version, wanted)
	e2e.EnsureImages(t, wanted...)
}

// migratingImage builds a control plane image from image whose binary is
// this tree's, built with the e2emigration tag: it migrates the state
// database from this tree's schema. It reports the version e2e-migration.
func (u *upgradePlatform) migratingImage(image string) string {
	t := u.t
	t.Helper()
	tag := "pressfleet-control-plane:" + u.Project.Name + "-migration"
	bin := buildPressfleet(u.ctx, t, "e2e-migration", true)
	dir := filepath.Dir(bin)
	dockerfile := fmt.Sprintf("FROM %s\nCOPY %s /usr/local/bin/pressfleet\n", image, filepath.Base(bin))
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(dockerfile), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := compose.BuildImageDir(u.ctx, nil, tag, dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = compose.RemoveImage(context.Background(), nil, tag) })
	return tag
}

// brokenImage builds a control plane image whose healthcheck always fails
// (brokenControlPlane), from image, or with migrate from migratingImage.
func (u *upgradePlatform) brokenImage(image string, migrate bool) string {
	t := u.t
	t.Helper()
	if migrate {
		image = u.migratingImage(image)
	}
	tag := "pressfleet-control-plane:" + u.Project.Name + "-broken"
	if err := compose.BuildImage(u.ctx, nil, tag, fmt.Sprintf(brokenControlPlane, image)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = compose.RemoveImage(context.Background(), nil, tag) })
	return tag
}

// buildPressfleet builds this tree's pressfleet binary, reporting version,
// into a new directory, and returns its path: for the host (the CLI), or
// with migration for the control plane image, with the e2emigration tag.
func buildPressfleet(ctx context.Context, t *testing.T, version string, migration bool) string {
	t.Helper()
	return goBuildPressfleet(ctx, t, version, migration, migration)
}

// goBuildPressfleet builds this tree's pressfleet binary, reporting
// version, into a new directory, and returns its path: for the control
// plane image with forImage, else for the host, and with migration, with
// the e2emigration tag.
func goBuildPressfleet(ctx context.Context, t *testing.T, version string, forImage, migration bool) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "pressfleet")
	args := []string{"build", "-trimpath", "-ldflags", "-s -w -X main.version=" + version, "-o", out}
	env := os.Environ()
	if migration {
		args = append(args, "-tags", "e2emigration")
	}
	if forImage {
		// As the control plane image's Dockerfile builds it.
		env = append(env, "CGO_ENABLED=0", "GOOS=linux", "GOARCH=amd64")
	}
	cmd := exec.CommandContext(ctx, "go", append(args, "github.com/rchouinard/pressfleet/control-plane/cmd/pressfleet")...)
	cmd.Env = env
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building pressfleet: %v\n%s", err, b)
	}
	return out
}

// checkRollouts checks that the control plane holds its core, image, and
// template rollouts, if held, because an upgrade to it isn't confirmed
// (#247), or else that it holds none of them.
func checkRollouts(t *testing.T, c *client.ClientWithResponses, held bool) {
	t.Helper()
	core, err := c.GetCoreRolloutWithResponse(t.Context())
	if err != nil || core.JSON200 == nil {
		t.Fatalf("core rollout: %v %v", err, core)
	}
	images, err := c.GetImageRolloutWithResponse(t.Context())
	if err != nil || images.JSON200 == nil {
		t.Fatalf("image rollout: %v %v", err, images)
	}
	templates, err := c.GetTemplateRolloutWithResponse(t.Context())
	if err != nil || templates.JSON200 == nil {
		t.Fatalf("template rollout: %v %v", err, templates)
	}
	for _, r := range []struct {
		name   string
		held   bool
		reason *string
	}{
		{"core", core.JSON200.Held, core.JSON200.HoldReason},
		{"image", images.JSON200.Held, images.JSON200.HoldReason},
		{"template", templates.JSON200.Held, templates.JSON200.HoldReason},
	} {
		switch {
		case held && (!r.held || r.reason == nil):
			t.Errorf("while the upgrade is unconfirmed, the %s rollout isn't held (held: %t)", r.name, r.held)
		case held:
			t.Logf("the %s rollout is held: %s", r.name, *r.reason)
		case r.held:
			reason := ""
			if r.reason != nil {
				reason = *r.reason
			}
			t.Errorf("after the upgrade was rolled back, the %s rollout is still held: %s", r.name, reason)
		}
	}
}

// upgradeStatus returns the status in the upgrade record.
func upgradeStatus(t *testing.T, dataDir string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dataDir, "upgrade", "upgrade.json"))
	if err != nil {
		t.Fatal(err)
	}
	var record struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatal(err)
	}
	return record.Status
}

// generator finds the WordPress release in a page's generator tag.
var generator = regexp.MustCompile(`<meta name="generator" content="WordPress ([^"]+)"`)

// wordPressVersion gets a page through Traefik and returns the WordPress
// release it says it was generated by.
func wordPressVersion(t *testing.T, hc *http.Client, url string) string {
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
	m := generator.FindSubmatch(body)
	if resp.StatusCode != http.StatusOK || m == nil {
		t.Fatalf("GET %s: %s, no generator tag: %.500s", url, resp.Status, body)
	}
	return string(m[1])
}
