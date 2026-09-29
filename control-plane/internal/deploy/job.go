package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/rchouinard/pressfleet/control-plane/internal/id"
	"github.com/rchouinard/pressfleet/control-plane/internal/jobs"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/uploads"
)

// Step names. Keep them stable: a queued job resumes by step name.
const (
	stepPrepare          = "prepare"
	stepValidate         = "validate_package"
	stepStage            = "stage"
	stepSwap             = "swap"
	stepReloadCode       = "reload_code"
	stepActivate         = "activate"
	stepPurgeObjectCache = "purge_object_cache"
	stepPurgePageCache   = "purge_page_cache"
	stepHealthCheck      = "health_check"
	stepRemovePrevious   = "remove_previous"
	stepFinish           = "finish"
)

// Job data keys.
const (
	dataManifest    = "manifest"
	dataStagedInode = "staged_inode"
	dataSwapStarted = "swap_started"
	dataSwap        = "swap"
	dataReverted    = "reverted"
	// dataPreviousTheme is the active theme before activation;
	// dataWasActive whether the plugin was active before.
	dataPreviousTheme = "previous_theme"
	dataWasActive     = "was_active"
	dataActivated     = "activated"
	// dataCommitted marks that the health check passed: from then on the
	// job never swaps the previous version back.
	dataCommitted = "committed"
)

// PackageFile is the package's name in a task's transfer directory.
const PackageFile = "package"

// Handler returns the deployment job's handler, to register on the pool.
func (s *Service) Handler() jobs.Handler {
	return jobs.Handler{Type: JobType, Steps: []jobs.Step{
		{Name: stepPrepare, Do: s.prepare, Undo: s.summarizeRollback},
		{Name: stepValidate, Do: s.validate},
		{Name: stepStage, Do: s.stage, Undo: s.discard},
		{Name: stepSwap, Do: s.swap, Undo: s.revert},
		{Name: stepReloadCode, Do: s.reloadCode},
		{Name: stepActivate, Do: s.activate, Undo: s.deactivate},
		{Name: stepPurgeObjectCache, Do: s.purgeObjectCache},
		{Name: stepPurgePageCache, Do: s.purgePageCache},
		{Name: stepHealthCheck, Do: s.healthCheck},
		{Name: stepRemovePrevious, Do: s.removePrevious},
		{Name: stepFinish, Do: s.finish},
	}}
}

func jobPayload(r *jobs.Run) (payload, error) {
	var p payload
	if err := r.Payload(&p); err != nil {
		return payload{}, jobs.Permanent(err)
	}
	if !id.Valid(p.UploadID) || !p.Kind.Valid() || ValidateName(p.Name) != nil {
		return payload{}, jobs.Permanent(errors.New("the deployment job's payload names no valid upload or target"))
	}
	return p, nil
}

// jobSite loads the job's site and stops the job unless it can still take
// the deployment.
func (s *Service) jobSite(ctx context.Context, r *jobs.Run) (store.Site, error) {
	job := r.Job()
	if job.TargetType != "site" {
		return store.Site{}, jobs.Permanent(fmt.Errorf("%s job targets %s %q, want a site", job.Type, job.TargetType, job.TargetID))
	}
	site, err := deployableSite(ctx, s.db, job.TargetID)
	if errors.Is(err, ErrSiteNotFound) || errors.Is(err, ErrNotDeployable) {
		return store.Site{}, jobs.Permanent(err)
	}
	return site, err
}

// uploadError makes the errors of a claimed upload that is gone permanent.
func uploadError(err error) error {
	if errors.Is(err, uploads.ErrNotFound) || errors.Is(err, uploads.ErrNotClaimed) {
		return jobs.Permanent(fmt.Errorf("the package's upload: %w", err))
	}
	return err
}

// dataFlag reads a boolean from the job's data; absent is false.
func dataFlag(r *jobs.Run, key string) (bool, error) {
	var v bool
	_, err := r.Get(key, &v)
	return v, err
}

// lastAttempt reports whether a failing step should give up rather than be
// retried.
func lastAttempt(r *jobs.Run, err error) bool {
	return jobs.IsPermanent(err) || r.Job().Attempts >= r.Job().MaxAttempts
}

