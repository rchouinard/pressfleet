package releasecheck_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/releasecheck"
)

var now = time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)

// rel is a published release of tag.
func rel(tag string) releasecheck.Release {
	return releasecheck.Release{Tag: tag, URL: "https://github.com/rchouinard/pressfleet/releases/tag/" + tag}
}

// fixed is a release source that lists releases, or fails with err.
func fixed(err error, releases ...releasecheck.Release) (releasecheck.Source, *int) {
	calls := 0
	return releasecheck.SourceFunc(func(context.Context) ([]releasecheck.Release, error) {
		calls++
		return releases, err
	}), &calls
}

// logger records every level, so tests can see what was logged at which.
func logger() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})), &buf
}

func TestCheck(t *testing.T) {
	prerelease := rel("v0.6.0")
	prerelease.Prerelease = true
	draft := rel("v0.7.0")
	draft.Draft = true
	notHTTPS := rel("v0.8.0")
	notHTTPS.URL = "javascript:alert(1)"

	tests := []struct {
		name     string
		current  string
		releases []releasecheck.Release
		// latest is the version found; empty means no result.
		latest string
		update bool
	}{
		{"patch release", "v1.2.3", []releasecheck.Release{rel("v1.2.3"), rel("v1.2.4")}, "v1.2.4", true},
		{"minor release", "v1.2.3", []releasecheck.Release{rel("v1.3.0"), rel("v1.2.3")}, "v1.3.0", true},
		{"major release", "v1.2.3", []releasecheck.Release{rel("v2.0.0")}, "v2.0.0", true},
		{"up to date", "v1.2.3", []releasecheck.Release{rel("v1.2.3"), rel("v1.2.2")}, "v1.2.3", false},
		{"running a newer build", "v1.3.0", []releasecheck.Release{rel("v1.2.3")}, "v1.2.3", false},
		{"pre-1.0 minor", "v0.4.2", []releasecheck.Release{rel("v0.5.0")}, "v0.5.0", true},
		{"pre-1.0 patch", "v0.4.2", []releasecheck.Release{rel("v0.4.3")}, "v0.4.3", true},
		{"pre-1.0 to 1.0", "v0.9.9", []releasecheck.Release{rel("v1.0.0")}, "v1.0.0", true},
		{"highest version, not the first listed", "v0.4.0", []releasecheck.Release{rel("v0.4.5"), rel("v0.5.1"), rel("v0.5.0")}, "v0.5.1", true},
		{"tag without v", "v0.4.0", []releasecheck.Release{rel("0.5.0")}, "v0.5.0", true},
		{"pre-release flag ignored", "v0.5.0", []releasecheck.Release{rel("v0.5.0"), prerelease}, "v0.5.0", false},
		{"pre-release tag ignored", "v0.5.0", []releasecheck.Release{rel("v0.6.0-rc.1"), rel("v0.5.0")}, "v0.5.0", false},
		{"drafts ignored", "v0.5.0", []releasecheck.Release{draft}, "", false},
		{"pages that aren't https ignored", "v0.5.0", []releasecheck.Release{notHTTPS, rel("v0.5.0")}, "v0.5.0", false},
		{"only pre-releases", "v0.5.0", []releasecheck.Release{rel("v0.6.0-rc.1"), prerelease}, "", false},
		{"unparsable tags ignored", "v0.5.0", []releasecheck.Release{rel("nightly"), rel("v1.2"), rel("v0.6.0"), rel("release-2")}, "v0.6.0", true},
		{"no releases", "v0.5.0", nil, "", false},
		{"running a pre-release of it", "v0.6.0-rc.2", []releasecheck.Release{rel("v0.6.0")}, "v0.6.0", true},
		{"running a pre-release of a later one", "v0.7.0-rc.1", []releasecheck.Release{rel("v0.6.0")}, "v0.6.0", false},
		{"git describe build after the release", "v0.6.0-3-gabc1234-dirty", []releasecheck.Release{rel("v0.6.0")}, "v0.6.0", false},
		{"git describe build before the next", "v0.6.0-3-gabc1234", []releasecheck.Release{rel("v0.6.1")}, "v0.6.1", true},
		{"development build", "dev", []releasecheck.Release{rel("v9.0.0")}, "v9.0.0", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src, _ := fixed(nil, tt.releases...)
			log, _ := logger()
			c, err := releasecheck.New(releasecheck.Options{Source: src, Version: tt.current, Now: func() time.Time { return now }, Logger: log})
			if err != nil {
				t.Fatal(err)
			}
			c.Check(context.Background())
			got, ok := c.Latest()
			if tt.latest == "" {
				if ok {
					t.Fatalf("result %+v, want none", got)
				}
				return
			}
			if !ok || got.LatestVersion != tt.latest || !got.CheckedAt.Equal(now) || !strings.HasSuffix(got.ReleaseURL, strings.TrimPrefix(tt.latest, "v")) {
				t.Fatalf("result %+v %v, want %s", got, ok, tt.latest)
			}
			if u := got.UpdateAvailable(tt.current); u != tt.update {
				t.Errorf("UpdateAvailable(%s) = %v, want %v", tt.current, u, tt.update)
			}
		})
	}
}

