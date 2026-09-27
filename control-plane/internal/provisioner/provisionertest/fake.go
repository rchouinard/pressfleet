// Package provisionertest provides an in-memory [provisioner.Driver] for unit
// tests.
package provisionertest

import (
	"context"
	"iter"
	"slices"
	"sync"

	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
)

// Call records one method invocation on the fake.
type Call struct {
	Op     string // method name, e.g. "CreateSite"
	SiteID string
	Args   []string          // ExecWPCLI arguments
	Task   *provisioner.Task // RunTask task
}

// Site is the fake's view of a site's runtime state.
type Site struct {
	Config      provisioner.SiteConfig
	Phase       provisioner.SitePhase
	Maintenance bool
	CachePurges int
}

// Fake is an in-memory Driver that follows the interfaces' idempotency and
// not-found contract. It is safe for concurrent use.
type Fake struct {
	// WPCLI, if set, handles ExecWPCLI calls for running sites. By default
	// they succeed with empty output; on a site that isn't running they
	// return provisioner.ErrSiteNotRunning.
	WPCLI func(siteID string, args []string) (string, error)
	// RunTaskFunc, if set, handles RunTask calls for existing sites. By
	// default tasks succeed with empty output.
	RunTaskFunc func(siteID string, task provisioner.Task) (*provisioner.TaskResult, error)
	// Err, if set, is consulted at the start of every call; a non-nil result
	// is returned without changing state. Use it to inject failures.
	Err func(op, siteID string) error

	mu    sync.Mutex
	sites map[string]*Site
	logs  map[string][]provisioner.LogEntry
	stats map[string][]provisioner.ComponentStats
	calls []Call
}

var _ provisioner.Driver = (*Fake)(nil)

// New returns an empty Fake.
func New() *Fake {
	return &Fake{
		sites: make(map[string]*Site),
		logs:  make(map[string][]provisioner.LogEntry),
		stats: make(map[string][]provisioner.ComponentStats),
	}
}

// AddLogs appends log entries returned by Logs for siteID.
func (f *Fake) AddLogs(siteID string, entries ...provisioner.LogEntry) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.logs[siteID] = append(f.logs[siteID], entries...)
}

// SetStats sets the samples returned by Stats for siteID.
func (f *Fake) SetStats(siteID string, stats ...provisioner.ComponentStats) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stats[siteID] = slices.Clone(stats)
}

// Site returns a copy of the site's state.
func (f *Fake) Site(siteID string) (Site, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.sites[siteID]
	if !ok {
		return Site{}, false
	}
	return *s, true
}

