package compose

import (
	"slices"
	"testing"
	"time"
)

func TestFPMArgv(t *testing.T) {
	argv := fpmArgv("20001:20001", 10*time.Second)
	if !slices.Contains(argv, "20001:20001") || argv[len(argv)-3] != "sh" || argv[len(argv)-2] != "-c" {
		t.Errorf("argv = %q", argv)
	}
	if slices.Contains(argv, wpcliPath) {
		t.Errorf("argv still runs WP-CLI: %q", argv)
	}
}
