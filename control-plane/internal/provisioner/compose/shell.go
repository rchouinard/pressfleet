package compose

import (
	"context"
	"fmt"
	"os/exec"
)

// RunShell runs a command line meant for an operator's shell, such as a
// step of the recovery a failed `pressfleet upgrade` prints, with sh -c,
// and returns its combined output. The end-to-end test of that recovery
// (#142) runs the printed commands verbatim through it, which keeps the
// docker CLI they call behind this package (§4.2). Nothing else uses it.
func RunShell(ctx context.Context, line string) ([]byte, error) {
	out, err := exec.CommandContext(ctx, "sh", "-c", line).CombinedOutput() //nolint:gosec // G204: running the operator's command line is the point
	if err != nil {
		return out, fmt.Errorf("%s: %w", line, err)
	}
	return out, nil
}
