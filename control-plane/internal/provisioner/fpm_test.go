package provisioner

import (
	"errors"
	"testing"
)

func TestParseFPMStatus(t *testing.T) {
	errStatus := errors.New("test: no status")
	const body = "Content-type: application/json\r\nX-Powered-By: PHP/8.3\r\n\r\n" +
		`{"pool":"www","process manager":"ondemand","idle processes":2,"active processes":3,` +
		`"total processes":5,"max children reached":4,"slow requests":7}`
	t.Run("full", func(t *testing.T) {
		st, err := ParseFPMStatus([]byte(body+"\nmax_children=8\n"), errStatus)
		if err != nil {
			t.Fatal(err)
		}
		if st.ActiveProcesses != 3 || st.IdleProcesses != 2 || st.MaxChildren != 8 ||
			st.MaxChildrenReached != 4 || st.SlowRequests != 7 {
			t.Errorf("status = %+v", st)
		}
	})
	t.Run("no max children", func(t *testing.T) {
		st, err := ParseFPMStatus([]byte(body+"\nmax_children=\n"), errStatus)
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
			if _, err := ParseFPMStatus([]byte(in), errStatus); !errors.Is(err, errStatus) {
				t.Errorf("err = %v, want errStatus", err)
			}
		})
	}
}
