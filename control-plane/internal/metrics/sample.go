package metrics

import (
	"errors"
	"fmt"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Label names every site metric carries.
const (
	LabelSiteID        = "site_id"
	LabelClientGroupID = "client_group_id"
)

// ErrInvalidSample is wrapped by the error [Client.Write] returns when it
// dropped samples with an invalid name, label, or value. Valid samples in
// the same call are still written.
var ErrInvalidSample = errors.New("invalid metric sample")

// Sample is one data point of a time series.
type Sample struct {
	// Name is the metric name, following the convention in the package
	// documentation.
	Name string
	// Labels are the series' labels. It may be nil.
	Labels map[string]string
	// Value must be finite.
	Value float64
	// Time is when the value was observed. The zero time means "now" when
	// the sample is written.
	Time time.Time
}

var (
	metricNameRE = regexp.MustCompile(`^[a-zA-Z_:][a-zA-Z0-9_:]*$`)
	labelNameRE  = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)
)

// Validate reports why s can't be written, or nil.
func (s Sample) Validate() error {
	if !metricNameRE.MatchString(s.Name) {
		return fmt.Errorf("%w: metric name %q", ErrInvalidSample, s.Name)
	}
	for k := range s.Labels {
		if !labelNameRE.MatchString(k) || strings.HasPrefix(k, "__") {
			return fmt.Errorf("%w: label name %q on %s", ErrInvalidSample, k, s.Name)
		}
	}
	if math.IsNaN(s.Value) || math.IsInf(s.Value, 0) {
		return fmt.Errorf("%w: value %v of %s is not finite", ErrInvalidSample, s.Value, s.Name)
	}
	return nil
}

var labelEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)

// appendLine appends s in the Prometheus text exposition format, with its
// timestamp in milliseconds. Labels are sorted so output is deterministic.
func (s Sample) appendLine(b []byte, now time.Time) []byte {
	b = append(b, s.Name...)
	if len(s.Labels) > 0 {
		keys := make([]string, 0, len(s.Labels))
		for k := range s.Labels {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		b = append(b, '{')
		for i, k := range keys {
			if i > 0 {
				b = append(b, ',')
			}
			b = append(b, k...)
			b = append(b, `="`...)
			b = append(b, labelEscaper.Replace(s.Labels[k])...)
			b = append(b, '"')
		}
		b = append(b, '}')
	}
	t := s.Time
	if t.IsZero() {
		t = now
	}
	b = append(b, ' ')
	b = strconv.AppendFloat(b, s.Value, 'g', -1, 64)
	b = append(b, ' ')
	b = strconv.AppendInt(b, t.UnixMilli(), 10)
	return append(b, '\n')
}
