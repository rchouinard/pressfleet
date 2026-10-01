package metricsapi

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/metrics"
)

// ErrInvalid means the request asks for something outside the limits or
// names something unknown. The error text says what, and is safe to show
// the caller.
var ErrInvalid = errors.New("invalid metrics request")

// Limits bound a request, so one chart can't make VictoriaMetrics scan
// unbounded data.
const (
	DefaultRange = time.Hour
	MaxRange     = 366 * 24 * time.Hour
	MinStep      = 15 * time.Second
	// MaxPoints is the most points one series may have.
	MaxPoints = 1500
	// MaxMetrics is the most metric ids (after expanding groups) one
	// request may name.
	MaxMetrics = 20
	// targetPoints is how many points an automatically chosen step aims
	// for: enough for a smooth chart, cheap to query and send.
	targetPoints = 300
	// minWindow is the shortest rate window: collectors report every 30
	// seconds by default, and a rate needs at least two samples.
	minWindow = 2 * time.Minute
	// SummaryWindow is the rate window of instant queries.
	SummaryWindow = 5 * time.Minute
	// concurrency bounds the queries one request runs at once.
	concurrency = 8
)

// niceSteps are the steps chosen automatically, so a chart's points fall on
// the same times from one request to the next.
var niceSteps = []time.Duration{
	15 * time.Second, 30 * time.Second, time.Minute, 2 * time.Minute, 5 * time.Minute,
	10 * time.Minute, 15 * time.Minute, 30 * time.Minute, time.Hour, 2 * time.Hour,
	6 * time.Hour, 12 * time.Hour, 24 * time.Hour,
}

// Request selects metrics of one scope.
type Request struct {
	// Site is true for the site endpoints (and SiteID is then required),
	// false for the platform ones.
	Site          bool
	SiteID        string
	ClientGroupID string
	// Metrics are catalog ids and Groups are group names, whose entries are
	// added. Together they name at most [MaxMetrics] entries.
	Metrics []string
	Groups  []string
	// Start, End, and Step apply to range requests; zero values get
	// defaults.
	Start, End time.Time
	Step       time.Duration
}

// Point is one value at one time.
type Point struct {
	Time  time.Time
	Value float64
}

// Series is one line of a chart.
type Series struct {
	// Labels tell the series apart from the others of its entry; empty if
	// the entry has one.
	Labels map[string]string
	Points []Point
}

// Result is the series of one entry. Series is empty, not an error, while
// nothing feeds the entry.
type Result struct {
	Entry  Entry
	Series []Series
}

// RangeResult is the answer to [Service.Range].
type RangeResult struct {
	// Start, End, and Step are what was queried, after defaults, clamping,
	// and alignment to Step.
	Start, End time.Time
	Step       time.Duration
	Results    []Result
}

// SummaryResult is the answer to [Service.Summary]. Every series has the
// one point that the instant query returned.
type SummaryResult struct {
	Time    time.Time
	Results []Result
}

// Service answers metric requests from the catalog.
type Service struct {
	q   metrics.Querier
	now func() time.Time
}

// New returns a service that queries q. A nil q means metrics aren't
// available: every request fails with [metrics.ErrUnavailable]. now defaults
// to time.Now.
func New(q metrics.Querier, now func() time.Time) *Service {
	if now == nil {
		now = time.Now
	}
	return &Service{q: q, now: now}
}

// Range returns the time series of the requested entries.
func (s *Service) Range(ctx context.Context, req Request) (RangeResult, error) {
	entries, err := resolve(req)
	if err != nil {
		return RangeResult{}, err
	}
	start, end, step, err := s.window(req)
	if err != nil {
		return RangeResult{}, err
	}
	sel := Selector{SiteID: req.SiteID, ClientGroupID: req.ClientGroupID}
	window := max(step, minWindow)
	results, err := s.run(ctx, entries, sel, window, func(ctx context.Context, expr string) (metrics.Result, error) {
		return s.q.QueryRange(ctx, expr, start, end, step)
	})
	if err != nil {
		return RangeResult{}, err
	}
	return RangeResult{Start: start, End: end, Step: step, Results: results}, nil
}

