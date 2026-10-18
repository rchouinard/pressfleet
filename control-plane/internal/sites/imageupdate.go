package sites

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/catalog"
	"github.com/rchouinard/pressfleet/control-plane/internal/jobs"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
)

// Platform image updates (#58, design doc §5.5).
//
// The platform rebuilds the site images for PHP patch releases, security
// fixes, and base OS updates without changing their tags: a site on PHP 8.4
// and WordPress 7.1.2 always runs site-php:php8.4-wp7.1.2 and
// site-nginx:wp7.1.2, but the builds behind them change.
//
// # Pins
//
// A site's containers therefore run pinned builds: SiteConfig.PinnedImages,
// references to the exact builds (repository@sha256:<digest> in Compose),
// which the driver resolves the tags to (provisioner.ImagePinner) and the
// state database keeps (store.ImagePins, keyed by site and tag). Whenever a
// site is rendered, a tag without a pin is pinned to the build the driver
// has for it (pinnedImages); a pinned tag keeps its build, whatever the
// tag names by then. So a site only moves to a new build when the image
// rollout moves it, with a health check, and never by the way: not when
// another site's update pulls the tag, and not when the site is re-rendered
// for another reason. A PHP or core version change selects other tags,
// which the site gets pinned to on the way; a reverted core update goes
// back to the old tags' pins, the exact builds the site ran before.
//
// # Finding new builds
//
// The image rollout ("images", rollout.go) looks at the tags running sites
// use. Every ImageUpdateOptions.PullInterval it pulls each from its
// registry (updates.images.check_hours); every pass it asks the driver for
// the build each tag names now, which also finds builds that reached the
// host another way (a local build, a pull by hand). A site whose pin of a
// tag differs from that build is a candidate. The rollout's fingerprint is
// the set of newest builds, so a new build lifts a halt, like a new
// WordPress release does for the core rollout.
//
// # The site update
//
// A [ImageUpdateJobType] job moves one site's pins to the new builds of the
// tags it runs, keeping its PHP and WordPress versions, so php and nginx
// keep the same core. Its steps:
//
//  1. prepare: check the site still runs those tags, pinned to the builds
//     the job moves it from (or to, when a retry resumes). Its Undo runs
//     last in a rollback and records the outcome.
//  2. recreate: record the new pins and recreate the containers
//     (provisioner.UpdateSite recreates the ones whose image changed and
//     waits for them to be healthy).
//  3. health_check: Service.CheckHealth, with the site's own core version:
//     a build carrying another core fails at once.
//  4. finish: set the status from the live phase and record the outcome.
//
// If anything fails before the site is healthy, the recreate step's Undo
// puts the previous pins back and recreates the containers on the previous
// builds; the site ends up running with a status message saying so. If
// that fails too, the site is marked failed.
//
// There is no pre-update backup, unlike a core update: the WordPress core
// doesn't change, so nothing runs `wp core update-db` and the database
// schema stays as it is, and the images hold no site data (wp-content is a
// volume). Moving back to the previous build restores everything the
// update changed, and skipping the backup keeps a weekly rebuild, which
// touches every site, from taking a snapshot of each.

// ImageUpdateJobType moves a site to new builds of its images.
const ImageUpdateJobType = "site.image_update"

// imageRollout names the image rollout.
const imageRollout = "images"

// ImageRolloutName is the name of the image rollout, for
// Service.Rollout and Service.ResumeRollout.
const ImageRolloutName = imageRollout

// Step names of the site.image_update job besides prepare, recreate, and
// health_check, which it shares with the core update. Keep them stable: a
// queued job resumes by step name.
const stepFinishImages = "finish"

// ImageUpdateOptions configure the image rollout.
type ImageUpdateOptions struct {
	// PullInterval is how often the rollout pulls the tags running sites
	// use from their registries, to find new builds; zero never pulls, so
	// only builds that reach the host another way roll out.
	PullInterval time.Duration
}

// siteComponents are the components whose images a site runs, in the order
// the image rollout lists them.
var siteComponents = []provisioner.Component{provisioner.ComponentPHP, provisioner.ComponentNginx}

