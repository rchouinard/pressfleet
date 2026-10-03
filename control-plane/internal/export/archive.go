package export

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/siteimport"
)

// archiveRoot is the directory every path in an export's archive starts
// with: the layout site import looks for first (§5.7).
const archiveRoot = "wp-content"

// maxReported bounds the skipped entries archiveStats lists.
const maxReported = 20

// archiveStats is what filterTar kept and left out.
type archiveStats struct {
	// Files counts the regular files kept.
	Files int
	// DropIns are the host drop-ins left out.
	DropIns []string
	// Skipped lists entries that aren't regular files or directories (at
	// most maxReported), and SkippedCount counts all of them.
	Skipped      []string
	SkippedCount int
}

// filterTar copies the tar archive of a site's wp-content in src, whose
// paths are relative to wp-content (with or without a leading "./"), to
// dst as the archive of an export: every path below archiveRoot, without
// the platform's host drop-ins (siteimport.HostDropIns), which point at
// its object cache and page cache, and with nothing but directories and
// regular files. Symbolic links and other special files are left out, as
// site import leaves them out, and so is anything outside wp-content.
// Owners are dropped, since the site's UID means nothing elsewhere;
// permission bits and modification times are kept. It reads src up to the
// archive's end.
func filterTar(dst io.Writer, src io.Reader) (archiveStats, error) {
	var stats archiveStats
	tr := tar.NewReader(src)
	tw := tar.NewWriter(dst)
	root := &tar.Header{Typeflag: tar.TypeDir, Name: archiveRoot + "/", Mode: 0o755, ModTime: time.Now(), Format: tar.FormatPAX}
	rootWritten := false
	writeRoot := func() error {
		if rootWritten {
			return nil
		}
		rootWritten = true
		return tw.WriteHeader(root)
	}
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return stats, fmt.Errorf("reading the wp-content archive: %w", err)
		}
		rel := path.Clean(strings.TrimLeft(strings.TrimPrefix(h.Name, "./"), "/"))
		if rel == "." && h.Typeflag == tar.TypeDir {
			// wp-content itself: keep its permissions and time.
			root.Mode, root.ModTime = h.Mode&0o7777, h.ModTime
			if err := writeRoot(); err != nil {
				return stats, err
			}
			continue
		}
		if err := writeRoot(); err != nil {
			return stats, err
		}
		out := &tar.Header{Name: archiveRoot + "/" + rel, Mode: h.Mode & 0o7777, ModTime: h.ModTime, Format: tar.FormatPAX}
		switch {
		case !fs.ValidPath(rel) || rel == ".":
			stats.skip(h.Name)
		case h.Typeflag == tar.TypeDir:
			out.Typeflag, out.Name = tar.TypeDir, out.Name+"/"
			if err := tw.WriteHeader(out); err != nil {
				return stats, err
			}
		case h.Typeflag == tar.TypeReg || h.Typeflag == tar.TypeRegA: //nolint:staticcheck // SA1019: old archives still use TypeRegA
			if slices.Contains(siteimport.HostDropIns, rel) {
				stats.DropIns = append(stats.DropIns, rel)
				continue
			}
			out.Typeflag, out.Size = tar.TypeReg, h.Size
			if err := tw.WriteHeader(out); err != nil {
				return stats, err
			}
			if _, err := io.Copy(tw, tr); err != nil { //nolint:gosec // G110: the site's own files, read uncompressed and sized by their headers
				return stats, fmt.Errorf("copying %s: %w", rel, err)
			}
			stats.Files++
		default:
			stats.skip(rel)
		}
	}
	if err := writeRoot(); err != nil {
		return stats, err
	}
	return stats, tw.Close()
}

func (s *archiveStats) skip(name string) {
	s.SkippedCount++
	if len(s.Skipped) < maxReported {
		s.Skipped = append(s.Skipped, name)
	}
}