// Summary returns the current value of the requested entries. Without ids
// or groups it returns the entries marked [Entry.Summary].
func (s *Service) Summary(ctx context.Context, req Request) (SummaryResult, error) {
	var entries []Entry
	var err error
	if len(req.Metrics) == 0 && len(req.Groups) == 0 {
		for _, e := range scopeEntries(req.Site) {
			if e.Summary {
				entries = append(entries, e)
			}
		}
	} else if entries, err = resolve(req); err != nil {
		return SummaryResult{}, err
	}
	if err := checkSelector(req); err != nil {
		return SummaryResult{}, err
	}
	sel := Selector{SiteID: req.SiteID, ClientGroupID: req.ClientGroupID}
	at := s.now().UTC()
	results, err := s.run(ctx, entries, sel, SummaryWindow, func(ctx context.Context, expr string) (metrics.Result, error) {
		return s.q.Query(ctx, expr, time.Time{})
	})
	if err != nil {
		return SummaryResult{}, err
	}
	return SummaryResult{Time: at, Results: results}, nil
}

func scopeEntries(site bool) []Entry {
	if site {
		return SiteEntries()
	}
	return PlatformEntries()
}

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}

func checkSelector(req Request) error {
	if req.Site && !ValidID(req.SiteID) {
		return invalid("the site ID is malformed")
	}
	if req.ClientGroupID != "" && (req.Site || !ValidID(req.ClientGroupID)) {
		return invalid("client_group_id is malformed, or not allowed here")
	}
	return nil
}

// resolve turns ids and groups into entries of the request's scope, without
// duplicates, in catalog order.
func resolve(req Request) ([]Entry, error) {
	if err := checkSelector(req); err != nil {
		return nil, err
	}
	if len(req.Metrics) == 0 && len(req.Groups) == 0 {
		return nil, invalid("name at least one metric or group")
	}
	available := scopeEntries(req.Site)
	want := map[string]bool{}
	for _, id := range req.Metrics {
		e, ok := Lookup(id)
		if !ok || !slices.ContainsFunc(available, func(a Entry) bool { return a.ID == e.ID }) {
			return nil, invalid("unknown metric %q for this endpoint; see GET /api/v1/metrics/catalog", truncate(id))
		}
		want[id] = true
	}
	for _, g := range req.Groups {
		found := false
		for _, e := range available {
			if e.Group == g {
				want[e.ID], found = true, true
			}
		}
		if !found {
			return nil, invalid("unknown group %q for this endpoint; see GET /api/v1/metrics/catalog", truncate(g))
		}
	}
	var out []Entry
	for _, e := range available {
		if want[e.ID] {
			out = append(out, e)
		}
	}
	if len(out) > MaxMetrics {
		return nil, invalid("a request may select at most %d metrics, got %d", MaxMetrics, len(out))
	}
	return out, nil
}

// truncate shortens a caller-supplied string before it goes into an error.
func truncate(s string) string {
	if len(s) > 40 {
		return s[:40] + "..."
	}
	return s
}

// window applies the defaults and limits to the request's times, and aligns
// them to the step.
func (s *Service) window(req Request) (start, end time.Time, step time.Duration, err error) {
	now := s.now().UTC()
	end = req.End
	if end.IsZero() {
		end = now
	}
	end = end.UTC()
	if end.After(now) {
		end = now
	}
	start = req.Start
	if start.IsZero() {
		start = end.Add(-DefaultRange)
	}
	start = start.UTC()
	if !start.Before(end) {
		return start, end, 0, invalid("start must be before end")
	}
	if end.Sub(start) > MaxRange {
		return start, end, 0, invalid("the range may be at most %d days", MaxRange/(24*time.Hour))
	}
	step = req.Step
	switch {
	case step == 0:
		step = autoStep(end.Sub(start))
	case step < MinStep:
		return start, end, 0, invalid("step must be at least %d seconds", MinStep/time.Second)
	case step%time.Second != 0:
		return start, end, 0, invalid("step must be a whole number of seconds")
	}
	if step > end.Sub(start) {
		return start, end, 0, invalid("step must not be longer than the range")
	}
	// Aligned times repeat from request to request, which lets
	// VictoriaMetrics reuse what it computed.
	start = start.Truncate(step)
	end = end.Truncate(step)
	if !end.After(start) {
		start = end.Add(-step)
	}
	if n := int(end.Sub(start)/step) + 1; n > MaxPoints {
		return start, end, 0, invalid("the range and step give %d points per series, at most %d are allowed; raise the step to at least %d seconds",
			n, MaxPoints, int((end.Sub(start)/time.Duration(MaxPoints-1)+time.Second-1)/time.Second))
	}
	return start, end, step, nil
}

