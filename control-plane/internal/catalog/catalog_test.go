package catalog

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

const testManifest = `{
  "registry": "registry.example/pf",
  "php": [
    {"version": "8.3", "activeSupportEnds": "2025-12-31", "securitySupportEnds": "2027-12-31"},
    {"version": "8.10", "activeSupportEnds": "2030-12-31", "securitySupportEnds": "2032-12-31"},
    {"version": "8.2", "activeSupportEnds": "2024-12-31", "securitySupportEnds": "2026-12-31"}
  ],
  "wordpress": {
    "latest": {"version": "7.1.2", "sha1": "761b8101538f0631a0bfc4fba7bc4abeea92f81c"},
    "pinned": [{"version": "6.9", "sha1": "0000000000000000000000000000000000000000"}]
  }
}`

func date(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

func mustParse(t *testing.T) *Catalog {
	t.Helper()
	c, err := Parse([]byte(testManifest))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return c
}

func versions(ps []PHPVersion) []string {
	out := make([]string, 0, len(ps))
	for _, p := range ps {
		out = append(out, p.Version)
	}
	return out
}

func TestLoadEmbedded(t *testing.T) {
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(c.PHPVersions()) == 0 {
		t.Error("embedded manifest has no PHP versions")
	}
	latest := c.LatestWordPress()
	imgs, err := c.Images(c.PHPVersions()[0].Version, latest.Version)
	if err != nil {
		t.Fatalf("Images: %v", err)
	}
	if !strings.HasPrefix(imgs.PHP, "ghcr.io/rchouinard/pressfleet/site-php:php") {
		t.Errorf("Images().PHP = %q, want the GHCR site-php repository", imgs.PHP)
	}
}

func TestPHPStatus(t *testing.T) {
	p := PHPVersion{
		Version:             "8.2",
		ActiveSupportEnds:   date("2024-12-31T00:00:00Z"),
		SecuritySupportEnds: date("2026-12-31T00:00:00Z"),
	}
	tests := []struct {
		now  string
		want Status
	}{
		{"2024-06-01T00:00:00Z", StatusActive},
		{"2024-12-31T23:59:59Z", StatusActive},
		{"2025-01-01T00:00:00Z", StatusSecurityOnly},
		{"2026-07-04T00:00:00Z", StatusSecurityOnly},
		{"2026-07-05T00:00:00Z", StatusEOLSoon}, // 180 days before 2027-01-01
		{"2026-12-31T23:59:59Z", StatusEOLSoon},
		{"2027-01-01T00:00:00Z", StatusEOL},
		{"2030-01-01T00:00:00Z", StatusEOL},
	}
	for _, tt := range tests {
		if got := p.Status(date(tt.now)); got != tt.want {
			t.Errorf("Status(%s) = %s, want %s", tt.now, got, tt.want)
		}
	}

	// A branch whose active support ends inside the EOL window is reported
	// as ending soon rather than active.
	short := PHPVersion{ActiveSupportEnds: date("2026-12-31T00:00:00Z"), SecuritySupportEnds: date("2026-12-31T00:00:00Z")}
	if got := short.Status(date("2026-12-01T00:00:00Z")); got != StatusEOLSoon {
		t.Errorf("Status = %s, want %s", got, StatusEOLSoon)
	}
}

func TestPHPVersions(t *testing.T) {
	c := mustParse(t)
	if got, want := versions(c.PHPVersions()), []string{"8.10", "8.3", "8.2"}; !slices.Equal(got, want) {
		t.Errorf("PHPVersions() = %v, want %v (newest first)", got, want)
	}
	tests := []struct {
		now  string
		want []string
	}{
		{"2026-09-26T12:00:00Z", []string{"8.10", "8.3", "8.2"}},
		{"2027-01-01T00:00:00Z", []string{"8.10", "8.3"}},
		{"2040-01-01T00:00:00Z", nil},
	}
	for _, tt := range tests {
		if got := versions(c.AvailablePHPVersions(date(tt.now))); !slices.Equal(got, tt.want) {
			t.Errorf("AvailablePHPVersions(%s) = %v, want %v", tt.now, got, tt.want)
		}
	}
	if p, ok := c.PHP("8.3"); !ok || p.SecuritySupportEnds.Format(time.DateOnly) != "2027-12-31" {
		t.Errorf("PHP(8.3) = %+v, %v", p, ok)
	}
	if _, ok := c.PHP("7.4"); ok {
		t.Error("PHP(7.4) found, want not found")
	}
}

func TestValidatePHPVersion(t *testing.T) {
	c := mustParse(t)
	now := date("2027-06-01T00:00:00Z")
	tests := []struct {
		version string
		want    error
	}{
		{"8.3", nil},
		{"8.10", nil},
		{"8.2", ErrPHPVersionEOL},
		{"7.4", ErrUnknownPHPVersion},
		{"8", ErrInvalidVersion},
		{"8.3.1", ErrInvalidVersion},
		{"php8.3", ErrInvalidVersion},
		{"", ErrInvalidVersion},
	}
	for _, tt := range tests {
		err := c.ValidatePHPVersion(tt.version, now)
		if !errors.Is(err, tt.want) || (tt.want == nil) != (err == nil) {
			t.Errorf("ValidatePHPVersion(%q) = %v, want %v", tt.version, err, tt.want)
		}
	}
}

func TestDefaultPHPVersion(t *testing.T) {
	c := mustParse(t)
	for _, tt := range []struct {
		now    string
		want   string
		wantOK bool
	}{
		{"2026-06-01T00:00:00Z", "8.10", true}, // newest and active
		{"2031-06-01T00:00:00Z", "8.10", true}, // none active: newest available
		{"2033-01-01T00:00:00Z", "", false},    // everything past end of life
	} {
		if got, ok := c.DefaultPHPVersion(date(tt.now)); got != tt.want || ok != tt.wantOK {
			t.Errorf("DefaultPHPVersion(%s) = %q, %v; want %q, %v", tt.now, got, ok, tt.want, tt.wantOK)
		}
	}
	// The embedded manifest always offers a default that validates.
	embedded, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	v, ok := embedded.DefaultPHPVersion(now)
	if !ok || embedded.ValidatePHPVersion(v, now) != nil {
		t.Errorf("embedded DefaultPHPVersion = %q, %v", v, ok)
	}
}

func TestWordPress(t *testing.T) {
	c := mustParse(t)
	if got := c.LatestWordPress(); got.Version != "7.1.2" || got.SHA1 != "761b8101538f0631a0bfc4fba7bc4abeea92f81c" {
		t.Errorf("LatestWordPress() = %+v", got)
	}
	var got []string
	for _, w := range c.WordPressVersions() {
		got = append(got, w.Version)
	}
	if want := []string{"7.1.2", "6.9"}; !slices.Equal(got, want) {
		t.Errorf("WordPressVersions() = %v, want %v", got, want)
	}
}

func TestImages(t *testing.T) {
	c := mustParse(t)
	tests := []struct {
		php, wp string
		want    Images
		err     error
	}{
		{"8.3", "7.1.2", Images{PHP: "registry.example/pf/site-php:php8.3-wp7.1.2", Nginx: "registry.example/pf/site-nginx:wp7.1.2"}, nil},
		{"8.2", "6.9", Images{PHP: "registry.example/pf/site-php:php8.2-wp6.9", Nginx: "registry.example/pf/site-nginx:wp6.9"}, nil},
		// Not in the manifest, but well-formed: images stay published.
		{"8.1", "6.4.3", Images{PHP: "registry.example/pf/site-php:php8.1-wp6.4.3", Nginx: "registry.example/pf/site-nginx:wp6.4.3"}, nil},
		{"8", "7.1.2", Images{}, ErrInvalidVersion},
		{"8.3", "latest", Images{}, ErrInvalidVersion},
		{"8.3", "7.1.2:evil", Images{}, ErrInvalidVersion},
	}
	for _, tt := range tests {
		got, err := c.Images(tt.php, tt.wp)
		if !errors.Is(err, tt.err) || (tt.err == nil) != (err == nil) || got != tt.want {
			t.Errorf("Images(%q, %q) = %+v, %v; want %+v, %v", tt.php, tt.wp, got, err, tt.want, tt.err)
		}
	}
}

func TestParseErrors(t *testing.T) {
	const wp = `"wordpress": {"latest": {"version": "7.1.2", "sha1": "761b8101538f0631a0bfc4fba7bc4abeea92f81c"}, "pinned": []}`
	const php = `"php": [{"version": "8.4", "activeSupportEnds": "2026-12-31", "securitySupportEnds": "2028-12-31"}]`
	tests := []struct {
		name, manifest string
	}{
		{"not json", `{`},
		{"unknown field", `{"registry": "r", "extra": 1, ` + php + `, ` + wp + `}`},
		{"no registry", `{` + php + `, ` + wp + `}`},
		{"trailing slash", `{"registry": "r/", ` + php + `, ` + wp + `}`},
		{"no php", `{"registry": "r", "php": [], ` + wp + `}`},
		{"bad php version", `{"registry": "r", "php": [{"version": "8", "activeSupportEnds": "2026-12-31", "securitySupportEnds": "2028-12-31"}], ` + wp + `}`},
		{"duplicate php", `{"registry": "r", "php": [` +
			`{"version": "8.4", "activeSupportEnds": "2026-12-31", "securitySupportEnds": "2028-12-31"},` +
			`{"version": "8.4", "activeSupportEnds": "2026-12-31", "securitySupportEnds": "2028-12-31"}], ` + wp + `}`},
		{"bad date", `{"registry": "r", "php": [{"version": "8.4", "activeSupportEnds": "31/12/2026", "securitySupportEnds": "2028-12-31"}], ` + wp + `}`},
		{"missing date", `{"registry": "r", "php": [{"version": "8.4", "activeSupportEnds": "2026-12-31"}], ` + wp + `}`},
		{"security before active", `{"registry": "r", "php": [{"version": "8.4", "activeSupportEnds": "2028-12-31", "securitySupportEnds": "2026-12-31"}], ` + wp + `}`},
		{"no latest wp", `{"registry": "r", ` + php + `, "wordpress": {"pinned": []}}`},
		{"bad wp version", `{"registry": "r", ` + php + `, "wordpress": {"latest": {"version": "7.x", "sha1": "761b8101538f0631a0bfc4fba7bc4abeea92f81c"}}}`},
		{"bad sha1", `{"registry": "r", ` + php + `, "wordpress": {"latest": {"version": "7.1.2", "sha1": "abc"}}}`},
		{"pinned duplicates latest", `{"registry": "r", ` + php + `, "wordpress": {` +
			`"latest": {"version": "7.1.2", "sha1": "761b8101538f0631a0bfc4fba7bc4abeea92f81c"},` +
			`"pinned": [{"version": "7.1.2", "sha1": "761b8101538f0631a0bfc4fba7bc4abeea92f81c"}]}}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := Parse([]byte(tt.manifest)); !errors.Is(err, ErrInvalidManifest) {
				t.Errorf("Parse = %v, want ErrInvalidManifest", err)
			}
		})
	}
}

func TestCompareVersions(t *testing.T) {
	tests := []struct {
		a, b string
		want int // sign only
	}{
		{"8.4", "8.4", 0},
		{"8.10", "8.9", 1},
		{"7.1", "7.1.2", -1},
		{"6.9.9", "7.0", -1},
	}
	for _, tt := range tests {
		got := compareVersions(tt.a, tt.b)
		if (got > 0) != (tt.want > 0) || (got < 0) != (tt.want < 0) {
			t.Errorf("compareVersions(%s, %s) = %d, want sign %d", tt.a, tt.b, got, tt.want)
		}
	}
}
