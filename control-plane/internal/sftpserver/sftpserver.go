// Package sftpserver is the SFTP server that runs in the SFTP gateway's
// per-session workers (design doc §5.8): `pressfleet sftp-server` speaks
// SFTP on its standard input and output, which the driver attaches to the
// gateway's SSH channel (provisioner.SessionRunner).
//
// Every path a client names is resolved inside one directory, the site's
// wp-content, through an [os.Root]: ".." and symbolic links can't lead out
// of it. The worker already mounts nothing else of the host and runs as the
// site's UID; the root keeps the client out of the worker image's own files
// too, so the client sees wp-content as "/" and nothing else.
//
// The server offers what file transfer clients use: reading, writing,
// listing, creating and removing directories, renaming, removing, and
// changing modes and times. Creating symbolic and hard links is refused:
// their targets would be paths in the client's view, not the server's, and
// nothing in wp-content needs them.
//
// It uses github.com/pkg/sftp's request server, which implements the SFTP
// protocol (version 3, as OpenSSH, WinSCP, and FileZilla speak it) and
// leaves every file system operation to the handlers here.
package sftpserver

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"syscall"

	"github.com/pkg/sftp"
)

// Serve serves SFTP on in and out, with every path confined to the
// directory root, until the client ends the stream. A clean end is not an
// error.
func Serve(in io.Reader, out io.Writer, root string) error {
	r, err := os.OpenRoot(root)
	if err != nil {
		return fmt.Errorf("sftp server: %w", err)
	}
	defer func() { _ = r.Close() }()
	srv := sftp.NewRequestServer(stream{in, out}, Handlers(r))
	err = srv.Serve()
	_ = srv.Close()
	if err == nil || errors.Is(err, io.EOF) {
		return nil
	}
	return fmt.Errorf("sftp server: %w", err)
}

// stream joins the two halves of the session into the ReadWriteCloser the
// request server wants. Closing it closes nothing: the process owns its
// standard streams.
type stream struct {
	io.Reader
	io.Writer
}

func (stream) Close() error { return nil }

// Handlers returns request server handlers that act on the files inside
// root. Tests serve them over in-memory pipes.
func Handlers(root *os.Root) sftp.Handlers {
	h := &handlers{root: root}
	return sftp.Handlers{FileGet: h, FilePut: h, FileCmd: h, FileList: h}
}

// handlers implements the request server's handler interfaces on an
// os.Root.
type handlers struct {
	root *os.Root
}

var (
	_ sftp.OpenFileWriter       = (*handlers)(nil)
	_ sftp.PosixRenameFileCmder = (*handlers)(nil)
	_ sftp.LstatFileLister      = (*handlers)(nil)
	_ sftp.ReadlinkFileLister   = (*handlers)(nil)
)

// rel turns a request path, which the request server has cleaned into an
// absolute path in the client's view, into a path relative to the root.
func rel(p string) string {
	p = path.Clean("/" + p)
	if p == "/" {
		return "."
	}
	return p[1:]
}

// newFileMode is the mode of files and directories clients create, before
// the process umask; clients can change it afterwards.
const (
	newFileMode = 0o644
	newDirMode  = 0o755
)

// Fileread opens a file for reading.
func (h *handlers) Fileread(r *sftp.Request) (io.ReaderAt, error) {
	return h.root.Open(rel(r.Filepath))
}

// Filewrite opens a file for writing.
func (h *handlers) Filewrite(r *sftp.Request) (io.WriterAt, error) {
	return h.openFile(r)
}

// OpenFile opens a file for reading and writing.
func (h *handlers) OpenFile(r *sftp.Request) (sftp.WriterAtReaderAt, error) {
	return h.openFile(r)
}

func (h *handlers) openFile(r *sftp.Request) (*os.File, error) {
	pf := r.Pflags()
	var flag int
	switch {
	case pf.Read && pf.Write:
		flag = os.O_RDWR
	case pf.Write, pf.Append, pf.Creat, pf.Trunc:
		flag = os.O_WRONLY
	default:
		flag = os.O_RDONLY
	}
	// Append isn't passed on: clients send offsets with every write, and
	// O_APPEND would make the kernel ignore them.
	if pf.Creat {
		flag |= os.O_CREATE
	}
	if pf.Trunc {
		flag |= os.O_TRUNC
	}
	if pf.Excl {
		flag |= os.O_EXCL
	}
	return h.root.OpenFile(rel(r.Filepath), flag, newFileMode)
}

