package mariadb

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rchouinard/pressfleet/control-plane/internal/id"
	"github.com/rchouinard/pressfleet/control-plane/internal/redact"
)

const testSiteID = "01j9z8y7x6w5v4t3s2r1q0p9n8"

func TestNewSiteName(t *testing.T) {
	n, err := newSiteName(testSiteID)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, got, want string }{
		{"String", n.String(), "pf_" + testSiteID},
		{"ident", n.ident(), "`pf_" + testSiteID + "`"},
		{"grantIdent", n.grantIdent(), "`pf\\_" + testSiteID + "`"},
		{"account", n.account(), "'pf_" + testSiteID + "'@'%'"},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %s, want %s", tc.name, tc.got, tc.want)
		}
	}
	if len(n.String()) > 64 {
		t.Errorf("name %s exceeds MariaDB's 64-character database name limit", n)
	}
}

func TestNewSiteNameRejectsInvalid(t *testing.T) {
	for _, in := range []string{
		"",
		"abc",
		strings.ToUpper(testSiteID),
		testSiteID + "0",
		testSiteID[:25],
		"81j9z8y7x6w5v4t3s2r1q0p9n8", // first character out of range
		"01j9z8y7x6w5v4t3s2r1q0p9n`",
		"01j9z8y7x6w5v4t3s2r1q0p9n'",
		"01j9z8y7x6w5v4t3s2r1q0p9n;",
		"01j9z8y7x6w5v4t3s2r1q0p9_%",
		"01j9z8y7x6w5v4t3s2r1q0p9 n",
		"01j9z8y7x6w5v4t3s2r1q0p9ni", // i is not in Crockford base32
	} {
		if _, err := newSiteName(in); !errors.Is(err, ErrInvalidSiteID) {
			t.Errorf("newSiteName(%q) err = %v, want ErrInvalidSiteID", in, err)
		}
	}
	if _, err := newSiteName(id.New()); err != nil {
		t.Errorf("newSiteName(id.New()) = %v", err)
	}
}

// The quoting functions escape their quote characters even though a
// validated name never contains them.
func TestQuotingEscapes(t *testing.T) {
	n := siteName{s: "a`b'c_d%e\\f"}
	if got, want := n.ident(), "`a``b'c_d%e\\f`"; got != want {
		t.Errorf("ident = %s, want %s", got, want)
	}
	if got, want := n.grantIdent(), "`a``b'c\\_d\\%e\\\\f`"; got != want {
		t.Errorf("grantIdent = %s, want %s", got, want)
	}
	if got, want := n.account(), "'a`b''c_d%e\\f'@'%'"; got != want {
		t.Errorf("account = %s, want %s", got, want)
	}
}

func TestNativePasswordHash(t *testing.T) {
	// Values from MariaDB's PASSWORD() function.
	for pw, want := range map[string]string{
		"abc": "*0D3CED9BEC10A777AEC23CCC353A8C08A633045E",
		"x":   "*B69027D44F6E5EDC07F1AEAD1477967B16F28227",
	} {
		got := nativePasswordHash(redact.Secret(pw))
		if got != want {
			t.Errorf("nativePasswordHash(%q) = %s, want %s", pw, got, want)
		}
		if !validNativeHash(got) {
			t.Errorf("validNativeHash(%s) = false", got)
		}
	}
	for _, h := range []string{"", "abc", "*0d3ced9bec10a777aec23ccc353a8c08a633045e", "*0D3CED9BEC10A777AEC23CCC353A8C08A633045", "*0D3CED9BEC10A777AEC23CCC353A8C08A633045' OR '1"} {
		if validNativeHash(h) {
			t.Errorf("validNativeHash(%q) = true", h)
		}
	}
}

func TestNewPassword(t *testing.T) {
	seen := map[string]bool{}
	for range 100 {
		pw := newPassword().Reveal()
		if len(pw) < 26 {
			t.Fatalf("password length %d, want at least 26", len(pw))
		}
		if seen[pw] {
			t.Fatal("duplicate password")
		}
		seen[pw] = true
	}
}

func TestReadPasswordFile(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	for content, want := range map[string]string{
		"s3cret":         "s3cret",
		"s3cret\n":       "s3cret",
		"s3cret\r\n\n":   "s3cret",
		"s3 cret\n":      "s3 cret",
		"line1\nline2\n": "line1\nline2",
	} {
		got, err := readPasswordFile(write("pw", content))
		if err != nil || got.Reveal() != want {
			t.Errorf("readPasswordFile(%q) = %q, %v; want %q", content, got.Reveal(), err, want)
		}
	}
	for name, p := range map[string]string{
		"missing": filepath.Join(dir, "missing"),
		"empty":   write("empty", "\n"),
		"large":   write("large", strings.Repeat("s3cret", 1000)),
	} {
		_, err := readPasswordFile(p)
		if err == nil {
			t.Errorf("%s: readPasswordFile succeeded", name)
		} else if strings.Contains(err.Error(), "s3cret") {
			t.Errorf("%s: error leaked the password: %v", name, err)
		}
	}
}
