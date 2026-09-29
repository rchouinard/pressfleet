package e2e_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"

	"github.com/rchouinard/pressfleet/control-plane/internal/api/client"
	"github.com/rchouinard/pressfleet/control-plane/internal/catalog"
	"github.com/rchouinard/pressfleet/control-plane/internal/e2e"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner/compose"
	"github.com/rchouinard/pressfleet/control-plane/internal/redact"
)

// TestSFTPGateway checks the SFTP gateway (#62, design doc §5.8) on a real
// platform with two sites:
//
//   - an Operator registers a key through the API and reaches the first
//     site's wp-content over SFTP, as "/": WordPress's directories are
//     there, and the worker image's own files and the host's are not;
//   - a file uploaded over SFTP is in the site's php container, owned by
//     the site's UID, and a file the site writes can be downloaded;
//   - the other site's files are out of reach, by name or with "..";
//   - a user without a registered key, a Viewer with one, a user
//     connecting to a site that doesn't exist, and a removed key can't
//     connect; removing the key also ends the open session;
//   - sessions are in the audit log, with the key's fingerprint and the
//     bytes transferred;
//   - no session worker container is left behind.
func TestSFTPGateway(t *testing.T) {
	var projects []compose.Project
	// Registered before Start, so it runs after the platform teardown: see
	// TestSiteCreateDelete.
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		for _, project := range projects {
			if res, err := project.Resources(ctx); err == nil && !res.Empty() {
				t.Logf("removing leftover site project %s", project.Name)
				if err := project.Down(ctx, compose.DownOptions{Volumes: true}); err != nil {
					t.Errorf("removing leftover site project %s: %v", project.Name, err)
				}
			}
		}
	})
	p := e2e.Start(t)
	ctx := t.Context()

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

	groupID := createGroup(t, p, "E2E SFTP Agency")
	var sites []client.Site
	for _, name := range []string{"E2E SFTP One", "E2E SFTP Two"} {
		created := createSite(t, p, groupID, name, php)
		sites = append(sites, created.Site)
		projects = append(projects, compose.Project{Name: compose.SiteProjectName(created.Site.ID), Dir: t.TempDir()})
		waitJob(t, p, created.Job.ID)
	}
	one, two := sites[0], sites[1]
	oneProject, twoProject := projects[0], projects[1]

	const otherMarker = "pf-e2e-site-two-only-4b1d"
	if out, err := twoProject.ExecService(ctx, "php", "sh", "-c",
		"echo "+otherMarker+" > /var/www/wordpress/wp-content/site-two.txt"); err != nil {
		t.Fatalf("writing to site two: %v: %s", err, out)
	}
	if out, err := oneProject.ExecService(ctx, "php", "sh", "-c",
		"echo from-the-site > /var/www/wordpress/wp-content/from-site.txt"); err != nil {
		t.Fatalf("writing to site one: %v: %s", err, out)
	}
	siteUID := strings.TrimSpace(execOut(t, oneProject, "id", "-u"))

	// An Operator and a Viewer, each with a key.
	operator := newUser(t, p, "operator@e2e.test", client.Operator)
	viewer := newUser(t, p, "viewer@e2e.test", client.Viewer)
	opSigner, opKey := registerKey(t, operator)
	viewerSigner, _ := registerKey(t, viewer)
	noKeySigner := newSigner(t)

	// The Operator reaches site one's wp-content, and only that.
	conn, err := dialSFTP(p, one.Slug, opSigner)
	if err != nil {
		t.Fatalf("operator connecting to %s: %v", one.Slug, err)
	}
	c, err := sftp.NewClient(conn)
	if err != nil {
		t.Fatalf("starting SFTP: %v", err)
	}
	entries, err := c.ReadDir("/")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	for _, want := range []string{"plugins", "themes", "from-site.txt"} {
		if !slices.Contains(names, want) {
			t.Errorf("/ lists %v, want wp-content with %s", names, want)
		}
	}
	for _, path := range []string{"/etc/passwd", "/usr/local/bin/pressfleet", "/site-two.txt", "../site-two.txt",
		"/../../" + two.ID + "/site-two.txt", "/../wp-config.php", "/../../etc/hostname"} {
		if _, err := c.Stat(path); err == nil {
			t.Errorf("Stat(%q) succeeded; only site one's wp-content should be reachable", path)
		}
	}
	if got := readSFTP(t, c, "/from-site.txt"); got != "from-the-site\n" {
		t.Errorf("downloaded %q", got)
	}

	// Uploads land in the site, owned by the site's UID.
	const upload = "<?php // uploaded over SFTP\n"
	if err := c.MkdirAll("/plugins/pf-sftp-e2e"); err != nil {
		t.Fatal(err)
	}
	f, err := c.Create("/plugins/pf-sftp-e2e/pf-sftp-e2e.php")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte(upload)); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if got := execOut(t, oneProject, "cat", "/var/www/wordpress/wp-content/plugins/pf-sftp-e2e/pf-sftp-e2e.php"); got != upload {
		t.Errorf("uploaded file in the php container = %q", got)
	}
	owners := execOut(t, oneProject, "stat", "-c", "%u:%g",
		"/var/www/wordpress/wp-content/plugins/pf-sftp-e2e", "/var/www/wordpress/wp-content/plugins/pf-sftp-e2e/pf-sftp-e2e.php")
	if want := siteUID + ":" + siteUID + "\n"; owners != want+want {
		t.Errorf("owners of uploaded files = %q, want %s (the site's UID)", owners, siteUID)
	}
	if out, err := twoProject.ExecService(ctx, "php", "ls", "/var/www/wordpress/wp-content/plugins/pf-sftp-e2e"); err == nil {
		t.Errorf("the upload reached site two: %s", out)
	}

	// Refused connections.
	if _, err := dialSFTP(p, one.Slug, noKeySigner); err == nil {
		t.Error("a key nobody registered connected")
	}
	if _, err := dialSFTP(p, one.Slug, viewerSigner); err == nil {
		t.Error("a Viewer connected")
	}
	if _, err := dialSFTP(p, "no-such-site", opSigner); err == nil {
		t.Error("connected to a site that doesn't exist")
	}
	if s, err := conn.NewSession(); err == nil {
		if err := s.Run("id"); err == nil {
			t.Error("a command ran through the gateway")
		}
		_ = s.Close()
	}
	if _, err := conn.Dial("tcp", "mariadb:3306"); err == nil {
		t.Error("port forwarding was allowed")
	}

	// Removing the key ends the open session and refuses new ones.
	resp, err := operator.DeleteOwnSSHKeyWithResponse(ctx, opKey.ID)
	if err != nil || resp.StatusCode() != 204 {
		t.Fatalf("deleting the key: %v, %d", err, resp.StatusCode())
	}
	closed := make(chan struct{})
	go func() { _ = conn.Wait(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(time.Minute):
		t.Error("the open session outlived its key")
	}
	_ = c.Close()
	if _, err := dialSFTP(p, one.Slug, opSigner); err == nil {
		t.Error("a removed key connected")
	}

	// The audit log.
	events := func(action string) []client.AuditEvent {
		resp, err := p.Client.ListAuditEventsWithResponse(ctx, &client.ListAuditEventsParams{Action: &action})
		if err != nil || resp.JSON200 == nil {
			t.Fatalf("audit events: %v", err)
		}
		return resp.JSON200.Items
	}
	var ends []client.AuditEvent
	for deadline := time.Now().Add(30 * time.Second); len(ends) == 0 && time.Now().Before(deadline); time.Sleep(time.Second) {
		ends = events("site.sftp.disconnect")
	}
	if len(ends) != 1 {
		t.Fatalf("session end events = %+v", ends)
	}
	end := *ends[0].Details
	if end["key_fingerprint"] != opKey.Fingerprint || end["reason"] != "access_revoked" || end["bytes_received"] == "0" ||
		end["bytes_sent"] == "0" || deref(ends[0].TargetID) != one.ID {
		t.Errorf("session end event = %+v, details %v", ends[0], end)
	}
	var results []string
	for _, e := range events("site.sftp.connect") {
		results = append(results, string(e.Result))
	}
	slices.Sort(results)
	// The Operator's session; the Viewer's refusal; the missing site.
	if want := []string{"denied", "failure", "success"}; !slices.Equal(results, want) {
		t.Errorf("connect events = %v, want %v", results, want)
	}

	// Every worker is gone.
	for _, site := range sites {
		for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(time.Second) {
			workers, err := compose.SessionWorkers(ctx, nil, site.ID)
			if err != nil {
				t.Fatal(err)
			}
			if len(workers) == 0 {
				break
			}
			if time.Now().After(deadline) {
				t.Errorf("session workers left behind: %v", workers)
				break
			}
		}
	}
}

