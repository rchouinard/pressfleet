package sftpserver_test

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/pkg/sftp"

	"github.com/rchouinard/pressfleet/control-plane/internal/sftpserver"
)

// serve starts the server on a fresh wp-content directory inside a parent
// that also holds a file the client must not reach, and returns a client.
func serve(t *testing.T) (*sftp.Client, string, string) {
	t.Helper()
	parent := t.TempDir()
	root := filepath.Join(parent, "wp-content")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(parent, "other-site.txt")
	if err := os.WriteFile(secret, []byte("another site's data"), 0o644); err != nil {
		t.Fatal(err)
	}

	clientToServer, serverIn := io.Pipe()
	serverOut, serverToClient := io.Pipe()
	done := make(chan error, 1)
	go func() {
		done <- sftpserver.Serve(clientToServer, serverToClient, root)
		_ = serverToClient.Close()
	}()
	c, err := sftp.NewClientPipe(serverOut, serverIn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = c.Close()
		_ = serverIn.Close()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Serve = %v, want nil when the client leaves", err)
			}
		case <-time.After(10 * time.Second):
			t.Error("Serve didn't return after the client left")
		}
	})
	return c, root, secret
}

func writeFile(t *testing.T, c *sftp.Client, name, content string) {
	t.Helper()
	f, err := c.Create(name)
	if err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
	if _, err := f.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, c *sftp.Client, name string) string {
	t.Helper()
	f, err := c.Open(name)
	if err != nil {
		t.Fatalf("open %s: %v", name, err)
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(f)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestFileOperations(t *testing.T) {
	c, root, _ := serve(t)

	if wd, err := c.Getwd(); err != nil || wd != "/" {
		t.Errorf("Getwd = %q, %v; want /", wd, err)
	}
	if err := c.Mkdir("/themes"); err != nil {
		t.Fatal(err)
	}
	if err := c.MkdirAll("plugins/acme/assets"); err != nil {
		t.Fatal(err)
	}
	writeFile(t, c, "/themes/style.css", "body{}")
	if b, err := os.ReadFile(filepath.Join(root, "themes", "style.css")); err != nil || string(b) != "body{}" {
		t.Errorf("file on disk = %q, %v", b, err)
	}
	if got := readFile(t, c, "themes/style.css"); got != "body{}" {
		t.Errorf("read back %q", got)
	}

	// Overwriting truncates; appending by offset works.
	writeFile(t, c, "/themes/style.css", "a")
	if got := readFile(t, c, "/themes/style.css"); got != "a" {
		t.Errorf("after overwrite: %q", got)
	}
	f, err := c.OpenFile("/themes/style.css", os.O_RDWR)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte("bc"), 1); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	if got := readFile(t, c, "/themes/style.css"); got != "abc" {
		t.Errorf("after WriteAt: %q", got)
	}

	entries, err := c.ReadDir("/")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	slices.Sort(names)
	if !slices.Equal(names, []string{"plugins", "themes"}) {
		t.Errorf("ReadDir(/) = %v", names)
	}
	if fi, err := c.Stat("/themes/style.css"); err != nil || fi.Size() != 3 || fi.IsDir() {
		t.Errorf("Stat = %+v, %v", fi, err)
	}

	// Modes, times, and size.
	if err := c.Chmod("/themes/style.css", 0o600); err != nil {
		t.Fatal(err)
	}
	if err := c.Truncate("/themes/style.css", 1); err != nil {
		t.Fatal(err)
	}
	mtime := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	if err := c.Chtimes("/themes/style.css", mtime, mtime); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(filepath.Join(root, "themes", "style.css")); err != nil || fi.Mode().Perm() != 0o600 || !fi.ModTime().Equal(mtime) || fi.Size() != 1 {
		t.Errorf("after setstat: %v %v %v %v", fi.Mode(), fi.ModTime(), fi.Size(), err)
	}

	// Renames: plain renames don't replace, POSIX renames do.
	writeFile(t, c, "/a.txt", "A")
	writeFile(t, c, "/b.txt", "B")
	if err := c.Rename("/a.txt", "/b.txt"); err == nil {
		t.Error("Rename over an existing file succeeded")
	}
	if err := c.PosixRename("/a.txt", "/b.txt"); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, c, "/b.txt"); got != "A" {
		t.Errorf("after PosixRename: %q", got)
	}
	if err := c.Rename("/b.txt", "/c.txt"); err != nil {
		t.Fatal(err)
	}

	// Removing files and directories.
	if err := c.Remove("/c.txt"); err != nil {
		t.Fatal(err)
	}
	if err := c.RemoveDirectory("/plugins/acme/assets"); err != nil {
		t.Fatal(err)
	}
	if err := c.RemoveDirectory("/themes"); err == nil {
		t.Error("removing a non-empty directory succeeded")
	}
	if _, err := c.Stat("/c.txt"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Stat of a removed file: %v, want not exist", err)
	}
}

func TestConfinedToRoot(t *testing.T) {
	c, root, secret := serve(t)

	// ".." never leaves the root: it resolves against the client's "/".
	for _, p := range []string{"../other-site.txt", "/../other-site.txt", "/themes/../../other-site.txt"} {
		if _, err := c.Stat(p); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("Stat(%q) = %v, want not exist", p, err)
		}
	}
	writeFile(t, c, "../escape.txt", "x")
	if _, err := os.Stat(filepath.Join(root, "escape.txt")); err != nil {
		t.Errorf("a write to ../escape.txt should land in the root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(root), "escape.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a write escaped the root: %v", err)
	}

	// Symbolic links out of the root can't be followed, and new links
	// can't be made.
	if err := os.Symlink(secret, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("..", filepath.Join(root, "up")); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Open("/link"); err == nil {
		t.Error("opened a file through a link out of the root")
	}
	if _, err := c.Stat("/up/other-site.txt"); err == nil {
		t.Error("stat through a link out of the root succeeded")
	}
	if _, err := c.ReadDir("/up"); err == nil {
		t.Error("listed a directory through a link out of the root")
	}
	if f, err := c.Create("/link"); err == nil {
		_ = f.Close()
		t.Error("wrote through a link out of the root")
	}
	if b, _ := os.ReadFile(secret); string(b) != "another site's data" {
		t.Errorf("the file outside the root changed: %q", b)
	}
	if fi, err := c.Lstat("/link"); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("Lstat of the link = %v, %v", fi, err)
	}
	if err := c.Symlink("/etc/passwd", "/newlink"); err == nil {
		t.Error("creating a symbolic link succeeded")
	}
	if err := c.Link("/escape.txt", "/hardlink"); err == nil {
		t.Error("creating a hard link succeeded")
	}
	// Removing the root itself fails.
	if err := c.RemoveDirectory("/"); err == nil {
		t.Error("removing the root succeeded")
	}
	if os.Getuid() != 0 {
		if err := c.Chown("/escape.txt", 0, 0); err == nil {
			t.Error("changing a file's owner succeeded")
		}
	}
}
