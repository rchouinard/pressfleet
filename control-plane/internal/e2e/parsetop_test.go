package e2e_test

import (
	"slices"
	"testing"

	"github.com/rchouinard/pressfleet/control-plane/internal/e2e"
)

func TestParseTop(t *testing.T) {
	out := `pfe2e-abc-control-plane-1
UID    PID    PPID   C    STIME   TTY   TIME       CMD
root   1001   990    0    12:00   ?     00:00:01   /usr/local/bin/pressfleet serve
root   1050   1001   0    12:01   ?     00:00:00   docker container start --attach -- pf-task
root   1060   1001   0    12:01   ?     00:00:00   /usr/local/bin/restic --no-cache snapshots
`
	got := e2e.ParseTop(out)
	if want := []string{"pressfleet", "docker", "restic"}; !slices.Equal(got, want) {
		t.Errorf("ParseTop = %v, want %v", got, want)
	}
	if got := e2e.ParseTop("name\n"); len(got) != 0 {
		t.Errorf("ParseTop of a name only = %v", got)
	}
}
