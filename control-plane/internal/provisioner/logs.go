package provisioner

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// ErrInvalidLogOptions is returned by [LogReader.Logs] for options that
// can't be satisfied, such as an unknown source.
var ErrInvalidLogOptions = errors.New("provisioner: invalid log options")

// LogSource is a kind of site log (§7.4). Each is written by one component
// to its container's output, where every orchestrator can read it, and told
// apart by its content ([LogClassifier]), because Kubernetes merges stdout
// and stderr.
type LogSource string

// Log sources.
const (
	// LogAccess is nginx's access log: one JSON object per request.
	LogAccess LogSource = "access"
	// LogError is nginx's error log.
	LogError LogSource = "error"
	// LogSlow is the PHP-FPM slow log: stack traces of requests that ran
	// longer than the slow log timeout, spanning several lines.
	LogSlow LogSource = "slow"
	// LogPHP is everything else the php component writes: PHP errors and
	// warnings, application output, and PHP-FPM's own messages.
	LogPHP LogSource = "php"
)

// LogSources lists every log source.
var LogSources = []LogSource{LogAccess, LogError, LogSlow, LogPHP}

// Valid reports whether s is a known source.
func (s LogSource) Valid() bool { return slices.Contains(LogSources, s) }

// LogLevel is the severity of a log entry.
type LogLevel string

// Log levels, from least to most severe.
const (
	LevelDebug LogLevel = "debug"
	LevelInfo  LogLevel = "info"
	LevelWarn  LogLevel = "warn"
	LevelError LogLevel = "error"
)

func (l LogLevel) rank() int {
	switch l {
	case LevelDebug:
		return 0
	case LevelInfo, "":
		return 1
	case LevelWarn:
		return 2
	case LevelError:
		return 3
	}
	return -1
}

// Valid reports whether l is a known level.
func (l LogLevel) Valid() bool { return l != "" && l.rank() >= 0 }

// Validate checks the options themselves, not whether a site has such logs.
func (o LogOptions) Validate() error {
	var errs []error
	for _, c := range o.Components {
		if c != ComponentNginx && c != ComponentPHP {
			errs = append(errs, fmt.Errorf("unknown component %q", c))
		}
	}
	for _, s := range o.Sources {
		if !s.Valid() {
			errs = append(errs, fmt.Errorf("unknown source %q", s))
		}
	}
	if o.MinLevel != "" && !o.MinLevel.Valid() {
		errs = append(errs, fmt.Errorf("unknown level %q", o.MinLevel))
	}
	if o.Tail < 0 {
		errs = append(errs, errors.New("tail must not be negative"))
	}
	if !o.Since.IsZero() && !o.Until.IsZero() && !o.Until.After(o.Since) {
		errs = append(errs, errors.New("until must be after since"))
	}
	if o.Follow && !o.Until.IsZero() {
		errs = append(errs, errors.New("follow can't be combined with until"))
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidLogOptions, err)
	}
	return nil
}

// Filtered reports whether the options select entries by anything a driver
// can't push down to the orchestrator: sources, content, or level.
func (o LogOptions) Filtered() bool {
	return len(o.Sources) > 0 || o.Contains != "" || o.MinLevel != ""
}

// Match reports whether e passes every option except Tail and Follow.
// Drivers apply it to entries after classifying them, whatever they pushed
// down to the orchestrator.
func (o LogOptions) Match(e LogEntry) bool {
	switch {
	case len(o.Components) > 0 && !slices.Contains(o.Components, e.Component):
		return false
	case len(o.Sources) > 0 && !slices.Contains(o.Sources, e.Source):
		return false
	case !o.Since.IsZero() && e.Time.Before(o.Since):
		return false
	case !o.Until.IsZero() && !e.Time.Before(o.Until):
		return false
	case o.MinLevel != "" && e.Level.rank() < o.MinLevel.rank():
		return false
	case o.Contains != "" && !strings.Contains(strings.ToLower(e.Line), strings.ToLower(o.Contains)):
		return false
	}
	return true
}

