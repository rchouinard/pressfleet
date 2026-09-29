package deploy

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// Format is the archive format of a package.
type Format string

// Package formats.
const (
	FormatZip   Format = "zip"
	FormatTarGz Format = "tar.gz"
)

// Limits on a package, against archives that expand to fill the disk.
const (
	// MaxEntries is the most files and directories a package may hold.
	MaxEntries = 100_000
	// MaxUnpackedBytes is the most a package may hold, unpacked.
	MaxUnpackedBytes int64 = 2 << 30
	// maxNameLength bounds an entry's path; filesystems refuse longer ones.
	maxNameLength = 4096
)

// ErrInvalidPackage means a package can't be deployed as it is: it isn't a
// zip or tar.gz archive, or it holds something other than plain files and
// directories inside its own directory. The wrapping error says what.
var ErrInvalidPackage = errors.New("invalid package")

// Manifest summarizes a valid package.
type Manifest struct {
	Format Format `json:"format"`
	// Root is the single top-level directory the package's files are in,
	// which is left out when it is unpacked (as WordPress does with theme
	// and plugin zips); empty if the files are at the top level.
	Root  string `json:"root,omitempty"`
	Files int    `json:"files"`
	Dirs  int    `json:"dirs"`
	// Bytes is the unpacked size of the files.
	Bytes int64 `json:"bytes"`
}

// macOSJunk is the top-level directory the macOS archive utility adds for
// resource forks; it is left out of a package.
const macOSJunk = "__MACOSX"

// entry is one member of an archive.
type entry struct {
	name string // as in the archive
	dir  bool
	size int64 // declared size of a file
	// kind is what the entry is if it is neither a file nor a directory,
	// e.g. "a symbolic link"; empty otherwise.
	kind string
}

// entryFunc is called for every entry of an archive. content reads a
// file's content.
type entryFunc func(e entry, content func() (io.Reader, error)) error

// detect reads the archive's format from its first bytes and rewinds it.
func detect(f io.ReadSeeker) (Format, error) {
	var magic [4]byte
	n, err := io.ReadFull(f, magic[:])
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return "", fmt.Errorf("reading the package: %w", err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return "", fmt.Errorf("reading the package: %w", err)
	}
	switch {
	case n == 4 && bytes.Equal(magic[:], []byte("PK\x03\x04")):
		return FormatZip, nil
	case n >= 2 && magic[0] == 0x1f && magic[1] == 0x8b:
		return FormatTarGz, nil
	}
	return "", fmt.Errorf("%w: it is neither a zip nor a tar.gz archive", ErrInvalidPackage)
}

// walk calls fn for each entry of the archive in f.
func walk(f *os.File, format Format, fn entryFunc) error {
	switch format {
	case FormatZip:
		return walkZip(f, fn)
	case FormatTarGz:
		return walkTarGz(f, fn)
	}
	return fmt.Errorf("%w: unknown format %q", ErrInvalidPackage, format)
}