// Filecmd changes the file system.
func (h *handlers) Filecmd(r *sftp.Request) error {
	name := rel(r.Filepath)
	switch r.Method {
	case "Setstat":
		return h.setstat(name, r)
	case "Rename":
		// SFTP version 3 renames refuse to replace an existing file;
		// PosixRename replaces it.
		if _, err := h.root.Lstat(rel(r.Target)); err == nil {
			return &os.LinkError{Op: "rename", Old: r.Filepath, New: r.Target, Err: os.ErrExist}
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return h.root.Rename(name, rel(r.Target))
	case "Rmdir":
		fi, err := h.root.Lstat(name)
		if err != nil {
			return err
		}
		if !fi.IsDir() {
			return &os.PathError{Op: "rmdir", Path: r.Filepath, Err: syscall.ENOTDIR}
		}
		return h.root.Remove(name)
	case "Remove":
		fi, err := h.root.Lstat(name)
		if err != nil {
			return err
		}
		if fi.IsDir() {
			return &os.PathError{Op: "remove", Path: r.Filepath, Err: syscall.EISDIR}
		}
		return h.root.Remove(name)
	case "Mkdir":
		return h.root.Mkdir(name, newDirMode)
	}
	// Link, Symlink, and anything newer.
	return sftp.ErrSSHFxOpUnsupported
}

// PosixRename renames, replacing an existing target.
func (h *handlers) PosixRename(r *sftp.Request) error {
	return h.root.Rename(rel(r.Filepath), rel(r.Target))
}

func (h *handlers) setstat(name string, r *sftp.Request) error {
	flags, attrs := r.AttrFlags(), r.Attributes()
	if flags.Size {
		f, err := h.root.OpenFile(name, os.O_WRONLY, 0)
		if err != nil {
			return err
		}
		err = f.Truncate(int64(attrs.Size)) //nolint:gosec // G115: a file size from the client; the kernel rejects impossible ones
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return err
		}
	}
	if flags.Permissions {
		if err := h.root.Chmod(name, attrs.FileMode().Perm()); err != nil {
			return err
		}
	}
	if flags.Acmodtime {
		if err := h.root.Chtimes(name, attrs.AccessTime(), attrs.ModTime()); err != nil {
			return err
		}
	}
	if flags.UidGid {
		// The worker runs as the site's user without CAP_CHOWN, so the
		// kernel refuses anything but a no-op.
		if err := h.root.Chown(name, int(attrs.UID), int(attrs.GID)); err != nil {
			return err
		}
	}
	return nil
}

// Filelist lists a directory or stats a file.
func (h *handlers) Filelist(r *sftp.Request) (sftp.ListerAt, error) {
	name := rel(r.Filepath)
	switch r.Method {
	case "List":
		d, err := h.root.Open(name)
		if err != nil {
			return nil, err
		}
		defer func() { _ = d.Close() }()
		entries, err := d.ReadDir(-1)
		if err != nil {
			return nil, err
		}
		infos := make(listerAt, 0, len(entries))
		for _, e := range entries {
			fi, err := e.Info()
			if errors.Is(err, os.ErrNotExist) {
				continue // removed since it was listed
			} else if err != nil {
				return nil, err
			}
			infos = append(infos, fi)
		}
		return infos, nil
	case "Stat":
		fi, err := h.root.Stat(name)
		if err != nil {
			return nil, err
		}
		return listerAt{fi}, nil
	}
	return nil, sftp.ErrSSHFxOpUnsupported
}

// Lstat stats a file without following a final symbolic link.
func (h *handlers) Lstat(r *sftp.Request) (sftp.ListerAt, error) {
	fi, err := h.root.Lstat(rel(r.Filepath))
	if err != nil {
		return nil, err
	}
	return listerAt{fi}, nil
}

// Readlink returns a symbolic link's target as stored. Links in wp-content
// come from elsewhere (the server doesn't create any), and a target
// outside it can't be followed through this server anyway.
func (h *handlers) Readlink(p string) (string, error) {
	return h.root.Readlink(rel(p))
}

// listerAt serves a fixed list of entries.
type listerAt []os.FileInfo

// ListAt copies entries from offset into ls.
func (l listerAt) ListAt(ls []os.FileInfo, offset int64) (int, error) {
	if offset >= int64(len(l)) {
		return 0, io.EOF
	}
	n := copy(ls, l[offset:])
	if n < len(ls) {
		return n, io.EOF
	}
	return n, nil
}
