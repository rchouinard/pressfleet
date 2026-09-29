// Package catalog answers which site images exist: the supported PHP branches
// and their upstream support status, the WordPress versions the platform
// builds, and the image references for a (PHP, WordPress) pair (design doc
// §4.1 Site Stack Topology, §5.5).
//
// The source is the version manifest, images/manifest.json, embedded in the
// binary through package images. The same manifest drives the image builds
// (images/matrix.sh and the Images workflow), so a PHP branch is "available"
// here exactly when CI builds images for it: through the last day of upstream
// security support. Branches past that date stay in the manifest so sites
// still on them can be flagged, but no new images are built for them.
//
// WordPress releases are grouped by major branch (x.y, [Branch]). The
// manifest has one release per branch, its newest: the latest release, and
// the newest release of every older branch that sites are held on or
// awaiting approval to leave (§5.5). Core updates (package sites) move a
// site to the newest release of its branch, or of a newer branch once the
// major release is approved.
//
// Image digests are not part of the manifest: they are only known after CI
// has pushed the images, and a weekly rebuild moves a tag to a new digest
// without a manifest change. Detecting updated images (#58) therefore means
// resolving the tags returned by [Catalog.Images] against the registry at
// runtime, not reading this package.
package catalog

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/rchouinard/pressfleet/images"
)

// EOLSoonWindow is how long before the end of security support a PHP branch
// is reported as [StatusEOLSoon], so the dashboard can warn ahead of time
// (§5.5).
const EOLSoonWindow = 180 * 24 * time.Hour

var (
	// ErrInvalidManifest means the manifest is malformed or inconsistent.
	ErrInvalidManifest = errors.New("invalid version manifest")
	// ErrInvalidVersion means a version string is not a PHP branch ("8.4")
	// or WordPress release ("7.1.2") in the expected format.
	ErrInvalidVersion = errors.New("invalid version")
	// ErrUnknownPHPVersion means the PHP branch is not in the manifest.
	ErrUnknownPHPVersion = errors.New("unknown PHP version")
	// ErrPHPVersionEOL means the PHP branch is past upstream end of life,
	// so no images are built for it any more.
	ErrPHPVersionEOL = errors.New("PHP version is past end of life")
)

// Status is a PHP branch's upstream support status on a given date.
type Status string

// Support statuses, from php.net's supported versions policy.
const (
	// StatusActive: bug and security fixes.
	StatusActive Status = "active"
	// StatusSecurityOnly: security fixes only.
	StatusSecurityOnly Status = "security-only"
	// StatusEOLSoon: security fixes only, ending within [EOLSoonWindow].
	StatusEOLSoon Status = "eol-soon"
	// StatusEOL: no upstream support; images are no longer built.
	StatusEOL Status = "eol"
)

// PHPVersion is a PHP branch and its upstream support dates. Each date is
// the last day of that kind of support, at midnight UTC.
type PHPVersion struct {
	Version             string
	ActiveSupportEnds   time.Time
	SecuritySupportEnds time.Time
}

// Status reports the branch's support status at now. Support lasts through
// the whole of the end date (UTC).
func (p PHPVersion) Status(now time.Time) Status {
	securityEnd := p.SecuritySupportEnds.AddDate(0, 0, 1)
	switch {
	case !now.Before(securityEnd):
		return StatusEOL
	case securityEnd.Sub(now) <= EOLSoonWindow:
		return StatusEOLSoon
	case !now.Before(p.ActiveSupportEnds.AddDate(0, 0, 1)):
		return StatusSecurityOnly
	default:
		return StatusActive
	}
}

// WordPressVersion is a WordPress release that site images are built for.
type WordPressVersion struct {
	Version string
	// SHA1 is the checksum of wordpress-<Version>.tar.gz the images were
	// built from.
	SHA1 string
	// Published is the day (midnight UTC) the release entered the manifest,
	// which is when its images were first published; zero if the manifest
	// doesn't say. Major releases can be approved automatically a set time
	// after it (§5.5).
	Published time.Time
	// Insecure means wordpress.org reports this release, the newest of its
	// branch, as having known security issues: the branch no longer gets
	// security releases.
	Insecure bool
	// DBVersion is the release's database schema version ($wp_db_version
	// in wp-includes/version.php), which a database records in its
	// db_version option. A site import reads it from a dump to tell
	// whether a site image can run the database (§5.7). Zero means the
	// manifest doesn't say.
	DBVersion int
}

