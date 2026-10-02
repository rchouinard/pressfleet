package api_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/rchouinard/pressfleet/control-plane/internal/api"
	"github.com/rchouinard/pressfleet/control-plane/internal/api/gen"
	"github.com/rchouinard/pressfleet/control-plane/internal/auth"
	"github.com/rchouinard/pressfleet/control-plane/internal/doctor"
	"github.com/rchouinard/pressfleet/control-plane/internal/doctor/doctortest"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
)

func TestGetPlatform(t *testing.T) {
	h := newHandler(t, api.Options{Authenticator: &fakeAuth{}, SFTP: api.SFTPInfo{Enabled: true, Host: "fleet.example.com", Port: 2022}})
	w := do(t, h, request{authed: true, method: http.MethodGet, target: "/api/v1/platform"})
	got := decodeJSON[gen.Platform](t, w.Body.Bytes())
	if w.Code != http.StatusOK || !got.Sftp.Enabled || got.Sftp.Host != "fleet.example.com" || got.Sftp.Port != 2022 {
		t.Errorf("platform: %d %s", w.Code, w.Body)
	}
	if w := do(t, h, request{method: http.MethodGet, target: "/api/v1/platform"}); w.Code != http.StatusUnauthorized {
		t.Errorf("unauthenticated: %d", w.Code)
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