// The line logged about a newer release names the upgrade of the install's
// driver: on Kubernetes, Helm does it (#319).
func TestCheckAnnouncesUpgrade(t *testing.T) {
	tests := []struct {
		name       string
		kubernetes bool
		want       string
		notWant    string
	}{
		{"compose", false, "install its CLI and run pressfleet upgrade", "helm"},
		{"kubernetes", true, "read its release notes and run helm upgrade", "pressfleet upgrade"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src, _ := fixed(nil, rel("v1.3.0"))
			log, buf := logger()
			c, err := releasecheck.New(releasecheck.Options{
				Source: src, Version: "v1.2.3", Kubernetes: tt.kubernetes, Now: func() time.Time { return now }, Logger: log,
			})
			if err != nil {
				t.Fatal(err)
			}
			c.Check(context.Background())
			out := buf.String()
			if !strings.Contains(out, "level=INFO") || !strings.Contains(out, tt.want) {
				t.Errorf("log lacks %q:\n%s", tt.want, out)
			}
			if strings.Contains(out, tt.notWant) {
				t.Errorf("log mentions %q:\n%s", tt.notWant, out)
			}
		})
	}
}

// A failed check is logged at debug level only, and keeps what an earlier
// check found.
func TestCheckFailure(t *testing.T) {
	var fail error
	src := releasecheck.SourceFunc(func(context.Context) ([]releasecheck.Release, error) {
		if fail != nil {
			return nil, fail
		}
		return []releasecheck.Release{rel("v0.5.0")}, nil
	})
	log, buf := logger()
	file := filepath.Join(t.TempDir(), "release-check.json")
	c, err := releasecheck.New(releasecheck.Options{Source: src, Version: "v0.4.0", File: file, Logger: log})
	if err != nil {
		t.Fatal(err)
	}
	c.Check(context.Background())
	if !strings.Contains(buf.String(), "level=INFO") || !strings.Contains(buf.String(), "latest=v0.5.0") {
		t.Errorf("no update logged:\n%s", buf)
	}
	buf.Reset()
	c.Check(context.Background())
	if buf.Len() != 0 {
		t.Errorf("the same update was logged again:\n%s", buf)
	}

	for _, fail = range []error{
		errors.New("dial tcp: lookup api.github.com: no such host"),
		releasecheck.ErrUnavailable,
	} {
		buf.Reset()
		c.Check(context.Background())
		if got, ok := c.Latest(); !ok || got.LatestVersion != "v0.5.0" {
			t.Errorf("after %v: result %+v %v, want the earlier one", fail, got, ok)
		}
		for line := range strings.Lines(buf.String()) {
			if !strings.Contains(line, "level=DEBUG") {
				t.Errorf("after %v, logged above debug: %s", fail, line)
			}
		}
		if !strings.Contains(buf.String(), "release check failed") {
			t.Errorf("after %v, nothing logged", fail)
		}
	}
	if r, ok, err := releasecheck.ReadFile(file); err != nil || !ok || r.LatestVersion != "v0.5.0" {
		t.Errorf("ReadFile = %+v %v %v", r, ok, err)
	}
}