func (s *Service) prepare(ctx context.Context, r *jobs.Run) error {
	site, err := s.jobSite(ctx, r)
	if err != nil {
		return err
	}
	p, err := jobPayload(r)
	if err != nil {
		return err
	}
	u, err := s.uploads.Claimed(ctx, p.UploadID, r.Job().ID)
	if err != nil {
		return uploadError(err)
	}
	what := "Deploying"
	if p.Activate {
		what = "Deploying and activating"
	}
	r.Logf("%s %s %s/%s on %s from %s (%d bytes).", what, p.Kind, p.Kind.Dir(), p.Name, siteHost(site), displayName(u), u.SizeBytes)
	return nil
}

func displayName(u store.Upload) string {
	if u.Filename != "" {
		return fmt.Sprintf("%q", u.Filename)
	}
	return "upload " + u.ID
}

// validate inspects the package before anything touches the site.
func (s *Service) validate(ctx context.Context, r *jobs.Run) error {
	p, err := jobPayload(r)
	if err != nil {
		return err
	}
	f, _, err := s.uploads.Open(ctx, p.UploadID, r.Job().ID)
	if err != nil {
		return uploadError(err)
	}
	defer func() { _ = f.Close() }()
	m, err := Inspect(f)
	if errors.Is(err, ErrInvalidPackage) {
		return jobs.Permanent(err)
	}
	if err != nil {
		return err
	}
	if err := r.Set(dataManifest, m); err != nil {
		return err
	}
	root := "at its top level"
	if m.Root != "" {
		root = "in " + m.Root + "/"
	}
	r.Logf("The package is a valid %s archive: %d files in %d directories (%d bytes), %s.", m.Format, m.Files, m.Dirs, m.Bytes, root)
	return nil
}

// runTask runs a site task operation and decodes its JSON result into out.
// An operation that can't succeed (ExitPermanent) is permanent.
func (s *Service) runTask(ctx context.Context, siteID string, task provisioner.Task, out any) error {
	res, err := s.tasks.RunTask(ctx, siteID, task)
	var ee *provisioner.ExecError
	switch {
	case errors.As(err, &ee):
		msg := strings.TrimSpace(ee.Stderr)
		msg = strings.TrimPrefix(msg, "Error: ")
		if ee.ExitCode == ExitPermanent {
			return jobs.Permanent(errors.New(msg))
		}
		return fmt.Errorf("site task %s failed (status %d): %s", task.Command[len(TaskCommand)], ee.ExitCode, msg)
	case errors.Is(err, provisioner.ErrSiteNotFound):
		return jobs.Permanent(err)
	case err != nil:
		return err
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal([]byte(res.Stdout), out); err != nil {
		return fmt.Errorf("reading the site task's result: %w", err)
	}
	return nil
}

// stage unpacks the package into the target's staging directory.
func (s *Service) stage(ctx context.Context, r *jobs.Run) error {
	site, err := s.jobSite(ctx, r)
	if err != nil {
		return err
	}
	p, err := jobPayload(r)
	if err != nil {
		return err
	}
	transfer, err := s.transferDir(r.Job().ID)
	if err != nil {
		return err
	}
	defer func() {
		if err := os.RemoveAll(transfer); err != nil {
			s.log.ErrorContext(ctx, "removing a deployment transfer directory failed", "site_id", site.ID, "dir", transfer, "error", err)
		}
	}()
	if err := s.copyPackage(ctx, p.UploadID, r.Job().ID, filepath.Join(transfer, PackageFile)); err != nil {
		return err
	}
	var res UnpackResult
	err = s.runTask(ctx, site.ID, provisioner.Task{
		Command:     taskArgs(opUnpack, p.Kind, p.Name, r.Job().ID, 0),
		WPContent:   provisioner.MountReadWrite,
		TransferDir: transfer,
		Timeout:     unpackTimeout,
	}, &res)
	if err != nil {
		return fmt.Errorf("unpacking the package: %w", err)
	}
	if res.Inode == 0 {
		return errors.New("unpacking the package: the task didn't identify the staging directory")
	}
	if err := r.Set(dataStagedInode, res.Inode); err != nil {
		return err
	}
	r.Logf("Unpacked %d files as the site's user next to %s/%s.", res.Manifest.Files, p.Kind.Dir(), p.Name)
	return nil
}

// transferDir creates an empty transfer directory for the job, replacing
// one an earlier attempt left.
func (s *Service) transferDir(jobID string) (string, error) {
	tmp := s.layout.TmpDir()
	if err := os.MkdirAll(tmp, 0o700); err != nil {
		return "", fmt.Errorf("creating the transfer directory: %w", err)
	}
	dir := filepath.Join(tmp, "deploy-"+jobID)
	if err := os.RemoveAll(dir); err != nil {
		return "", fmt.Errorf("removing an earlier attempt's transfer directory: %w", err)
	}
	if err := os.Mkdir(dir, 0o700); err != nil {
		return "", fmt.Errorf("creating the transfer directory: %w", err)
	}
	return dir, nil
}

// copyPackage copies the upload for the task, which runs as the site's
// user and can't read the upload itself.
func (s *Service) copyPackage(ctx context.Context, uploadID, jobID, dest string) error {
	src, _, err := s.uploads.Open(ctx, uploadID, jobID)
	if err != nil {
		return uploadError(err)
	}
	defer func() { _ = src.Close() }()
	out, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // G304: under the data directory
	if err != nil {
		return fmt.Errorf("copying the package: %w", err)
	}
	_, err = io.Copy(out, src)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("copying the package: %w", err)
	}
	return nil
}

