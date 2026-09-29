package deploy_test

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/rchouinard/pressfleet/control-plane/internal/deploy"
)

// member is one entry of a test archive.
type member struct {
	name string
	body string
	// typ is a tar type flag; for zips, tar.TypeSymlink makes a symbolic
	// link and tar.TypeChar a device file. Zero is a regular file, or a
	// directory for a name ending in "/".
	typ  byte
	link string
}

func zipOf(t *testing.T, members ...member) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, m := range members {
		h := &zip.FileHeader{Name: m.name, Method: zip.Deflate}
		switch {
		case m.typ == tar.TypeSymlink:
			h.SetMode(fs.ModeSymlink | 0o777)
		case m.typ == tar.TypeChar:
			h.SetMode(fs.ModeDevice | fs.ModeCharDevice | 0o644)
		case strings.HasSuffix(m.name, "/"):
			h.SetMode(fs.ModeDir | 0o755)
		default:
			h.SetMode(0o644)
		}
		w, err := zw.CreateHeader(h)
		if err != nil {
			t.Fatal(err)
		}
		body := m.body
		if m.typ == tar.TypeSymlink {
			body = m.link
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func tarGzOf(t *testing.T, members ...member) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, m := range members {
		h := &tar.Header{Name: m.name, Typeflag: m.typ, Linkname: m.link, Mode: 0o644}
		switch {
		case m.typ == 0 && strings.HasSuffix(m.name, "/"):
			h.Typeflag, h.Mode = tar.TypeDir, 0o755
		case m.typ == 0:
			h.Typeflag, h.Size = tar.TypeReg, int64(len(m.body))
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg {
			if _, err := tw.Write([]byte(m.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// writeFile writes data to a file in a temporary directory and opens it.
func openData(t *testing.T, data []byte) *os.File {
	t.Helper()
	p := filepath.Join(t.TempDir(), "package")
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f
}

// tree lists the files and directories below dir, with file contents.
func tree(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	if _, err := os.Stat(dir); errors.Is(err, fs.ErrNotExist) {
		return out
	}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || p == dir {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		if d.IsDir() {
			out[rel+"/"] = ""
			return nil
		}
		b, err := os.ReadFile(p)
		out[rel] = string(b)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestInspectAndExtract(t *testing.T) {
	theme := []member{
		{name: "acme/", typ: 0},
		{name: "acme/style.css", body: "/* Theme Name: Acme */"},
		{name: "acme/inc/functions.php", body: "<?php"},
		{name: "__MACOSX/acme/._style.css", body: "junk"},
	}
	flat := []member{{name: "./"}, {name: "./style.css", body: "css"}, {name: "./index.php", body: "<?php"}}
	for _, format := range []deploy.Format{deploy.FormatZip, deploy.FormatTarGz} {
		pack := func(members ...member) []byte {
			if format == deploy.FormatZip {
				return zipOf(t, members...)
			}
			return tarGzOf(t, members...)
		}
		t.Run(string(format), func(t *testing.T) {
			tests := []struct {
				name     string
				data     []byte
				manifest deploy.Manifest
				files    map[string]string
			}{
				{
					name:     "one top-level directory",
					data:     pack(theme...),
					manifest: deploy.Manifest{Format: format, Root: "acme", Files: 2, Dirs: 0, Bytes: 27},
					files:    map[string]string{"style.css": "/* Theme Name: Acme */", "inc/": "", "inc/functions.php": "<?php"},
				},
				{
					name:     "files at the top level",
					data:     pack(flat...),
					manifest: deploy.Manifest{Format: format, Files: 2, Bytes: 8},
					files:    map[string]string{"style.css": "css", "index.php": "<?php"},
				},
				{
					name:     "a single file stays",
					data:     pack(member{name: "hello.php", body: "<?php"}),
					manifest: deploy.Manifest{Format: format, Files: 1, Bytes: 5},
					files:    map[string]string{"hello.php": "<?php"},
				},
			}
			for _, tc := range tests {
				t.Run(tc.name, func(t *testing.T) {
					m, err := deploy.Inspect(openData(t, tc.data))
					if err != nil {
						t.Fatal(err)
					}
					if m != tc.manifest {
						t.Errorf("manifest = %+v, want %+v", m, tc.manifest)
					}
					dest := t.TempDir()
					root, err := os.OpenRoot(dest)
					if err != nil {
						t.Fatal(err)
					}
					defer func() { _ = root.Close() }()
					if _, err := deploy.Extract(openData(t, tc.data), root); err != nil {
						t.Fatal(err)
					}
					got := tree(t, dest)
					if len(got) != len(tc.files) {
						t.Errorf("unpacked %v, want %v", got, tc.files)
					}
					for name, body := range tc.files {
						if got[name] != body {
							t.Errorf("%s = %q, want %q (tree %v)", name, got[name], body, got)
						}
					}
					fi, err := os.Stat(filepath.Join(dest, slices.Sorted(func(yield func(string) bool) {
						for n := range tc.files {
							if !strings.HasSuffix(n, "/") && !yield(n) {
								return
							}
						}
					})[0]))
					if err != nil || fi.Mode().Perm() != 0o644 {
						t.Errorf("file mode = %v, %v", fi.Mode(), err)
					}
				})
			}
		})
	}
}

func TestInspectRejects(t *testing.T) {
	ok := member{name: "ok.php", body: "<?php"}
	tests := []struct {
		name    string
		members []member
		zipOnly bool
		tarOnly bool
		want    string
	}{
		{name: "zip slip", members: []member{ok, {name: "../evil.php", body: "x"}}, want: "outside the package"},
		{name: "deep zip slip", members: []member{ok, {name: "a/../../evil.php", body: "x"}}, want: "outside the package"},
		{name: "absolute path", members: []member{ok, {name: "/etc/cron.d/evil", body: "x"}}, want: "absolute"},
		{name: "backslash", members: []member{ok, {name: `..\evil.php`, body: "x"}}, want: "backslash"},
		{name: "symbolic link", members: []member{ok, {name: "link", typ: tar.TypeSymlink, link: "/etc/passwd"}}, want: "symbolic link"},
		{name: "hard link", members: []member{ok, {name: "link", typ: tar.TypeLink, link: "ok.php"}}, tarOnly: true, want: "hard link"},
		{name: "device", members: []member{ok, {name: "dev", typ: tar.TypeChar}}, want: "device file"},
		{name: "fifo", members: []member{ok, {name: "fifo", typ: tar.TypeFifo}}, tarOnly: true, want: "named pipe"},
		{name: "duplicate", members: []member{ok, ok}, want: "twice"},
		{name: "file below a file", members: []member{ok, {name: "ok.php/x", body: "x"}}, want: "is a file"},
		{name: "no files", members: []member{{name: "empty/"}}, want: "no files"},
	}
	for _, tc := range tests {
		for _, format := range []deploy.Format{deploy.FormatZip, deploy.FormatTarGz} {
			if (tc.zipOnly && format != deploy.FormatZip) || (tc.tarOnly && format != deploy.FormatTarGz) {
				continue
			}
			t.Run(tc.name+"/"+string(format), func(t *testing.T) {
				data := tarGzOf(t, tc.members...)
				if format == deploy.FormatZip {
					data = zipOf(t, tc.members...)
				}
				_, err := deploy.Inspect(openData(t, data))
				if !errors.Is(err, deploy.ErrInvalidPackage) || !strings.Contains(err.Error(), tc.want) {
					t.Errorf("Inspect = %v, want an invalid package error mentioning %q", err, tc.want)
				}
				// Extract refuses it before writing anything.
				dest := t.TempDir()
				root, err := os.OpenRoot(dest)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = root.Close() }()
				if _, err := deploy.Extract(openData(t, data), root); !errors.Is(err, deploy.ErrInvalidPackage) {
					t.Errorf("Extract = %v", err)
				}
				if got := tree(t, dest); len(got) != 0 {
					t.Errorf("Extract wrote %v", got)
				}
			})
		}
	}
	t.Run("not an archive", func(t *testing.T) {
		if _, err := deploy.Inspect(openData(t, []byte("<?php echo 1;"))); !errors.Is(err, deploy.ErrInvalidPackage) {
			t.Errorf("Inspect = %v", err)
		}
	})
	t.Run("truncated tar.gz", func(t *testing.T) {
		data := tarGzOf(t, member{name: "a.php", body: strings.Repeat("x", 10000)})
		if _, err := deploy.Inspect(openData(t, data[:len(data)/2])); !errors.Is(err, deploy.ErrInvalidPackage) {
			t.Errorf("Inspect = %v", err)
		}
	})
}
