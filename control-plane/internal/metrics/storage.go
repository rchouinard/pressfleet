package metrics

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/backup/repository"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
)

// Sizes change slowly and are costly to measure (a container per site, a
// restic run per repository), so the storage collectors default to long
// intervals rather than the service's.
const (
	// DefaultWPContentInterval is how often wp-content sizes are measured.
	DefaultWPContentInterval = 10 * time.Minute
	// DefaultRepositoryInterval is how often Restic repository sizes are
	// measured.
	DefaultRepositoryInterval = 30 * time.Minute
)

// WPContentCollector reports pressfleet_site_wp_content_size_bytes (design
// doc §7.1 Storage): the disk space a site's wp-content volume uses.
//
// The volume belongs to Docker, so the size is measured the way every other
// access to site data is: a short-lived task container with the volume
// mounted read-only runs du through the provisioner. That keeps the docker
// CLI and SDK inside provisioner/compose, at the price of one container
// start per site per run, which is why the interval is long.
type WPContentCollector struct {
	// Tasks runs the du task.
	Tasks provisioner.TaskRunner
	// Every is the collection interval; zero means [DefaultWPContentInterval].
	Every time.Duration
	// TaskTimeout bounds one site's task; zero means one minute.
	TaskTimeout time.Duration
}

var _ Collector = WPContentCollector{}

// Name implements [Collector].
func (WPContentCollector) Name() string { return "wp_content_size" }

// Interval implements [Collector].
func (c WPContentCollector) Interval() time.Duration {
	if c.Every <= 0 {
		return DefaultWPContentInterval
	}
	return c.Every
}

// Collect implements [Collector]. Sites are measured one after another so a
// large fleet never starts many containers at once; one that fails is
// reported in the error and doesn't stop the rest. Only running and stopped
// sites are measured: the others' volumes are being created, changed, or
// removed.
func (c WPContentCollector) Collect(ctx context.Context, targets []Target) ([]Sample, error) {
	timeout := c.TaskTimeout
	if timeout <= 0 {
		timeout = time.Minute
	}
	var out []Sample
	var errs []error
	for _, t := range targets {
		if t.Site.Status != store.SiteRunning && t.Site.Status != store.SiteStopped {
			continue
		}
		if ctx.Err() != nil {
			errs = append(errs, ctx.Err())
			break
		}
		res, err := c.Tasks.RunTask(ctx, t.SiteID, provisioner.Task{
			Command:   []string{"du", "-sk", provisioner.TaskWPContentPath},
			WPContent: provisioner.MountReadOnly,
			Timeout:   timeout,
		})
		if err != nil {
			errs = append(errs, fmt.Errorf("measure wp-content of site %s: %w", t.SiteID, err))
			continue
		}
		size, err := parseDU(res.Stdout)
		if err != nil {
			errs = append(errs, fmt.Errorf("measure wp-content of site %s: %w", t.SiteID, err))
			continue
		}
		out = append(out, t.Sample("pressfleet_site_wp_content_size_bytes", float64(size)))
	}
	return out, errors.Join(errs...)
}

// parseDU reads the size in bytes from the output of "du -sk PATH". Errors
// du prints about files it can't read (it still prints a total) are on
// stderr, not here.
func parseDU(stdout string) (uint64, error) {
	f := strings.Fields(stdout)
	if len(f) == 0 {
		return 0, errors.New("du printed nothing")
	}
	kib, err := strconv.ParseUint(f[0], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse du output %q: %w", strings.TrimSpace(stdout), err)
	}
	return kib * 1024, nil
}

// ResticRunner runs restic against a client group's repository. It is
// implemented by [repository.Manager].
type ResticRunner interface {
	Restic(ctx context.Context, clientGroupID string, args ...string) ([]byte, error)
}

// RepositoryCollector reports pressfleet_restic_repository_size_bytes
// (design doc §7.2 Restic repositories) per client group, with the
// client_group_id label and no site_id.
//
// It asks restic ("stats --mode raw-data") instead of walking the
// repository's directory, so local and S3 repositories are measured the same
// way, and the size is what restic stores, after deduplication and
// compression, not the sum of the snapshots' sizes.
type RepositoryCollector struct {
	// Restic runs restic commands.
	Restic ResticRunner
	// Every is the collection interval; zero means
	// [DefaultRepositoryInterval].
	Every time.Duration
}

var _ Collector = RepositoryCollector{}

// Name implements [Collector].
func (RepositoryCollector) Name() string { return "restic_repository" }

// Interval implements [Collector].
func (c RepositoryCollector) Interval() time.Duration {
	if c.Every <= 0 {
		return DefaultRepositoryInterval
	}
	return c.Every
}

// Collect implements [Collector]. The groups are those with a site, as the
// service knows no others; one without a repository yet (it is created with
// the group's first backup) is skipped without error.
func (c RepositoryCollector) Collect(ctx context.Context, targets []Target) ([]Sample, error) {
	var groups []string
	for _, t := range targets {
		groups = append(groups, t.ClientGroupID)
	}
	slices.Sort(groups)
	groups = slices.Compact(groups)

	var out []Sample
	var errs []error
	for _, g := range groups {
		if ctx.Err() != nil {
			errs = append(errs, ctx.Err())
			break
		}
		stdout, err := c.Restic.Restic(ctx, g, "stats", "--mode", "raw-data", "--json")
		if errors.Is(err, repository.ErrNoRepository) {
			continue
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("measure backup repository of client group %s: %w", g, err))
			continue
		}
		size, err := parseResticStats(stdout)
		if err != nil {
			errs = append(errs, fmt.Errorf("measure backup repository of client group %s: %w", g, err))
			continue
		}
		out = append(out, Sample{
			Name:   "pressfleet_restic_repository_size_bytes",
			Labels: map[string]string{LabelClientGroupID: g},
			Value:  float64(size),
		})
	}
	return out, errors.Join(errs...)
}

// parseResticStats reads total_size from "restic stats --json".
func parseResticStats(stdout []byte) (uint64, error) {
	var st struct {
		TotalSize *uint64 `json:"total_size"`
	}
	if err := json.Unmarshal(stdout, &st); err != nil {
		return 0, fmt.Errorf("parse restic stats: %w", err)
	}
	if st.TotalSize == nil {
		return 0, errors.New("restic stats has no total_size")
	}
	return *st.TotalSize, nil
}