// stagedInode returns the staging directory's inode; 0 if it wasn't
// recorded.
func stagedInode(r *jobs.Run) (uint64, error) {
	var ino uint64
	_, err := r.Get(dataStagedInode, &ino)
	return ino, err
}

// discard removes the staging directory.
func (s *Service) discard(ctx context.Context, r *jobs.Run) error {
	p, err := jobPayload(r)
	if err != nil {
		return nil //nolint:nilerr // nothing can have been staged
	}
	ino, err := stagedInode(r)
	if err != nil {
		return err
	}
	err = s.runTask(ctx, r.Job().TargetID, provisioner.Task{
		Command:   taskArgs(opDiscard, p.Kind, p.Name, r.Job().ID, ino),
		WPContent: provisioner.MountReadWrite,
		Timeout:   taskTimeout,
	}, nil)
	switch {
	case errors.Is(err, provisioner.ErrSiteNotFound):
		return nil
	case err != nil && lastAttempt(r, err):
		r.Warnf("Couldn't remove the unpacked package: %v", err)
		return nil
	}
	return err
}

// swap puts the new version in the target's place.
func (s *Service) swap(ctx context.Context, r *jobs.Run) error {
	site, err := s.jobSite(ctx, r)
	if err != nil {
		return err
	}
	p, err := jobPayload(r)
	if err != nil {
		return err
	}
	ino, err := stagedInode(r)
	if err != nil {
		return err
	}
	if ino == 0 {
		return jobs.Permanent(ErrNotStaged)
	}
	if err := r.Set(dataSwapStarted, true); err != nil {
		return err
	}
	var res SwapResult
	if err := s.runTask(ctx, site.ID, provisioner.Task{
		Command:   taskArgs(opSwap, p.Kind, p.Name, r.Job().ID, ino),
		WPContent: provisioner.MountReadWrite,
		Timeout:   taskTimeout,
	}, &res); err != nil {
		return fmt.Errorf("swapping in the new version: %w", err)
	}
	if err := r.Set(dataSwap, res); err != nil {
		return err
	}
	switch {
	case !res.Previous:
		r.Logf("Installed %s/%s; there was no previous version.", p.Kind.Dir(), p.Name)
	case res.Atomic:
		r.Logf("Swapped the new version into %s/%s in one rename; the previous version is kept until the health check passes.", p.Kind.Dir(), p.Name)
	default:
		r.Warnf("Swapped the new version into %s/%s with two renames, since the filesystem can't exchange directories atomically; the previous version is kept until the health check passes.", p.Kind.Dir(), p.Name)
	}
	return nil
}