func walkZip(f *os.File, fn entryFunc) error {
	fi, err := f.Stat()
	if err != nil {
		return fmt.Errorf("reading the package: %w", err)
	}
	zr, err := zip.NewReader(f, fi.Size())
	if err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidPackage, err)
	}
	for _, zf := range zr.File {
		mode := zf.Mode()
		e := entry{name: zf.Name, dir: mode.IsDir() || strings.HasSuffix(zf.Name, "/"), size: int64(min(zf.UncompressedSize64, 1<<62))}
		switch t := mode.Type() &^ fs.ModeDir; {
		case zf.Flags&0x1 != 0:
			e.kind = "an encrypted file"
		case t&fs.ModeSymlink != 0:
			e.kind = "a symbolic link"
		case t&(fs.ModeDevice|fs.ModeCharDevice) != 0:
			e.kind = "a device file"
		case t != 0:
			e.kind = "a special file"
		}
		var rc io.ReadCloser
		err := fn(e, func() (io.Reader, error) {
			var err error
			rc, err = zf.Open()
			if err != nil {
				return nil, fmt.Errorf("%w: %q: %w", ErrInvalidPackage, zf.Name, err)
			}
			return rc, nil
		})
		if rc != nil {
			_ = rc.Close()
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func walkTarGz(f *os.File, fn entryFunc) error {
	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidPackage, err)
	}
	defer func() { _ = gz.Close() }()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("%w: %w", ErrInvalidPackage, err)
		}
		e := entry{name: hdr.Name, size: hdr.Size}
		switch hdr.Typeflag {
		case tar.TypeReg, '\x00': // '\x00' is the pre-POSIX regular file
		case tar.TypeDir:
			e.dir = true
		case tar.TypeXGlobalHeader:
			// PAX defaults for later entries, not an entry itself (git
			// archive writes one).
			continue
		case tar.TypeSymlink:
			e.kind = "a symbolic link"
		case tar.TypeLink:
			e.kind = "a hard link"
		case tar.TypeChar, tar.TypeBlock:
			e.kind = "a device file"
		case tar.TypeFifo:
			e.kind = "a named pipe"
		default:
			e.kind = fmt.Sprintf("an unsupported entry (type %q)", hdr.Typeflag)
		}
		if err := fn(e, func() (io.Reader, error) { return tr, nil }); err != nil {
			return err
		}
	}
}

// cleanName checks an entry's name and returns it as a relative, clean,
// slash-separated path, or "" for the archive's own root ("./").
func cleanName(raw string) (string, error) {
	bad := func(why string) error {
		return fmt.Errorf("%w: %q %s", ErrInvalidPackage, raw, why)
	}
	switch {
	case len(raw) > maxNameLength:
		return "", bad("is too long")
	case strings.ContainsAny(raw, "\x00\\"):
		return "", bad("contains a NUL byte or backslash")
	case strings.HasPrefix(raw, "/"):
		return "", bad("is an absolute path")
	}
	name := raw
	for strings.HasPrefix(name, "./") {
		name = name[2:]
	}
	name = strings.TrimSuffix(name, "/")
	if name == "" || name == "." {
		return "", nil
	}
	if path.Clean(name) != name || !filepath.IsLocal(name) {
		return "", bad("points outside the package or isn't a clean path")
	}
	for _, part := range strings.Split(name, "/") {
		if len(part) > 255 {
			return "", bad("has a path component that is too long")
		}
	}
	return name, nil
}

// Inspect checks that the archive in f is a package that can be unpacked
// safely, and summarizes it. Problems wrap ErrInvalidPackage:
//
//   - a format other than zip or tar.gz;
//   - an entry that is anything but a plain file or directory: symbolic
//     and hard links, device files, named pipes, encrypted zip entries;
//   - a path that is absolute, leaves the package (".." components, the
//     zip slip), or isn't clean;
//   - the same path twice, or a path below a file;
//   - no files at all, or more than MaxEntries entries or MaxUnpackedBytes.
//
// macOS resource forks (__MACOSX/) are left out. A package whose entries
// are all in one top-level directory has it as its Root.
func Inspect(f *os.File) (Manifest, error) {
	format, err := detect(f)
	if err != nil {
		return Manifest{}, err
	}
	m := Manifest{Format: format}
	seen := map[string]bool{} // clean name -> is a directory
	var names []string
	err = walk(f, format, func(e entry, _ func() (io.Reader, error)) error {
		name, err := cleanName(e.name)
		if err != nil {
			return err
		}
		if e.kind != "" {
			return fmt.Errorf("%w: %q is %s; packages may only hold files and directories", ErrInvalidPackage, e.name, e.kind)
		}
		if name == "" || isJunk(name) {
			return nil
		}
		if len(seen) >= MaxEntries {
			return fmt.Errorf("%w: it holds more than %d files and directories", ErrInvalidPackage, MaxEntries)
		}
		if dir, dup := seen[name]; dup {
			if dir && e.dir {
				return nil
			}
			return fmt.Errorf("%w: %q is in it twice", ErrInvalidPackage, e.name)
		}
		seen[name] = e.dir
		names = append(names, name)
		if e.dir {
			m.Dirs++
			return nil
		}
		m.Files++
		if e.size < 0 || e.size > MaxUnpackedBytes-m.Bytes {
			return fmt.Errorf("%w: it unpacks to more than %d MiB", ErrInvalidPackage, MaxUnpackedBytes>>20)
		}
		m.Bytes += e.size
		return nil
	})
	if _, serr := f.Seek(0, io.SeekStart); err == nil && serr != nil {
		err = fmt.Errorf("reading the package: %w", serr)
	}
	if err != nil {
		return Manifest{}, err
	}
	if m.Files == 0 {
		return Manifest{}, fmt.Errorf("%w: it holds no files", ErrInvalidPackage)
	}
	tops := map[string]bool{}
	for _, name := range names {
		// Every parent of an entry must be a directory, not a file.
		for dir := path.Dir(name); dir != "."; dir = path.Dir(dir) {
			if isDir, ok := seen[dir]; ok && !isDir {
				return Manifest{}, fmt.Errorf("%w: %q is a file, but %q is inside it", ErrInvalidPackage, dir, name)
			}
		}
		top, _, _ := strings.Cut(name, "/")
		tops[top] = true
	}
	if len(tops) == 1 {
		for top := range tops {
			// A single top-level directory, not a single file.
			if isDir, ok := seen[top]; !ok || isDir {
				m.Root = top
				if ok {
					m.Dirs--
				}
			}
		}
	}
	return m, nil
}

