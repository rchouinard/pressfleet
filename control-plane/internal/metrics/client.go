package metrics

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Errors callers branch on, compared with errors.Is.
var (
	// ErrUnavailable means VictoriaMetrics could not be reached, or kept
	// answering with server errors, through every attempt.
	ErrUnavailable = errors.New("victoriametrics unavailable")
	// ErrRejected means VictoriaMetrics refused a write with a client error
	// (4xx). Retrying the same data won't help.
	ErrRejected = errors.New("victoriametrics rejected the write")
	// ErrQuery means VictoriaMetrics rejected a query, for example because
	// it doesn't parse. The error text carries VictoriaMetrics' message.
	ErrQuery = errors.New("query failed")
)

// Writer writes samples to the metrics store.
type Writer interface {
	// Write stores samples. It is all or nothing per batch, not per call:
	// on error, some batches may already be stored.
	Write(ctx context.Context, samples []Sample) error
}

// Querier reads from the metrics store with PromQL (MetricsQL).
type Querier interface {
	// Query evaluates q at one instant; the zero time means now.
	Query(ctx context.Context, q string, at time.Time) (Result, error)
	// QueryRange evaluates q at every step from start to end.
	QueryRange(ctx context.Context, q string, start, end time.Time, step time.Duration) (Result, error)
}

// ResultType is the shape of a query result, as Prometheus names it.
type ResultType string

// Result types. A scalar result is returned as a [Vector] with one series
// without labels.
const (
	Vector ResultType = "vector"
	Matrix ResultType = "matrix"
)

// Result is a query result.
type Result struct {
	Type ResultType
	// Series are the matching series. An instant query's series have one
	// point each, a range query's one per step that has a value.
	Series []Series
}

// Series is one time series in a [Result].
type Series struct {
	// Labels includes __name__ when the query kept the metric name.
	Labels map[string]string
	Points []Point
}

// Point is one value at one time.
type Point struct {
	Time  time.Time
	Value float64
}

// Defaults of [Options].
const (
	DefaultBatchSize   = 5000
	DefaultTimeout     = 10 * time.Second
	DefaultMaxAttempts = 3
	DefaultBackoff     = 250 * time.Millisecond
	maxBackoff         = 5 * time.Second
	maxErrorBody       = 512
)

// Options configure a [Client].
type Options struct {
	// URL is VictoriaMetrics' base URL, such as http://victoria-metrics:8428.
	URL string
	// HTTPClient sends the requests; default is a client without its own
	// timeout, as Timeout bounds each attempt.
	HTTPClient *http.Client
	// BatchSize is the most samples sent in one request; default
	// [DefaultBatchSize].
	BatchSize int
	// Timeout bounds each HTTP attempt; default [DefaultTimeout].
	Timeout time.Duration
	// MaxAttempts is how many times a request is tried in all, so 1 means no
	// retry; default [DefaultMaxAttempts]. Network errors, 429, and 5xx are
	// retried.
	MaxAttempts int
	// Backoff is the wait before the first retry, doubling each time up to
	// five seconds; default [DefaultBackoff].
	Backoff time.Duration
	// Now returns the current time; default time.Now. Samples without a
	// time get it.
	Now func() time.Time
	// Logger receives retry notices; default discards.
	Logger *slog.Logger
}

// Client talks to VictoriaMetrics. It implements [Writer] and [Querier] and
// is safe for concurrent use.
type Client struct {
	base        string
	http        *http.Client
	batchSize   int
	timeout     time.Duration
	maxAttempts int
	backoff     time.Duration
	now         func() time.Time
	log         *slog.Logger
}

var (
	_ Writer  = (*Client)(nil)
	_ Querier = (*Client)(nil)
)

