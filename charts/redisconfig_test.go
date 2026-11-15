package charts_test

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The init container that writes valkey.conf hashes the admin password. The
// control plane trims \r and \n from its copy of the secret, so a secret
// made from a file with CRLF line endings must give the same hash as the
// trimmed value, or every Valkey admin command fails with WRONGPASS.
func TestValkeyAdminPasswordHash(t *testing.T) {
	objects := renderObjects(t, podCIDRs)
	var script string
	for _, o := range objects {
		if o.Metadata.Name != "redis" {
			continue
		}
		for _, c := range o.Spec.Template.Spec.InitContainers {
			if c.Name == "config" && len(c.Command) == 3 {
				script = c.Command[2]
			}
		}
	}
	if script == "" {
		t.Fatal("no config init container in the redis workload")
	}

	const password = "s3cret-pass"
	sum := sha256.Sum256([]byte(password))
	want := hex.EncodeToString(sum[:])
	for name, content := range map[string]string{
		"bare":     password,
		"LF":       password + "\n",
		"CRLF":     password + "\r\n",
		"CR":       password + "\r",
		"two CRLF": password + "\r\n\r\n",
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			secret := filepath.Join(dir, "redis_admin_password")
			tmplDir, outDir := filepath.Join(dir, "template"), filepath.Join(dir, "out")
			for _, d := range []string{tmplDir, outDir} {
				if err := os.Mkdir(d, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			for path, data := range map[string]string{
				secret:                                content,
				filepath.Join(tmplDir, "valkey.conf"): "user pressfleet on #@REDIS_ADMIN_PASSWORD_HASH@\n",
			} {
				if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			// The script runs in a container with fixed paths.
			s := strings.NewReplacer(
				"/run/secrets/redis_admin_password", secret,
				"/etc/valkey-template", tmplDir,
				"/etc/valkey/", outDir+"/",
			).Replace(script)
			if out, err := exec.CommandContext(t.Context(), "sh", "-c", s).CombinedOutput(); err != nil {
				t.Fatalf("script failed: %v\n%s", err, out)
			}
			got, err := os.ReadFile(filepath.Join(outDir, "valkey.conf"))
			if err != nil {
				t.Fatal(err)
			}
			if wantLine := "user pressfleet on #" + want + "\n"; string(got) != wantLine {
				t.Errorf("valkey.conf = %q, want %q", got, wantLine)
			}
		})
	}
}
