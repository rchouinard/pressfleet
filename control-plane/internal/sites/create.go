package sites

import (
	"context"
	"errors"
	"fmt"

	"github.com/rchouinard/pressfleet/control-plane/internal/backup/sitebackup"
	"github.com/rchouinard/pressfleet/control-plane/internal/jobs"
	"github.com/rchouinard/pressfleet/control-plane/internal/pluginupdates"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
	"github.com/rchouinard/pressfleet/control-plane/internal/secrets"
	"github.com/rchouinard/pressfleet/control-plane/internal/sitecron"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
)

// Step names of the site.create job. Keep them stable: a queued job resumes
// by step name.
const (
	stepPrepare          = "prepare"
	stepCreateDatabase   = "create_database"
	stepCreateCache      = "create_object_cache"
	stepStartStack       = "start_stack"
	stepInstallWordPress = "install_wordpress"
	stepFinish           = "finish"
)

// Job data keys holding the references to the site's database and object
// cache credentials.
const (
	dataDBSecretRef    = "db_secret_ref"    //nolint:gosec // G101: a job data key, not a credential
	dataCacheSecretRef = "cache_secret_ref" //nolint:gosec // G101: a job data key, not a credential
)

// maxStatusMessage bounds the error text kept in a site's status message.
const maxStatusMessage = 1000

func (s *Service) createHandler() jobs.Handler {
	return jobs.Handler{
		Type: CreateJobType,
		Steps: []jobs.Step{
			{Name: stepPrepare, Do: s.prepareCreate, Undo: s.failProvisioning},
			{Name: stepCreateDatabase, Do: s.createDatabase, Undo: s.dropDatabase},
			{Name: stepCreateCache, Do: s.createObjectCache, Undo: s.deleteObjectCache},
			{Name: stepStartStack, Do: s.startStack, Undo: s.deleteStack},
			{Name: stepInstallWordPress, Do: s.installWordPress},
			{Name: stepConfigurePlugins, Do: s.configureCreatedSitePlugins},
			{Name: stepFinish, Do: s.finishCreate},
		},
	}
}

// jobSite loads the site a job targets.
func (s *Service) jobSite(ctx context.Context, r *jobs.Run) (store.Site, error) {
	job := r.Job()
	if job.TargetType != store.JobTargetSite || job.TargetID == "" {
		return store.Site{}, jobs.Permanent(fmt.Errorf("%s job targets %s %q, want a site", job.Type, job.TargetType, job.TargetID))
	}
	site, err := store.NewSites(s.db).Get(ctx, job.TargetID)
	if errors.Is(err, store.ErrNotFound) {
		return store.Site{}, jobs.Permanent(fmt.Errorf("site %s: %w", job.TargetID, ErrNotFound))
	}
	return site, err
}

// prepareCreate stops a job whose site no longer wants provisioning, e.g.
// because a deletion was requested while the job waited to retry.
func (s *Service) prepareCreate(ctx context.Context, r *jobs.Run) error {
	site, err := s.jobSite(ctx, r)
	if err != nil {
		return err
	}
	if site.Status != store.SiteProvisioning {
		return jobs.Permanent(fmt.Errorf("site is %s, not provisioning", site.Status))
	}
	r.Logf("Provisioning %s (PHP %s, WordPress %s).", site.PreviewDomain, site.PHPVersion, site.WPCoreVersion)
	return nil
}

// failProvisioning is the last Undo of a failed site.create job: the steps
// after it have removed the stack, the object cache user, and the database. It deletes the secrets
// and the cron and backup schedules created with the site and marks the site failed,
// unless another request has changed its status meanwhile (a deletion).
func (s *Service) failProvisioning(ctx context.Context, r *jobs.Run) error {
	siteID := r.Job().TargetID
	msg := statusMessage("provisioning failed", r.Job().Error)
	err := s.db.InTx(ctx, func(q store.Querier) error {
		sites := store.NewSites(q)
		site, err := sites.Get(ctx, siteID)
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := s.deleteSiteSecrets(ctx, q, site); err != nil {
			return err
		}
		if err := sitecron.RemoveSchedule(ctx, q, siteID); err != nil {
			return err
		}
		if err := pluginupdates.RemoveSchedule(ctx, q, siteID); err != nil {
			return err
		}
		if err := sitebackup.RemoveSchedule(ctx, q, siteID); err != nil {
			return err
		}
		_, err = sites.SetStatus(ctx, siteID, store.SiteFailed, msg, store.SiteProvisioning)
		return err
	})
	if err != nil {
		return err
	}
	r.Logf("Removed the site's secrets; the site is marked failed.")
	return nil
}

