// Package releasecheck finds out whether a newer PressFleet release than the
// running one is available (#140). The control plane asks GitHub for the
// project's releases at startup and once a day, keeps the newest stable one,
// and records it in a file so that `pressfleet admin info`, which runs in
// another process, can report it to the host CLI.
//
// The check is best effort: an offline or air-gapped host, rate limiting,
// or a repository GitHub doesn't show without credentials only cost a debug
// log line. The request carries nothing about the install beyond a
// User-Agent naming the PressFleet version.
package releasecheck

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/skew"
)

// DefaultURL lists the project's most recent releases. The list rather than
// /releases/latest lets the check pick the highest version itself: GitHub
// marks the most recently published release as latest, which after a patch
// release for an older minor version isn't the newest one.
const DefaultURL = "https://api.github.com/repos/rchouinard/pressfleet/releases?per_page=30"

// DefaultInterval is how often the control plane checks after the check at
// startup.
const DefaultInterval = 24 * time.Hour

// requestTimeout bounds one check, so a host whose outbound traffic is
// silently dropped doesn't hold a connection for long.
const requestTimeout = 30 * time.Second

// maxBody bounds the release list read from the source.
const maxBody = 4 << 20

// ErrUnavailable means the release source couldn't say which releases
// exist: it answered with an error status, as when GitHub rate-limits the
// host or, for a private repository, answers 404 without credentials.
// Network failures are returned as they come.
var ErrUnavailable = errors.New("release information unavailable")

// Release is one release as the source lists it.
type Release struct {
	// Tag is the release's tag, such as "v1.2.3".
	Tag string
	// URL is the release's web page.
	URL        string
	Draft      bool
	Prerelease bool
}

// Source lists the published releases.
type Source interface {
	Releases(ctx context.Context) ([]Release, error)
}

// SourceFunc adapts a function to Source.
type SourceFunc func(ctx context.Context) ([]Release, error)

// Releases calls f.
func (f SourceFunc) Releases(ctx context.Context) ([]Release, error) { return f(ctx) }

// GitHub lists releases through the GitHub REST API, unauthenticated.
type GitHub struct {
	// URL is the release list endpoint; empty means DefaultURL.
	URL string
	// Version is the running PressFleet version, sent in the User-Agent.
	Version string
	// Client sends the request; nil means a client with a short timeout.
	Client *http.Client
}