// revert puts the previous version back, unless the deployment was
// committed, then makes the site run it.
func (s *Service) revert(ctx context.Context, r *jobs.Run) error {
	for _, key := range []string{dataCommitted, dataReverted} {
		if done, err := dataFlag(r, key); err != nil || done {
			return err
		}
	}
	p, err := jobPayload(r)
	if err != nil {
		return nil //nolint:nilerr // nothing can have been swapped
	}
	ino, err := stagedInode(r)
	if err != nil || ino == 0 {
		return err
	}
	siteID := r.Job().TargetID
	var res RevertResult
	if err := s.runTask(ctx, siteID, provisioner.Task{
		Command:   taskArgs(opRevert, p.Kind, p.Name, r.Job().ID, ino),
		WPContent: provisioner.MountReadWrite,
		Timeout:   taskTimeout,
	}, &res); err != nil {
		if errors.Is(err, provisioner.ErrSiteNotFound) {
			return nil
		}
		return fmt.Errorf("putting the previous version back: %w", err)
	}
	if err := r.Set(dataReverted, true); err != nil {
		return err
	}
	switch {
	case res.Restored:
		r.Warnf("Put the previous version of %s/%s back.", p.Kind.Dir(), p.Name)
	case res.Reverted:
		r.Warnf("Removed %s/%s, which had no previous version.", p.Kind.Dir(), p.Name)
	default:
		return nil
	}
	// Best effort: the files are what matters.
	if err := s.prov.ReloadCode(ctx, siteID); err != nil {
		r.Warnf("Couldn't reload PHP after putting the previous version back: %v", err)
	}
	if err := s.caches.FlushCache(ctx, siteID); err != nil {
		r.Warnf("Couldn't flush the object cache after putting the previous version back: %v", err)
	}
	if err := s.prov.PurgeCache(ctx, siteID); err != nil {
		r.Warnf("Couldn't purge the page cache after putting the previous version back: %v", err)
	}
	return nil
}

func (s *Service) reloadCode(ctx context.Context, r *jobs.Run) error {
	site, err := s.jobSite(ctx, r)
	if err != nil {
		return err
	}
	if err := s.prov.ReloadCode(ctx, site.ID); err != nil {
		if errors.Is(err, provisioner.ErrSiteNotFound) {
			return jobs.Permanent(err)
		}
		return fmt.Errorf("reloading PHP: %w", err)
	}
	r.Logf("Reloaded PHP, so it runs the new version's files.")
	return nil
}

// wpcli runs WP-CLI on the deployed site. WP-CLI exiting with an error is
// permanent: the deployed code won't load any better on the next attempt.
func (s *Service) wpcli(ctx context.Context, siteID string, args ...string) (string, error) {
	out, err := s.prov.ExecWPCLI(ctx, siteID, args)
	var ee *provisioner.ExecError
	if errors.As(err, &ee) {
		return out, jobs.Permanent(fmt.Errorf("wp %s: %s", strings.Join(args, " "), trimWPCLIError(ee.Stderr)))
	}
	if errors.Is(err, provisioner.ErrSiteNotFound) || errors.Is(err, provisioner.ErrSiteNotRunning) {
		return out, jobs.Permanent(err)
	}
	return strings.TrimSpace(out), err
}

// maxWPCLIError bounds the WP-CLI output kept in an error.
const maxWPCLIError = 500

// trimWPCLIError keeps WP-CLI's error output up to a PHP stack trace.
func trimWPCLIError(stderr string) string {
	msg, _, _ := strings.Cut(stderr, "\nStack trace:")
	msg = strings.TrimSpace(msg)
	if len(msg) > maxWPCLIError {
		msg = msg[:maxWPCLIError-3] + "..."
	}
	return msg
}