// imageChange moves a site's pin of one image tag.
type imageChange struct {
	Component provisioner.Component `json:"component"`
	// Image is the tag.
	Image string `json:"image"`
	From  string `json:"from"`
	To    string `json:"to"`
}

// imageUpdatePayload is the payload of a site.image_update job.
type imageUpdatePayload struct {
	Images []imageChange `json:"images"`
	// Rollout is the rollout that queued the job.
	Rollout string `json:"rollout,omitempty"`
}

// ImageBuild is the newest build of an image tag that the image rollout
// knows of.
type ImageBuild struct {
	// Image is the tag and Build the pinned reference of its build.
	Image string
	Build string
	// CheckedAt is when the rollout last asked the driver, and PulledAt
	// when it last pulled the tag (zero if never).
	CheckedAt time.Time
	PulledAt  time.Time
}

// imageChecker keeps what the image rollout found out about builds.
type imageChecker struct {
	opts ImageUpdateOptions

	mu     sync.Mutex
	pulled map[string]time.Time
	latest map[string]ImageBuild
}

// siteImageTags returns the image tags the site's versions select.
func (s *Service) siteImageTags(site store.Site) (map[provisioner.Component]string, error) {
	imgs, err := s.catalog.Images(site.PHPVersion, site.WPCoreVersion)
	if err != nil {
		return nil, err
	}
	return map[provisioner.Component]string{provisioner.ComponentPHP: imgs.PHP, provisioner.ComponentNginx: imgs.Nginx}, nil
}

// pinnedImages returns the builds to render the site with, pinning each tag
// it has no pin for to the build the driver has for it. Without an
// ImagePinner there are none, and the site runs the tags.
func (s *Service) pinnedImages(ctx context.Context, site store.Site) (map[provisioner.Component]string, error) {
	if s.pinner == nil {
		return nil, nil
	}
	tags, err := s.siteImageTags(site)
	if err != nil {
		return nil, jobs.Permanent(err)
	}
	pins := store.NewImagePins(s.db)
	current, err := pins.Site(ctx, site.ID)
	if err != nil {
		return nil, err
	}
	out := map[provisioner.Component]string{}
	for _, comp := range siteComponents {
		image := tags[comp]
		pinned, ok := current[image]
		if !ok {
			if pinned, err = s.pinner.PinImage(ctx, image); err != nil {
				return nil, err
			}
			if err := pins.Set(ctx, site.ID, image, pinned); err != nil {
				return nil, err
			}
		}
		out[comp] = pinned
	}
	return out, nil
}

// runningSites lists the running sites, oldest first.
func (s *Service) runningSites(ctx context.Context) ([]store.Site, error) {
	var running []store.Site
	sites := store.NewSites(s.db)
	for before := ""; ; {
		page, err := sites.List(ctx, store.SiteFilter{Status: store.SiteRunning, Before: before, Limit: 200})
		if err != nil {
			return nil, err
		}
		running = append(running, page.Sites...)
		if page.NextCursor == "" {
			break
		}
		before = page.NextCursor
	}
	slices.Reverse(running)
	return running, nil
}

// imagesInUse returns the running sites and the tags they use, sorted.
func (s *Service) imagesInUse(ctx context.Context) ([]store.Site, []string, error) {
	running, err := s.runningSites(ctx)
	if err != nil {
		return nil, nil, err
	}
	set := map[string]bool{}
	for _, site := range running {
		tags, err := s.siteImageTags(site)
		if err != nil {
			continue
		}
		for _, t := range tags {
			set[t] = true
		}
	}
	return running, slices.Sorted(maps.Keys(set)), nil
}