var (
	// An access log line starts as the log_format of images/site-nginx does.
	accessLine = regexp.MustCompile(`^\{"time":"`)
	accessStat = regexp.MustCompile(`"status":(\d{3})`)
	// nginx's error log: "2026/10/01 12:00:00 [error] 7#7: message".
	nginxLevel = regexp.MustCompile(`^\d{4}/\d\d/\d\d \d\d:\d\d:\d\d \[(\w+)\]`)
	// The first line of a PHP-FPM slow log entry, then its continuation
	// lines.
	slowHeader = regexp.MustCompile(`^\[\d\d-\w{3}-\d{4} \d\d:\d\d:\d\d\]  \[pool [^\]]+\] pid \d+$`)
	slowCont   = regexp.MustCompile(`^(script_filename = |\[0x[0-9a-f]+\] )`)
	phpLevel   = regexp.MustCompile(`^(?:\[[^\]]*\] )?(?:PHP )?(Fatal error|Parse error|Warning|Notice|Deprecated|WARNING|ERROR|NOTICE|DEBUG|ALERT)\b`)
)

// LogClassifier derives the source and level of lines in the order a
// component wrote them. It is not safe for concurrent use; a driver keeps
// one per component and container.
type LogClassifier struct {
	component Component
	inSlow    bool
}

// NewLogClassifier returns a classifier for the output of one component.
func NewLogClassifier(c Component) *LogClassifier { return &LogClassifier{component: c} }

// Classify returns the source and level of the next line.
func (c *LogClassifier) Classify(line string) (LogSource, LogLevel) {
	if c.component == ComponentNginx {
		return classifyNginx(line)
	}
	// A slow log entry is a header and the lines that follow it; any other
	// line ends it.
	switch {
	case slowHeader.MatchString(line):
		c.inSlow = true
		return LogSlow, LevelWarn
	case c.inSlow && slowCont.MatchString(line):
		return LogSlow, LevelWarn
	}
	c.inSlow = false
	level := LevelInfo
	if m := phpLevel.FindStringSubmatch(line); m != nil {
		switch m[1] {
		case "Fatal error", "Parse error", "ERROR", "ALERT":
			level = LevelError
		case "Warning", "WARNING":
			level = LevelWarn
		case "DEBUG":
			level = LevelDebug
		}
	}
	return LogPHP, level
}

func classifyNginx(line string) (LogSource, LogLevel) {
	if accessLine.MatchString(line) {
		if m := accessStat.FindStringSubmatch(line); m != nil {
			n, _ := strconv.Atoi(m[1])
			switch {
			case n >= 500:
				return LogAccess, LevelError
			case n >= 400:
				return LogAccess, LevelWarn
			}
		}
		return LogAccess, LevelInfo
	}
	level := LevelInfo
	if m := nginxLevel.FindStringSubmatch(line); m != nil {
		switch m[1] {
		case "debug":
			level = LevelDebug
		case "warn":
			level = LevelWarn
		case "error", "crit", "alert", "emerg":
			level = LevelError
		}
	}
	return LogError, level
}

// SelectLogs returns the entries that pass opts, oldest first, cut to the
// newest opts.Tail. Entries with equal times keep their order. Drivers that
// collect entries from several containers use it to merge them.
func SelectLogs(entries []LogEntry, opts LogOptions) []LogEntry {
	out := make([]LogEntry, 0, len(entries))
	for _, e := range entries {
		if opts.Match(e) {
			out = append(out, e)
		}
	}
	slices.SortStableFunc(out, func(a, b LogEntry) int { return a.Time.Compare(b.Time) })
	if opts.Tail > 0 && len(out) > opts.Tail {
		out = slices.Clone(out[len(out)-opts.Tail:])
	}
	return out
}

// Component returns the component that writes the source.
func (s LogSource) Component() Component {
	if s == LogAccess || s == LogError {
		return ComponentNginx
	}
	return ComponentPHP
}
