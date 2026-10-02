package metrics

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"
)

// NamePrefix starts every metric name.
const NamePrefix = "pressfleet_"

// ErrCollectorPanic is wrapped by the error recorded for a run of a
// collector that panicked.
var ErrCollectorPanic = errors.New("collector panicked")

// ServiceOptions configure a [Service].
type ServiceOptions struct {
	// Writer receives the samples.
	Writer Writer
	// Targets lists the sites, once per run of each collector.
	Targets TargetSource
	// DefaultInterval applies to collectors that don't ask for their own;
	// it must be positive.
	DefaultInterval time.Duration
	// Timeout bounds one run of a collector, from listing targets to the
	// end of Collect. Zero means the collector's interval, so a run never
	// overlaps the next one.
	Timeout time.Duration
	// Now returns the current time; default time.Now.
	Now func() time.Time
	// Logger is required.
	Logger *slog.Logger
}

// Service runs collectors on their own intervals and writes what they
// return. Each collector has its own goroutine, so a slow, failing, or
// panicking collector delays and harms no other.
//
// Besides the collectors' samples, every run writes the collector's own
// metrics, labelled collector=<name>:
//
//	pressfleet_collector_scrape_duration_seconds           gauge, last run's duration
//	pressfleet_collector_scrapes_total{result}             counter, result is "success" or "error"
//	pressfleet_collector_samples_written_total             counter, samples stored
//	pressfleet_collector_samples_dropped_total             counter, samples dropped as invalid
//	pressfleet_collector_write_errors_total                counter, failed writes
//	pressfleet_collector_targets                           gauge, sites in the last run
//	pressfleet_collector_last_success_timestamp_seconds    gauge, Unix time of the last successful run
//
// The counters live in memory and start from zero at each restart, which
// rate() and increase() handle. A write that fails loses that run's
// samples, self-metrics included; the counters catch up with the next
// write.
type Service struct {
	opts ServiceOptions

	mu         sync.Mutex
	collectors []Collector
	started    bool
}

// NewService returns a service without collectors; add them with
// [Service.Register] before [Service.Run].
func NewService(opts ServiceOptions) (*Service, error) {
	switch {
	case opts.Writer == nil:
		return nil, errors.New("metrics service: a writer is required")
	case opts.Targets == nil:
		return nil, errors.New("metrics service: a target source is required")
	case opts.Logger == nil:
		return nil, errors.New("metrics service: a logger is required")
	case opts.DefaultInterval <= 0:
		return nil, errors.New("metrics service: the default interval must be positive")
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Service{opts: opts}, nil
}

// Register adds a collector. It fails for a duplicate or invalid name, and
// once the service is running.
func (s *Service) Register(c Collector) error {
	name := c.Name()
	if name == "" || strings.ContainsFunc(name, func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '_'
	}) {
		return fmt.Errorf("register collector: name %q must be snake_case", name)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started {
		return errors.New("register collector: the service is running")
	}
	for _, o := range s.collectors {
		if o.Name() == name {
			return fmt.Errorf("register collector: %q is already registered", name)
		}
	}
	s.collectors = append(s.collectors, c)
	return nil
}

// Run runs every collector until ctx ends, then waits for runs in
// progress to finish. It returns nil on a normal stop.
func (s *Service) Run(ctx context.Context) error {
	s.mu.Lock()
	s.started = true
	collectors := append([]Collector(nil), s.collectors...)
	s.mu.Unlock()

	var wg sync.WaitGroup
	for _, c := range collectors {
		wg.Go(func() { s.loop(ctx, c) })
	}
	wg.Wait()
	return nil
}

// runState is a collector's counters, owned by its loop.
type runState struct {
	ok, failed      float64
	written         float64
	dropped         float64
	writeErrors     float64
	lastSuccess     time.Time
	haveLastSuccess bool
}

func (s *Service) loop(ctx context.Context, c Collector) {
	interval := c.Interval()
	if interval <= 0 {
		interval = s.opts.DefaultInterval
	}
	log := s.opts.Logger.With("collector", c.Name())
	st := &runState{}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		s.run(ctx, c, interval, st, log)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *Service) run(ctx context.Context, c Collector, interval time.Duration, st *runState, log *slog.Logger) {
	if ctx.Err() != nil {
		return
	}
	timeout := s.opts.Timeout
	if timeout <= 0 {
		timeout = interval
	}
	start := s.opts.Now()
	cctx, cancel := context.WithTimeout(ctx, timeout)
	targets, samples, err := s.collect(cctx, c)
	cancel()
	took := s.opts.Now().Sub(start)
	if ctx.Err() != nil {
		// Shutting down: the run was cut short, don't count it.
		return
	}

	samples, dropped := normalize(samples, targets, start)
	st.dropped += float64(dropped)
	if dropped > 0 {
		log.Warn("dropped samples that break the metric conventions", "count", dropped)
	}
	result := "success"
	if err != nil {
		result = "error"
		st.failed++
		log.Warn("collector run failed", "error", err, "duration", took)
	} else {
		st.ok++
		st.lastSuccess, st.haveLastSuccess = start, true
	}

	// The samples written counter includes this run's samples only once
	// the write succeeds, so the next run reports them.
	out := slices.Concat(samples, s.selfMetrics(c.Name(), st, took, len(targets), start))
	if werr := s.opts.Writer.Write(ctx, out); werr != nil {
		if ctx.Err() != nil {
			return
		}
		st.writeErrors++
		log.Error("writing collector samples failed", "error", werr, "samples", len(out))
		return
	}
	st.written += float64(len(samples))
	log.Debug("collector run done", "result", result, "samples", len(samples), "targets", len(targets), "duration", took)
}

// collect lists the targets and calls the collector, turning a panic into
// an error.
func (s *Service) collect(ctx context.Context, c Collector) (targets []Target, samples []Sample, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%w: %v", ErrCollectorPanic, r)
		}
	}()
	targets, err = s.opts.Targets.Targets(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("list targets: %w", err)
	}
	samples, err = c.Collect(ctx, targets)
	if err != nil {
		err = fmt.Errorf("collect: %w", err)
	}
	return targets, samples, err
}

