package kubernetes

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"iter"
	"slices"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
)

const (
	// maxLogLine is the longest line the reader keeps; the rest of a
	// longer one is dropped.
	maxLogLine = 1 << 20
	// logRetryDelay is how long a following reader waits before it
	// reattaches to a container whose log stream ended, e.g. because the
	// pod was replaced by a rollout.
	logRetryDelay = time.Second
	// logMaxFailures is how many times in a row a following reader may
	// fail to attach before the error ends the stream.
	logMaxFailures = 5
)

// ErrNoPod is returned when following a site's logs and the site has no
// pod to read them from (it is stopped, or its pod hasn't come up).
var ErrNoPod = errors.New("kubernetes: the site has no running pod")

// Logs reads the site's container logs (provisioner.LogReader) from the
// Pod logs API, one stream per container.
//
// Kubernetes merges a container's stdout and stderr, so Stream stays empty
// and the lines are told apart by content (provisioner.LogClassifier), as
// on Compose. Without filters, Tail is pushed down to the API server; with
// them the whole window is read and filtered here, which the kubelet's log
// rotation bounds. Logs of a pod that was deleted are gone, so a stopped
// site has none; its Logs yield nothing rather than an error. A site that
// does not exist yields provisioner.ErrSiteNotFound.
//
// With Follow, the backlog is followed by new entries until ctx ends. A pod
// that is replaced while following is picked up again.
func (p *Provisioner) Logs(ctx context.Context, siteID string, opts provisioner.LogOptions) iter.Seq2[provisioner.LogEntry, error] {
	return func(yield func(provisioner.LogEntry, error) bool) {
		if err := opts.Validate(); err != nil {
			yield(provisioner.LogEntry{}, fmt.Errorf("read logs of site %s: %w", siteID, err))
			return
		}
		if _, err := p.deployment(ctx, siteID); err != nil {
			yield(provisioner.LogEntry{}, fmt.Errorf("read logs: %w", err))
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

// currentPod returns the site's newest pod that has the component's
// container, or nil if there is none. During a rollout the old pod
// lingers; the new one is where the logs go on.
func (p *Provisioner) currentPod(ctx context.Context, siteID string, c provisioner.Component) (*corev1.Pod, error) {
	pods, err := p.typed.CoreV1().Pods(p.SiteNamespace(siteID)).List(ctx, metav1.ListOptions{LabelSelector: sitePodSelector})
	if err != nil {
		return nil, fmt.Errorf("list pods of site %s: %w", siteID, err)
	}
	return newestPod(pods.Items, string(c)), nil
}

// newestPod returns the newest pod that isn't finished and has a container
// of the given name.
func newestPod(pods []corev1.Pod, container string) *corev1.Pod {
	var best *corev1.Pod
	for i := range pods {
		pod := &pods[i]
		if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
			continue
		}
		if !slices.ContainsFunc(pod.Spec.Containers, func(c corev1.Container) bool { return c.Name == container }) {
			continue
		}
		if best == nil || pod.CreationTimestamp.After(best.CreationTimestamp.Time) {
			best = pod
		}
	}
	return best
}

// openLogs opens one container's log stream.
func (p *Provisioner) openLogs(ctx context.Context, ns, pod string, o *corev1.PodLogOptions) (io.ReadCloser, error) {
	if p.observe.podLogs != nil {
		return p.observe.podLogs(ctx, ns, pod, o)
	}
	return p.typed.CoreV1().Pods(ns).GetLogs(pod, o).Stream(ctx)
}

// logOptions returns the Pod logs API options for a container.
func logOptions(container string, since time.Time, tail int, follow bool) *corev1.PodLogOptions {
	o := &corev1.PodLogOptions{Container: container, Timestamps: true, Follow: follow}
	if !since.IsZero() {
		t := metav1.NewTime(since)
		o.SinceTime = &t
	}
	if tail > 0 {
		n := int64(tail)
		o.TailLines = &n
	}
	return o
}

// fetchLogs reads the logs window of opts, ignoring Follow, and returns
// what opts selects, oldest first.
func (p *Provisioner) fetchLogs(ctx context.Context, siteID string, comps []provisioner.Component, opts provisioner.LogOptions) ([]provisioner.LogEntry, error) {
	// Tail can go to the API server only if nothing else decides which
	// lines count; it is per container there, and SelectLogs trims the
	// merged result.
	tail := 0
	if !opts.Filtered() && opts.Until.IsZero() {
		tail = opts.Tail
	}
	ns := p.SiteNamespace(siteID)
	var all []provisioner.LogEntry
	for _, c := range comps {
		pod, err := p.currentPod(ctx, siteID, c)
		if err != nil {
			return nil, fmt.Errorf("read logs: %w", err)
		}
		if pod == nil {
			continue
		}
		rc, err := p.openLogs(ctx, ns, pod.Name, logOptions(string(c), opts.Since, tail, false))
		if err != nil {
			if apierrors.IsBadRequest(err) {
				// The container is waiting to start: it has no logs yet.
				continue
			}
			return nil, fmt.Errorf("read logs of site %s: %w", siteID, err)
		}
		cl := provisioner.NewLogClassifier(c)
		err = readLogLines(rc, func(e provisioner.LogEntry) bool {
			e.Component = c
			e.Source, e.Level = cl.Classify(e.Line)
			all = append(all, e)
			return true
		})
		_ = rc.Close()
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, fmt.Errorf("read logs of site %s: %w", siteID, ctxErr)
			}
			return nil, fmt.Errorf("read logs of site %s: %w", siteID, err)
		}
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
	classifiers := map[provisioner.Component]*provisioner.LogClassifier{}
	for _, c := range comps {
		classifiers[c] = provisioner.NewLogClassifier(c)
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := p.followComponent(ctx, siteID, c, watermark, func(e provisioner.LogEntry) bool { return send(message{entry: e}) }); err != nil {
				send(message{err: err})
			}
		}()
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
// ends, reattaching when the stream ends while the site still has its pod,
// and returns the error that makes it give up. Lines at or before the
// newest it has emitted are skipped after a reattach, which the API
// server's sinceTime doesn't exclude.
func (p *Provisioner) followComponent(ctx context.Context, siteID string, c provisioner.Component, watermark time.Time, emit func(provisioner.LogEntry) bool) error {
	ns := p.SiteNamespace(siteID)
	failures := 0
	for {
		pod, err := p.currentPod(ctx, siteID, c)
		if err == nil && pod == nil {
			err = fmt.Errorf("follow logs of site %s: %w", siteID, ErrNoPod)
		}
		if err == nil {
			var rc io.ReadCloser
			rc, err = p.openLogs(ctx, ns, pod.Name, logOptions(string(c), watermark, 0, true))
			if err == nil {
				emitted := false
				stopped := false
				err = readLogLines(rc, func(e provisioner.LogEntry) bool {
					if !e.Time.After(watermark) {
						return true
					}
					e.Component = c
					emitted = true
					watermark = e.Time
					if !emit(e) {
						stopped = true
						return false
					}
					return true
				})
				_ = rc.Close()
				if stopped {
					return nil
				}
				if emitted {
					failures = 0
				}
			}
		}
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			if apierrors.IsNotFound(err) {
				// The pod went away between the list and the open.
				err = fmt.Errorf("follow logs of site %s: %w", siteID, ErrNoPod)
			}
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

// readLogLines reads timestamped log lines and passes them to emit until
// the stream ends or emit returns false. A line without a timestamp can't
// be placed and is dropped.
func readLogLines(r io.Reader, emit func(provisioner.LogEntry) bool) error {
	br := bufio.NewReaderSize(r, 64<<10)
	for {
		line, err := readLine(br)
		if len(line) > 0 {
			if e, ok := parseLogLine(line); ok && !emit(e) {
				return nil
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}

// readLine reads up to a newline, keeping at most maxLogLine bytes of it.
func readLine(br *bufio.Reader) (string, error) {
	var sb strings.Builder
	for {
		chunk, err := br.ReadSlice('\n')
		if room := maxLogLine - sb.Len(); room > 0 {
			sb.Write(chunk[:min(len(chunk), room)])
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		return strings.TrimRight(sb.String(), "\r\n"), err
	}
}

// parseLogLine splits "<RFC 3339 timestamp> <text>" as the Pod logs API
// prints it with timestamps.
func parseLogLine(s string) (provisioner.LogEntry, bool) {
	ts, text, ok := strings.Cut(s, " ")
	if !ok {
		// A timestamp and an empty line.
		ts, text = s, ""
	}
	t, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		return provisioner.LogEntry{}, false
	}
	return provisioner.LogEntry{Time: t.UTC(), Line: text}, true
}
