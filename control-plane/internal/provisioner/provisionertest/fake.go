// Package provisionertest provides an in-memory [provisioner.Driver] for unit
// tests.
package provisionertest

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"iter"
	"maps"
	"slices"
	"sync"

	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
)

// Call records one method invocation on the fake.
type Call struct {
	Op         string // method name, e.g. "CreateSite"
	SiteID     string
	Repository string            // RunRepositoryTask's client group ID or provisioner.StateRepository
	Args       []string          // ExecWPCLI arguments
	Stdin      string            // ExecWPCLIInput standard input
	Task       *provisioner.Task // RunTask and RunRepositoryTask task
}

// Site is the fake's view of a site's runtime state.
type Site struct {
	Config      provisioner.SiteConfig
	Phase       provisioner.SitePhase
	Maintenance bool
	CachePurges int
	CodeReloads int
}

// Fake is an in-memory Driver that follows the interfaces' idempotency and
// not-found contract. It is safe for concurrent use.
type Fake struct {
	// WPCLI, if set, handles ExecWPCLI calls for running sites. By default
	// they succeed with empty output; on a site that isn't running they
	// return provisioner.ErrSiteNotRunning.
	WPCLI func(siteID string, args []string) (string, error)
	// MailSwitch, if set, says whether a running site's image has the
	// outbound mail switch (provisioner.MailSwitchCheckArgs), which
	// ExecWPCLI answers itself instead of calling WPCLI. By default every
	// image has it.
	MailSwitch func(siteID string) bool
	// RunTaskFunc, if set, handles RunTask calls for existing sites. By
	// default tasks succeed with empty output.
	RunTaskFunc func(siteID string, task provisioner.Task) (*provisioner.TaskResult, error)
	// RunRepositoryTaskFunc, if set, handles RunRepositoryTask calls. By
	// default tasks succeed with empty output.
	RunRepositoryTaskFunc func(repository string, task provisioner.Task) (*provisioner.TaskResult, error)
	// RunSessionFunc, if set, handles RunSession calls for existing sites,
	// e.g. by serving SFTP from a temporary directory in the test process.
	RunSessionFunc func(ctx context.Context, siteID string, s provisioner.Session) error
	// Probe, if set, handles ProbeHTTP calls for running sites. By default
	// they answer 200.
	Probe func(siteID string, req provisioner.ProbeRequest) (*provisioner.ProbeResponse, error)
	// Err, if set, is consulted at the start of every call; a non-nil result
	// is returned without changing state. Use it to inject failures. For
	// RunRepositoryTask the second argument is the repository.
	Err func(op, siteID string) error

	mu    sync.Mutex
	sites map[string]*Site
	logs  map[string][]provisioner.LogEntry
	// logWakeCh is closed when logs are added, waking followers.
	logWakeCh chan struct{}
	stats     map[string][]provisioner.ComponentStats
	calls     []Call
	// local and registry map image tags to builds (ImagePinner).
	local    map[string]string
	registry map[string]string
}

var _ provisioner.Driver = (*Fake)(nil)

// New returns an empty Fake.
func New() *Fake {
	return &Fake{
		sites: make(map[string]*Site),
		logs:  make(map[string][]provisioner.LogEntry),
		stats: make(map[string][]provisioner.ComponentStats),

		local:    make(map[string]string),
		registry: make(map[string]string),
	}
}

// AddLogs appends log entries returned by Logs for siteID. Entries without a
// Source are classified from their component and line when read.
func (f *Fake) AddLogs(siteID string, entries ...provisioner.LogEntry) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.logs[siteID] = append(f.logs[siteID], entries...)
	if f.logWakeCh != nil {
		close(f.logWakeCh)
		f.logWakeCh = nil
	}
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
func (f *Fake) ExecWPCLI(ctx context.Context, siteID string, args []string) (string, error) {
	return f.ExecWPCLIInput(ctx, siteID, args, nil)
}

