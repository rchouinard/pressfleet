package api_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/api"
	"github.com/rchouinard/pressfleet/control-plane/internal/api/gen"
	"github.com/rchouinard/pressfleet/control-plane/internal/auth"
	"github.com/rchouinard/pressfleet/control-plane/internal/buildinfo"
	"github.com/rchouinard/pressfleet/control-plane/internal/doctor"
	"github.com/rchouinard/pressfleet/control-plane/internal/doctor/doctortest"
	"github.com/rchouinard/pressfleet/control-plane/internal/releasecheck"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
)

func TestGetPlatform(t *testing.T) {
	build := buildinfo.Info{Version: "v1.2.3", Commit: "0123456789abcdef", Modified: new(false), GoVersion: "go1.27.1", OS: "linux", Arch: "amd64"}
	h := newHandler(t, api.Options{Authenticator: &fakeAuth{}, SFTP: api.SFTPInfo{Enabled: true, Host: "fleet.example.com", Port: 2022}, Build: build})
	w := do(t, h, request{authed: true, method: http.MethodGet, target: "/api/v1/platform"})
	got := decodeJSON[gen.Platform](t, w.Body.Bytes())
	if w.Code != http.StatusOK || !got.Sftp.Enabled || got.Sftp.Host != "fleet.example.com" || got.Sftp.Port != 2022 {
		t.Errorf("platform: %d %s", w.Code, w.Body)
	}
	b := got.Build
	if b.Version != "v1.2.3" || b.Commit == nil || *b.Commit != "0123456789abcdef" || b.Modified == nil || *b.Modified ||
		b.GoVersion != "go1.27.1" || b.Os != "linux" || b.Arch != "amd64" {
		t.Errorf("build = %+v", b)
	}

	if got.Orchestrator != gen.Compose {
		t.Errorf("orchestrator = %q, want the default, compose", got.Orchestrator)
	}

	// An unknown commit is left out rather than sent empty.
	h = newHandler(t, api.Options{Authenticator: &fakeAuth{}, Build: buildinfo.Info{Version: "dev"}})
	w = do(t, h, request{authed: true, method: http.MethodGet, target: "/api/v1/platform"})
	if got := decodeJSON[gen.Platform](t, w.Body.Bytes()); got.Build.Commit != nil || got.Build.Modified != nil {
		t.Errorf("dev build = %s", w.Body)
	}
	if w := do(t, h, request{method: http.MethodGet, target: "/api/v1/platform"}); w.Code != http.StatusUnauthorized {
		t.Errorf("unauthenticated: %d", w.Code)
	}
}

func TestGetPlatformOrchestrator(t *testing.T) {
	h := newHandler(t, api.Options{Authenticator: &fakeAuth{}, Orchestrator: "kubernetes"})
	w := do(t, h, request{authed: true, method: http.MethodGet, target: "/api/v1/platform"})
	if got := decodeJSON[gen.Platform](t, w.Body.Bytes()); got.Orchestrator != gen.Kubernetes {
		t.Errorf("orchestrator = %q, want kubernetes (%s)", got.Orchestrator, w.Body)
	}
}

