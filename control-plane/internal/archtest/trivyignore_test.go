package archtest_test

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"
)

// maxIgnoreDays bounds how long a reviewed Trivy exception lasts, so that none
// goes unreviewed for long (docs/releasing.md, "Vulnerability scanning").
const maxIgnoreDays = 60

// TestTrivyIgnoreEntriesAreReviewed enforces the policy for .trivyignore.yaml:
// every entry names one vulnerability, a scope, the reason it is safe to
// ship, and an expiry date. Trivy itself stops honoring an entry after its
// expiry, which fails the scan gate; this test keeps entries from being added
// without the reason and the date.
func TestTrivyIgnoreEntriesAreReviewed(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(repoRoot(t), ".trivyignore.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var file struct {
		Vulnerabilities []struct {
			ID        string   `yaml:"id"`
			Paths     []string `yaml:"paths"`
			PURLs     []string `yaml:"purls"`
			Statement string   `yaml:"statement"`
			ExpiredAt string   `yaml:"expired_at"`
		} `yaml:"vulnerabilities"`
	}
	if err := yaml.Unmarshal(data, &file); err != nil {
		t.Fatal(err)
	}
	id := regexp.MustCompile(`^(CVE-\d{4}-\d{4,}|GHSA(-[0-9a-z]{4}){3}|GO-\d{4}-\d+)$`)
	for _, v := range file.Vulnerabilities {
		if !id.MatchString(v.ID) {
			t.Errorf("entry %q: want a CVE, GHSA, or GO identifier", v.ID)
		}
		if len(v.Paths)+len(v.PURLs) == 0 {
			t.Errorf("%s: scope it with paths or purls, so it can't hide the same ID elsewhere", v.ID)
		}
		if len(v.Statement) < 40 {
			t.Errorf("%s: statement must say why it is safe to ship and what unblocks the fix", v.ID)
		}
		expires, err := time.Parse(time.DateOnly, v.ExpiredAt)
		if err != nil {
			t.Errorf("%s: expired_at %q: want YYYY-MM-DD", v.ID, v.ExpiredAt)
			continue
		}
		// The ceiling is relative to today, so it catches an entry added with
		// a far date without needing the entry's review date.
		if limit := time.Now().AddDate(0, 0, maxIgnoreDays); expires.After(limit) {
			t.Errorf("%s: expired_at %s is more than %d days away", v.ID, v.ExpiredAt, maxIgnoreDays)
		}
	}
}