// latestBuilds returns the build each tag names now, pulling the tags whose
// PullInterval has passed first. A failed pull leaves the build the driver
// has; a tag the driver can't resolve is left out.
func (s *Service) latestBuilds(ctx context.Context, images []string) map[string]string {
	c := s.images
	out := map[string]string{}
	for _, image := range images {
		now := s.now()
		c.mu.Lock()
		pull := c.opts.PullInterval > 0 && now.Sub(c.pulled[image]) >= c.opts.PullInterval
		if pull {
			c.pulled[image] = now
		}
		c.mu.Unlock()
		if pull {
			if err := s.pinner.PullImage(ctx, image); err != nil {
				s.log.WarnContext(ctx, "checking the registry for a new image build failed", "image", image, "error", err)
			}
		}
		build, err := s.pinner.PinImage(ctx, image)
		if err != nil {
			s.log.WarnContext(ctx, "resolving an image's build failed", "image", image, "error", err)
			continue
		}
		c.mu.Lock()
		b := c.latest[image]
		b.Image, b.Build, b.CheckedAt = image, build, now
		if pull {
			b.PulledAt = now
		}
		c.latest[image] = b
		c.mu.Unlock()
		out[image] = build
	}
	return out
}

// ImageBuilds returns the newest builds the image rollout knows of, by
// tag; empty until its first pass.
func (s *Service) ImageBuilds() []ImageBuild {
	s.images.mu.Lock()
	defer s.images.mu.Unlock()
	out := []ImageBuild{}
	for _, image := range slices.Sorted(maps.Keys(s.images.latest)) {
		out = append(out, s.images.latest[image])
	}
	return out
}

// imageFingerprint identifies the newest builds of the tags in use.
func (s *Service) imageFingerprint(ctx context.Context) (string, error) {
	_, images, err := s.imagesInUse(ctx)
	if err != nil {
		return "", err
	}
	latest := s.latestBuilds(ctx, images)
	var parts []string
	for _, image := range slices.Sorted(maps.Keys(latest)) {
		parts = append(parts, image+"="+latest[image])
	}
	return strings.Join(parts, " "), nil
}

// imageCandidates lists the running sites with a tag pinned to another
// build than the newest, oldest site first. A tag the site has no pin for
// yet is left alone: it gets one the next time the site is rendered.
func (s *Service) imageCandidates(ctx context.Context) ([]rolloutCandidate, error) {
	running, images, err := s.imagesInUse(ctx)
	if err != nil {
		return nil, err
	}
	pins, err := store.NewImagePins(s.db).All(ctx)
	if err != nil {
		return nil, err
	}
	latest := s.latestBuilds(ctx, images)
	var out []rolloutCandidate
	for _, site := range running {
		tags, err := s.siteImageTags(site)
		if err != nil {
			s.log.WarnContext(ctx, "resolving a site's images failed", "site_id", site.ID, "error", err)
			continue
		}
		var changes []imageChange
		for _, comp := range siteComponents {
			image := tags[comp]
			from, to := pins[site.ID][image], latest[image]
			if from != "" && to != "" && from != to {
				changes = append(changes, imageChange{Component: comp, Image: image, From: from, To: to})
			}
		}
		if len(changes) > 0 {
			out = append(out, rolloutCandidate{site: site, target: imageTarget(changes), images: changes})
		}
	}
	return out, nil
}

// imageTarget names the builds an update moves a site to, for failure
// records: "php=<build> nginx=<build>".
func imageTarget(changes []imageChange) string {
	parts := make([]string, 0, len(changes))
	for _, c := range changes {
		parts = append(parts, string(c.Component)+"="+c.To)
	}
	return strings.Join(parts, " ")
}

// enqueueImageUpdate sets a running site's status to updating and queues
// its image update, in q's transaction.
func (s *Service) enqueueImageUpdate(ctx context.Context, q store.Querier, site store.Site, changes []imageChange, rollout string) (store.Job, error) {
	if site.Status != store.SiteRunning {
		return store.Job{}, fmt.Errorf("%w (it is %s)", ErrNotRunning, site.Status)
	}
	changed, err := store.NewSites(q).SetStatus(ctx, site.ID, store.SiteUpdating, "", store.SiteRunning)
	if err != nil {
		return store.Job{}, err
	}
	if !changed {
		return store.Job{}, fmt.Errorf("%w: the site's status changed", ErrBusy)
	}
	return jobs.Enqueue(ctx, q, jobs.Spec{
		Type:       ImageUpdateJobType,
		TargetType: store.JobTargetSite,
		TargetID:   site.ID,
		Payload:    imageUpdatePayload{Images: changes, Rollout: rollout},
		LockKey:    store.SiteLockKey(site.ID),
	})
}