// NewClient returns a client for opts.URL.
func NewClient(opts Options) (*Client, error) {
	u, err := url.Parse(opts.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("victoriametrics URL must be an http or https URL, got %q", opts.URL)
	}
	c := &Client{
		base:        strings.TrimRight(opts.URL, "/"),
		http:        opts.HTTPClient,
		batchSize:   opts.BatchSize,
		timeout:     opts.Timeout,
		maxAttempts: opts.MaxAttempts,
		backoff:     opts.Backoff,
		now:         opts.Now,
		log:         opts.Logger,
	}
	if c.http == nil {
		c.http = &http.Client{}
	}
	if c.batchSize <= 0 {
		c.batchSize = DefaultBatchSize
	}
	if c.timeout <= 0 {
		c.timeout = DefaultTimeout
	}
	if c.maxAttempts <= 0 {
		c.maxAttempts = DefaultMaxAttempts
	}
	if c.backoff <= 0 {
		c.backoff = DefaultBackoff
	}
	if c.now == nil {
		c.now = time.Now
	}
	if c.log == nil {
		c.log = slog.New(slog.DiscardHandler)
	}
	return c, nil
}

// Write implements [Writer]. Samples that fail [Sample.Validate] are
// dropped and reported in an error wrapping [ErrInvalidSample] after the
// valid ones are sent.
func (c *Client) Write(ctx context.Context, samples []Sample) error {
	now := c.now()
	var (
		invalid []error
		sent    int
		buf     []byte
	)
	flush := func() error {
		if sent == 0 {
			return nil
		}
		_, status, err := c.do(ctx, "/api/v1/import/prometheus", "text/plain; charset=utf-8", buf)
		buf, sent = buf[:0], 0
		if err != nil {
			return err
		}
		if status/100 != 2 {
			return fmt.Errorf("%w: status %d", ErrRejected, status)
		}
		return nil
	}
	for _, s := range samples {
		if err := s.Validate(); err != nil {
			invalid = append(invalid, err)
			continue
		}
		buf = s.appendLine(buf, now)
		if sent++; sent >= c.batchSize {
			if err := flush(); err != nil {
				return err
			}
		}
	}
	if err := flush(); err != nil {
		return err
	}
	if len(invalid) > 0 {
		// Name the first problem only: a collector that gets one wrong
		// gets them all wrong.
		return fmt.Errorf("dropped %d samples, first: %w", len(invalid), invalid[0])
	}
	return nil
}

// Query implements [Querier].
func (c *Client) Query(ctx context.Context, q string, at time.Time) (Result, error) {
	form := url.Values{"query": {q}}
	if !at.IsZero() {
		form.Set("time", formatTime(at))
	}
	return c.query(ctx, "/api/v1/query", form)
}

// QueryRange implements [Querier].
func (c *Client) QueryRange(ctx context.Context, q string, start, end time.Time, step time.Duration) (Result, error) {
	if step <= 0 {
		return Result{}, fmt.Errorf("%w: step must be positive", ErrQuery)
	}
	form := url.Values{
		"query": {q},
		"start": {formatTime(start)},
		"end":   {formatTime(end)},
		"step":  {strconv.FormatFloat(step.Seconds(), 'f', -1, 64)},
	}
	return c.query(ctx, "/api/v1/query_range", form)
}

func formatTime(t time.Time) string {
	return strconv.FormatFloat(float64(t.UnixMilli())/1000, 'f', 3, 64)
}

// apiResponse is the Prometheus HTTP API envelope.
type apiResponse struct {
	Status    string `json:"status"`
	ErrorType string `json:"errorType"`
	Error     string `json:"error"`
	Data      struct {
		ResultType string          `json:"resultType"`
		Result     json.RawMessage `json:"result"`
	} `json:"data"`
}

type apiSeries struct {
	Metric map[string]string `json:"metric"`
	Value  *[2]any           `json:"value"`
	Values [][2]any          `json:"values"`
}

