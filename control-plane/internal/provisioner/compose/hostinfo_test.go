package compose

import (
	"context"
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const sampleInfo = `{"ServerVersion":"27.3.1","OSType":"linux","Architecture":"x86_64",
"CgroupVersion":"2","LoggingDriver":"json-file",
"DefaultAddressPools":[{"Base":"10.200.0.0/16","Size":24}]}`

func TestParseEngineInfo(t *testing.T) {
	info, err := parseEngineInfo([]byte(sampleInfo))
	if err != nil {
		t.Fatal(err)
	}
	if info.ServerVersion != "27.3.1" || info.OSType != "linux" || info.Architecture != "x86_64" ||
		info.CgroupVersion != "2" || info.LoggingDriver != "json-file" {
		t.Errorf("info = %+v", info)
	}
	want := AddressPool{Base: netip.MustParsePrefix("10.200.0.0/16"), Size: 24}
	if len(info.DefaultAddressPools) != 1 || info.DefaultAddressPools[0] != want {
		t.Errorf("pools = %+v, want [%+v]", info.DefaultAddressPools, want)
	}
}

func TestParseEngineInfoErrors(t *testing.T) {
	for name, in := range map[string]string{
		"not JSON":      "Cannot connect to the Docker daemon",
		"server errors": `{"ServerErrors":["Cannot connect to the Docker daemon at unix:///var/run/docker.sock"]}`,
		"bad pool":      `{"DefaultAddressPools":[{"Base":"nonsense","Size":24}]}`,
	} {
		if _, err := parseEngineInfo([]byte(in)); err == nil {
			t.Errorf("%s: parseEngineInfo() = nil error", name)
		}
	}
}

func TestParseDaemonLogOpts(t *testing.T) {
	opts, err := parseDaemonLogOpts([]byte(`{"log-driver":"json-file","log-opts":{"max-size":"10m","max-file":"3"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if opts["max-size"] != "10m" || opts["max-file"] != "3" {
		t.Errorf("opts = %v", opts)
	}
	if _, err := parseDaemonLogOpts([]byte("{")); err == nil {
		t.Error("parseDaemonLogOpts(invalid) = nil error")
	}
}

// fakeDocker writes a shell script standing in for the docker CLI.
func fakeDocker(t *testing.T, script string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "docker")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestHostInspector(t *testing.T) {
	docker := fakeDocker(t, `case "$1" in
info) echo '`+strings.ReplaceAll(sampleInfo, "\n", "")+`' ;;
compose) echo v2.29.1 ;;
esac
`)
	daemonJSON := filepath.Join(t.TempDir(), "daemon.json")
	if err := os.WriteFile(daemonJSON, []byte(`{"log-opts":{"max-size":"10m"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	h := HostInspector{Docker: docker, DaemonConfig: daemonJSON}

	info, err := h.Engine(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if info.ServerVersion != "27.3.1" || !info.DaemonConfigFound || info.DaemonConfigErr != nil || info.LogOpts["max-size"] != "10m" {
		t.Errorf("info = %+v", info)
	}
	v, err := h.ComposeVersion(context.Background())
	if err != nil || v != "2.29.1" {
		t.Errorf("ComposeVersion() = %q, %v", v, err)
	}

	h.DaemonConfig = filepath.Join(t.TempDir(), "missing.json")
	info, err = h.Engine(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if info.DaemonConfigFound || info.DaemonConfigErr != nil || info.LogOpts != nil {
		t.Errorf("missing daemon.json: info = %+v", info)
	}
}

func TestHostInspectorErrors(t *testing.T) {
	h := HostInspector{Docker: fakeDocker(t, "echo 'Cannot connect to the Docker daemon' >&2\nexit 1\n")}
	if _, err := h.Engine(context.Background()); err == nil || !strings.Contains(err.Error(), "Cannot connect") {
		t.Errorf("Engine() error = %v", err)
	}
	if _, err := h.ComposeVersion(context.Background()); err == nil {
		t.Error("ComposeVersion() = nil error")
	}

	h = HostInspector{Docker: filepath.Join(t.TempDir(), "no-such-docker")}
	if _, err := h.Engine(context.Background()); !errors.Is(err, ErrDockerCLINotFound) {
		t.Errorf("Engine() error = %v, want ErrDockerCLINotFound", err)
	}
}

// TestHostInspectorRealDocker talks to the local Docker daemon.
func TestHostInspectorRealDocker(t *testing.T) {
	if os.Getenv("PRESSFLEET_DOCKER_TESTS") != "1" {
		t.Skip("set PRESSFLEET_DOCKER_TESTS=1 to run tests that need Docker")
	}
	var h HostInspector
	info, err := h.Engine(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if info.ServerVersion == "" || info.OSType == "" {
		t.Errorf("info = %+v", info)
	}
	if _, err := h.ComposeVersion(context.Background()); err != nil {
		t.Error(err)
	}
}