// Releases fetches the release list. It sends no header of its own but the
// User-Agent: no credentials, cookies, or install identifier.
func (g GitHub) Releases(ctx context.Context) ([]Release, error) {
	endpoint := g.URL
	if endpoint == "" {
		endpoint = DefaultURL
	}
	client := g.Client
	if client == nil {
		client = &http.Client{Timeout: requestTimeout}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", UserAgent(g.Version))
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxBody))
		return nil, fmt.Errorf("%w: %s answered %s", ErrUnavailable, endpoint, resp.Status)
	}
	var list []struct {
		TagName    string `json:"tag_name"`
		HTMLURL    string `json:"html_url"`
		Draft      bool   `json:"draft"`
		Prerelease bool   `json:"prerelease"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxBody)).Decode(&list); err != nil {
		return nil, fmt.Errorf("decoding the release list: %w", err)
	}
	out := make([]Release, len(list))
	for i, r := range list {
		out[i] = Release{Tag: r.TagName, URL: r.HTMLURL, Draft: r.Draft, Prerelease: r.Prerelease}
	}
	return out, nil
}

// UserAgent is the User-Agent of the check's request.
func UserAgent(version string) string { return "PressFleet/" + version }

// Result is the newest stable release a check found.
type Result struct {
	LatestVersion string    `json:"latest_version"`
	ReleaseURL    string    `json:"release_url"`
	CheckedAt     time.Time `json:"checked_at"`
}

// UpdateAvailable reports whether the result is newer than current.
func (r Result) UpdateAvailable(current string) bool {
	return UpdateAvailable(current, r.LatestVersion)
}

// Latest returns the highest stable release in releases. Drafts,
// pre-releases (by GitHub's flag or a pre-release version), tags that aren't
// semantic versions, and releases without an https page are skipped; ok is
// false if none is left.
func Latest(releases []Release) (_ Release, v skew.Version, ok bool) {
	var best Release
	for _, r := range releases {
		if r.Draft || r.Prerelease || !httpsURL(r.URL) {
			continue
		}
		rv, err := skew.Parse(r.Tag)
		if err != nil || rv.Pre != "" {
			continue
		}
		if !ok || rv.Compare(v) > 0 {
			best, v, ok = r, rv, true
		}
	}
	return best, v, ok
}

// describeSuffix matches the pre-release part `git describe` gives a build
// commits after a release, "3-gabc1234" or "3-gabc1234-dirty".
var describeSuffix = regexp.MustCompile(`^\d+-g[0-9a-f]+(-dirty)?$`)

// UpdateAvailable reports whether latest, a stable release, is newer than
// current. A current version that isn't a semantic version (a development
// build) never has one. A `git describe` build after a release counts as
// that release, since semantic versioning would order it before it.
func UpdateAvailable(current, latest string) bool {
	lv, err := skew.Parse(latest)
	if err != nil || lv.Pre != "" {
		return false
	}
	cv, err := skew.Parse(current)
	if err != nil {
		return false
	}
	if describeSuffix.MatchString(cv.Pre) {
		cv.Pre = ""
	}
	return lv.Compare(cv) > 0
}

func httpsURL(s string) bool {
	u, err := url.Parse(s)
	return err == nil && u.Scheme == "https" && u.Host != ""
}

// Options configures a Checker.
type Options struct {
	// Source lists the releases. Required.
	Source Source
	// Version is the running version, to log when a newer one appears.
	Version string
	// Kubernetes says the platform runs from the Helm chart (driver:
	// kubernetes), where helm upgrade takes the place of pressfleet
	// upgrade, so the line logged about a newer release says so.
	Kubernetes bool
	// File is where the last result is kept, for `pressfleet admin info`;
	// empty keeps it in memory only.
	File string
	// Interval is the time between checks after the first; zero means
	// DefaultInterval.
	Interval time.Duration
	// Now is the clock; nil means time.Now.
	Now    func() time.Time
	Logger *slog.Logger
}

// Checker runs the check and holds its last result.
type Checker struct {
	opts Options

	mu     sync.Mutex
	result Result
	ok     bool
	// announced is the latest version an update was logged for.
	announced string
}

// New returns a Checker holding the result recorded by an earlier run, if
// File has one, so the API reports it before the first check finishes.
func New(opts Options) (*Checker, error) {
	if opts.Source == nil {
		return nil, errors.New("releasecheck: Source is required")
	}
	if opts.Logger == nil {
		return nil, errors.New("releasecheck: Logger is required")
	}
	if opts.Interval <= 0 {
		opts.Interval = DefaultInterval
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	c := &Checker{opts: opts}
	if opts.File != "" {
		r, ok, err := ReadFile(opts.File)
		if err != nil {
			opts.Logger.Debug("ignoring the recorded release check", "file", opts.File, "error", err)
		}
		c.result, c.ok = r, ok
	}
	return c, nil
}

// Run checks now and then every Interval until ctx is canceled.
func (c *Checker) Run(ctx context.Context) error {
	t := time.NewTicker(c.opts.Interval)
	defer t.Stop()
	for {
		c.Check(ctx)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}

// Check asks the source once and records what it found. A failure is
// logged at debug level and keeps the previous result.
func (c *Checker) Check(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	log := c.opts.Logger
	releases, err := c.opts.Source.Releases(ctx)
	if err != nil {
		log.Debug("release check failed", "error", err)
		return
	}
	rel, v, ok := Latest(releases)
	if !ok {
		log.Debug("release check found no stable release", "releases", len(releases))
		return
	}
	r := Result{LatestVersion: v.String(), ReleaseURL: rel.URL, CheckedAt: c.opts.Now().UTC()}
	c.mu.Lock()
	c.result, c.ok = r, true
	announce := r.UpdateAvailable(c.opts.Version) && c.announced != r.LatestVersion
	if announce {
		c.announced = r.LatestVersion
	}
	c.mu.Unlock()
	if announce {
		attrs := []any{"version", c.opts.Version, "latest", r.LatestVersion, "url", r.ReleaseURL}
		if c.opts.Kubernetes {
			log.Info("a newer PressFleet release is available; read its release notes and run helm upgrade", attrs...)
		} else {
			log.Info("a newer PressFleet release is available; install its CLI and run pressfleet upgrade", attrs...)
		}
	}
	if c.opts.File != "" {
		if err := writeFile(c.opts.File, r); err != nil {
			log.Warn("recording the release check failed", "file", c.opts.File, "error", err)
		}
	}
}

// Latest returns the last result; ok is false if no check has succeeded.
func (c *Checker) Latest() (_ Result, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.result, c.ok
}

// ReadFile reads a result a Checker recorded. ok is false, with a nil
// error, if there is none yet.
func ReadFile(path string) (_ Result, ok bool, _ error) {
	b, err := os.ReadFile(path) //nolint:gosec // G304: a fixed file in the data directory
	if errors.Is(err, fs.ErrNotExist) {
		return Result{}, false, nil
	} else if err != nil {
		return Result{}, false, err
	}
	var r Result
	if err := json.Unmarshal(b, &r); err != nil {
		return Result{}, false, fmt.Errorf("%s: %w", path, err)
	}
	if r.LatestVersion == "" {
		return Result{}, false, fmt.Errorf("%s: no latest_version", path)
	}
	return r, true, nil
}

// writeFile replaces path with r atomically, so a reader never sees half a
// file.
func writeFile(path string, r Result) error {
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".release-check-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
