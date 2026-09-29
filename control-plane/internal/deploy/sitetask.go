package deploy

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"

	"github.com/rchouinard/pressfleet/control-plane/internal/id"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
)

// The operations below run inside a site task (provisioner.Task): a
// short-lived container of the control plane's own image, as the site's
// UID, with the site's wp-content mounted read-write. The control plane
// starts them as `pressfleet site-task <op> ...` ([TaskCommand]); the CLI's
// hidden site-task command hands its arguments to [RunTaskCommand], which
// prints the result as JSON.
//
// A deployment job uses three hidden names next to its target, in the
// target's directory (themes, plugins, or mu-plugins), all ending with the
// job's ID so a retried step finds what an earlier attempt left:
//
//   - staging, .pressfleet-deploy-<job>: the unpacked package;
//   - previous, .pressfleet-previous-<job>: the version it replaced, kept
//     until the health check passes;
//   - discard, .pressfleet-discard-<job>: a version on its way out.
//
// WordPress skips hidden directories when it looks for themes and plugins,
// and only loads top-level files from mu-plugins, so none of them is ever
// loaded, and nginx refuses to serve hidden paths.
//
// The swap is atomic: renameat2 with RENAME_EXCHANGE trades the staging
// directory and the target in one step, so every request sees either the
// old version or the new one, never a mix or a missing directory. (On a
// filesystem without it, two renames leave the target missing for an
// instant, which the result reports.) The staging directory's inode number,
// recorded when it is unpacked, tells the operations which version is
// where, so each one is idempotent: run again after a crash, it finishes
// the job or finds it done.

// TaskCommand is the command a site task runs: the control plane's own
// binary, whose hidden site-task command runs RunTaskCommand.
var TaskCommand = []string{"pressfleet", "site-task"}

// Site task operations.
const (
	opUnpack  = "deploy-unpack"
	opSwap    = "deploy-swap"
	opRevert  = "deploy-revert"
	opDiscard = "deploy-discard"
	opCleanup = "deploy-cleanup"
)

// ExitPermanent is the exit status of a site task operation that can't
// succeed however often it is retried, such as an invalid package.
const ExitPermanent = 3

// Errors of the site task operations. Like ErrInvalidPackage, they are
// permanent (IsPermanentTaskError).
var (
	// ErrTargetNotDirectory: the target exists and isn't a directory, so
	// a package can't replace it.
	ErrTargetNotDirectory = errors.New("the target exists and is not a directory")
	// ErrNotStaged: the unpacked package isn't where the job left it.
	ErrNotStaged = errors.New("the unpacked package is missing")
	// ErrUnexpectedState: the target's directory holds something the job
	// didn't expect, e.g. a previous version where there shouldn't be one.
	ErrUnexpectedState = errors.New("the target directory is in an unexpected state")
)

// errNoExchange means the filesystem can't swap two names atomically.
var errNoExchange = errors.New("atomic exchange is not supported")

// IsPermanentTaskError reports whether a site task operation's error is
// one that retrying can't fix.
func IsPermanentTaskError(err error) bool {
	for _, target := range []error{ErrInvalidPackage, ErrTargetNotDirectory, ErrNotStaged, ErrUnexpectedState} {
		if errors.Is(err, target) {
			return true
		}
	}
	return false
}

func stagingName(jobID string) string  { return ".pressfleet-deploy-" + jobID }
func previousName(jobID string) string { return ".pressfleet-previous-" + jobID }
func discardName(jobID string) string  { return ".pressfleet-discard-" + jobID }

// UnpackResult is the result of unpacking a package into its staging
// directory.
type UnpackResult struct {
	Manifest Manifest `json:"manifest"`
	// Inode identifies the staging directory, across renames.
	Inode uint64 `json:"inode"`
}

// SwapResult is the result of swapping the new version in.
type SwapResult struct {
	// Previous reports whether a previous version was replaced (and is
	// kept under the previous name); false for a first deployment.
	Previous bool `json:"previous"`
	// Atomic is false if the filesystem couldn't exchange the directories
	// in one step (see the package documentation).
	Atomic bool `json:"atomic"`
}

