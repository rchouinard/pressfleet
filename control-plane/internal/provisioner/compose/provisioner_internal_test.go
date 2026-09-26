package compose

import (
	"strings"
	"testing"

	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
)

func TestSitePhase(t *testing.T) {
	ctr := func(service, state, health string, exit int) Container {
		return Container{Name: "pf-x-" + service + "-1", Service: service, State: state, Health: health, ExitCode: exit}
	}
	tests := []struct {
		name       string
		containers []Container
		want       provisioner.SitePhase
		msg        string // substring of the message
	}{
		{"healthy", []Container{ctr("php", "running", "healthy", 0), ctr("nginx", "running", "healthy", 0)},
			provisioner.SiteRunning, ""},
		{"no health checks", []Container{ctr("php", "running", "", 0), ctr("nginx", "running", "", 0)},
			provisioner.SiteRunning, ""},
		{"starting", []Container{ctr("php", "running", "starting", 0), ctr("nginx", "running", "healthy", 0)},
			provisioner.SitePending, "php is starting"},
		{"unhealthy", []Container{ctr("php", "running", "healthy", 0), ctr("nginx", "running", "unhealthy", 0)},
			provisioner.SiteFailed, "nginx is unhealthy"},
		{"restarting", []Container{ctr("php", "restarting", "", 255), ctr("nginx", "running", "healthy", 0)},
			provisioner.SiteFailed, "php is restarting (last exit code 255)"},
		{"stopped", []Container{ctr("php", "exited", "", 0), ctr("nginx", "exited", "", 137)},
			provisioner.SiteStopped, "stopped"},
		{"created by update of stopped site", []Container{ctr("php", "created", "", 0), ctr("nginx", "exited", "", 0)},
			provisioner.SiteStopped, "stopped"},
		{"one stopped", []Container{ctr("php", "running", "healthy", 0), ctr("nginx", "exited", "", 1)},
			provisioner.SiteFailed, "nginx is exited (exit code 1)"},
		{"missing", []Container{ctr("php", "running", "healthy", 0)},
			provisioner.SiteFailed, "nginx container is missing"},
		{"paused", []Container{ctr("php", "paused", "", 0), ctr("nginx", "running", "healthy", 0)},
			provisioner.SiteFailed, "php is paused"},
		{"dead", []Container{ctr("php", "dead", "", 0), ctr("nginx", "running", "", 0)},
			provisioner.SiteFailed, "php is dead"},
		{"failure wins over starting", []Container{ctr("php", "running", "starting", 0), ctr("nginx", "restarting", "", 1)},
			provisioner.SiteFailed, "nginx is restarting"},
		{"other services ignored", []Container{
			ctr("php", "running", "", 0), ctr("nginx", "running", "", 0), ctr("task", "exited", "", 1),
		}, provisioner.SiteRunning, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, msg := sitePhase(tt.containers)
			if got != tt.want {
				t.Errorf("phase = %s, want %s (message %q)", got, tt.want, msg)
			}
			if tt.msg == "" && msg != "" || !strings.Contains(msg, tt.msg) {
				t.Errorf("message = %q, want it to contain %q", msg, tt.msg)
			}
		})
	}
}
