package compose_test

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"testing"

	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner/compose"
)

func TestProbeHTTP(t *testing.T) {
	f := runningFixture(t)
	f.docker.execOut = []byte("302")
	req := provisioner.ProbeRequest{Host: "acme.preview.example.test", Path: "/?pressfleet-health=1"}
	res, err := f.prov.ProbeHTTP(context.Background(), testSiteID, req)
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusFound {
		t.Errorf("status = %d, want 302", res.StatusCode)
	}
	// As the site's UID in php, under timeout(1), with the host and path
	// as argv after the script.
	got := f.docker.execArgs
	prefix := []string{"exec", "--no-TTY", "--user", "20042:20042", "--", "php", "timeout", "--kill-after=10s"}
	if len(got) != len(prefix)+7 || !slices.Equal(got[:len(prefix)], prefix) {
		t.Fatalf("exec argv = %q", got)
	}
	if tail := got[len(prefix)+1:]; tail[0] != "php" || tail[1] != "-r" || !slices.Equal(tail[3:], []string{"--", req.Host, req.Path}) {
		t.Errorf("command = %q", tail)
	}
}

func TestProbeHTTPErrors(t *testing.T) {
	f := runningFixture(t)
	for _, req := range []provisioner.ProbeRequest{
		{Host: "evil\r\nX-Injected: 1", Path: "/"},
		{Host: "acme.example.test", Path: "no-slash"},
		{Host: "acme.example.test", Path: "/with space"},
		{Host: "", Path: "/"},
	} {
		f.docker.execArgs = nil
		if _, err := f.prov.ProbeHTTP(context.Background(), testSiteID, req); !errors.Is(err, compose.ErrInvalidCommand) {
			t.Errorf("ProbeHTTP(%q) error = %v, want ErrInvalidCommand", req, err)
		}
		if f.docker.execArgs != nil {
			t.Errorf("ProbeHTTP(%q) ran exec anyway", req)
		}
	}

	ok := provisioner.ProbeRequest{Host: "acme.example.test", Path: "/"}
	f.docker.execOut = []byte("not a status")
	if _, err := f.prov.ProbeHTTP(context.Background(), testSiteID, ok); !errors.Is(err, compose.ErrProbeFailed) {
		t.Errorf("garbage output: error = %v, want ErrProbeFailed", err)
	}
	f.docker.execOut = nil
	f.docker.execErr = &compose.CommandError{Stderr: "connecting to nginx: Connection refused", Err: exitError(1)}
	if _, err := f.prov.ProbeHTTP(context.Background(), testSiteID, ok); !errors.Is(err, compose.ErrProbeFailed) {
		t.Errorf("failed probe: error = %v, want ErrProbeFailed", err)
	}
}
