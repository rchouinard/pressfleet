package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/api/gen"
	"github.com/rchouinard/pressfleet/control-plane/internal/authz"
	"github.com/rchouinard/pressfleet/control-plane/internal/httpserver"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
)

// Site logs (#78).

const (
	// defaultLogLimit and maxLogLimit bound a page of runtime log entries or
	// job log lines; maxLogTail bounds the backlog of a followed stream.
	defaultLogLimit = 200
	maxLogLimit     = 1000
	defaultLogTail  = 100
	// logKeepalive is how often an idle stream sends a comment, so proxies
	// keep it open and a vanished client's writes fail.
	logKeepalive = 15 * time.Second
)

// errNoLogs is a wiring bug: serve always passes the log reader.
var errNoLogs = errors.New("api: no log reader configured")

// ListSiteLogs returns a page of the site's runtime logs, newest matching
// entries first by page and oldest first within it. The cursor is the time
// in Unix nanoseconds that the older page ends before.
func (h *Handlers) ListSiteLogs(ctx context.Context, req gen.ListSiteLogsRequestObject) (gen.ListSiteLogsResponseObject, error) {
	if _, err := h.authorize(ctx, authz.ViewMonitoring, authz.Site(req.SiteID)); err != nil {
		return nil, err
	}
	if h.logs == nil {
		return nil, errNoLogs
	}
	if h.sites == nil {
		return nil, errNoSites
	}
	if _, _, err := h.sites.Get(ctx, req.SiteID); err != nil {
		return nil, siteError(err)
	}
	p := req.Params
	opts := logOptions(p.Source, p.Level, p.Q)
	if p.Since != nil {
		opts.Since = *p.Since
	}
	if p.Until != nil {
		opts.Until = *p.Until
	}
	if p.Cursor != nil {
		before, err := parseLogCursor(*p.Cursor)
		if err != nil {
			return nil, httpserver.NewProblem(http.StatusBadRequest, "The cursor is invalid; pass the next_cursor of a previous page.")
		}
		if opts.Until.IsZero() || before.Before(opts.Until) {
			opts.Until = before
		}
	}
	limit := defaultLogLimit
	if p.Limit != nil {
		limit = min(*p.Limit, maxLogLimit)
	}
	if !opts.Since.IsZero() && !opts.Until.IsZero() && !opts.Until.After(opts.Since) {
		if p.Cursor != nil {
			// The cursor reached the start of the window: nothing is older.
			return gen.ListSiteLogs200JSONResponse{Items: []gen.SiteLogEntry{}}, nil
		}
		return nil, httpserver.NewProblem(http.StatusBadRequest, "until must be after since.")
	}
	// Fetch one more than the page, to learn whether there are older
	// entries, and more while the entries that share the page's oldest time
	// reach the start of what was fetched: more of them, and older entries,
	// may follow.
	var page []provisioner.LogEntry
	var older bool
	for tail := limit + 1; ; tail *= 2 {
		opts.Tail = tail
		var found []provisioner.LogEntry
		for e, err := range h.logs.Logs(ctx, req.SiteID, opts) {
			if errors.Is(err, provisioner.ErrSiteNotFound) {
				// A site that is still being created has no containers,
				// and so no logs.
				break
			}
			if err != nil {
				return nil, err
			}
			found = append(found, e)
		}
		var complete bool
		page, older, complete = logPage(found, limit, tail)
		if complete || tail >= maxLogScan {
			break
		}
	}
	out := gen.ListSiteLogs200JSONResponse{Items: make([]gen.SiteLogEntry, len(page))}
	for i, e := range page {
		out.Items[i] = siteLogEntry(e)
	}
	if older {
		c := formatLogCursor(page[0].Time)
		out.NextCursor = &c
	}
	return out, nil
}

// maxLogScan bounds how many entries ListSiteLogs fetches to keep the
// entries of one time together on a page.
const maxLogScan = 16 * maxLogLimit

// logPage cuts the newest limit entries from found (oldest first), which
// holds at most fetched entries, and reports whether older ones remain.
// Entries that share a time with the page's oldest one stay in the page, so
// a cursor, which is a time, never splits them. complete is false if found
// may be missing entries that belong in the page.
func logPage(found []provisioner.LogEntry, limit, fetched int) (page []provisioner.LogEntry, older, complete bool) {
	start := max(len(found)-limit, 0)
	for start > 0 && found[start-1].Time.Equal(found[start].Time) {
		start--
	}
	truncated := len(found) == fetched
	return found[start:], start > 0 || truncated && len(found) > limit, start > 0 || !truncated
}