// autoStep is the smallest nice step that keeps a range at about
// targetPoints points.
func autoStep(d time.Duration) time.Duration {
	for _, s := range niceSteps {
		if d/s <= targetPoints {
			return s
		}
	}
	return niceSteps[len(niceSteps)-1]
}

// run executes the queries of every entry, at most [concurrency] at once.
// The first failure stops the rest.
func (s *Service) run(ctx context.Context, entries []Entry, sel Selector, window time.Duration,
	query func(context.Context, string) (metrics.Result, error)) ([]Result, error) {
	if s.q == nil {
		return nil, fmt.Errorf("%w: the metrics store isn't configured", metrics.ErrUnavailable)
	}
	type task struct {
		entry int
		r     Rendered
	}
	var tasks []task
	for i, e := range entries {
		for _, r := range e.Render(sel, window) {
			tasks = append(tasks, task{i, r})
		}
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		mu       sync.Mutex
		wg       sync.WaitGroup
		firstErr error
		gathered = make([][]Series, len(tasks))
		slots    = make(chan struct{}, concurrency)
	)
	for i, t := range tasks {
		wg.Add(1)
		slots <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-slots }()
			res, err := query(ctx, t.r.Expr)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if firstErr == nil {
					firstErr = fmt.Errorf("querying metric %s: %w", entries[t.entry].ID, err)
					cancel()
				}
				return
			}
			gathered[i] = convert(res, entries[t.entry].Scope, t.r.Labels)
		}()
	}
	wg.Wait()
	if firstErr != nil {
		return nil, firstErr
	}
	out := make([]Result, len(entries))
	for i, e := range entries {
		out[i] = Result{Entry: e, Series: []Series{}}
	}
	for i, t := range tasks {
		out[t.entry].Series = append(out[t.entry].Series, gathered[i]...)
	}
	for i := range out {
		slices.SortStableFunc(out[i].Series, func(a, b Series) int { return strings.Compare(labelKey(a.Labels), labelKey(b.Labels)) })
	}
	return out, nil
}

// convert maps a query result to series: without the labels that every
// series of the scope shares and the ones the store adds, with the query's
// own labels added, and without points that can't be charted (NaN is what
// a division by zero returns).
func convert(res metrics.Result, scope Scope, extra map[string]string) []Series {
	var out []Series
	for _, s := range res.Series {
		labels := make(map[string]string, len(s.Labels)+len(extra))
		for k, v := range s.Labels {
			if k == "__name__" || (scope == ScopeSite && (k == metrics.LabelSiteID || k == metrics.LabelClientGroupID)) {
				continue
			}
			labels[k] = v
		}
		maps.Copy(labels, extra)
		points := make([]Point, 0, len(s.Points))
		for _, p := range s.Points {
			if math.IsNaN(p.Value) || math.IsInf(p.Value, 0) {
				continue
			}
			points = append(points, Point{Time: p.Time.UTC(), Value: p.Value})
		}
		if len(points) > 0 {
			out = append(out, Series{Labels: labels, Points: points})
		}
	}
	return out
}

func labelKey(labels map[string]string) string {
	var b strings.Builder
	for _, k := range slices.Sorted(maps.Keys(labels)) {
		b.WriteString(k + "=" + labels[k] + "\xff")
	}
	return b.String()
}