// activate activates the theme or plugin, recording what was active.
func (s *Service) activate(ctx context.Context, r *jobs.Run) error {
	site, err := s.jobSite(ctx, r)
	if err != nil {
		return err
	}
	p, err := jobPayload(r)
	if err != nil {
		return err
	}
	if !p.Activate {
		r.Logf("Not activating: the request didn't ask for it.")
		return nil
	}
	switch p.Kind {
	case KindTheme:
		var previous string
		found, err := r.Get(dataPreviousTheme, &previous)
		if err != nil {
			return err
		}
		if !found {
			if previous, err = s.wpcli(ctx, site.ID, "option", "get", "stylesheet"); err != nil {
				return fmt.Errorf("reading the active theme: %w", err)
			}
			if err := r.Set(dataPreviousTheme, previous); err != nil {
				return err
			}
		}
		if previous == p.Name {
			r.Logf("The theme %s is already active.", p.Name)
			return nil
		}
		if _, err := s.wpcli(ctx, site.ID, "theme", "activate", p.Name); err != nil {
			return fmt.Errorf("activating the theme: %w", err)
		}
		r.Logf("Activated the theme %s (it replaces %s).", p.Name, previous)
	case KindPlugin:
		var active bool
		found, err := r.Get(dataWasActive, &active)
		if err != nil {
			return err
		}
		if !found {
			_, err := s.prov.ExecWPCLI(ctx, site.ID, []string{"plugin", "is-active", p.Name})
			var ee *provisioner.ExecError
			switch {
			case err == nil:
				active = true
			case errors.As(err, &ee) && ee.ExitCode == 1:
			default:
				return fmt.Errorf("checking whether the plugin is active: %w", err)
			}
			if err := r.Set(dataWasActive, active); err != nil {
				return err
			}
		}
		if active {
			r.Logf("The plugin %s is already active.", p.Name)
			return nil
		}
		if _, err := s.wpcli(ctx, site.ID, "plugin", "activate", p.Name); err != nil {
			return fmt.Errorf("activating the plugin: %w", err)
		}
		r.Logf("Activated the plugin %s.", p.Name)
	default:
		return nil
	}
	return r.Set(dataActivated, true)
}

// deactivate puts back what was active before activate. WP-CLI skips the
// deployed code, which may be what broke the site. A failure only warns:
// the swap's Undo takes the code away anyway.
func (s *Service) deactivate(ctx context.Context, r *jobs.Run) error {
	p, err := jobPayload(r)
	if err != nil || !p.Activate {
		return nil //nolint:nilerr // nothing was activated
	}
	siteID := r.Job().TargetID
	var args []string
	switch p.Kind {
	case KindTheme:
		var previous string
		if found, err := r.Get(dataPreviousTheme, &previous); err != nil || !found || previous == p.Name {
			return err
		}
		args = []string{"theme", "activate", previous, "--skip-themes=" + p.Name}
	case KindPlugin:
		var active bool
		if found, err := r.Get(dataWasActive, &active); err != nil || !found || active {
			return err
		}
		args = []string{"plugin", "deactivate", p.Name, "--skip-plugins=" + p.Name}
	default:
		return nil
	}
	if _, err := s.wpcli(ctx, siteID, args...); err != nil {
		if !lastAttempt(r, err) {
			return err
		}
		r.Warnf("Couldn't undo the activation: %v", err)
		return nil
	}
	r.Warnf("Undid the activation (wp %s).", strings.Join(args[:3], " "))
	return nil
}

func (s *Service) purgeObjectCache(ctx context.Context, r *jobs.Run) error {
	site, err := s.jobSite(ctx, r)
	if err != nil {
		return err
	}
	if err := s.caches.FlushCache(ctx, site.ID); err != nil {
		return fmt.Errorf("flushing the object cache: %w", err)
	}
	r.Logf("Flushed the object cache.")
	return nil
}

func (s *Service) purgePageCache(ctx context.Context, r *jobs.Run) error {
	site, err := s.jobSite(ctx, r)
	if err != nil {
		return err
	}
	if err := s.prov.PurgeCache(ctx, site.ID); err != nil {
		if errors.Is(err, provisioner.ErrSiteNotFound) {
			return jobs.Permanent(err)
		}
		return fmt.Errorf("purging the page cache: %w", err)
	}
	r.Logf("Purged the page cache.")
	return nil
}

