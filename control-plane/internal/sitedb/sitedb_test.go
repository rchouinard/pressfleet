package sitedb_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/rchouinard/pressfleet/control-plane/internal/redact"
	"github.com/rchouinard/pressfleet/control-plane/internal/sitedb"
)

func TestCredentialsRoundTrip(t *testing.T) {
	in := sitedb.Credentials{Database: "pf_x", Username: "pf_x", Password: "s3cret"}
	enc, err := sitedb.MarshalCredentials(in)
	if err != nil {
		t.Fatal(err)
	}
	out, err := sitedb.UnmarshalCredentials(enc)
	if err != nil {
		t.Fatal(err)
	}
	if out.Database != in.Database || out.Username != in.Username || out.Password.Reveal() != "s3cret" {
		t.Errorf("round trip = %+v", out)
	}
	if s := fmt.Sprintf("%+v %v", in, enc); strings.Contains(s, "s3cret") {
		t.Errorf("formatting leaked the password: %s", s)
	}
}

func TestUnmarshalCredentialsErrors(t *testing.T) {
	for _, in := range []string{"not json s3cret", `{"database":"d","username":"u"}`} {
		_, err := sitedb.UnmarshalCredentials(redact.Secret(in))
		if err == nil {
			t.Errorf("UnmarshalCredentials(%q) succeeded", in)
		} else if strings.Contains(err.Error(), "s3cret") {
			t.Errorf("error leaked input: %v", err)
		}
	}
}
