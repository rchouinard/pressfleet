package provisioner_test

import (
	"strings"
	"testing"

	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
)

func TestBasicAuthValidate(t *testing.T) {
	const hash = "$2a$04$abcdefghijklmnopqrstuuq5MvA9Wh8m6cHfVZ1gXWLU4.o9rSPTa"
	for _, tc := range []struct {
		name string
		auth provisioner.BasicAuth
		ok   bool
	}{
		{"valid", provisioner.BasicAuth{Username: "preview", PasswordHash: hash}, true},
		{"2y hash", provisioner.BasicAuth{Username: "a.b-c_d@e", PasswordHash: "$2y" + hash[3:]}, true},
		{"no username", provisioner.BasicAuth{PasswordHash: hash}, false},
		{"colon", provisioner.BasicAuth{Username: "a:b", PasswordHash: hash}, false},
		{"space", provisioner.BasicAuth{Username: "a b", PasswordHash: hash}, false},
		{"non-ASCII", provisioner.BasicAuth{Username: "prévu", PasswordHash: hash}, false},
		{"too long", provisioner.BasicAuth{Username: strings.Repeat("a", 65), PasswordHash: hash}, false},
		{"plaintext", provisioner.BasicAuth{Username: "preview", PasswordHash: "secret"}, false},
		{"md5", provisioner.BasicAuth{Username: "preview", PasswordHash: "$apr1$abc$def"}, false},
		{"newline", provisioner.BasicAuth{Username: "preview", PasswordHash: hash + "\n"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.auth.Validate(); (err == nil) != tc.ok {
				t.Errorf("Validate() = %v, want ok = %v", err, tc.ok)
			}
			cfg := validConfig()
			cfg.PreviewAuth = &tc.auth
			if err := cfg.Validate(); (err == nil) != tc.ok {
				t.Errorf("SiteConfig.Validate() = %v, want ok = %v", err, tc.ok)
			}
		})
	}
}