// deleteSiteSecrets deletes the secrets the site row refers to and clears
// the references.
func (s *Service) deleteSiteSecrets(ctx context.Context, q store.Querier, site store.Site) error {
	sec := s.secrets.With(q)
	for _, ref := range []string{site.WPSaltsSecretRef, site.AdminPasswordSecretRef, site.PreviewPasswordSecretRef} {
		if ref == "" {
			continue
		}
		if err := sec.Delete(ctx, secrets.Ref(ref)); err != nil {
			return err
		}
	}
	return store.NewSites(q).ClearSecretRefs(ctx, site.ID)
}

func (s *Service) createDatabase(ctx context.Context, r *jobs.Run) error {
	siteID := r.Job().TargetID
	ref, err := s.databases.CreateDatabase(ctx, siteID)
	if err != nil {
		return err
	}
	if err := r.Set(dataDBSecretRef, ref); err != nil {
		return err
	}
	r.Logf("Database and user are ready.")
	return nil
}

func (s *Service) dropDatabase(ctx context.Context, r *jobs.Run) error {
	if err := s.databases.DropDatabase(ctx, r.Job().TargetID); err != nil {
		return err
	}
	r.Logf("Dropped the database and user.")
	return nil
}

// dbSecretRef returns the reference create_database stored, asking the
// database manager again (it is idempotent) if the job has none.
func (s *Service) dbSecretRef(ctx context.Context, r *jobs.Run) (string, error) {
	var ref string
	if ok, err := r.Get(dataDBSecretRef, &ref); err != nil || (ok && ref != "") {
		return ref, err
	}
	return s.databases.CreateDatabase(ctx, r.Job().TargetID)
}

func (s *Service) createObjectCache(ctx context.Context, r *jobs.Run) error {
	ref, err := s.caches.CreateUser(ctx, r.Job().TargetID)
	if err != nil {
		return err
	}
	if err := r.Set(dataCacheSecretRef, ref); err != nil {
		return err
	}
	r.Logf("Object cache user is ready.")
	return nil
}

func (s *Service) deleteObjectCache(ctx context.Context, r *jobs.Run) error {
	if err := s.caches.DeleteUser(ctx, r.Job().TargetID); err != nil {
		return err
	}
	r.Logf("Deleted the object cache user and its cached data.")
	return nil
}

// cacheSecretRef returns the reference create_object_cache stored, asking
// the object cache manager again (it is idempotent) if the job has none.
// For a site created before the object cache existed, that creates its
// user.
func (s *Service) cacheSecretRef(ctx context.Context, r *jobs.Run) (string, error) {
	var ref string
	if ok, err := r.Get(dataCacheSecretRef, &ref); err != nil || (ok && ref != "") {
		return ref, err
	}
	return s.caches.CreateUser(ctx, r.Job().TargetID)
}

// secretRefs returns the site's database and object cache credential
// references.
func (s *Service) secretRefs(ctx context.Context, r *jobs.Run) (db, cache string, err error) {
	if db, err = s.dbSecretRef(ctx, r); err != nil {
		return "", "", err
	}
	if cache, err = s.cacheSecretRef(ctx, r); err != nil {
		return "", "", err
	}
	return db, cache, nil
}

// SiteConfig returns the provisioner configuration of a site with the
// given aliases.
func SiteConfig(site store.Site, aliases []string, dbSecretRef, cacheSecretRef string) *provisioner.SiteConfig {
	return &provisioner.SiteConfig{
		ID:            site.ID,
		ClientGroupID: site.ClientGroupID,
		PreviewDomain: site.PreviewDomain,
		PrimaryDomain: site.PrimaryDomain,
		Aliases:       aliases,
		PHPVersion:    site.PHPVersion,
		WPCoreVersion: site.WPCoreVersion,
		EnableCache:   site.EnableCache,
		Resources: provisioner.ResourceLimits{
			CPU:                 site.CPULimit,
			MemoryReservationMB: site.MemoryReservationMB,
			MemoryLimitMB:       site.MemoryLimitMB,
		},
		DBSecretRef:      dbSecretRef,
		RedisSecretRef:   cacheSecretRef,
		UID:              site.UID,
		WPSaltsSecretRef: site.WPSaltsSecretRef,
		TablePrefix:      site.TablePrefix,
		PreviewAuth:      previewAuth(site),
		Maintenance:      site.Maintenance,
	}
}