// Calls returns every call made so far, in order.
func (f *Fake) Calls() []Call {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

// begin records the call and returns an injected error, if any. It must be
// called with f.mu held.
func (f *Fake) begin(op, siteID string, args []string) error {
	f.calls = append(f.calls, Call{Op: op, SiteID: siteID, Args: slices.Clone(args)})
	if f.Err != nil {
		return f.Err(op, siteID)
	}
	return nil
}

func (f *Fake) lookup(siteID string) (*Site, error) {
	s, ok := f.sites[siteID]
	if !ok {
		return nil, provisioner.ErrSiteNotFound
	}
	return s, nil
}

func status(s *Site) *provisioner.SiteStatus {
	host := s.Config.PreviewDomain
	if s.Config.PrimaryDomain != "" {
		host = s.Config.PrimaryDomain
	}
	return &provisioner.SiteStatus{ID: s.Config.ID, Phase: s.Phase, URL: "https://" + host}
}

// CreateSite implements provisioner.Provisioner.
func (f *Fake) CreateSite(_ context.Context, cfg *provisioner.SiteConfig) (*provisioner.SiteStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin("CreateSite", cfg.ID, nil); err != nil {
		return nil, err
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	s, ok := f.sites[cfg.ID]
	if !ok {
		s = &Site{}
		f.sites[cfg.ID] = s
	}
	s.Config = *cfg
	s.Maintenance = cfg.Maintenance
	s.Phase = provisioner.SiteRunning
	return status(s), nil
}

// UpdateSite implements provisioner.Provisioner.
func (f *Fake) UpdateSite(_ context.Context, cfg *provisioner.SiteConfig) (*provisioner.SiteStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin("UpdateSite", cfg.ID, nil); err != nil {
		return nil, err
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	s, err := f.lookup(cfg.ID)
	if err != nil {
		return nil, err
	}
	s.Config = *cfg
	// Like the Compose driver, the config's maintenance flag wins.
	s.Maintenance = cfg.Maintenance
	return status(s), nil
}

// GetSite implements provisioner.Provisioner.
func (f *Fake) GetSite(_ context.Context, siteID string) (*provisioner.SiteStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin("GetSite", siteID, nil); err != nil {
		return nil, err
	}
	s, err := f.lookup(siteID)
	if err != nil {
		return nil, err
	}
	return status(s), nil
}

// DeleteSite implements provisioner.Provisioner.
func (f *Fake) DeleteSite(_ context.Context, siteID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin("DeleteSite", siteID, nil); err != nil {
		return err
	}
	delete(f.sites, siteID)
	return nil
}

// StartSite implements provisioner.Provisioner.
func (f *Fake) StartSite(_ context.Context, siteID string) error {
	return f.setPhase("StartSite", siteID, provisioner.SiteRunning)
}

// StopSite implements provisioner.Provisioner.
func (f *Fake) StopSite(_ context.Context, siteID string) error {
	return f.setPhase("StopSite", siteID, provisioner.SiteStopped)
}

func (f *Fake) setPhase(op, siteID string, phase provisioner.SitePhase) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin(op, siteID, nil); err != nil {
		return err
	}
	s, err := f.lookup(siteID)
	if err != nil {
		return err
	}
	s.Phase = phase
	return nil
}

// SetMaintenance implements provisioner.Provisioner.
func (f *Fake) SetMaintenance(_ context.Context, siteID string, enabled bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin("SetMaintenance", siteID, nil); err != nil {
		return err
	}
	s, err := f.lookup(siteID)
	if err != nil {
		return err
	}
	s.Maintenance = enabled
	return nil
}

// ExecWPCLI implements provisioner.Provisioner.
func (f *Fake) ExecWPCLI(_ context.Context, siteID string, args []string) (string, error) {
	f.mu.Lock()
	if err := f.begin("ExecWPCLI", siteID, args); err != nil {
		f.mu.Unlock()
		return "", err
	}
	s, err := f.lookup(siteID)
	if err != nil {
		f.mu.Unlock()
		return "", err
	}
	if s.Phase != provisioner.SiteRunning {
		f.mu.Unlock()
		return "", provisioner.ErrSiteNotRunning
	}
	handler := f.WPCLI
	f.mu.Unlock()

	// Call the handler without holding the lock so it may inspect the fake.
	if handler == nil {
		return "", nil
	}
	return handler(siteID, slices.Clone(args))
}

// PurgeCache implements provisioner.Provisioner.
func (f *Fake) PurgeCache(_ context.Context, siteID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin("PurgeCache", siteID, nil); err != nil {
		return err
	}
	s, err := f.lookup(siteID)
	if err != nil {
		return err
	}
	s.CachePurges++
	return nil
}

// Logs implements provisioner.LogReader. It filters stored entries by
// component, Since, and Tail; Follow is ignored.
func (f *Fake) Logs(_ context.Context, siteID string, opts provisioner.LogOptions) iter.Seq2[provisioner.LogEntry, error] {
	f.mu.Lock()
	err := f.begin("Logs", siteID, nil)
	if err == nil {
		_, err = f.lookup(siteID)
	}
	var out []provisioner.LogEntry
	if err == nil {
		counts := map[provisioner.Component]int{}
		stored := f.logs[siteID]
		// Walk backwards so Tail keeps the newest lines per component.
		for i := len(stored) - 1; i >= 0; i-- {
			e := stored[i]
			if len(opts.Components) > 0 && !slices.Contains(opts.Components, e.Component) {
				continue
			}
			if !opts.Since.IsZero() && e.Time.Before(opts.Since) {
				continue
			}
			if opts.Tail > 0 && counts[e.Component] >= opts.Tail {
				continue
			}
			counts[e.Component]++
			out = append(out, e)
		}
		slices.Reverse(out)
	}
	f.mu.Unlock()

	return func(yield func(provisioner.LogEntry, error) bool) {
		if err != nil {
			yield(provisioner.LogEntry{}, err)
			return
		}
		for _, e := range out {
			if !yield(e, nil) {
				return
			}
		}
	}
}

// Stats implements provisioner.StatsReader.
func (f *Fake) Stats(_ context.Context, siteID string) ([]provisioner.ComponentStats, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin("Stats", siteID, nil); err != nil {
		return nil, err
	}
	if _, err := f.lookup(siteID); err != nil {
		return nil, err
	}
	return slices.Clone(f.stats[siteID]), nil
}

// RunTask implements provisioner.TaskRunner.
func (f *Fake) RunTask(_ context.Context, siteID string, task provisioner.Task) (*provisioner.TaskResult, error) {
	f.mu.Lock()
	if err := f.begin("RunTask", siteID, nil); err != nil {
		f.mu.Unlock()
		return nil, err
	}
	t := task
	f.calls[len(f.calls)-1].Task = &t
	if _, err := f.lookup(siteID); err != nil {
		f.mu.Unlock()
		return nil, err
	}
	handler := f.RunTaskFunc
	f.mu.Unlock()

	if handler == nil {
		return &provisioner.TaskResult{}, nil
	}
	return handler(siteID, task)
}