// RevertResult is the result of undoing a swap.
type RevertResult struct {
	// Reverted reports whether the new version was in place and has been
	// taken out.
	Reverted bool `json:"reverted"`
	// Restored reports whether a previous version was put back.
	Restored bool `json:"restored"`
}

// targetDir is a deployment target's directory in wp-content, opened
// through an os.Root so nothing outside it is reached, even through
// symbolic links the site created.
type targetDir struct {
	root *os.Root // the kind's directory (themes, plugins, mu-plugins)
	name string
	job  string
}

// openTarget opens the target's directory under wpContent, creating it if
// create is set (mu-plugins often doesn't exist). found is false if it
// doesn't exist and create isn't set.
func openTarget(wpContent string, kind Kind, name, jobID string, create bool) (t targetDir, found bool, err error) {
	if err := checkTarget(kind, name, jobID); err != nil {
		return targetDir{}, false, err
	}
	wc, err := os.OpenRoot(wpContent)
	if err != nil {
		return targetDir{}, false, err
	}
	defer func() { _ = wc.Close() }()
	if create {
		if err := wc.MkdirAll(kind.Dir(), 0o755); err != nil {
			return targetDir{}, false, err
		}
	}
	root, err := wc.OpenRoot(kind.Dir())
	if errors.Is(err, fs.ErrNotExist) && !create {
		return targetDir{}, false, nil
	}
	if err != nil {
		return targetDir{}, false, err
	}
	return targetDir{root: root, name: name, job: jobID}, true, nil
}

func checkTarget(kind Kind, name, jobID string) error {
	if !kind.Valid() {
		return fmt.Errorf("unknown kind %q", kind)
	}
	if err := ValidateName(name); err != nil {
		return err
	}
	if !id.Valid(jobID) {
		return fmt.Errorf("invalid job ID %q", jobID)
	}
	return nil
}

func (t targetDir) close() { _ = t.root.Close() }

// stat returns the inode of the named entry, and whether it exists.
func (t targetDir) stat(name string) (fs.FileInfo, bool, error) {
	fi, err := t.root.Lstat(name)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return fi, true, nil
}

// exchangeOrRename puts the directory named from where to is, keeping
// what was at to under keep: atomically if the filesystem can.
func (t targetDir) exchangeOrRename(from, to, keep string) (atomic bool, err error) {
	dir, err := t.root.Open(".")
	if err != nil {
		return false, err
	}
	defer func() { _ = dir.Close() }()
	err = exchange(dir, from, to)
	if err == nil {
		// from now holds what was at to.
		return true, t.root.Rename(from, keep)
	}
	if !errors.Is(err, errNoExchange) {
		return false, err
	}
	if err := t.root.Rename(to, keep); err != nil {
		return false, err
	}
	return false, t.root.Rename(from, to)
}

// Unpack checks the package at archive and unpacks it into the target's
// staging directory, replacing what an earlier attempt left there.
func Unpack(wpContent string, kind Kind, name, jobID, archive string) (UnpackResult, error) {
	f, err := os.Open(archive) //nolint:gosec // G304: the path is the task's own transfer file
	if err != nil {
		return UnpackResult{}, err
	}
	defer func() { _ = f.Close() }()
	// Before touching the site.
	if _, err := Inspect(f); err != nil {
		return UnpackResult{}, err
	}
	t, _, err := openTarget(wpContent, kind, name, jobID, true)
	if err != nil {
		return UnpackResult{}, err
	}
	defer t.close()
	if fi, ok, err := t.stat(name); err != nil {
		return UnpackResult{}, err
	} else if ok && !fi.IsDir() {
		return UnpackResult{}, fmt.Errorf("%w: %s/%s", ErrTargetNotDirectory, kind.Dir(), name)
	}
	staging := stagingName(jobID)
	if err := t.root.RemoveAll(staging); err != nil {
		return UnpackResult{}, err
	}
	if err := t.root.Mkdir(staging, 0o755); err != nil {
		return UnpackResult{}, err
	}
	dest, err := t.root.OpenRoot(staging)
	if err != nil {
		return UnpackResult{}, err
	}
	m, err := Extract(f, dest)
	_ = dest.Close()
	if err != nil {
		_ = t.root.RemoveAll(staging)
		return UnpackResult{}, err
	}
	fi, err := t.root.Lstat(staging)
	if err != nil {
		return UnpackResult{}, err
	}
	ino := inode(fi)
	if ino == 0 {
		return UnpackResult{}, errors.New("can't identify the staging directory on this system")
	}
	return UnpackResult{Manifest: m, Inode: ino}, nil
}