func (s *Service) imageUpdateHandler() jobs.Handler {
	return jobs.Handler{Type: ImageUpdateJobType, Steps: []jobs.Step{
		{Name: stepPrepareUpdate, Do: s.prepareImageUpdate, Undo: s.finishFailedImageUpdate},
		{Name: stepRecreate, Do: s.recreateOnImages, Undo: s.revertImages},
		{Name: stepHealthCheck, Do: s.checkImageHealth},
		{Name: stepFinishImages, Do: s.finishImageUpdate},
	}}
}

func imageJobPayload(r *jobs.Run) (imageUpdatePayload, error) {
	var p imageUpdatePayload
	if err := r.Payload(&p); err != nil {
		return p, err
	}
	if len(p.Images) == 0 {
		return p, jobs.Permanent(fmt.Errorf("the image update job's payload has no images"))
	}
	for _, c := range p.Images {
		if !slices.Contains(siteComponents, c.Component) || c.Image == "" || c.From == "" || c.To == "" {
			return p, jobs.Permanent(fmt.Errorf("the image update job's payload has an invalid change %+v", c))
		}
	}
	return p, nil
}

// components lists the components an update changes, for messages.
func (p imageUpdatePayload) components() string {
	var names []string
	for _, c := range p.Images {
		names = append(names, string(c.Component))
	}
	return strings.Join(names, " and ")
}

func (s *Service) prepareImageUpdate(ctx context.Context, r *jobs.Run) error {
	site, err := s.managedSite(ctx, r)
	if err != nil {
		return err
	}
	p, err := imageJobPayload(r)
	if err != nil {
		return err
	}
	tags, err := s.siteImageTags(site)
	if err != nil {
		return jobs.Permanent(err)
	}
	pins, err := store.NewImagePins(s.db).Site(ctx, site.ID)
	if err != nil {
		return err
	}
	for _, c := range p.Images {
		if tags[c.Component] != c.Image {
			return jobs.Permanent(fmt.Errorf("the site's %s image is %s, not %s, since the update was queued", c.Component, tags[c.Component], c.Image))
		}
		if cur := pins[c.Image]; cur != c.From && cur != c.To {
			return jobs.Permanent(fmt.Errorf("the site's %s build changed since the update was queued", c.Component))
		}
	}
	for _, c := range p.Images {
		r.Logf("Moving %s from build %s to %s of %s.", c.Component, c.From, c.To, c.Image)
	}
	r.Logf("The WordPress core stays at %s, so the database is left alone and no pre-update backup is needed.", site.WPCoreVersion)
	return nil
}

// recreateOnImages records the new pins and recreates the containers on
// them.
func (s *Service) recreateOnImages(ctx context.Context, r *jobs.Run) error {
	site, err := s.managedSite(ctx, r)
	if err != nil {
		return err
	}
	p, err := imageJobPayload(r)
	if err != nil {
		return err
	}
	// First, so a revert happens even if this attempt dies half way.
	if err := r.Set(dataImagesChanged, true); err != nil {
		return err
	}
	if err := s.setPins(ctx, site.ID, p, false); err != nil {
		return err
	}
	if _, err := s.converge(ctx, r, site); err != nil {
		return err
	}
	r.Logf("Recreated the site's %s on the new builds.", p.components())
	return nil
}

// setPins moves the site's pins to the job's new builds, or back to the
// previous ones.
func (s *Service) setPins(ctx context.Context, siteID string, p imageUpdatePayload, previous bool) error {
	pins := store.NewImagePins(s.db)
	for _, c := range p.Images {
		build := c.To
		if previous {
			build = c.From
		}
		if err := pins.Set(ctx, siteID, c.Image, build); err != nil {
			return err
		}
	}
	return nil
}

