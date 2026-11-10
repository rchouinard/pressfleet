package e2e_test

import (
	"context"
	"crypto/rand"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/rchouinard/pressfleet/control-plane/internal/e2e"
)

// TestKubernetesDisasterRecovery runs the drill of docs/disaster-recovery.md
// on Kubernetes (design doc §5.10; #224's carry-overs in #317). A platform
// gets a site with a backup, and a state backup and a repository export
// through kubectl exec. Then:
//
//   - On the running install, with the control plane scaled to zero, the
//     chart's recovery values restore the state backup over the state
//     database (recovery.force), and the control plane recreates the site
//     from its backup once recovery is off again.
//   - The cluster is lost: the release, the namespaces, the
//     PersistentVolumes but the data volume's, and the state database. The
//     data volume keeps only the backups (local repositories), as a
//     ReadWriteMany volume restored from them would. A fresh install with
//     recovery.enabled on that volume (controlPlane.data.existingClaim)
//     restores the state into the new, empty database with the control
//     plane at zero, and turning it off starts the control plane, which
//     recreates the site.
func TestKubernetesDisasterRecovery(t *testing.T) {
	p := e2e.StartKubernetes(t, e2e.KubeOptions{})
	k := p.Kube
	ctx := t.Context()

	groupID := createGroup(t, p, "E2E Kubernetes Recovery Agency")
	site, one := k8sSite(t, p, groupID, "E2E Kubernetes Recovery")
	siteWP(t, one, "option", "update", "pf_restore_marker", "before")
	backupSite(t, p, site.ID)
	first := k8sStateBackup(ctx, t, p)

	t.Run("repository export", func(t *testing.T) {
		dir := e2e.KubeDataDir + "/tmp/export-" + strings.ToLower(rand.Text())[:8]
		out, err := p.ExecControlPlane(ctx, nil, "pressfleet", "repository", "export", groupID, dir)
		if err != nil {
			t.Fatalf("repository export: %v: %s", err, out)
		}
		t.Logf("%s", strings.TrimSpace(string(out)))
		repo, password := exportLine(t, out, "Repository"), exportLine(t, out, "Password file")
		// Export checked that restic opens the copy with the password file.
		if out, err := p.ExecControlPlane(ctx, nil, "sh", "-c", `test -s "$1/config" && test -s "$2"`, "sh", repo, password); err != nil {
			t.Errorf("the export has no repository config or password file: %v: %s", err, out)
		}
		if out, err := p.ExecControlPlane(ctx, nil, "rm", "-rf", dir); err != nil {
			t.Errorf("removing the export: %v: %s", err, out)
		}
	})

	hc := p.IngressClient(t)
	preview := "https://" + site.PreviewDomain + "/"
	checkSite := func(t *testing.T) {
		t.Helper()
		waitRecovered(ctx, t, p, site.ID)
		// Both the database and WordPress: over a running install, the
		// object cache held the option from before the recovery until
		// recovery flushed it (#341).
		got := siteWP(t, k.Site(site.ID), "eval",
			`global $wpdb; echo $wpdb->get_var("SELECT option_value FROM {$wpdb->options} WHERE option_name = 'pf_restore_marker'");`)
		if got != "before" {
			t.Errorf("pf_restore_marker = %q in the database, want the backup's", got)
		}
		if got := siteWP(t, k.Site(site.ID), "option", "get", "pf_restore_marker"); got != "before" {
			t.Errorf("pf_restore_marker = %q through WordPress, want the backup's (the object cache wasn't flushed?)", got)
		}
		_, body := waitIngress(t, p.IngressClient(t), preview, nil, http.StatusOK)
		if !strings.Contains(body, "<title>E2E Kubernetes Recovery") {
			t.Errorf("home page:\n%.300s", body)
		}
	}

	t.Run("running install", func(t *testing.T) {
		// Recorded after the state backup: the restore loses both.
		later := createGroup(t, p, "E2E Kubernetes Recovery Later")
		siteWP(t, one, "option", "update", "pf_restore_marker", "after")
		waitIngress(t, hc, preview, nil, http.StatusOK)

		k.ScaleDownControlPlane(ctx, t)
		k.InstallChart(ctx, t, "upgrade", map[string]any{"recovery": map[string]any{"enabled": true, "force": true}})
		checkStateRecoverJob(t, k, first)
		if got := k.MustKubectl(t, "get", "deployment", "control-plane", "-n", k.Namespace, "-o", "jsonpath={.spec.replicas}"); got != "0" {
			t.Errorf("with recovery on, the control plane has %s replicas, want 0", got)
		}
		k.InstallChart(ctx, t, "upgrade", nil)
		p.Connect(ctx, t)
		p.Client = p.Login(t, p.AdminEmail, p.AdminPassword)
		if groupExists(t, p, later) {
			t.Error("the client group created after the state backup survived restoring it")
		}
		if !groupExists(t, p, groupID) {
			t.Error("the client group is gone after the restore")
		}
		checkSite(t)
	})

	t.Run("lost cluster", func(t *testing.T) {
		// What survives: the data volume, with only the backups in it (and
		// the test's image manifest), the master key and init's values and
		// Secrets (in k.ValuesDir), and the PostgreSQL server.
		if out, err := p.ExecControlPlane(ctx, nil, "find", e2e.KubeDataDir, "-mindepth", "1", "-maxdepth", "1",
			"!", "-name", "backups", "!", "-name", "e2e", "-exec", "rm", "-rf", "{}", "+"); err != nil {
			t.Fatalf("emptying the data volume but its backups: %v: %s", err, out)
		}
		pv := k.MustKubectl(t, "get", "pvc", "control-plane-data", "-n", k.Namespace, "-o", "jsonpath={.spec.volumeName}")
		if out, err := k.Helm(ctx, "uninstall", "pressfleet", "--namespace", k.Namespace, "--wait", "--timeout", "5m"); err != nil {
			t.Fatalf("helm uninstall: %v\n%s", err, out)
		}
		k.MustKubectl(t, "delete", "namespace", k.SiteNamespace(site.ID), k.Namespace, "--wait=true", "--timeout=5m")
		k.MustKubectl(t, "delete", "pv", "-l", "pressfleet.io/data-clone="+k.Namespace, "--wait=true", "--timeout=2m")
		k.EmptyDatabase(ctx, t)

		// The new cluster: the data volume's PersistentVolume is bound to a
		// claim of the new install's, which the chart is told to use.
		k.MustKubectl(t, "patch", "pv", pv, "--type=json", `-p=[{"op":"remove","path":"/spec/claimRef"}]`)
		k.CreateNamespace(ctx, t)
		const claim = "restored-data"
		pvc := `{"apiVersion":"v1","kind":"PersistentVolumeClaim","metadata":{"name":"` + claim + `"},` +
			`"spec":{"accessModes":["ReadWriteMany"],"storageClassName":"nfs","volumeName":"` + pv + `",` +
			`"resources":{"requests":{"storage":"2Gi"}}}}`
		if out, err := k.Kubectl(ctx, strings.NewReader(pvc), "apply", "-n", k.Namespace, "-f", "-"); err != nil {
			t.Fatalf("creating the claim of the restored data volume: %v: %s", err, out)
		}
		existing := map[string]any{"controlPlane": map[string]any{"data": map[string]any{"existingClaim": claim}}}
		k.InstallChart(ctx, t, "install", map[string]any{
			"controlPlane": existing["controlPlane"],
			"recovery":     map[string]any{"enabled": true},
		})
		checkStateRecoverJob(t, k, first)
		// The harness's manifest of the tree's site images survived on the
		// volume; the new Deployment has to be pointed at it again before
		// the control plane starts and recreates the site.
		k.SetEnv(ctx, t, e2e.KubeTreeImagesEnv())
		k.InstallChart(ctx, t, "upgrade", existing)
		p.Connect(ctx, t)
		// The restored admin, with the old password.
		p.Client = p.Login(t, p.AdminEmail, p.AdminPassword)
		if !groupExists(t, p, groupID) {
			t.Error("the client group is gone after recovery")
		}
		checkSite(t)

		// State backups go on, into the same repository.
		k8sStateBackup(ctx, t, p)
		out, err := p.ExecControlPlane(ctx, nil, "pressfleet", "state", "backups", "--json")
		if err != nil || !strings.Contains(string(out), first) {
			t.Errorf("state backups after recovery don't include %s: %v: %s", first, err, out)
		}
	})
}