func (c *Client) query(ctx context.Context, path string, form url.Values) (Result, error) {
	// POST, so long queries don't hit URL length limits.
	body, status, err := c.do(ctx, path, "application/x-www-form-urlencoded", []byte(form.Encode()))
	if err != nil {
		return Result{}, err
	}
	var resp apiResponse
	if jerr := json.Unmarshal(body, &resp); jerr != nil {
		if status/100 != 2 {
			return Result{}, fmt.Errorf("%w: status %d: %s", ErrQuery, status, snippet(body))
		}
		return Result{}, fmt.Errorf("%w: decoding response: %w", ErrQuery, jerr)
	}
	if resp.Status != "success" || status/100 != 2 {
		msg := resp.Error
		if msg == "" {
			msg = snippet(body)
		}
		return Result{}, fmt.Errorf("%w: %s", ErrQuery, msg)
	}
	res, err := decodeResult(resp.Data.ResultType, resp.Data.Result)
	if err != nil {
		return Result{}, fmt.Errorf("%w: %w", ErrQuery, err)
	}
	return res, nil
}

func decodeResult(kind string, raw json.RawMessage) (Result, error) {
	switch kind {
	case "scalar":
		var pair [2]any
		if err := json.Unmarshal(raw, &pair); err != nil {
			return Result{}, fmt.Errorf("decoding scalar: %w", err)
		}
		p, err := decodePoint(pair)
		if err != nil {
			return Result{}, err
		}
		return Result{Type: Vector, Series: []Series{{Labels: map[string]string{}, Points: []Point{p}}}}, nil
	case "vector", "matrix":
		var in []apiSeries
		if err := json.Unmarshal(raw, &in); err != nil {
			return Result{}, fmt.Errorf("decoding %s: %w", kind, err)
		}
		res := Result{Type: ResultType(kind), Series: make([]Series, 0, len(in))}
		for _, s := range in {
			out := Series{Labels: s.Metric}
			if out.Labels == nil {
				out.Labels = map[string]string{}
			}
			if s.Value != nil {
				p, err := decodePoint(*s.Value)
				if err != nil {
					return Result{}, err
				}
				out.Points = append(out.Points, p)
			}
			for _, v := range s.Values {
				p, err := decodePoint(v)
				if err != nil {
					return Result{}, err
				}
				out.Points = append(out.Points, p)
			}
			res.Series = append(res.Series, out)
		}
		return res, nil
	}
	return Result{}, fmt.Errorf("unsupported result type %q", kind)
}

// decodePoint decodes Prometheus's [unix seconds, "value"] pair.
func decodePoint(pair [2]any) (Point, error) {
	ts, ok := pair[0].(float64)
	if !ok {
		return Point{}, fmt.Errorf("sample time %v is not a number", pair[0])
	}
	str, ok := pair[1].(string)
	if !ok {
		return Point{}, fmt.Errorf("sample value %v is not a string", pair[1])
	}
	v, err := strconv.ParseFloat(str, 64)
	if err != nil {
		return Point{}, fmt.Errorf("sample value %q: %w", str, err)
	}
	return Point{Time: time.UnixMilli(int64(ts * 1000)).UTC(), Value: v}, nil
}

func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > maxErrorBody {
		s = s[:maxErrorBody] + "..."
	}
	return s
}

// do POSTs body to path, retrying network errors, 429, and 5xx with
// exponential backoff. It returns the response of the first other
// status, whatever it is, for the caller to judge.
func (c *Client) do(ctx context.Context, path, contentType string, body []byte) ([]byte, int, error) {
	var last error
	wait := c.backoff
	for attempt := 1; ; attempt++ {
		data, status, err := c.once(ctx, path, contentType, body)
		if err == nil && status != http.StatusTooManyRequests && status < 500 {
			return data, status, nil
		}
		if err == nil {
			err = fmt.Errorf("status %d: %s", status, snippet(data))
		}
		last = err
		if cerr := ctx.Err(); cerr != nil {
			return nil, 0, cerr
		}
		if attempt >= c.maxAttempts {
			return nil, 0, fmt.Errorf("%w: %s after %d attempts: %w", ErrUnavailable, path, attempt, last)
		}
		c.log.Debug("retrying VictoriaMetrics request", "path", path, "attempt", attempt, "error", last)
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil, 0, ctx.Err()
		case <-t.C:
		}
		wait = min(wait*2, maxBackoff)
	}
}

func (c *Client) once(ctx context.Context, path, contentType string, body []byte) ([]byte, int, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, bytes.NewReader(body))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", contentType)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, 0, err
	}
	return data, resp.StatusCode, nil
}