// isJunk reports whether a clean entry name is left out of packages.
func isJunk(name string) bool {
	return name == macOSJunk || strings.HasPrefix(name, macOSJunk+"/")
}

// Extract inspects the archive in f (see Inspect) and unpacks it into dest,
// which should be empty, without its Root directory. Everything is created
// through dest, so nothing lands outside it whatever the archive says:
// directories with mode 0755, files with mode 0644 (their modes in the
// archive, like set-user-ID bits, are ignored), owned by the caller. File
// contents are checked against the limits as they are written, since a
// zip's declared sizes can lie.
func Extract(f *os.File, dest *os.Root) (Manifest, error) {
	m, err := Inspect(f)
	if err != nil {
		return Manifest{}, err
	}
	budget := MaxUnpackedBytes
	err = walk(f, m.Format, func(e entry, content func() (io.Reader, error)) error {
		name, err := cleanName(e.name)
		if err != nil {
			return err
		}
		if name == "" || isJunk(name) {
			return nil
		}
		if m.Root != "" {
			name = strings.TrimPrefix(strings.TrimPrefix(name, m.Root), "/")
			if name == "" {
				return nil
			}
		}
		if e.dir {
			return dest.MkdirAll(name, 0o755)
		}
		if dir := path.Dir(name); dir != "." {
			if err := dest.MkdirAll(dir, 0o755); err != nil {
				return err
			}
		}
		r, err := content()
		if err != nil {
			return err
		}
		out, err := dest.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err != nil {
			return err
		}
		src := &sourceReader{r: io.LimitReader(r, budget+1)}
		n, err := io.Copy(out, src)
		if cerr := out.Close(); err == nil {
			err = cerr
		}
		switch {
		case src.err != nil:
			// Corrupt or truncated content, or a checksum mismatch.
			return fmt.Errorf("%w: %q: %w", ErrInvalidPackage, e.name, src.err)
		case err != nil:
			return fmt.Errorf("writing %q: %w", name, err)
		case n > budget:
			return fmt.Errorf("%w: it unpacks to more than %d MiB", ErrInvalidPackage, MaxUnpackedBytes>>20)
		}
		budget -= n
		return nil
	})
	if err != nil {
		return Manifest{}, err
	}
	return m, nil
}

// sourceReader remembers the error reading from the archive, to tell a
// broken package from a failure writing its files.
type sourceReader struct {
	r   io.Reader
	err error
}

func (s *sourceReader) Read(p []byte) (int, error) {
	n, err := s.r.Read(p)
	if err != nil && !errors.Is(err, io.EOF) {
		s.err = err
	}
	return n, err
}