// ExecWPCLIInput implements provisioner.Provisioner. It is recorded as an
// "ExecWPCLI" call whose Stdin holds what it read, and WPCLI handles it as
// it does ExecWPCLI.
func (f *Fake) ExecWPCLIInput(_ context.Context, siteID string, args []string, stdin io.Reader) (string, error) {
	var input []byte
	if stdin != nil {
		var err error
		if input, err = io.ReadAll(stdin); err != nil {
			return "", fmt.Errorf("reading standard input: %w", err)
		}
	}
	f.mu.Lock()
	err := f.begin("ExecWPCLI", siteID, args)
	f.calls[len(f.calls)-1].Stdin = string(input)
	if err != nil {
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
	handler, mailSwitch := f.WPCLI, f.MailSwitch
	f.mu.Unlock()

	if slices.Equal(args, provisioner.MailSwitchCheckArgs()) {
		return provisioner.MailSwitchAnswer(mailSwitch == nil || mailSwitch(siteID)), nil
	}
	// Call the handler without holding the lock so it may inspect the fake.
	if handler == nil {
		return "", nil
	}
	return handler(siteID, slices.Clone(args))
}

// ProbeHTTP implements provisioner.Prober. By default a running site
// answers 200; Probe, if set, answers instead.
func (f *Fake) ProbeHTTP(_ context.Context, siteID string, req provisioner.ProbeRequest) (*provisioner.ProbeResponse, error) {
	f.mu.Lock()
	if err := f.begin("ProbeHTTP", siteID, []string{req.Host, req.Path}); err != nil {
		f.mu.Unlock()
		return nil, err
	}
	s, err := f.lookup(siteID)
	if err != nil {
		f.mu.Unlock()
		return nil, err
	}
	if s.Phase != provisioner.SiteRunning {
		f.mu.Unlock()
		return nil, provisioner.ErrSiteNotRunning
	}
	handler := f.Probe
	f.mu.Unlock()
	if handler == nil {
		return &provisioner.ProbeResponse{StatusCode: 200}, nil
	}
	return handler(siteID, req)
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

// ReloadCode implements provisioner.Provisioner. It counts reloads of
// running sites.
func (f *Fake) ReloadCode(_ context.Context, siteID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin("ReloadCode", siteID, nil); err != nil {
		return err
	}
	s, err := f.lookup(siteID)
	if err != nil {
		return err
	}
	if s.Phase == provisioner.SiteRunning {
		s.CodeReloads++
	}
	return nil
}

// Logs implements provisioner.LogReader. It selects stored entries with
// provisioner.SelectLogs, classifying those added without a source or level
// as a driver would. With Follow it then yields entries added by AddLogs
// until ctx is canceled.
func (f *Fake) Logs(ctx context.Context, siteID string, opts provisioner.LogOptions) iter.Seq2[provisioner.LogEntry, error] {
	f.mu.Lock()
	err := f.begin("Logs", siteID, nil)
	if err == nil {
		_, err = f.lookup(siteID)
	}
	if err == nil {
		err = opts.Validate()
	}
	var out []provisioner.LogEntry
	var next int
	var wake <-chan struct{}
	if err == nil {
		stored := f.classifiedLogs(siteID)
		out = provisioner.SelectLogs(stored, opts)
		next = len(stored)
		wake = f.logWake()
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
		for opts.Follow {
			select {
			case <-ctx.Done():
				return
			case <-wake:
			}
			f.mu.Lock()
			stored := f.classifiedLogs(siteID)
			wake = f.logWake()
			fresh := slices.Clone(stored[next:])
			next = len(stored)
			f.mu.Unlock()
			for _, e := range fresh {
				if opts.Match(e) && !yield(e, nil) {
					return
				}
			}
		}
	}
}

// classifiedLogs returns the site's stored entries with Source and Level
// filled in. The caller holds f.mu.
func (f *Fake) classifiedLogs(siteID string) []provisioner.LogEntry {
	classifiers := map[provisioner.Component]*provisioner.LogClassifier{}
	out := slices.Clone(f.logs[siteID])
	for i, e := range out {
		if e.Source != "" {
			continue
		}
		c := classifiers[e.Component]
		if c == nil {
			c = provisioner.NewLogClassifier(e.Component)
			classifiers[e.Component] = c
		}
		out[i].Source, out[i].Level = c.Classify(e.Line)
	}
	return out
}

// logWake returns a channel that is closed by the next AddLogs. The caller
// holds f.mu.
func (f *Fake) logWake() <-chan struct{} {
	if f.logWakeCh == nil {
		f.logWakeCh = make(chan struct{})
	}
	return f.logWakeCh
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
	// A copy, so the caller changing its maps or slices afterwards doesn't
	// change the record. TransferDir and RepositoryDir are recorded as given.
	t := task
	t.Command = slices.Clone(task.Command)
	t.Env = maps.Clone(task.Env)
	t.SecretFiles = maps.Clone(task.SecretFiles)
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

// RunRepositoryTask implements provisioner.RepositoryTaskRunner. It records
// the call, and refuses what the contract does: a task with wp-content, or
// one that doesn't say where its repository is.
func (f *Fake) RunRepositoryTask(_ context.Context, repository string, task provisioner.Task) (*provisioner.TaskResult, error) {
	f.mu.Lock()
	if err := f.begin("RunRepositoryTask", repository, nil); err != nil {
		f.mu.Unlock()
		return nil, err
	}
	call := &f.calls[len(f.calls)-1]
	call.SiteID, call.Repository = "", repository
	t := task
	t.Command = slices.Clone(task.Command)
	t.Env = maps.Clone(task.Env)
	t.SecretFiles = maps.Clone(task.SecretFiles)
	call.Task = &t
	handler := f.RunRepositoryTaskFunc
	f.mu.Unlock()

	switch {
	case task.WPContent != provisioner.MountNone:
		return nil, fmt.Errorf("fake: repository task with wp-content (mount mode %d)", task.WPContent)
	case task.RepositoryDir == "" && !task.BackupNetwork:
		return nil, errors.New("fake: repository task without RepositoryDir or BackupNetwork")
	case handler == nil:
		return &provisioner.TaskResult{}, nil
	}
	return handler(repository, task)
}

// RunSession records the call and hands the session to RunSessionFunc for
// existing sites. By default a session consumes its input and ends with it,
// like a server whose client says nothing and leaves.
func (f *Fake) RunSession(ctx context.Context, siteID string, s provisioner.Session) error {
	f.mu.Lock()
	if err := f.begin("RunSession", siteID, s.Command); err != nil {
		f.mu.Unlock()
		return err
	}
	if _, err := f.lookup(siteID); err != nil {
		f.mu.Unlock()
		return err
	}
	handler := f.RunSessionFunc
	f.mu.Unlock()

	if handler != nil {
		return handler(ctx, siteID, s)
	}
	_, err := io.Copy(io.Discard, s.Stdin)
	return err
}

// SetLocalImage makes pinned the build the image tag names locally, as a
// local rebuild would; PinImage returns it from now on.
func (f *Fake) SetLocalImage(image, pinned string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.local[image] = pinned
}

// PublishImage makes pinned the newest build of the image tag in its
// registry; PullImage fetches it.
func (f *Fake) PublishImage(image, pinned string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.registry[image] = pinned
}

// PinImage implements provisioner.ImagePinner. An image that isn't local
// is fetched from the fake registry; without a build there, its build is
// sha256:<the SHA-256 of the tag>.
func (f *Fake) PinImage(_ context.Context, image string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin("PinImage", "", []string{image}); err != nil {
		return "", err
	}
	if pinned, ok := f.local[image]; ok {
		return pinned, nil
	}
	pinned, ok := f.registry[image]
	if !ok {
		pinned = fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(image)))
	}
	f.local[image] = pinned
	return pinned, nil
}

// PullImage implements provisioner.ImagePinner: the local build becomes
// the registry's, if it has one.
func (f *Fake) PullImage(_ context.Context, image string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin("PullImage", "", []string{image}); err != nil {
		return err
	}
	if pinned, ok := f.registry[image]; ok {
		f.local[image] = pinned
	}
	return nil
}