// Branch returns the release's major branch (see [Branch]).
func (w WordPressVersion) Branch() string {
	b, _ := Branch(w.Version)
	return b
}

// Images are the references of a site's two container images.
type Images struct {
	PHP   string // <registry>/site-php:php<PHP>-wp<WP>
	Nginx string // <registry>/site-nginx:wp<WP>
}

// Catalog is a parsed version manifest. It is immutable and safe for
// concurrent use.
type Catalog struct {
	registry  string
	php       []PHPVersion       // newest first
	wordpress []WordPressVersion // latest first, then older branches, newest first
}

var (
	phpVersionRE = regexp.MustCompile(`^[0-9]+\.[0-9]+$`)
	wpVersionRE  = regexp.MustCompile(`^[0-9]+\.[0-9]+(\.[0-9]+)?$`)
	wpBranchRE   = regexp.MustCompile(`^[0-9]+\.[0-9]+$`)
	sha1RE       = regexp.MustCompile(`^[0-9a-f]{40}$`)
)

// manifest mirrors images/manifest.json; images/README.md documents it.
type manifest struct {
	Registry string `json:"registry"`
	PHP      []struct {
		Version             string `json:"version"`
		ActiveSupportEnds   string `json:"activeSupportEnds"`
		SecuritySupportEnds string `json:"securitySupportEnds"`
	} `json:"php"`
	WordPress struct {
		Latest *manifestWP  `json:"latest"`
		Pinned []manifestWP `json:"pinned"`
	} `json:"wordpress"`
}

type manifestWP struct {
	Version   string `json:"version"`
	SHA1      string `json:"sha1"`
	Published string `json:"published"`
	Insecure  bool   `json:"insecure"`
	DBVersion int    `json:"dbVersion,omitempty"`
}

// Load parses the manifest embedded in the binary.
func Load() (*Catalog, error) {
	return Parse(images.Manifest())
}

// LoadFile parses the manifest in a file, which replaces the embedded one
// (config image_manifest_file).
func LoadFile(path string) (*Catalog, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: the path is the operator's configuration
	if err != nil {
		return nil, fmt.Errorf("reading version manifest: %w", err)
	}
	c, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}

// Parse parses and validates a version manifest.
func Parse(data []byte) (*Catalog, error) {
	var m manifest
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidManifest, err)
	}
	if m.Registry == "" || strings.HasSuffix(m.Registry, "/") {
		return nil, fmt.Errorf("%w: registry %q must be a repository prefix without a trailing slash", ErrInvalidManifest, m.Registry)
	}
	c := &Catalog{registry: m.Registry}

	if len(m.PHP) == 0 {
		return nil, fmt.Errorf("%w: no PHP versions", ErrInvalidManifest)
	}
	for _, p := range m.PHP {
		if !phpVersionRE.MatchString(p.Version) {
			return nil, fmt.Errorf("%w: PHP version %q: %w", ErrInvalidManifest, p.Version, ErrInvalidVersion)
		}
		if _, dup := c.PHP(p.Version); dup {
			return nil, fmt.Errorf("%w: duplicate PHP version %s", ErrInvalidManifest, p.Version)
		}
		active, err := time.Parse(time.DateOnly, p.ActiveSupportEnds)
		if err != nil {
			return nil, fmt.Errorf("%w: PHP %s activeSupportEnds: %w", ErrInvalidManifest, p.Version, err)
		}
		security, err := time.Parse(time.DateOnly, p.SecuritySupportEnds)
		if err != nil {
			return nil, fmt.Errorf("%w: PHP %s securitySupportEnds: %w", ErrInvalidManifest, p.Version, err)
		}
		if security.Before(active) {
			return nil, fmt.Errorf("%w: PHP %s security support ends before active support", ErrInvalidManifest, p.Version)
		}
		c.php = append(c.php, PHPVersion{Version: p.Version, ActiveSupportEnds: active, SecuritySupportEnds: security})
	}
	slices.SortFunc(c.php, func(a, b PHPVersion) int { return compareVersions(b.Version, a.Version) })

	if m.WordPress.Latest == nil {
		return nil, fmt.Errorf("%w: no latest WordPress version", ErrInvalidManifest)
	}
	for _, w := range append([]manifestWP{*m.WordPress.Latest}, m.WordPress.Pinned...) {
		if !wpVersionRE.MatchString(w.Version) {
			return nil, fmt.Errorf("%w: WordPress version %q: %w", ErrInvalidManifest, w.Version, ErrInvalidVersion)
		}
		if !sha1RE.MatchString(w.SHA1) {
			return nil, fmt.Errorf("%w: WordPress %s: sha1 must be 40 lowercase hex digits", ErrInvalidManifest, w.Version)
		}
		if w.DBVersion < 0 {
			return nil, fmt.Errorf("%w: WordPress %s: dbVersion must be positive", ErrInvalidManifest, w.Version)
		}
		if slices.ContainsFunc(c.wordpress, func(v WordPressVersion) bool { return v.Version == w.Version }) {
			return nil, fmt.Errorf("%w: duplicate WordPress version %s", ErrInvalidManifest, w.Version)
		}
		v := WordPressVersion{Version: w.Version, SHA1: w.SHA1, Insecure: w.Insecure, DBVersion: w.DBVersion}
		// Each branch has one release in the matrix, its newest, which
		// every site on the branch moves to (§5.5).
		if slices.ContainsFunc(c.wordpress, func(o WordPressVersion) bool { return o.Branch() == v.Branch() }) {
			return nil, fmt.Errorf("%w: more than one WordPress release of branch %s", ErrInvalidManifest, v.Branch())
		}
		if len(c.wordpress) > 0 && compareVersions(v.Version, c.wordpress[0].Version) > 0 {
			return nil, fmt.Errorf("%w: WordPress %s is newer than the latest, %s", ErrInvalidManifest, v.Version, c.wordpress[0].Version)
		}
		if w.Published != "" {
			published, err := time.Parse(time.DateOnly, w.Published)
			if err != nil {
				return nil, fmt.Errorf("%w: WordPress %s published: %w", ErrInvalidManifest, w.Version, err)
			}
			v.Published = published
		}
		c.wordpress = append(c.wordpress, v)
	}
	slices.SortStableFunc(c.wordpress[1:], func(a, b WordPressVersion) int { return compareVersions(b.Version, a.Version) })
	return c, nil
}

