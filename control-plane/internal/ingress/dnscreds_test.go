package ingress_test

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/rchouinard/pressfleet/control-plane/internal/ingress"
)

func TestParseDNSCredentials(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  ingress.DNSCredentials
		err   string // substring; empty for success
	}{
		{
			name:  "one variable",
			input: "CF_DNS_API_TOKEN=s3cret-token\n",
			want:  ingress.DNSCredentials{"CF_DNS_API_TOKEN": "s3cret-token"},
		},
		{
			name: "comments, blanks, spaces, CRLF, no final newline",
			input: "# Route 53\r\n\r\n  AWS_ACCESS_KEY_ID = AKIAEXAMPLE \r\n" +
				"AWS_SECRET_ACCESS_KEY=s3cret/with=equals\r\nAWS_REGION=us-east-1",
			want: ingress.DNSCredentials{
				"AWS_ACCESS_KEY_ID":     "AKIAEXAMPLE",
				"AWS_SECRET_ACCESS_KEY": "s3cret/with=equals",
				"AWS_REGION":            "us-east-1",
			},
		},
		{name: "quotes are kept", input: `TOKEN="s3cret"`, want: ingress.DNSCredentials{"TOKEN": `"s3cret"`}},
		{name: "empty", input: "", err: "no VARIABLE=value lines"},
		{name: "only comments", input: "# nothing\n", err: "no VARIABLE=value lines"},
		{name: "no equals", input: "s3cret\n", err: "line 1 is not VARIABLE=value"},
		{name: "lowercase name", input: "# c\ncf_token=s3cret\n", err: `line 2: "cf_token" is not an environment variable name`},
		{name: "export prefix", input: "export CF_DNS_API_TOKEN=s3cret\n", err: "is not an environment variable name"},
		{name: "file variable", input: "CF_DNS_API_TOKEN_FILE=/run/s3cret\n", err: "not a file"},
		{name: "no value", input: "CF_DNS_API_TOKEN=\n", err: "CF_DNS_API_TOKEN has no value"},
		{name: "duplicate", input: "A=s3cret\nA=s3cret2\n", err: "A is given twice"},
		{name: "too big", input: "A=" + strings.Repeat("s3cret", 20000) + "\n", err: "more than"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ingress.ParseDNSCredentials(strings.NewReader(tt.input))
			if tt.err != "" {
				if !errors.Is(err, ingress.ErrInvalidDNSCredentials) || !strings.Contains(err.Error(), tt.err) {
					t.Fatalf("err = %v, want %q", err, tt.err)
				}
				// Errors name lines and variables, never values.
				if strings.Contains(err.Error(), "s3cret") {
					t.Errorf("error leaks a value: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("got %d variables, want %d", len(got), len(tt.want))
			}
			for k, v := range tt.want {
				if got[k] != v {
					t.Errorf("%s = %q, want %q", k, got[k].Reveal(), v.Reveal())
				}
			}
		})
	}
}

func TestDNSCredentialsMarshal(t *testing.T) {
	c := ingress.DNSCredentials{"B_TOKEN": "two", "A_KEY": "one=1"}
	if got := c.Names(); !slices.Equal(got, []string{"A_KEY", "B_TOKEN"}) {
		t.Errorf("names = %v", got)
	}
	m := c.Marshal()
	if m.Reveal() != "A_KEY=one=1\nB_TOKEN=two\n" {
		t.Errorf("marshal = %q", m.Reveal())
	}
	back, err := ingress.ParseDNSCredentials(strings.NewReader(m.Reveal()))
	if err != nil || len(back) != 2 || back["A_KEY"] != "one=1" {
		t.Errorf("round trip = %v, %v", back.Names(), err)
	}
	// The values are secrets: printing the credentials doesn't show them.
	if s := fmt.Sprint(m, c); strings.Contains(s, "one") || strings.Contains(s, "two") {
		t.Errorf("credentials print their values: %s", s)
	}
}

func TestWriteDNSCredentials(t *testing.T) {
	dir := t.TempDir()
	stale := filepath.Join(dir, "OLD_TOKEN")
	if err := os.WriteFile(stale, []byte("old"), 0o400); err != nil {
		t.Fatal(err)
	}
	c := ingress.DNSCredentials{"CF_DNS_API_TOKEN": "s3cret", "CF_ZONE_API_TOKEN": "z0ne"}
	if err := ingress.WriteDNSCredentials(dir, c); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if want := []string{ingress.DNSCredentialsReady, "CF_DNS_API_TOKEN", "CF_ZONE_API_TOKEN"}; !slices.Equal(names, want) {
		t.Errorf("files = %v, want %v", names, want)
	}
	for name, want := range map[string]string{"CF_DNS_API_TOKEN": "s3cret", "CF_ZONE_API_TOKEN": "z0ne"} {
		path := filepath.Join(dir, name)
		got, err := os.ReadFile(path)
		if err != nil || string(got) != want {
			t.Errorf("%s = %q, %v", name, got, err)
		}
		if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o400 {
			t.Errorf("%s mode = %v, %v", name, info.Mode(), err)
		}
	}

	// Writing no credentials leaves only the marker, so Traefik starts
	// without any.
	if err := ingress.WriteDNSCredentials(dir, nil); err != nil {
		t.Fatal(err)
	}
	entries, _ = os.ReadDir(dir)
	if len(entries) != 1 || entries[0].Name() != ingress.DNSCredentialsReady {
		t.Errorf("after writing none: %v", entries)
	}

	if err := ingress.WriteDNSCredentials(filepath.Join(dir, "missing"), c); err == nil {
		t.Error("writing into a missing directory succeeded")
	}
}