// revertImages moves the site back to the previous builds unless the update
// succeeded. If that keeps failing, it gives up on its last attempt and
// records why, so the final Undo marks the site failed.
func (s *Service) revertImages(ctx context.Context, r *jobs.Run) error {
	if healthy, err := flag(r, dataHealthy); err != nil || healthy {
		return err
	}
	if changed, err := flag(r, dataImagesChanged); err != nil || !changed {
		return err
	}
	site, err := s.jobSite(ctx, r)
	if err != nil || site.Status != store.SiteUpdating {
		return nil
	}
	p, err := imageJobPayload(r)
	if err != nil {
		return err
	}
	if err := s.setPins(ctx, site.ID, p, true); err != nil {
		return err
	}
	if _, err := s.converge(ctx, r, site); err != nil {
		if !lastAttempt(r, err) {
			return err
		}
		r.Warnf("Couldn't move the site back to its previous builds: %v", err)
		return r.Set(dataRevertError, err.Error())
	}
	r.Warnf("Moved the site back to its previous builds: %s run them again.", p.components())
	return nil
}

func (s *Service) checkImageHealth(ctx context.Context, r *jobs.Run) error {
	site, err := s.managedSite(ctx, r)
	if err != nil {
		return err
	}
	if err := s.CheckHealth(ctx, site, site.WPCoreVersion); err != nil {
		return jobs.Permanent(err)
	}
	if err := r.Set(dataHealthy, true); err != nil {
		return err
	}
	r.Logf("The site is healthy on the new builds: it answers HTTP requests, and WP-CLI loads WordPress %s.", site.WPCoreVersion)
	return nil
}

// finishImageUpdate sets the status from the live phase, records the
// success, and puts the new builds in the job's result.
func (s *Service) finishImageUpdate(ctx context.Context, r *jobs.Run) error {
	if err := s.finishUpdate(ctx, r); err != nil {
		return err
	}
	p, err := imageJobPayload(r)
	if err != nil {
		return err
	}
	for _, c := range p.Images {
		if err := r.SetResult(string(c.Component), c.To); err != nil {
			return err
		}
	}
	return s.recordRolloutOutcome(ctx, r, imageRollout, p.Rollout, imageTarget(p.Images), "")
}

// finishFailedImageUpdate is the last Undo of a failed site.image_update
// job: it sets the site's status, says in its status message what the
// failed update left behind, and records the failure.
func (s *Service) finishFailedImageUpdate(ctx context.Context, r *jobs.Run) error {
	siteID := r.Job().TargetID
	p, err := imageJobPayload(r)
	if err != nil {
		return err
	}
	var revertErr string
	if _, err := r.Get(dataRevertError, &revertErr); err != nil {
		return err
	}
	healthy, err := flag(r, dataHealthy)
	if err != nil {
		return err
	}
	changed, err := flag(r, dataImagesChanged)
	if err != nil {
		return err
	}
	status := s.observedStatus(ctx, siteID)
	var prefix string
	switch {
	case healthy:
		prefix = "the image update finished, but the job failed afterwards"
	case revertErr != "":
		status = store.SiteFailed
		prefix = "the image update failed, and the site couldn't be moved back to its previous builds"
	case changed:
		prefix = "the image update failed and was reverted to the previous builds"
	default:
		prefix = "the image update failed before changing the site"
	}
	msg := statusMessage(prefix, r.Job().Error)
	changedStatus, err := store.NewSites(s.db).SetStatus(ctx, siteID, status, msg, store.SiteUpdating)
	if err != nil {
		return err
	}
	if changedStatus {
		r.Warnf("The site is %s: %s.", status, prefix)
	}
	if healthy {
		return s.recordRolloutOutcome(ctx, r, imageRollout, p.Rollout, imageTarget(p.Images), "")
	}
	return s.recordRolloutOutcome(ctx, r, imageRollout, p.Rollout, imageTarget(p.Images), msg)
}

// PHPCatalog returns every PHP branch in the version manifest, newest first,
// with the branch new sites default to ("" if all are past end of life), and
// the time the statuses are computed at, so the API reports the same status
// that site creation and updates enforce.
func (s *Service) PHPCatalog() (versions []catalog.PHPVersion, defaultVersion string, now time.Time) {
	now = s.now()
	defaultVersion, _ = s.catalog.DefaultPHPVersion(now)
	return s.catalog.PHPVersions(), defaultVersion, now
}
