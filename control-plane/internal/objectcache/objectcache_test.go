package objectcache_test

import (
	"strings"
	"testing"

	"github.com/rchouinard/pressfleet/control-plane/internal/objectcache"
	"github.com/rchouinard/pressfleet/control-plane/internal/redact"
)

func TestCredentialsRoundTrip(t *testing.T) {
	in := objectcache.Credentials{Username: "pf_x", Password: "pw", Database: 3, KeyPrefix: objectcache.KeyPrefix("x")}
	s, err := objectcache.MarshalCredentials(in)
	if err != nil {
		t.Fatal(err)
	}
	out, err := objectcache.UnmarshalCredentials(s)
	if err != nil {
		t.Fatal(err)
	}
	if out != in {
		t.Errorf("round trip = %+v, want %+v", out, in)
	}
	if in.KeyPrefix != "x:" {
		t.Errorf("KeyPrefix = %q", in.KeyPrefix)
	}
}

func TestUnmarshalCredentialsRejects(t *testing.T) {
	for _, s := range []string{
		`not json with secret-pw`,
		`{"username":"u","password":"p","database":0,"key_prefix":"x:"}`,
		`{"username":"u","password":"","database":1,"key_prefix":"x:"}`,
		`{"username":"","password":"p","database":1,"key_prefix":"x:"}`,
		`{"username":"u","password":"p","database":1}`,
	} {
		_, err := objectcache.UnmarshalCredentials(redact.Secret(s))
		if err == nil {
			t.Errorf("%s: accepted", s)
		} else if strings.Contains(err.Error(), "secret-pw") {
			t.Errorf("error quotes the input: %v", err)
		}
	}
}
