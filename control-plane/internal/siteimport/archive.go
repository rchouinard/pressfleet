package siteimport

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"bytes"
	"compress/bzip2"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"slices"
	"strings"
	"time"
)

// Archive formats InspectArchive accepts.
const (
	FormatTar      = "tar"
	FormatTarGzip  = "tar.gz"
	FormatTarBzip2 = "tar.bz2"
	FormatZip      = "zip"
)

// HostDropIns are the wp-content drop-ins that belong to a hosting
// environment rather than to the site: a persistent object cache, a page
// cache, and a database layer. The platform provides its own object cache
// and page cache, and a foreign db.php (HyperDB, a host's database proxy)
// would point WordPress elsewhere, so an import leaves them out.
var HostDropIns = []string{"object-cache.php", "advanced-cache.php", "db.php"}

// maxReported bounds the skipped entries ArchiveInfo lists.
const maxReported = 20

// ArchiveInfo describes a wp-content archive.
type ArchiveInfo struct {
	// Format is one of the Format constants.
	Format string `json:"format"`
	// Root is wp-content's path inside the archive, "" for its top.
	Root string `json:"root"`
	// Files and Bytes count the regular files that will be imported.
	Files int   `json:"files"`
	Bytes int64 `json:"bytes"`
	// Themes are the directories in themes/.
	Themes []string `json:"themes,omitempty"`
	// MUPlugins are the must-use plugins: the entries of mu-plugins/,
	// apart from index.php and hidden files.
	MUPlugins []string `json:"mu_plugins,omitempty"`
	// DropIns are the HostDropIns the archive has, which WriteTar leaves
	// out.
	DropIns []string `json:"drop_ins,omitempty"`
	// Skipped lists entries that aren't regular files or directories (at
	// most maxReported), and SkippedCount counts all of them.
	Skipped      []string `json:"skipped,omitempty"`
	SkippedCount int      `json:"skipped_count,omitempty"`
}

// entryType is what an archive entry is.
type entryType int

const (
	entryFile entryType = iota
	entryDir
	entryOther
)

// entry is one archive entry, with a cleaned relative path.
type entry struct {
	name    string // slash-separated, no leading or trailing slash
	typ     entryType
	size    int64
	modTime time.Time
}

// archive iterates over an archive's entries.
type archive struct {
	format string
	// walk calls fn for every entry; body reads a file's content.
	walk func(fn func(e entry, body io.Reader) error) error
}

// openArchive recognizes the archive format from its first bytes.
func openArchive(f *os.File) (*archive, error) {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	head := make([]byte, 512)
	n, err := io.ReadFull(f, head)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return nil, err
	}
	head = head[:n]
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	switch {
	case bytes.HasPrefix(head, []byte("PK\x03\x04")), bytes.HasPrefix(head, []byte("PK\x05\x06")):
		return zipArchive(f, fi.Size())
	case bytes.HasPrefix(head, []byte{0x1f, 0x8b}):
		return tarArchive(f, FormatTarGzip, func(r io.Reader) (io.Reader, error) { return gzip.NewReader(r) }), nil
	case bytes.HasPrefix(head, []byte("BZh")):
		return tarArchive(f, FormatTarBzip2, func(r io.Reader) (io.Reader, error) { return bzip2.NewReader(r), nil }), nil
	case isTar(head):
		return tarArchive(f, FormatTar, func(r io.Reader) (io.Reader, error) { return r, nil }), nil
	case bytes.HasPrefix(head, []byte{0xfd, '7', 'z', 'X', 'Z', 0}), bytes.HasPrefix(head, []byte("7z\xbc\xaf")),
		bytes.HasPrefix(head, []byte("Rar!")), bytes.HasPrefix(head, []byte{0x28, 0xb5, 0x2f, 0xfd}):
		return nil, invalidArchive("its format isn't supported; send a .tar.gz, .tar, or .zip file")
	}
	return nil, invalidArchive("it isn't a tar or zip archive; send a .tar.gz, .tar, or .zip file")
}

// isTar reports whether a block is a POSIX or GNU tar header.
func isTar(block []byte) bool {
	return len(block) >= 263 && bytes.Equal(block[257:262], []byte("ustar"))
}

