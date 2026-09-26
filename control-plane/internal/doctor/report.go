package doctor

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// WriteText writes a human-readable report: one line per check, with the fix
// indented below each warning or failure, then a summary.
func (r Report) WriteText(w io.Writer) error {
	width := 0
	for _, res := range r.Results {
		width = max(width, len(res.Name))
	}
	var b strings.Builder
	counts := map[Status]int{}
	for _, res := range r.Results {
		counts[res.Status]++
		name := res.Name
		if res.Severity == Recommended {
			name += "*"
		}
		fmt.Fprintf(&b, "%-4s  %-*s  %s\n", strings.ToUpper(string(res.Status)), width+1, name, res.Message)
		if res.Fix != "" && (res.Status == StatusFail || res.Status == StatusWarn) {
			indent := strings.Repeat(" ", 6)
			for i, line := range strings.Split(res.Fix, "\n") {
				prefix := "fix: "
				if i > 0 {
					prefix = "     "
				}
				fmt.Fprintf(&b, "%s%s%s\n", indent, prefix, line)
			}
		}
	}
	fmt.Fprintf(&b, "\n* recommended\n%d passed, %d failed, %d warned, %d skipped\n",
		counts[StatusPass], counts[StatusFail], counts[StatusWarn], counts[StatusSkip])
	_, err := io.WriteString(w, b.String())
	return err
}

// WriteJSON writes the report and its outcome ("ok", "warnings", or
// "failed") as indented JSON.
func (r Report) WriteJSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(struct {
		Outcome string   `json:"outcome"`
		Results []Result `json:"results"`
	}{r.Outcome().String(), r.Results})
}

func (o Outcome) String() string {
	switch o {
	case OK:
		return "ok"
	case Warnings:
		return "warnings"
	case Failed:
		return "failed"
	}
	return fmt.Sprintf("Outcome(%d)", int(o))
}