// normalize enforces the conventions on a collector's samples: the metric
// prefix, a client_group_id on every site sample (taken from the target,
// whatever the collector set), no sample for a site or a client group
// outside targets, and a time. It returns the samples to write and how many it dropped.
func normalize(samples []Sample, targets []Target, runStart time.Time) ([]Sample, int) {
	groups := make(map[string]string, len(targets))
	known := make(map[string]bool, len(targets))
	for _, t := range targets {
		groups[t.SiteID] = t.ClientGroupID
		known[t.ClientGroupID] = true
	}
	out := make([]Sample, 0, len(samples))
	dropped := 0
	for _, sm := range samples {
		if !strings.HasPrefix(sm.Name, NamePrefix) || strings.HasPrefix(sm.Name, NamePrefix+"collector_") {
			dropped++
			continue
		}
		if siteID, ok := sm.Labels[LabelSiteID]; ok {
			group, known := groups[siteID]
			if !known {
				dropped++
				continue
			}
			// Copy before changing: the collector may share the map
			// between samples.
			labels := make(map[string]string, len(sm.Labels)+1)
			for k, v := range sm.Labels {
				labels[k] = v
			}
			labels[LabelClientGroupID] = group
			sm.Labels = labels
		} else if group, ok := sm.Labels[LabelClientGroupID]; ok && !known[group] {
			// A per-group metric (the Restic repository's size) is for a
			// group with sites; one deleted mid-run is dropped like a site.
			dropped++
			continue
		}
		if sm.Time.IsZero() {
			sm.Time = runStart
		}
		out = append(out, sm)
	}
	return out, dropped
}

func (s *Service) selfMetrics(name string, st *runState, took time.Duration, targets int, at time.Time) []Sample {
	l := func(kv ...string) map[string]string {
		m := map[string]string{"collector": name}
		for i := 0; i < len(kv); i += 2 {
			m[kv[i]] = kv[i+1]
		}
		return m
	}
	out := []Sample{
		{Name: "pressfleet_collector_scrape_duration_seconds", Labels: l(), Value: took.Seconds(), Time: at},
		{Name: "pressfleet_collector_scrapes_total", Labels: l("result", "success"), Value: st.ok, Time: at},
		{Name: "pressfleet_collector_scrapes_total", Labels: l("result", "error"), Value: st.failed, Time: at},
		{Name: "pressfleet_collector_samples_written_total", Labels: l(), Value: st.written, Time: at},
		{Name: "pressfleet_collector_samples_dropped_total", Labels: l(), Value: st.dropped, Time: at},
		{Name: "pressfleet_collector_write_errors_total", Labels: l(), Value: st.writeErrors, Time: at},
		{Name: "pressfleet_collector_targets", Labels: l(), Value: float64(targets), Time: at},
	}
	if st.haveLastSuccess {
		out = append(out, Sample{
			Name: "pressfleet_collector_last_success_timestamp_seconds", Labels: l(),
			Value: float64(st.lastSuccess.UnixMilli()) / 1000, Time: at,
		})
	}
	return out
}