func tarArchive(f *os.File, format string, decompress func(io.Reader) (io.Reader, error)) *archive {
	return &archive{format: format, walk: func(fn func(entry, io.Reader) error) error {
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return err
		}
		r, err := decompress(bufio.NewReaderSize(f, 1<<20))
		if err != nil {
			return invalidArchive("it can't be decompressed: %v", err)
		}
		tr := tar.NewReader(r)
		first := true
		for {
			h, err := tr.Next()
			if errors.Is(err, io.EOF) {
				if first {
					return invalidArchive("it is empty")
				}
				return nil
			}
			if err != nil {
				return invalidArchive("it can't be read: %v", err)
			}
			first = false
			e := entry{size: h.Size, modTime: h.ModTime}
			switch h.Typeflag {
			case tar.TypeReg, tar.TypeRegA: //nolint:staticcheck // SA1019: old archives still use TypeRegA
				e.typ = entryFile
			case tar.TypeDir:
				e.typ = entryDir
			case tar.TypeXGlobalHeader:
				continue
			default:
				e.typ = entryOther
			}
			if e.name, err = cleanName(h.Name); err != nil {
				return err
			}
			if e.name == "" {
				continue
			}
			if err := fn(e, tr); err != nil {
				return err
			}
		}
	}}
}

func zipArchive(f *os.File, size int64) (*archive, error) {
	zr, err := zip.NewReader(f, size)
	if err != nil {
		return nil, invalidArchive("it can't be read as a zip file: %v", err)
	}
	return &archive{format: FormatZip, walk: func(fn func(entry, io.Reader) error) error {
		if len(zr.File) == 0 {
			return invalidArchive("it is empty")
		}
		for _, zf := range zr.File {
			if zf.Flags&0x1 != 0 {
				return invalidArchive("it is encrypted")
			}
			name, err := cleanName(strings.ReplaceAll(zf.Name, `\`, "/"))
			if err != nil {
				return err
			}
			if name == "" {
				continue
			}
			mode := zf.Mode()
			e := entry{name: name, size: int64(zf.UncompressedSize64), modTime: zf.Modified} //nolint:gosec // G115: sizes fit int64
			switch {
			case mode.IsDir() || strings.HasSuffix(zf.Name, "/"):
				e.typ = entryDir
			case mode.IsRegular():
				e.typ = entryFile
			default:
				e.typ = entryOther
			}
			var body io.Reader
			if e.typ == entryFile {
				rc, err := zf.Open()
				if err != nil {
					return invalidArchive("%s can't be read: %v", name, err)
				}
				body = rc
				err = fn(e, body)
				_ = rc.Close()
				if err != nil {
					return err
				}
				continue
			}
			if err := fn(e, nil); err != nil {
				return err
			}
		}
		return nil
	}}, nil
}

// cleanName returns an entry's path relative to the archive's top, or
// refuses an absolute path or one with "..", which no honest export has.
func cleanName(name string) (string, error) {
	if strings.HasPrefix(name, "/") || strings.ContainsRune(name, 0) || (len(name) > 1 && name[1] == ':') {
		return "", invalidArchive("entry %q has an absolute path", name)
	}
	for _, part := range strings.Split(name, "/") {
		if part == ".." {
			return "", invalidArchive("entry %q leads outside the archive", name)
		}
	}
	clean := path.Clean(name)
	if clean == "." {
		return "", nil
	}
	return clean, nil
}

// InspectArchive checks that f is a wp-content archive and describes it;
// see the package documentation. Problems are ErrInvalidArchive.
func InspectArchive(f *os.File) (ArchiveInfo, error) {
	a, err := openArchive(f)
	if err != nil {
		return ArchiveInfo{}, err
	}
	// First pass: the directory tree, to find wp-content.
	dirs := map[string]bool{}
	addParents := func(name string) {
		for d := path.Dir(name); d != "."; d = path.Dir(d) {
			dirs[d] = true
		}
	}
	err = a.walk(func(e entry, _ io.Reader) error {
		addParents(e.name)
		if e.typ == entryDir {
			dirs[e.name] = true
		}
		return nil
	})
	if err != nil {
		return ArchiveInfo{}, err
	}
	root, err := findRoot(dirs)
	if err != nil {
		return ArchiveInfo{}, err
	}

	info := ArchiveInfo{Format: a.format, Root: root}
	themes, mu := map[string]bool{}, map[string]bool{}
	err = a.walk(func(e entry, _ io.Reader) error {
		rel, ok := relative(root, e.name)
		if !ok || rel == "" {
			return nil
		}
		top, rest, nested := strings.Cut(rel, "/")
		switch {
		case top == "themes" && nested:
			if t, _, _ := strings.Cut(rest, "/"); !strings.HasPrefix(t, ".") && (e.typ == entryDir || strings.Contains(rest, "/")) {
				themes[t] = true
			}
		case top == "mu-plugins" && nested:
			if m, _, _ := strings.Cut(rest, "/"); m != "index.php" && !strings.HasPrefix(m, ".") {
				mu[m] = true
			}
		}
		switch e.typ {
		case entryFile:
			if slices.Contains(HostDropIns, rel) {
				info.DropIns = append(info.DropIns, rel)
				return nil
			}
			info.Files++
			info.Bytes += e.size
		case entryOther:
			info.SkippedCount++
			if len(info.Skipped) < maxReported {
				info.Skipped = append(info.Skipped, rel)
			}
		}
		return nil
	})
	if err != nil {
		return ArchiveInfo{}, err
	}
	if info.Files == 0 {
		return ArchiveInfo{}, invalidArchive("its wp-content has no files")
	}
	info.Themes = sortedKeys(themes)
	info.MUPlugins = sortedKeys(mu)
	slices.Sort(info.DropIns)
	return info, nil
}

// wpContentDirs are the directories that make a directory look like
// wp-content.
var wpContentDirs = []string{"themes", "plugins", "uploads"}

// findRoot finds wp-content in an archive's directory tree.
func findRoot(dirs map[string]bool) (string, error) {
	// A directory named wp-content, at the top or one level down: an
	// archive of wp-content itself, or of a whole install.
	var named []string
	for d := range dirs {
		if path.Base(d) == "wp-content" && strings.Count(d, "/") <= 1 {
			named = append(named, d)
		}
	}
	slices.SortFunc(named, func(a, b string) int { return strings.Count(a, "/") - strings.Count(b, "/") })
	switch {
	case len(named) == 1, len(named) > 1 && strings.Count(named[0], "/") < strings.Count(named[1], "/"):
		return named[0], nil
	case len(named) > 1:
		return "", invalidArchive("it has several wp-content directories (%s)", strings.Join(named, ", "))
	}
	looksLike := func(dir string) bool {
		for _, sub := range wpContentDirs {
			if dirs[path.Join(dir, sub)] {
				return true
			}
		}
		return false
	}
	if looksLike("") {
		return "", nil
	}
	var top []string
	for d := range dirs {
		if !strings.Contains(d, "/") {
			top = append(top, d)
		}
	}
	if len(top) == 1 && looksLike(top[0]) {
		return top[0], nil
	}
	return "", invalidArchive("it doesn't hold a wp-content directory: no wp-content, themes, plugins, or uploads directory at its top")
}

// relative returns name relative to root, and whether it is inside it.
func relative(root, name string) (string, bool) {
	if root == "" {
		return name, true
	}
	if name == root {
		return "", true
	}
	rest, ok := strings.CutPrefix(name, root+"/")
	return rest, ok
}

func sortedKeys(m map[string]bool) []string {
	if len(m) == 0 {
		return nil
	}
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// WriteTar writes the wp-content that InspectArchive found in f to w as a
// plain tar, with paths relative to wp-content: regular files (mode 0644)
// and directories (0755), without HostDropIns and without entries that are
// neither. The importing task extracts it into the site's wp-content as
// the site's user.
func WriteTar(w io.Writer, f *os.File, info ArchiveInfo) error {
	a, err := openArchive(f)
	if err != nil {
		return err
	}
	tw := tar.NewWriter(w)
	err = a.walk(func(e entry, body io.Reader) error {
		rel, ok := relative(info.Root, e.name)
		if !ok || rel == "" {
			return nil
		}
		h := &tar.Header{Name: rel, ModTime: e.modTime, Format: tar.FormatPAX}
		switch e.typ {
		case entryDir:
			h.Typeflag, h.Mode, h.Name = tar.TypeDir, 0o755, rel+"/"
			return tw.WriteHeader(h)
		case entryFile:
			if slices.Contains(HostDropIns, rel) {
				return nil
			}
			h.Typeflag, h.Mode, h.Size = tar.TypeReg, 0o644, e.size
			if err := tw.WriteHeader(h); err != nil {
				return err
			}
			if n, err := io.Copy(tw, body); err != nil {
				return fmt.Errorf("copying %s: %w", rel, err)
			} else if n != e.size {
				return invalidArchive("%s is shorter than its header says", rel)
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	return tw.Close()
}
