package compose

import (
	"errors"
	"slices"
	"testing"
	"time"
)

func TestParseFPMStatus(t *testing.T) {
	const body = "Content-type: application/json\r\nX-Powered-By: PHP/8.3\r\n\r\n" +
		`{"pool":"www","process manager":"ondemand","idle processes":2,"active processes":3,` +
		`"total processes":5,"max children reached":4,"slow requests":7}`
	t.Run("full", func(t *testing.T) {
		st, err := parseFPMStatus([]byte(body + "\nmax_children=8\n"))
		if err != nil {
			t.Fatal(err)
		}
		if st.ActiveProcesses != 3 || st.IdleProcesses != 2 || st.MaxChildren != 8 ||
			st.MaxChildrenReached != 4 || st.SlowRequests != 7 {
			t.Errorf("status = %+v", st)
		}
	})
	t.Run("no max children", func(t *testing.T) {
		st, err := parseFPMStatus([]byte(body + "\nmax_children=\n"))
		if err != nil || st.MaxChildren != 0 {
			t.Errorf("status = %+v, err = %v", st, err)
		}
	})
	for name, in := range map[string]string{
		"empty":    "",
		"no json":  "Status: 404\r\n\r\nFile not found.\n",
		"bad json": "\r\n\r\n{\"idle processes\":",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseFPMStatus([]byte(in)); !errors.Is(err, ErrFPMStatus) {
				t.Errorf("err = %v, want ErrFPMStatus", err)
			}
		})
	}
}

func TestFPMArgv(t *testing.T) {
	argv := fpmArgv("20001:20001", 10*time.Second)
	if !slices.Contains(argv, "20001:20001") || argv[len(argv)-3] != "sh" || argv[len(argv)-2] != "-c" {
		t.Errorf("argv = %q", argv)
	}
	if slices.Contains(argv, wpcliPath) {
		t.Errorf("argv still runs WP-CLI: %q", argv)
	}
}