// logOptions converts the filters that the list and stream operations
// share.
func logOptions(sources *[]gen.SiteLogSource, level *gen.SiteLogLevel, q *string) provisioner.LogOptions {
	var opts provisioner.LogOptions
	if sources != nil {
		for _, s := range *sources {
			opts.Sources = append(opts.Sources, provisioner.LogSource(s))
		}
	}
	if level != nil {
		opts.MinLevel = provisioner.LogLevel(*level)
	}
	if q != nil {
		opts.Contains = *q
	}
	return opts
}

func siteLogEntry(e provisioner.LogEntry) gen.SiteLogEntry {
	out := gen.SiteLogEntry{
		Time:      e.Time.UTC(),
		Component: gen.SiteLogEntryComponent(e.Component),
		Source:    gen.SiteLogSource(e.Source),
		Level:     gen.SiteLogLevel(e.Level),
		Message:   e.Line,
	}
	if e.Stream != "" {
		s := gen.SiteLogEntryStream(e.Stream)
		out.Stream = &s
	}
	return out
}

// logCursor is the time in Unix nanoseconds: precise to what the log
// drivers keep, and as opaque to clients as any cursor.
func formatLogCursor(t time.Time) string { return strconv.FormatInt(t.UnixNano(), 10) }

func parseLogCursor(s string) (time.Time, error) {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n <= 0 {
		return time.Time{}, fmt.Errorf("invalid log cursor %q", s)
	}
	return time.Unix(0, n).UTC(), nil
}

// StreamSiteLogs follows the site's logs as server-sent events.
func (h *Handlers) StreamSiteLogs(ctx context.Context, req gen.StreamSiteLogsRequestObject) (gen.StreamSiteLogsResponseObject, error) {
	if _, err := h.authorize(ctx, authz.ViewMonitoring, authz.Site(req.SiteID)); err != nil {
		return nil, err
	}
	if h.logs == nil {
		return nil, errNoLogs
	}
	if h.sites == nil {
		return nil, errNoSites
	}
	if _, _, err := h.sites.Get(ctx, req.SiteID); err != nil {
		return nil, siteError(err)
	}
	p := req.Params
	opts := logOptions(p.Source, p.Level, p.Q)
	opts.Tail = defaultLogTail
	if p.Tail != nil {
		opts.Tail = min(*p.Tail, maxLogLimit)
	}
	started := time.Now()
	if p.LastEventID != nil && *p.LastEventID != "" {
		last, err := parseLogCursor(*p.LastEventID)
		if err != nil {
			return nil, httpserver.NewProblem(http.StatusBadRequest, "Last-Event-ID is invalid.")
		}
		// A reconnecting client has the backlog; continue after its last entry.
		opts.Since, opts.Tail = last.Add(time.Nanosecond), 0
	}

	// Read the backlog first, so that a site without containers is an
	// error response and not an empty stream.
	var backlog []provisioner.LogEntry
	for e, err := range h.logs.Logs(ctx, req.SiteID, opts) {
		if errors.Is(err, provisioner.ErrSiteNotFound) {
			return nil, fmt.Errorf("%w: the site has no running resources yet, so it has no logs", ErrConflict)
		}
		if err != nil {
			return nil, err
		}
		backlog = append(backlog, e)
	}
	follow := opts
	follow.Tail = 0
	switch {
	case len(backlog) > 0:
		follow.Since = backlog[len(backlog)-1].Time.Add(time.Nanosecond)
	case follow.Since.IsZero():
		follow.Since = started
	}
	follow.Follow = true

	ctx, cancel := context.WithCancel(ctx)
	pr, pw := io.Pipe()
	go func() {
		defer cancel()
		pw.CloseWithError(h.writeLogEvents(ctx, pw, req.SiteID, backlog, follow))
	}()
	return gen.StreamSiteLogs200TextEventStreamResponse{
		Body:    &logStream{PipeReader: pr, cancel: cancel},
		Headers: gen.StreamSiteLogs200ResponseHeaders{CacheControl: new("no-store")},
	}, nil
}

// logStream is the body of a followed log: closing it, which the response
// does when the client is gone, stops the follow.
type logStream struct {
	*io.PipeReader
	cancel context.CancelFunc
	once   sync.Once
}

func (s *logStream) Close() error {
	s.once.Do(s.cancel)
	return s.PipeReader.Close()
}

