package compose

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"iter"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
)

const (
	// maxLogLine is the longest line the reader buffers; the rest of a
	// longer one becomes a line of its own. Docker splits at 16 KiB itself.
	maxLogLine = 1 << 20
	// logRetryDelay is how long a following reader waits before it
	// reattaches to a container whose log stream ended, e.g. because the
	// container was recreated by a rollout.
	logRetryDelay = time.Second
	// logMaxFailures is how many times in a row a following reader may
	// fail to attach before the error ends the stream.
	logMaxFailures = 5
)

// logsTimeFormat is how docker accepts and prints timestamps.
const logsTimeFormat = time.RFC3339Nano

// Logs reads the site's container logs (provisioner.LogReader).
//
// Docker keeps each container's output, and both nginx and PHP-FPM write
// all of their logs to it (images/README.md): nginx's access log to stdout
// and its error log to stderr, PHP-FPM's error and slow logs and PHP's own
// errors to the php container's stderr. The provisioner tells the sources
// apart by content (provisioner.LogClassifier), as a Kubernetes driver has
// to, because its pod logs merge the streams.
//
// Without filters, Tail is pushed down to docker. With them, the whole
// window is read and filtered here, which json-file rotation bounds to a
// few tens of megabytes per container (templates/compose/site.yaml.tmpl).
// A stopped site's logs can be read; one that was never rendered yields
// provisioner.ErrSiteNotFound.
//
// With Follow, the backlog is followed by new entries until ctx ends. A
// container that is recreated while following is picked up again.
func (p *Provisioner) Logs(ctx context.Context, siteID string, opts provisioner.LogOptions) iter.Seq2[provisioner.LogEntry, error] {
	return func(yield func(provisioner.LogEntry, error) bool) {
		if err := validSiteID(siteID); err != nil {
			yield(provisioner.LogEntry{}, fmt.Errorf("read logs: %w", err))
			return
		}
		if err := opts.Validate(); err != nil {
			yield(provisioner.LogEntry{}, fmt.Errorf("read logs of site %s: %w", siteID, err))
			return
		}
		comps := logComponents(opts)
		started := time.Now()
		backlog, err := p.fetchLogs(ctx, siteID, comps, opts)
		if err != nil {
			yield(provisioner.LogEntry{}, err)
			return
		}
		for _, e := range backlog {
			if !yield(e, nil) {
				return
			}
		}
		if !opts.Follow {
			return
		}
		// The follow starts where the backlog ended; the entries before
		// that are the backlog's, filtered or not.
		watermark := started
		if n := len(backlog); n > 0 && backlog[n-1].Time.After(watermark) {
			watermark = backlog[n-1].Time
		}
		p.followLogs(ctx, siteID, comps, opts, watermark, yield)
	}
}

// logComponents returns the components whose logs hold what opts selects.
func logComponents(opts provisioner.LogOptions) []provisioner.Component {
	var out []provisioner.Component
	for _, c := range []provisioner.Component{provisioner.ComponentPHP, provisioner.ComponentNginx} {
		if len(opts.Components) > 0 && !slices.Contains(opts.Components, c) {
			continue
		}
		if len(opts.Sources) > 0 && !slices.ContainsFunc(opts.Sources, func(s provisioner.LogSource) bool { return s.Component() == c }) {
			continue
		}
		out = append(out, c)
	}
	return out
}

// logContainers returns the container of each of comps.
func (p *Provisioner) logContainers(ctx context.Context, siteID string, comps []provisioner.Component) (map[provisioner.Component]string, error) {
	proj, err := p.existing(ctx, siteID)
	if err != nil {
		return nil, fmt.Errorf("read logs: %w", err)
	}
	containers, err := proj.Ps(ctx)
	if err != nil {
		return nil, fmt.Errorf("read logs of site %s: %w", siteID, err)
	}
	names := make(map[provisioner.Component]string, len(comps))
	for _, c := range comps {
		ctr, ok := findService(containers, c)
		if !ok {
			// A site without the component has none of its logs; the
			// other component's are still readable.
			continue
		}
		names[c] = ctr.Name
	}
	return names, nil
}

// logsArgs returns the docker arguments that read a container's logs.
func logsArgs(container string, since, until time.Time, tail int, follow bool) []string {
	args := []string{"container", "logs", "--timestamps"}
	if !since.IsZero() {
		args = append(args, "--since", since.UTC().Format(logsTimeFormat))
	}
	if !until.IsZero() {
		args = append(args, "--until", until.UTC().Format(logsTimeFormat))
	}
	if tail > 0 {
		args = append(args, "--tail", strconv.Itoa(tail))
	}
	if follow {
		args = append(args, "--follow")
	}
	return append(args, "--", container)
}

// fetchLogs reads the logs window of opts, ignoring Follow, and returns
// what opts selects, oldest first.
func (p *Provisioner) fetchLogs(ctx context.Context, siteID string, comps []provisioner.Component, opts provisioner.LogOptions) ([]provisioner.LogEntry, error) {
	names, err := p.logContainers(ctx, siteID, comps)
	if err != nil {
		return nil, err
	}
	// Tail can go to docker only if nothing else decides which lines count.
	// It also has to be the only limit: docker's own order of applying
	// --tail and --until isn't one to rely on.
	tail := 0
	if !opts.Filtered() && opts.Until.IsZero() {
		tail = opts.Tail
	}
	var all []provisioner.LogEntry
	for _, c := range comps {
		name, ok := names[c]
		if !ok {
			continue
		}
		var (
			mu  sync.Mutex
			raw []provisioner.LogEntry
		)
		emit := func(e provisioner.LogEntry) {
			e.Component = c
			mu.Lock()
			raw = append(raw, e)
			mu.Unlock()
		}
		stdout := &logLineWriter{stream: provisioner.Stdout, emit: emit}
		stderr := &logLineWriter{stream: provisioner.Stderr, emit: emit}
		err := p.taskDocker(ctx, stdout, stderr, logsArgs(name, opts.Since, opts.Until, tail, false)...)
		stdout.flush()
		stderr.flush()
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, fmt.Errorf("read logs of site %s: %w", siteID, ctxErr)
			}
			return nil, fmt.Errorf("read logs of site %s: %w", siteID, err)
		}
		// The streams were read side by side; classification needs the
		// lines in the order they were written.
		slices.SortStableFunc(raw, func(a, b provisioner.LogEntry) int { return a.Time.Compare(b.Time) })
		cl := provisioner.NewLogClassifier(c)
		for i := range raw {
			raw[i].Source, raw[i].Level = cl.Classify(raw[i].Line)
		}
		all = append(all, raw...)
	}
	return provisioner.SelectLogs(all, opts), nil
}