// PHPVersions returns every PHP branch in the manifest, newest first,
// including branches past end of life.
func (c *Catalog) PHPVersions() []PHPVersion {
	return slices.Clone(c.php)
}

// AvailablePHPVersions returns the PHP branches images are built for at now
// (every status except [StatusEOL]), newest first.
func (c *Catalog) AvailablePHPVersions(now time.Time) []PHPVersion {
	var out []PHPVersion
	for _, p := range c.php {
		if p.Status(now) != StatusEOL {
			out = append(out, p)
		}
	}
	return out
}

// DefaultPHPVersion returns the PHP branch new sites get when none is
// chosen: the newest branch in active support at now, so a new site starts
// with the longest runway, or else the newest one images are still built
// for. It returns false if every branch is past end of life.
func (c *Catalog) DefaultPHPVersion(now time.Time) (string, bool) {
	available := c.AvailablePHPVersions(now)
	for _, p := range available {
		if p.Status(now) == StatusActive {
			return p.Version, true
		}
	}
	if len(available) > 0 {
		return available[0].Version, true
	}
	return "", false
}

// PHP looks up a PHP branch.
func (c *Catalog) PHP(version string) (PHPVersion, bool) {
	i := slices.IndexFunc(c.php, func(p PHPVersion) bool { return p.Version == version })
	if i < 0 {
		return PHPVersion{}, false
	}
	return c.php[i], true
}

// ValidatePHPVersion checks that a site may be set to the PHP branch at now:
// it must be well-formed ([ErrInvalidVersion]), in the manifest
// ([ErrUnknownPHPVersion]), and not past end of life ([ErrPHPVersionEOL]).
func (c *Catalog) ValidatePHPVersion(version string, now time.Time) error {
	if !phpVersionRE.MatchString(version) {
		return fmt.Errorf("%w: PHP version %q (want <major>.<minor>, e.g. 8.4)", ErrInvalidVersion, version)
	}
	p, ok := c.PHP(version)
	if !ok {
		return fmt.Errorf("%w: %s", ErrUnknownPHPVersion, version)
	}
	if p.Status(now) == StatusEOL {
		return fmt.Errorf("%w: PHP %s (security support ended %s)", ErrPHPVersionEOL, version, p.SecuritySupportEnds.Format(time.DateOnly))
	}
	return nil
}

// LatestWordPress returns the newest WordPress release, the one new sites
// and core updates use.
func (c *Catalog) LatestWordPress() WordPressVersion {
	return c.wordpress[0]
}