// stateBackupLine finds the snapshot in `pressfleet state backup`'s output.
var stateBackupLine = regexp.MustCompile(`as snapshot (\S+)`)

// k8sStateBackup takes a state backup with `kubectl exec ... pressfleet
// state backup`, as docs/disaster-recovery.md says, and returns its
// snapshot ID.
func k8sStateBackup(ctx context.Context, t *testing.T, p *e2e.Platform) string {
	t.Helper()
	out, err := p.ExecControlPlane(ctx, nil, "pressfleet", "state", "backup")
	if err != nil {
		t.Fatalf("state backup: %v: %s", err, out)
	}
	m := stateBackupLine.FindSubmatch(out)
	if m == nil {
		t.Fatalf("state backup printed no snapshot: %s", out)
	}
	t.Logf("%s", strings.TrimSpace(string(out)))
	return string(m[1])
}

// checkStateRecoverJob checks the log of the chart's state-recover Job:
// it restored the snapshot, and the site will be recreated.
func checkStateRecoverJob(t *testing.T, k *e2e.KubePlatform, snapshot string) {
	t.Helper()
	logs := k.MustKubectl(t, "logs", "-n", k.Namespace, "job/state-recover")
	t.Logf("state-recover:\n%s", logs)
	if !strings.Contains(logs, "Restoring state backup "+snapshot[:min(8, len(snapshot))]) {
		t.Errorf("state-recover didn't restore snapshot %s", snapshot)
	}
	if !strings.Contains(logs, "1 sites will be recovered") {
		t.Error("state-recover doesn't say the site will be recovered")
	}
}

// exportLine returns the value of a "Name: value" line of `pressfleet
// repository export`'s output.
func exportLine(t *testing.T, out []byte, name string) string {
	t.Helper()
	for line := range strings.Lines(string(out)) {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), name+":"); ok {
			return strings.TrimSpace(v)
		}
	}
	t.Fatalf("repository export printed no %s: %s", name, out)
	return ""
}