// followLogs yields the entries that arrive after watermark until ctx ends
// or yield returns false.
func (p *Provisioner) followLogs(ctx context.Context, siteID string, comps []provisioner.Component, opts provisioner.LogOptions, watermark time.Time, yield func(provisioner.LogEntry, error) bool) {
	ctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	defer wg.Wait()
	defer cancel()

	type message struct {
		entry provisioner.LogEntry
		err   error
	}
	msgs := make(chan message)
	send := func(m message) bool {
		select {
		case msgs <- m:
			return true
		case <-ctx.Done():
			return false
		}
	}
	for _, c := range comps {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := p.followComponent(ctx, siteID, c, watermark, func(e provisioner.LogEntry) bool { return send(message{entry: e}) }); err != nil {
				send(message{err: err})
			}
		}()
	}
	classifiers := map[provisioner.Component]*provisioner.LogClassifier{}
	for _, c := range comps {
		classifiers[c] = provisioner.NewLogClassifier(c)
	}
	for {
		select {
		case <-ctx.Done():
			return
		case m := <-msgs:
			if m.err != nil {
				yield(provisioner.LogEntry{}, m.err)
				return
			}
			e := m.entry
			e.Source, e.Level = classifiers[e.Component].Classify(e.Line)
			if opts.Match(e) && !yield(e, nil) {
				return
			}
		}
	}
}

// followComponent streams one component's new log lines to emit until ctx
// ends, reattaching when docker's stream ends while the site still has
// containers, and returns the error that makes it give up. Lines at or
// before the newest it has emitted are skipped after a reattach, which
// docker's --since doesn't exclude.
func (p *Provisioner) followComponent(ctx context.Context, siteID string, c provisioner.Component, watermark time.Time, emit func(provisioner.LogEntry) bool) error {
	failures := 0
	for {
		names, err := p.logContainers(ctx, siteID, []provisioner.Component{c})
		if err == nil && names[c] == "" {
			err = fmt.Errorf("read logs of site %s: %w", siteID, ErrNoContainer)
		}
		if err == nil {
			emitted := false
			stopped := false
			filter := func(e provisioner.LogEntry) {
				if !e.Time.After(watermark) || stopped {
					return
				}
				e.Component = c
				emitted = true
				watermark = e.Time
				if !emit(e) {
					stopped = true
				}
			}
			stdout := &logLineWriter{stream: provisioner.Stdout, emit: filter}
			stderr := &logLineWriter{stream: provisioner.Stderr, emit: filter}
			// Both writers run in docker's copy goroutines; serialize them
			// so the watermark is consistent.
			var mu sync.Mutex
			stdout.mu, stderr.mu = &mu, &mu
			err = p.taskDocker(ctx, stdout, stderr, logsArgs(names[c], watermark, time.Time{}, 0, true)...)
			stdout.flush()
			stderr.flush()
			if emitted {
				failures = 0
			}
		}
		if ctx.Err() != nil {
			return nil
		}
		if errors.Is(err, provisioner.ErrSiteNotFound) {
			return err
		}
		if err != nil {
			failures++
			if failures >= logMaxFailures {
				return fmt.Errorf("following logs of site %s: %w", siteID, err)
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(logRetryDelay):
		}
	}
}

// logLineWriter splits what docker logs --timestamps prints into entries.
// Lines without a timestamp (docker's own error messages, which share the
// standard error stream with the container's) are dropped.
type logLineWriter struct {
	stream provisioner.LogStream
	emit   func(provisioner.LogEntry)
	mu     *sync.Mutex // optional; held while emitting
	buf    []byte
}

func (w *logLineWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	start := 0
	for {
		i := bytes.IndexByte(w.buf[start:], '\n')
		if i < 0 {
			break
		}
		w.line(w.buf[start : start+i])
		start += i + 1
	}
	w.buf = append(w.buf[:0], w.buf[start:]...)
	if len(w.buf) > maxLogLine {
		w.line(w.buf)
		w.buf = w.buf[:0]
	}
	return len(p), nil
}

// flush emits the last line, which has no terminator if the container was
// killed mid-write.
func (w *logLineWriter) flush() {
	if len(w.buf) > 0 {
		w.line(w.buf)
		w.buf = nil
	}
}

func (w *logLineWriter) line(b []byte) {
	ts, text, ok := strings.Cut(strings.TrimSuffix(string(b), "\r"), " ")
	if !ok {
		return
	}
	t, err := time.Parse(logsTimeFormat, ts)
	if err != nil {
		return
	}
	if w.mu != nil {
		w.mu.Lock()
		defer w.mu.Unlock()
	}
	w.emit(provisioner.LogEntry{Time: t.UTC(), Stream: w.stream, Line: text})
}
