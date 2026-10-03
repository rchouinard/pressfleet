package export

import (
	"archive/tar"
	"bytes"
	"errors"
	"io"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestFilterTar(t *testing.T) {
	type entry struct {
		name     string
		typ      byte
		mode     int64
		content  string
		linkname string
	}
	build := func(entries []entry) []byte {
		var buf bytes.Buffer
		tw := tar.NewWriter(&buf)
		for _, en := range entries {
			h := &tar.Header{Name: en.name, Typeflag: en.typ, Mode: en.mode, Linkname: en.linkname,
				Size: int64(len(en.content)), ModTime: time.Unix(1_700_000_000, 0), Uid: 33, Uname: "www-data"}
			if en.typ != tar.TypeReg {
				h.Size = 0
			}
			if err := tw.WriteHeader(h); err != nil {
				t.Fatal(err)
			}
			_, _ = io.WriteString(tw, en.content)
		}
		_ = tw.Close()
		return buf.Bytes()
	}
	long := strings.Repeat("d", 120) + "/" + strings.Repeat("f", 120) + ".php"
	for _, tc := range []struct {
		name      string
		in        []entry
		want      map[string]int64 // name → mode
		wantStats archiveStats
	}{
		{
			name: "GNU tar",
			in: []entry{
				{name: "./", typ: tar.TypeDir, mode: 0o750},
				{name: "./themes/", typ: tar.TypeDir, mode: 0o755},
				{name: "./themes/a.php", typ: tar.TypeReg, mode: 0o4755, content: "a"},
				{name: "./object-cache.php", typ: tar.TypeReg, mode: 0o644, content: "x"},
				{name: "./" + long, typ: tar.TypeReg, mode: 0o600, content: "long"},
				{name: "./link", typ: tar.TypeSymlink, linkname: "/etc/passwd"},
				{name: "./fifo", typ: tar.TypeFifo},
			},
			want: map[string]int64{
				"wp-content/": 0o750, "wp-content/themes/": 0o755, "wp-content/themes/a.php": 0o4755,
				"wp-content/" + long: 0o600,
			},
			wantStats: archiveStats{Files: 2, DropIns: []string{"object-cache.php"}, Skipped: []string{"link", "fifo"}, SkippedCount: 2},
		},
		{
			name: "restic",
			in: []entry{
				{name: "plugins/", typ: tar.TypeDir, mode: 0o755},
				{name: "plugins/db.php", typ: tar.TypeReg, mode: 0o644, content: "kept"},
				{name: "advanced-cache.php", typ: tar.TypeReg, mode: 0o644, content: "x"},
				{name: "db.php", typ: tar.TypeReg, mode: 0o644, content: "x"},
			},
			want:      map[string]int64{"wp-content/": 0o755, "wp-content/plugins/": 0o755, "wp-content/plugins/db.php": 0o644},
			wantStats: archiveStats{Files: 1, DropIns: []string{"advanced-cache.php", "db.php"}},
		},
		{
			name: "outside wp-content",
			in: []entry{
				{name: "../wp-config.php", typ: tar.TypeReg, mode: 0o644, content: "secret"},
				{name: "/etc/passwd", typ: tar.TypeReg, mode: 0o644, content: "root"},
				{name: "a/../../b", typ: tar.TypeReg, mode: 0o644, content: "b"},
			},
			// An absolute path is read as relative to wp-content.
			want:      map[string]int64{"wp-content/": 0o755, "wp-content/etc/passwd": 0o644},
			wantStats: archiveStats{Files: 1, Skipped: []string{"../wp-config.php", "a/../../b"}, SkippedCount: 2},
		},
		{
			name:      "empty",
			in:        nil,
			want:      map[string]int64{"wp-content/": 0o755},
			wantStats: archiveStats{},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			stats, err := filterTar(&out, bytes.NewReader(build(tc.in)))
			if err != nil {
				t.Fatal(err)
			}
			if stats.Files != tc.wantStats.Files || !slices.Equal(stats.DropIns, tc.wantStats.DropIns) ||
				!slices.Equal(stats.Skipped, tc.wantStats.Skipped) || stats.SkippedCount != tc.wantStats.SkippedCount {
				t.Errorf("stats = %+v, want %+v", stats, tc.wantStats)
			}
			got := map[string]int64{}
			tr := tar.NewReader(&out)
			for {
				h, err := tr.Next()
				if errors.Is(err, io.EOF) {
					break
				}
				if err != nil {
					t.Fatal(err)
				}
				if h.Uid != 0 || h.Uname != "" {
					t.Errorf("%s keeps its owner", h.Name)
				}
				got[h.Name] = h.Mode
			}
			if !maps.Equal(got, tc.want) {
				t.Errorf("archive = %v, want %v", got, tc.want)
			}
		})
	}

	// An archive that isn't one fails.
	if _, err := filterTar(io.Discard, strings.NewReader(strings.Repeat("x", 1024))); err == nil {
		t.Error("filterTar accepted garbage")
	}
}