// Swap puts the staged package, identified by its inode, in the target's
// place, keeping the version it replaces under the previous name.
func Swap(wpContent string, kind Kind, name, jobID string, staged uint64) (SwapResult, error) {
	t, _, err := openTarget(wpContent, kind, name, jobID, true)
	if err != nil {
		return SwapResult{}, err
	}
	defer t.close()
	staging, previous := stagingName(jobID), previousName(jobID)
	tfi, tok, err := t.stat(name)
	if err != nil {
		return SwapResult{}, err
	}
	sfi, sok, err := t.stat(staging)
	if err != nil {
		return SwapResult{}, err
	}
	_, pok, err := t.stat(previous)
	if err != nil {
		return SwapResult{}, err
	}
	switch {
	case tok && inode(tfi) == staged:
		// Swapped already. After an exchange, staging holds the previous
		// version until it is renamed.
		if sok {
			if pok {
				return SwapResult{}, fmt.Errorf("%w: both %s and %s exist", ErrUnexpectedState, staging, previous)
			}
			if err := t.root.Rename(staging, previous); err != nil {
				return SwapResult{}, err
			}
			return SwapResult{Previous: true, Atomic: true}, nil
		}
		return SwapResult{Previous: pok, Atomic: true}, nil
	case !sok || inode(sfi) != staged:
		return SwapResult{}, fmt.Errorf("%w: %s/%s", ErrNotStaged, kind.Dir(), staging)
	case !tok:
		// A first deployment, or the second rename of a non-atomic swap.
		if err := t.root.Rename(staging, name); err != nil {
			return SwapResult{}, err
		}
		return SwapResult{Previous: pok, Atomic: !pok}, nil
	case !tfi.IsDir():
		return SwapResult{}, fmt.Errorf("%w: %s/%s", ErrTargetNotDirectory, kind.Dir(), name)
	case pok:
		return SwapResult{}, fmt.Errorf("%w: %s exists before the swap", ErrUnexpectedState, previous)
	}
	atomic, err := t.exchangeOrRename(staging, name, previous)
	if err != nil {
		return SwapResult{}, err
	}
	return SwapResult{Previous: true, Atomic: atomic}, nil
}

// Revert undoes Swap: if the staged version is in the target's place, it
// puts the previous version back (or, for a first deployment, removes the
// target), then removes the staged version.
func Revert(wpContent string, kind Kind, name, jobID string, staged uint64) (RevertResult, error) {
	t, found, err := openTarget(wpContent, kind, name, jobID, false)
	if err != nil || !found {
		return RevertResult{}, err
	}
	defer t.close()
	staging, previous, discard := stagingName(jobID), previousName(jobID), discardName(jobID)
	tfi, tok, err := t.stat(name)
	if err != nil {
		return RevertResult{}, err
	}
	sfi, sok, err := t.stat(staging)
	if err != nil {
		return RevertResult{}, err
	}
	pfi, pok, err := t.stat(previous)
	if err != nil {
		return RevertResult{}, err
	}
	var res RevertResult
	if tok && inode(tfi) == staged {
		// The new version is in place. The old one is under the previous
		// name, or still under the staging name after an exchange.
		old := ""
		switch {
		case pok:
			old = previous
		case sok && inode(sfi) != staged:
			old = staging
		}
		if old != "" {
			if _, err := t.exchangeOrRename(old, name, discard); err != nil {
				return RevertResult{}, err
			}
			res = RevertResult{Reverted: true, Restored: true}
		} else {
			if err := t.root.Rename(name, discard); err != nil {
				return RevertResult{}, err
			}
			res = RevertResult{Reverted: true}
		}
	} else {
		if !tok && pok {
			// A non-atomic swap stopped between its renames.
			if err := t.root.Rename(previous, name); err != nil {
				return RevertResult{}, err
			}
			res.Restored = true
		}
		// A revert stopped after its exchange leaves the new version
		// under the name the old one had.
		for _, e := range []struct {
			name string
			fi   fs.FileInfo
			ok   bool
		}{{previous, pfi, pok && !res.Restored}, {staging, sfi, sok}} {
			if e.ok && inode(e.fi) == staged {
				if err := t.root.RemoveAll(e.name); err != nil {
					return RevertResult{}, err
				}
			}
		}
	}
	// The discarded version is hidden, so it can go bit by bit.
	return res, t.root.RemoveAll(discard)
}

