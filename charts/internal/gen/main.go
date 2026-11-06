// Command gen writes the chart's copies of platform files
// (charts.SharedFiles). go generate runs it in package charts's directory.
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/rchouinard/pressfleet/charts"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "gen:", err)
		os.Exit(1)
	}
}

func run() error {
	for _, f := range charts.SharedFiles {
		content, err := charts.Shared(f)
		if err != nil {
			return err
		}
		path := filepath.Join(charts.ChartDir, filepath.FromSlash(f.Chart))
		if err := os.WriteFile(path, content, 0o644); err != nil { //nolint:gosec // G306: a source file in the repository, readable by all
			return err
		}
	}
	return nil
}