// The release check's result is reported when there is one, compared with
// the running version.
func TestGetPlatformRelease(t *testing.T) {
	checked := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	found := func(v string) api.LatestReleaseFunc {
		return func() (releasecheck.Result, bool) {
			return releasecheck.Result{LatestVersion: v, ReleaseURL: "https://example.com/" + v, CheckedAt: checked}, true
		}
	}
	tests := []struct {
		name    string
		version string
		latest  api.LatestReleaseFunc
		// want is the reported release; nil means none.
		want *gen.PlatformRelease
	}{
		{"check off", "v0.4.0", nil, nil},
		{"no result yet", "v0.4.0", func() (releasecheck.Result, bool) { return releasecheck.Result{}, false }, nil},
		{"update available", "v0.4.0", found("v0.5.0"),
			&gen.PlatformRelease{LatestVersion: "v0.5.0", ReleaseURL: "https://example.com/v0.5.0", CheckedAt: checked, UpdateAvailable: true}},
		{"up to date", "v0.5.0", found("v0.5.0"),
			&gen.PlatformRelease{LatestVersion: "v0.5.0", ReleaseURL: "https://example.com/v0.5.0", CheckedAt: checked}},
		{"development build", "dev", found("v0.5.0"),
			&gen.PlatformRelease{LatestVersion: "v0.5.0", ReleaseURL: "https://example.com/v0.5.0", CheckedAt: checked}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHandler(t, api.Options{Authenticator: &fakeAuth{}, Build: buildinfo.Info{Version: tt.version}, LatestRelease: tt.latest})
			w := do(t, h, request{authed: true, method: http.MethodGet, target: "/api/v1/platform"})
			if w.Code != http.StatusOK {
				t.Fatalf("platform: %d %s", w.Code, w.Body)
			}
			got := decodeJSON[gen.Platform](t, w.Body.Bytes()).Release
			if (got == nil) != (tt.want == nil) || (got != nil && (got.LatestVersion != tt.want.LatestVersion ||
				got.ReleaseURL != tt.want.ReleaseURL || !got.CheckedAt.Equal(tt.want.CheckedAt) || got.UpdateAvailable != tt.want.UpdateAvailable)) {
				t.Errorf("release = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestPlatformDoctorEndpoint(t *testing.T) {
	docker := &doctortest.Docker{}
	var asked int
	run := func(ctx context.Context) doctor.Report {
		asked++
		return doctor.Run(ctx, doctor.Options{Host: doctortest.Healthy(), Docker: docker, DataDir: "/data", SkipHostOnly: true},
			doctor.ScopeInstall, doctor.ScopeRuntime)
	}
	viewer := &auth.Principal{UserID: "01j9zq3v6t8k4m2n5p7r9s1x3y", Role: store.RoleViewer, Method: auth.MethodSession, SessionID: "test"}
	h := newHandler(t, api.Options{Authenticator: &fakeAuth{principal: viewer}, Doctor: run})

	if w := do(t, h, request{method: http.MethodGet, target: "/api/v1/platform/doctor"}); w.Code != http.StatusUnauthorized {
		t.Errorf("unauthenticated: %d", w.Code)
	}
	w := do(t, h, request{authed: true, method: http.MethodGet, target: "/api/v1/platform/doctor"})
	if w.Code != http.StatusOK {
		t.Fatalf("viewer: %d %s", w.Code, w.Body)
	}
	rep := decodeJSON[gen.DoctorReport](t, w.Body.Bytes())
	if rep.Outcome != gen.DoctorReportOutcomeOk {
		t.Errorf("outcome = %q, want ok: host-only checks are unavailable, not failed", rep.Outcome)
	}
	byID := map[string]gen.DoctorResult{}
	for _, r := range rep.Results {
		byID[r.ID] = r
	}
	if r := byID["docker"]; r.Status != gen.DoctorResultStatusUnavailable || r.Severity != gen.Required || r.Message == "" {
		t.Errorf("docker = %+v, want unavailable", r)
	}
	if r := byID["platform"]; r.Status != gen.DoctorResultStatusPass || r.Name == "" {
		t.Errorf("platform = %+v, want pass", r)
	}
	if docker.EngineCalls != 0 {
		t.Error("the control plane asked Docker")
	}
	if asked != 1 {
		t.Errorf("doctor ran %d times", asked)
	}
}

func TestPlatformDoctorReportsFailures(t *testing.T) {
	host := doctortest.Healthy()
	host.Free = 1 << 30
	run := func(ctx context.Context) doctor.Report {
		return doctor.Run(ctx, doctor.Options{Host: host, Docker: &doctortest.Docker{}, DataDir: "/data", SkipHostOnly: true}, doctor.ScopeRuntime)
	}
	h := newHandler(t, api.Options{Authenticator: &fakeAuth{}, Doctor: run})
	w := do(t, h, request{authed: true, method: http.MethodGet, target: "/api/v1/platform/doctor"})
	rep := decodeJSON[gen.DoctorReport](t, w.Body.Bytes())
	if rep.Outcome != gen.DoctorReportOutcomeWarnings {
		t.Fatalf("outcome = %q: %s", rep.Outcome, w.Body)
	}
	for _, r := range rep.Results {
		if r.ID == "disk" && (r.Status != gen.DoctorResultStatusWarn || r.Fix == nil || *r.Fix == "") {
			t.Errorf("disk = %+v, want a warning with a fix", r)
		}
	}
}