// WordPressVersions returns every WordPress release images are built for,
// newest first: the latest, then the newest release of each older branch
// kept for sites held on it or awaiting approval of a newer one (§5.5).
func (c *Catalog) WordPressVersions() []WordPressVersion {
	return slices.Clone(c.wordpress)
}

// LatestInBranch returns the newest release images are built for on a
// major branch ("7.1"), and whether the branch is in the manifest at all.
func (c *Catalog) LatestInBranch(branch string) (WordPressVersion, bool) {
	i := slices.IndexFunc(c.wordpress, func(w WordPressVersion) bool { return w.Branch() == branch })
	if i < 0 {
		return WordPressVersion{}, false
	}
	return c.wordpress[i], true
}

// Branch returns a WordPress version's major branch. WordPress numbers its
// major releases x.y and their minor and security releases x.y.z (§5.5),
// so "7.1.2" and "7.1" are both on branch "7.1". A malformed version is
// ErrInvalidVersion.
func Branch(version string) (string, error) {
	if !wpVersionRE.MatchString(version) {
		return "", fmt.Errorf("%w: WordPress version %q", ErrInvalidVersion, version)
	}
	major, rest, _ := strings.Cut(version, ".")
	minor, _, _ := strings.Cut(rest, ".")
	return major + "." + minor, nil
}

// ValidBranch reports whether s is a WordPress major branch, like "7.1".
func ValidBranch(s string) bool { return wpBranchRE.MatchString(s) }

// CompareBranches orders two WordPress branches like cmp.Compare.
// Malformed branches are ErrInvalidVersion.
func CompareBranches(a, b string) (int, error) {
	for _, v := range []string{a, b} {
		if !wpBranchRE.MatchString(v) {
			return 0, fmt.Errorf("%w: WordPress branch %q", ErrInvalidVersion, v)
		}
	}
	return compareVersions(a, b), nil
}

// WordPressAtLeast returns the oldest WordPress release images are built
// for that is at least version, and whether there is one. A restore uses
// it to move a site to a core that can run a database from a newer one
// (§5.4) while changing as little as possible. A malformed version is
// ErrInvalidVersion.
func (c *Catalog) WordPressAtLeast(version string) (WordPressVersion, bool, error) {
	if !wpVersionRE.MatchString(version) {
		return WordPressVersion{}, false, fmt.Errorf("%w: WordPress version %q", ErrInvalidVersion, version)
	}
	var best WordPressVersion
	found := false
	for _, w := range c.wordpress {
		if compareVersions(w.Version, version) >= 0 && (!found || compareVersions(w.Version, best.Version) < 0) {
			best, found = w, true
		}
	}
	return best, found, nil
}

// CompareWordPressVersions orders two WordPress versions like cmp.Compare:
// negative if a is older than b, zero if they are the same release ("6.9"
// and "6.9.0" are), positive if a is newer. Malformed versions are
// ErrInvalidVersion.
func CompareWordPressVersions(a, b string) (int, error) {
	for _, v := range []string{a, b} {
		if !wpVersionRE.MatchString(v) {
			return 0, fmt.Errorf("%w: WordPress version %q", ErrInvalidVersion, v)
		}
	}
	return compareVersions(a, b), nil
}

// Images returns the image references for a PHP branch and WordPress
// release. It checks only the version formats, not that the pair is in the
// manifest: images for versions that have left the build matrix stay
// published, and sites pinned to them keep using them.
func (c *Catalog) Images(php, wordpress string) (Images, error) {
	if !phpVersionRE.MatchString(php) {
		return Images{}, fmt.Errorf("%w: PHP version %q", ErrInvalidVersion, php)
	}
	if !wpVersionRE.MatchString(wordpress) {
		return Images{}, fmt.Errorf("%w: WordPress version %q", ErrInvalidVersion, wordpress)
	}
	return Images{
		PHP:   c.registry + "/site-php:php" + php + "-wp" + wordpress,
		Nginx: c.registry + "/site-nginx:wp" + wordpress,
	}, nil
}

// compareVersions orders dotted numeric versions that have already been
// validated by one of the version regexps.
func compareVersions(a, b string) int {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := range max(len(as), len(bs)) {
		var x, y int
		if i < len(as) {
			x, _ = strconv.Atoi(as[i])
		}
		if i < len(bs) {
			y, _ = strconv.Atoi(bs[i])
		}
		if x != y {
			return x - y
		}
	}
	return 0
}