// execOut runs a command in the site's php container and returns its
// output.
func execOut(t *testing.T, project compose.Project, command ...string) string {
	t.Helper()
	out, err := project.ExecService(t.Context(), "php", command...)
	if err != nil {
		t.Fatalf("%v: %v: %s", command, err, out)
	}
	return string(out)
}

// newUser creates a user through the API and returns a client logged in
// as them.
func newUser(t *testing.T, p *e2e.Platform, email string, role client.Role) *client.ClientWithResponses {
	t.Helper()
	password := "e2e-password-" + strings.Repeat("x", 12)
	resp, err := p.Client.CreateUserWithResponse(t.Context(), client.CreateUserJSONRequestBody{
		Email: client.Email(email), Password: &password, Role: role,
	})
	if err != nil || resp.JSON201 == nil {
		t.Fatalf("creating %s: %v, %s", email, err, resp.Body)
	}
	return p.Login(t, email, redact.Secret(password))
}

func newSigner(t *testing.T) ssh.Signer {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	s, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// registerKey registers a new key for the logged-in user.
func registerKey(t *testing.T, c *client.ClientWithResponses) (ssh.Signer, client.SSHKey) {
	t.Helper()
	s := newSigner(t)
	resp, err := c.AddOwnSSHKeyWithResponse(t.Context(), client.AddOwnSSHKeyJSONRequestBody{
		PublicKey: string(ssh.MarshalAuthorizedKey(s.PublicKey())),
	})
	if err != nil || resp.JSON201 == nil {
		t.Fatalf("registering a key: %v, %s", err, resp.Body)
	}
	return s, *resp.JSON201
}

func dialSFTP(p *e2e.Platform, slug string, signer ssh.Signer) (*ssh.Client, error) {
	return ssh.Dial("tcp", p.SFTPAddr, &ssh.ClientConfig{
		User:            slug,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: ssh.FixedHostKey(p.HostKey),
		Timeout:         30 * time.Second,
	})
}

func readSFTP(t *testing.T, c *sftp.Client, path string) string {
	t.Helper()
	f, err := c.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(f)
	if err != nil && !errors.Is(err, os.ErrClosed) {
		t.Fatal(err)
	}
	return string(b)
}
