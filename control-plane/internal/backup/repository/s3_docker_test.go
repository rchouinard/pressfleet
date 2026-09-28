package repository_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/backup/repository"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner/compose"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
)

// s3Image is an S3-compatible server for the S3 backend test. MinIO no
// longer publishes images; RustFS is a drop-in replacement.
const s3Image = "rustfs/rustfs:1.0.0@sha256:8cc9801755448b71a786705ce76692c77e14936cccd87cf2fc31842e58f4d1ff"

const (
	s3TestAccessKey = "pftest-access"
	s3TestSecretKey = "pftest-secret-key-0123456789"
)

// startS3 runs an S3-compatible server on a free loopback port and returns
// its endpoint URL. It is removed when the test ends.
func startS3(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	_, port, _ := net.SplitHostPort(addr)

	dir := t.TempDir()
	composeFile := fmt.Sprintf(`services:
  s3:
    image: %q
    environment:
      RUSTFS_ACCESS_KEY: %q
      RUSTFS_SECRET_KEY: %q
    ports:
      - "127.0.0.1:%s:9000"
`, s3Image, s3TestAccessKey, s3TestSecretKey, port)
	if err := os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte(composeFile), 0o600); err != nil {
		t.Fatal(err)
	}
	p := compose.Project{Name: fmt.Sprintf("pftest-s3-%d", time.Now().UnixNano()), Dir: dir}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if err := p.Down(ctx, compose.DownOptions{Volumes: true}); err != nil {
			t.Errorf("down: %v", err)
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if err := p.Up(ctx, compose.UpOptions{Wait: true, WaitTimeout: 3 * time.Minute}); err != nil {
		t.Fatal(err)
	}
	endpoint := "http://" + addr
	for {
		// Any HTTP answer (even 403) means the server is up.
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"/", nil)
		if resp, err := http.DefaultClient.Do(req); err == nil {
			_ = resp.Body.Close()
			return endpoint
		} else if ctx.Err() != nil {
			t.Fatalf("S3 server not reachable: %v", err)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// TestS3Backend runs the repository lifecycle against an S3-compatible
// server.
func TestS3Backend(t *testing.T) {
	if os.Getenv("PRESSFLEET_DOCKER_TESTS") != "1" {
		t.Skip("set PRESSFLEET_DOCKER_TESTS=1 to run tests against Docker")
	}
	endpoint := startS3(t)
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		ctx := context.Background()
		s3 := repository.S3Config{
			Endpoint: endpoint, Bucket: "pf-backups", Prefix: "pressfleet/" + string(db.Dialect()),
			AccessKeyID: s3TestAccessKey, SecretAccessKey: s3TestSecretKey,
		}
		f := newFixture(t, db, repository.Options{Backend: repository.BackendS3, S3: s3})
		awsEnv := []string{"AWS_ACCESS_KEY_ID=" + s3TestAccessKey, "AWS_SECRET_ACCESS_KEY=" + s3TestSecretKey}

		groupA, groupB := f.group(t, "Acme"), f.group(t, "Beta")
		a, err := f.m.Ensure(ctx, groupA)
		if err != nil {
			t.Fatal(err)
		}
		if want := "s3:" + endpoint + "/pf-backups/" + s3.Prefix + "/" + groupA; a.Location != want || a.Backend != repository.BackendS3 {
			t.Errorf("Ensure = %+v, want location %s", a, want)
		}
		if again, err := f.m.Ensure(ctx, groupA); err != nil || again.SecretRef != a.SecretRef {
			t.Fatalf("second Ensure = %+v, %v", again, err)
		}
		b, err := f.m.Ensure(ctx, groupB)
		if err != nil {
			t.Fatal(err)
		}
		pwA := f.password(t, a).Reveal()
		if _, err := restic(t, append(awsEnv, "RESTIC_REPOSITORY="+b.Location, "RESTIC_PASSWORD="+pwA), "cat", "config"); err == nil {
			t.Error("group A's password opens group B's repository")
		}

		access, err := f.m.TaskAccess(ctx, groupA)
		if err != nil {
			t.Fatal(err)
		}
		if access.RepositoryDir != "" || access.Env["RESTIC_REPOSITORY"] != a.Location ||
			access.SecretEnv["AWS_ACCESS_KEY_ID"] != s3TestAccessKey || access.SecretEnv["AWS_SECRET_ACCESS_KEY"] != s3TestSecretKey ||
			access.SecretEnv["RESTIC_PASSWORD"].Reveal() != pwA {
			t.Errorf("TaskAccess = %+v", access)
		}
		env := []string{}
		for k, v := range access.Env {
			env = append(env, k+"="+v)
		}
		for k, v := range access.SecretEnv {
			env = append(env, k+"="+v.Reveal())
		}
		src := t.TempDir()
		content := []byte("uploaded media\n")
		if err := os.WriteFile(filepath.Join(src, "photo.jpg"), content, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := restic(t, env, "backup", "--tag", "site-1", src); err != nil {
			t.Fatal(err)
		}

		exp, err := f.m.Export(ctx, groupA, filepath.Join(t.TempDir(), "export"))
		if err != nil {
			t.Fatal(err)
		}
		if err := f.m.Delete(ctx, groupA); err != nil {
			t.Fatal(err)
		}
		_, err = restic(t, append(awsEnv, "RESTIC_REPOSITORY="+a.Location, "RESTIC_PASSWORD="+pwA), "cat", "config")
		if err == nil {
			t.Error("the deleted repository still opens")
		}
		target := t.TempDir()
		if _, err := restic(t, []string{"RESTIC_REPOSITORY=" + exp.RepositoryDir, "RESTIC_PASSWORD_FILE=" + exp.PasswordFile},
			"restore", "latest", "--target", target); err != nil {
			t.Fatal(err)
		}
		if got, err := os.ReadFile(filepath.Join(target, src, "photo.jpg")); err != nil || !bytes.Equal(got, content) {
			t.Errorf("restored file = %q, %v", got, err)
		}

		// Group B's repository is untouched by A's deletion.
		if _, err := f.m.Restic(ctx, groupB, "cat", "config"); err != nil {
			t.Errorf("group B's repository after deleting A's: %v", err)
		}
		if err := f.m.Delete(ctx, groupB); err != nil {
			t.Fatal(err)
		}
		if err := f.m.Delete(ctx, groupB); err != nil {
			t.Errorf("second Delete: %v", err)
		}
		if _, err := f.m.Get(ctx, groupB); !errors.Is(err, repository.ErrNoRepository) {
			t.Errorf("Get after Delete: err = %v", err)
		}
	})
}