func (s *Service) startStack(ctx context.Context, r *jobs.Run) error {
	site, err := s.jobSite(ctx, r)
	if err != nil {
		return err
	}
	if err := abortIfDeleting(site); err != nil {
		return err
	}
	dbRef, cacheRef, err := s.secretRefs(ctx, r)
	if err != nil {
		return err
	}
	cfg := SiteConfig(site, nil, dbRef, cacheRef)
	if err := cfg.Validate(); err != nil {
		// Retrying can't fix the site's own settings.
		return jobs.Permanent(err)
	}
	if cfg.PinnedImages, err = s.pinnedImages(ctx, site); err != nil {
		return err
	}
	st, err := s.prov.CreateSite(ctx, cfg)
	if err != nil {
		return err
	}
	if err := s.recordRender(ctx, site.ID); err != nil {
		return err
	}
	r.Logf("Containers are up (%s).", st.Phase)
	return nil
}

func (s *Service) deleteStack(ctx context.Context, r *jobs.Run) error {
	if err := s.prov.DeleteSite(ctx, r.Job().TargetID); err != nil {
		return err
	}
	r.Logf("Removed the containers, network, volumes, and rendered files.")
	return nil
}

// installWordPress runs `wp core install` unless WordPress is already
// installed, so a retry after an install that succeeded but wasn't recorded
// (a crash, a lost lease) doesn't fail.
func (s *Service) installWordPress(ctx context.Context, r *jobs.Run) error {
	site, err := s.jobSite(ctx, r)
	if err != nil {
		return err
	}
	if err := abortIfDeleting(site); err != nil {
		return err
	}
	_, err = s.prov.ExecWPCLI(ctx, site.ID, []string{"core", "is-installed"})
	var execErr *provisioner.ExecError
	switch {
	case err == nil:
		r.Logf("WordPress is already installed.")
		return nil
	case errors.As(err, &execErr) && execErr.ExitCode == 1:
		// Not installed yet.
	default:
		return fmt.Errorf("checking whether WordPress is installed: %w", err)
	}
	if site.AdminPasswordSecretRef == "" {
		return jobs.Permanent(errors.New("the site's generated admin password is missing"))
	}
	password, err := s.secrets.Get(ctx, secrets.Ref(site.AdminPasswordSecretRef))
	if err != nil {
		return fmt.Errorf("reading the admin password: %w", err)
	}
	// The arguments go to WP-CLI as argv, never through a shell, and the
	// provisioner keeps them out of errors and logs.
	_, err = s.prov.ExecWPCLI(ctx, site.ID, []string{
		"core", "install",
		"--url=" + URL(site),
		"--title=" + site.Name,
		"--admin_user=" + site.AdminUsername,
		"--admin_password=" + password.Reveal(),
		"--admin_email=" + site.AdminEmail,
		"--skip-email",
	})
	if err != nil {
		return fmt.Errorf("installing WordPress: %w", err)
	}
	r.Logf("Installed WordPress at %s with administrator %s.", URL(site), site.AdminUsername)
	return nil
}

// finishCreate marks the site running. A site whose status changed while
// the job ran (a deletion was requested) keeps its new status.
func (s *Service) finishCreate(ctx context.Context, r *jobs.Run) error {
	site, err := s.jobSite(ctx, r)
	if err != nil {
		return err
	}
	changed, err := store.NewSites(s.db).SetStatus(ctx, site.ID, store.SiteRunning, "", store.SiteProvisioning)
	if err != nil {
		return err
	}
	if err := r.SetResult("site_id", site.ID); err != nil {
		return err
	}
	if !changed {
		r.Warnf("The site is %s now; leaving its status alone.", site.Status)
		return nil
	}
	r.Logf("The site is ready at %s.", URL(site))
	return nil
}