// A new Checker starts from the result recorded by the last run.
func TestRecordedResult(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "release-check.json")
	if _, ok, err := releasecheck.ReadFile(file); ok || err != nil {
		t.Fatalf("missing file: %v %v", ok, err)
	}
	src, _ := fixed(nil, rel("v0.5.0"))
	log, _ := logger()
	c, _ := releasecheck.New(releasecheck.Options{Source: src, File: file, Now: func() time.Time { return now }, Logger: log})
	c.Check(context.Background())

	offline, calls := fixed(errors.New("offline"))
	c, err := releasecheck.New(releasecheck.Options{Source: offline, File: file, Logger: log})
	if err != nil {
		t.Fatal(err)
	}
	got, ok := c.Latest()
	if !ok || got.LatestVersion != "v0.5.0" || !got.CheckedAt.Equal(now) || got.ReleaseURL != rel("v0.5.0").URL {
		t.Errorf("recorded result = %+v %v", got, ok)
	}
	if *calls != 0 {
		t.Errorf("New asked the source %d times", *calls)
	}
}

func TestRun(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	checked := make(chan struct{})
	src := releasecheck.SourceFunc(func(ctx context.Context) ([]releasecheck.Release, error) {
		select {
		case checked <- struct{}{}:
		case <-ctx.Done():
		}
		return nil, nil
	})
	log, _ := logger()
	c, _ := releasecheck.New(releasecheck.Options{Source: src, Interval: time.Millisecond, Logger: log})
	done := make(chan error)
	go func() { done <- c.Run(ctx) }()
	// Once at startup, then on the interval.
	<-checked
	<-checked
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Errorf("Run = %v", err)
	}
}

func TestGitHub(t *testing.T) {
	var got *http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r
		_, _ = w.Write([]byte(`[
			{"tag_name":"v0.6.0-rc.1","html_url":"https://github.com/o/r/releases/tag/v0.6.0-rc.1","prerelease":true,"draft":false},
			{"tag_name":"v0.5.0","html_url":"https://github.com/o/r/releases/tag/v0.5.0","prerelease":false,"draft":false,"body":"notes"}
		]`))
	}))
	defer srv.Close()

	releases, err := releasecheck.GitHub{URL: srv.URL + "/releases?per_page=30", Version: "v0.4.0", Client: srv.Client()}.Releases(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []releasecheck.Release{
		{Tag: "v0.6.0-rc.1", URL: "https://github.com/o/r/releases/tag/v0.6.0-rc.1", Prerelease: true},
		{Tag: "v0.5.0", URL: "https://github.com/o/r/releases/tag/v0.5.0"},
	}
	if len(releases) != len(want) || releases[0] != want[0] || releases[1] != want[1] {
		t.Errorf("releases = %+v", releases)
	}
	// Nothing about the install but the version in the User-Agent.
	if ua := got.Header.Get("User-Agent"); ua != "PressFleet/v0.4.0" {
		t.Errorf("User-Agent = %q", ua)
	}
	for name := range got.Header {
		if name != "User-Agent" && name != "Accept-Encoding" {
			t.Errorf("request sent header %s: %v", name, got.Header[name])
		}
	}
	if got.Method != http.MethodGet || got.URL.RawQuery != "per_page=30" {
		t.Errorf("request = %s %s", got.Method, got.URL)
	}
}

// Error statuses, such as the 404 GitHub answers without credentials for a
// private repository or a rate limit's 403, are ErrUnavailable.
func TestGitHubUnavailable(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusForbidden, http.StatusTooManyRequests, http.StatusInternalServerError} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, `{"message":"Not Found"}`, status)
		}))
		_, err := releasecheck.GitHub{URL: srv.URL, Version: "v0.4.0", Client: srv.Client()}.Releases(context.Background())
		srv.Close()
		if !errors.Is(err, releasecheck.ErrUnavailable) {
			t.Errorf("%d: err = %v, want ErrUnavailable", status, err)
		}
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"not":"a list"}`))
	}))
	defer srv.Close()
	if _, err := (releasecheck.GitHub{URL: srv.URL, Client: srv.Client()}).Releases(context.Background()); err == nil {
		t.Error("a malformed answer was accepted")
	}
}

func TestUpdateAvailable(t *testing.T) {
	tests := []struct {
		current, latest string
		want            bool
	}{
		{"v0.4.0", "v0.4.1", true},
		{"v0.4.1", "v0.4.1", false},
		{"v0.4.0", "garbage", false},
		{"v0.4.0", "v0.5.0-rc.1", false},
		{"", "v0.5.0", false},
	}
	for _, tt := range tests {
		if got := releasecheck.UpdateAvailable(tt.current, tt.latest); got != tt.want {
			t.Errorf("UpdateAvailable(%q, %q) = %v, want %v", tt.current, tt.latest, got, tt.want)
		}
	}
}