// Discard removes the staging directory if it holds the staged package
// (or, with staged 0, whatever is there: an unpacking that never recorded
// its inode never got to a swap either).
func Discard(wpContent string, kind Kind, name, jobID string, staged uint64) error {
	t, found, err := openTarget(wpContent, kind, name, jobID, false)
	if err != nil || !found {
		return err
	}
	defer t.close()
	staging := stagingName(jobID)
	fi, ok, err := t.stat(staging)
	if err != nil || !ok {
		return err
	}
	if staged != 0 && inode(fi) != staged {
		return fmt.Errorf("%w: %s holds another version than the staged one", ErrUnexpectedState, staging)
	}
	return t.root.RemoveAll(staging)
}

// Cleanup removes the previous version once the new one has passed its
// health check.
func Cleanup(wpContent string, kind Kind, name, jobID string, staged uint64) error {
	t, found, err := openTarget(wpContent, kind, name, jobID, false)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("%w: %s is missing", ErrUnexpectedState, kind.Dir())
	}
	defer t.close()
	fi, ok, err := t.stat(name)
	if err != nil {
		return err
	}
	if !ok || inode(fi) != staged {
		return fmt.Errorf("%w: the deployed version isn't in %s/%s", ErrUnexpectedState, kind.Dir(), name)
	}
	for _, n := range []string{previousName(jobID), stagingName(jobID), discardName(jobID)} {
		if err := t.root.RemoveAll(n); err != nil {
			return err
		}
	}
	return nil
}

// taskArgs is the argv of a site task operation on a deployment target.
func taskArgs(op string, kind Kind, name, jobID string, staged uint64, extra ...string) []string {
	args := append([]string{}, TaskCommand...)
	args = append(args, op, "--kind", string(kind), "--name", name, "--job", jobID)
	if staged != 0 {
		args = append(args, "--inode", fmt.Sprint(staged))
	}
	return append(args, extra...)
}

// RunTaskCommand runs the site task operation that args (after
// "site-task") name and writes its result to stdout as JSON. Errors that
// retrying can't fix satisfy IsPermanentTaskError.
func RunTaskCommand(args []string, stdout io.Writer) error {
	if len(args) == 0 {
		return errors.New("site-task: an operation is required")
	}
	op := args[0]
	fs := flag.NewFlagSet("site-task "+op, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var (
		wpContent = fs.String("wp-content", provisioner.TaskWPContentPath, "the site's wp-content")
		kind      = fs.String("kind", "", "theme, plugin, or mu-plugin")
		name      = fs.String("name", "", "the target's directory name")
		jobID     = fs.String("job", "", "the deployment job's ID")
		staged    = fs.Uint64("inode", 0, "the staging directory's inode")
		archive   = fs.String("archive", path.Join(provisioner.TaskTransferPath, PackageFile), "the package")
	)
	if err := fs.Parse(args[1:]); err != nil {
		return fmt.Errorf("site-task %s: %w", op, err)
	}
	k := Kind(*kind)
	var (
		out any
		err error
	)
	switch op {
	case opUnpack:
		out, err = Unpack(*wpContent, k, *name, *jobID, *archive)
	case opSwap:
		out, err = Swap(*wpContent, k, *name, *jobID, *staged)
	case opRevert:
		out, err = Revert(*wpContent, k, *name, *jobID, *staged)
	case opDiscard:
		out, err = struct{}{}, Discard(*wpContent, k, *name, *jobID, *staged)
	case opCleanup:
		out, err = struct{}{}, Cleanup(*wpContent, k, *name, *jobID, *staged)
	default:
		return fmt.Errorf("site-task: unknown operation %q", op)
	}
	if err != nil {
		return err
	}
	return json.NewEncoder(stdout).Encode(out)
}