// writeLogEvents writes the backlog and then the followed entries to w as
// server-sent events until ctx ends, and returns the error that ended the
// stream early, if any; it has been sent as an error event then.
func (h *Handlers) writeLogEvents(ctx context.Context, w io.Writer, siteID string, backlog []provisioner.LogEntry, follow provisioner.LogOptions) error {
	for _, e := range backlog {
		if err := writeLogEvent(w, e); err != nil {
			return err
		}
	}
	type item struct {
		entry provisioner.LogEntry
		err   error
	}
	items := make(chan item)
	go func() {
		defer close(items)
		for e, err := range h.logs.Logs(ctx, siteID, follow) {
			select {
			case items <- item{e, err}:
			case <-ctx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()
	keepalive := time.NewTicker(logKeepalive)
	defer keepalive.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-keepalive.C:
			if _, err := io.WriteString(w, ": keepalive\n\n"); err != nil {
				return err
			}
		case it, ok := <-items:
			if !ok {
				return nil
			}
			if it.err != nil {
				return h.writeLogError(ctx, w, siteID, it.err)
			}
			if err := writeLogEvent(w, it.entry); err != nil {
				return err
			}
		}
	}
}

// writeLogEvent writes one entry as a `log` event whose ID lets a
// reconnecting client continue after it.
func writeLogEvent(w io.Writer, e provisioner.LogEntry) error {
	data, err := json.Marshal(siteLogEntry(e))
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "id: %s\nevent: log\ndata: %s\n\n", formatLogCursor(e.Time), data)
	return err
}

// writeLogError tells the client why the stream ends, as the Problem
// Details a failed request would have got, and returns the error.
func (h *Handlers) writeLogError(ctx context.Context, w io.Writer, siteID string, cause error) error {
	var p *httpserver.Problem
	switch {
	case errors.Is(cause, provisioner.ErrSiteNotFound):
		p = httpserver.NewProblem(http.StatusNotFound, "The site no longer exists.")
	default:
		h.log.ErrorContext(ctx, "following site logs failed", "site_id", siteID, "error", cause)
		p = httpserver.NewProblem(http.StatusInternalServerError, "")
	}
	data, err := json.Marshal(p)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "event: error\ndata: %s\n\n", data); err != nil {
		return err
	}
	return cause
}

// ListSiteJobLogs returns the log lines of the site's jobs, newest first.
// The cursor is the ID of the last line of the previous page.
func (h *Handlers) ListSiteJobLogs(ctx context.Context, req gen.ListSiteJobLogsRequestObject) (gen.ListSiteJobLogsResponseObject, error) {
	if _, err := h.authorize(ctx, authz.ViewJobs, authz.Site(req.SiteID)); err != nil {
		return nil, err
	}
	if h.db == nil {
		return nil, errNoDatabase
	}
	if h.sites == nil {
		return nil, errNoSites
	}
	if _, _, err := h.sites.Get(ctx, req.SiteID); err != nil {
		return nil, siteError(err)
	}
	var before int64
	if req.Params.Cursor != nil {
		n, err := strconv.ParseInt(*req.Params.Cursor, 10, 64)
		if err != nil || n <= 0 {
			return nil, httpserver.NewProblem(http.StatusBadRequest, "The cursor is invalid; pass the next_cursor of a previous page.")
		}
		before = n
	}
	limit := defaultLogLimit
	if req.Params.Limit != nil {
		limit = min(*req.Params.Limit, maxLogLimit)
	}
	// One more than the page, to learn whether there is an older one.
	lines, err := store.NewJobs(h.db).LogsForTarget(ctx, string(gen.JobTargetTypeSite), req.SiteID, before, limit+1)
	if err != nil {
		return nil, err
	}
	more := len(lines) > limit
	if more {
		lines = lines[:limit]
	}
	out := gen.ListSiteJobLogs200JSONResponse{Items: make([]gen.SiteJobLogEntry, len(lines))}
	for i, l := range lines {
		out.Items[i] = gen.SiteJobLogEntry{
			ID:      strconv.FormatInt(l.ID, 10),
			Time:    l.LoggedAt.UTC(),
			Level:   gen.SiteJobLogEntryLevel(l.Level),
			Message: l.Message,
			JobID:   l.JobID,
			JobType: l.JobType,
		}
		if l.StepName != "" {
			out.Items[i].Step = &l.StepName
		}
	}
	if more {
		c := strconv.FormatInt(lines[len(lines)-1].ID, 10)
		out.NextCursor = &c
	}
	return out, nil
}