// ErrUnhealthy means the deployed site failed its health check.
var ErrUnhealthy = errors.New("the site failed its health check")

// healthCheck checks that the home page answers without a server error and
// WordPress loads, with the platform's site health check (§5.5).
func (s *Service) healthCheck(ctx context.Context, r *jobs.Run) error {
	site, err := s.jobSite(ctx, r)
	if err != nil {
		return err
	}
	if err := s.health.CheckHealth(ctx, site, ""); err != nil {
		if ctx.Err() != nil {
			return err
		}
		// The check has already waited for the site to settle.
		return jobs.Permanent(unhealthyError{err})
	}
	r.Logf("Health check passed: the home page answers, and WordPress loads.")
	return nil
}

// unhealthyError is a failed health check. The checker's own message
// already says the site is unhealthy.
type unhealthyError struct{ err error }

func (e unhealthyError) Error() string {
	if strings.Contains(e.err.Error(), ErrUnhealthy.Error()) {
		return e.err.Error()
	}
	return ErrUnhealthy.Error() + ": " + e.err.Error()
}

func (e unhealthyError) Unwrap() []error { return []error{ErrUnhealthy, e.err} }

// removePrevious commits the deployment and removes the previous version.
func (s *Service) removePrevious(ctx context.Context, r *jobs.Run) error {
	p, err := jobPayload(r)
	if err != nil {
		return err
	}
	ino, err := stagedInode(r)
	if err != nil {
		return err
	}
	if err := r.Set(dataCommitted, true); err != nil {
		return err
	}
	err = s.runTask(ctx, r.Job().TargetID, provisioner.Task{
		Command:   taskArgs(opCleanup, p.Kind, p.Name, r.Job().ID, ino),
		WPContent: provisioner.MountReadWrite,
		Timeout:   taskTimeout,
	}, nil)
	if err != nil {
		if !lastAttempt(r, err) {
			return err
		}
		r.Warnf("The deployment is complete, but removing the previous version failed: %v", err)
		return nil
	}
	var swap SwapResult
	if _, err := r.Get(dataSwap, &swap); err != nil {
		return err
	}
	if swap.Previous {
		r.Logf("Removed the previous version.")
	}
	return nil
}

func (s *Service) finish(ctx context.Context, r *jobs.Run) error {
	p, err := jobPayload(r)
	if err != nil {
		return err
	}
	var (
		m    Manifest
		swap SwapResult
	)
	if _, err := r.Get(dataManifest, &m); err != nil {
		return err
	}
	if _, err := r.Get(dataSwap, &swap); err != nil {
		return err
	}
	activated, err := dataFlag(r, dataActivated)
	if err != nil {
		return err
	}
	result := map[string]any{
		"site_id": r.Job().TargetID, "kind": p.Kind, "name": p.Name, "path": p.Kind.Dir() + "/" + p.Name,
		"files": m.Files, "bytes": m.Bytes, "replaced": swap.Previous, "activated": activated,
	}
	for k, v := range result {
		if err := r.SetResult(k, v); err != nil {
			return err
		}
	}
	// The cleanup job removes it anyway once the job has ended.
	if err := s.uploads.Release(ctx, p.UploadID, r.Job().ID); err != nil {
		s.log.WarnContext(ctx, "releasing a deployed upload failed", "upload_id", p.UploadID, "job_id", r.Job().ID, "error", err)
	}
	r.Logf("Deployed %s/%s.", p.Kind.Dir(), p.Name)
	return nil
}

// summarizeRollback is the last Undo of a failed deployment: it says what
// the site runs now.
func (s *Service) summarizeRollback(_ context.Context, r *jobs.Run) error {
	started, err := dataFlag(r, dataSwapStarted)
	if err != nil {
		return err
	}
	if !started {
		r.Warnf("The deployment failed before changing the site's code.")
		return nil
	}
	if committed, err := dataFlag(r, dataCommitted); err != nil || committed {
		return err
	}
	r.Warnf("The deployment was rolled back: the site runs the version it had before.")
	return nil
}
