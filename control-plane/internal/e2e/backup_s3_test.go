package e2e_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/catalog"
	"github.com/rchouinard/pressfleet/control-plane/internal/config"
	"github.com/rchouinard/pressfleet/control-plane/internal/e2e"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner/compose"
	"github.com/rchouinard/pressfleet/control-plane/internal/redact"
)

// s3Image is the self-hosted S3 service of TestSiteBackupS3. MinIO no
// longer publishes images; RustFS is a drop-in replacement. Keep in sync
// with backup/repository's S3 test.
const s3Image = "rustfs/rustfs:1.0.0@sha256:8cc9801755448b71a786705ce76692c77e14936cccd87cf2fc31842e58f4d1ff"

const (
	s3Service   = "pf-e2e-s3"
	s3Bucket    = "pf-e2e-backups"
	s3AccessKey = "pfe2e-access"
	s3SecretKey = "pfe2e-secret-key-0123456789"
)

// startBackupS3 runs an S3-compatible service the way docs/configuration.md
// ("Self-hosted S3") describes: in a Compose project of its own, attached to
// the platform's backup network as an external network, with no published
// ports. It is removed before the platform, whose network it is on.
func startBackupS3(t *testing.T, p *e2e.Platform) {
	t.Helper()
	dir := t.TempDir()
	file := fmt.Sprintf(`services:
  %s:
    image: %q
    environment:
      RUSTFS_ACCESS_KEY: %q
      RUSTFS_SECRET_KEY: %q
    networks:
      - backup
networks:
  backup:
    name: %q
    external: true
`, s3Service, s3Image, s3AccessKey, s3SecretKey, p.BackupNetwork())
	if err := os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte(file), 0o600); err != nil {
		t.Fatal(err)
	}
	proj := compose.Project{Name: p.Project.Name + "-s3", Dir: dir}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if err := proj.Down(ctx, compose.DownOptions{Volumes: true}); err != nil {
			t.Errorf("removing the S3 service: %v", err)
		}
	})
	e2e.EnsureImages(t, s3Image)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	if err := proj.Up(ctx, compose.UpOptions{Wait: true, WaitTimeout: 2 * time.Minute}); err != nil {
		t.Fatalf("starting the S3 service: %v", err)
	}
}

// TestSiteBackupS3 checks backups, restores, and repository maintenance
// with the s3 backend (#54, #63) and an S3 service on the platform's backup
// network, reached by its container name: the control plane creates,
// prunes, and checks the repository, and the backup and restore tasks,
// which run on the site's network, reach it through the backup network.
// The restores are restoreChecks, the same as TestSiteRestore's with the
// local backend, and the snapshot they restore is exported too, as
// TestSiteExport exports one with the local backend.
func TestSiteBackupS3(t *testing.T) {
	projectDir := t.TempDir()
	var siteProject compose.Project
	// Registered before Start, so it runs after the platform teardown: see
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
	p := e2e.Start(t)
	ctx := t.Context()
	startBackupS3(t, p)
	endpoint := "http://" + s3Service + ":9000"
	p.Restart(t, map[string]string{
		"PRESSFLEET_BACKUP_BACKEND":              config.BackupBackendS3,
		"PRESSFLEET_BACKUP_S3_ENDPOINT":          endpoint,
		"PRESSFLEET_BACKUP_S3_BUCKET":            s3Bucket,
		"PRESSFLEET_BACKUP_S3_ACCESS_KEY_ID":     s3AccessKey,
		"PRESSFLEET_BACKUP_S3_SECRET_ACCESS_KEY": p.SealSetting(t, config.BackupS3SecretAccessKeySetting, redact.Secret(s3SecretKey)),
	})

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

	groupID := createGroup(t, p, "E2E S3 Agency")
	created := createSite(t, p, groupID, "E2E S3", php)
	site := created.Site
	siteProject = compose.Project{Name: compose.SiteProjectName(site.ID), Dir: projectDir}
	waitJob(t, p, created.Job.ID)

	// The control plane creates the repository in the bucket (restic
	// creates the bucket); the S3 service may still be starting.
	deadline := time.Now().Add(2 * time.Minute)
	for {
		out, err := p.Project.Exec(ctx, e2e.Service, "pressfleet", "repository", "create", groupID)
		if err == nil {
			if want := "s3:" + endpoint + "/" + s3Bucket + "/" + groupID; !strings.Contains(string(out), want) {
				t.Errorf("repository create printed %s, want %s", out, want)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("repository create: %v: %s", err, out)
		}
		time.Sleep(3 * time.Second)
	}

	// The restores of the M3 acceptance criteria, as TestSiteRestore runs
	// them with the local backend: here the backups, the pre-restore
	// backups, and the rollback's restore all go through the bucket.
	rc := restoreChecks{p: p, site: site, proj: siteProject}
	snapshot := rc.setUp(t)
	t.Run("restore", func(t *testing.T) { rc.sameSite(t, snapshot) })
	t.Run("rollback", rc.rollback)
	// Site export from a snapshot (#134), with the snapshot in the bucket.
	t.Run("snapshot export", func(t *testing.T) { snapshotExportChecks(t, p, site.ID, snapshot) })

	t.Run("prune and check", func(t *testing.T) {
		for _, command := range []string{"prune", "check"} {
			out, err := p.Project.Exec(ctx, e2e.Service, "pressfleet", "repository", command, groupID)
			if err != nil {
				t.Fatalf("repository %s: %v: %s", command, err, out)
			}
			m := regexp.MustCompile(`job (\S+) for`).FindStringSubmatch(string(out))
			if m == nil {
				t.Fatalf("repository %s printed no job: %s", command, out)
			}
			waitJob(t, p, m[1])
		}
	})
}
