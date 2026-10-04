package compose

import (
	"testing"
	"testing/fstest"
)

func TestFingerprintTemplates(t *testing.T) {
	base := fstest.MapFS{"site.yaml.tmpl": {Data: []byte("services: {}\n")}}
	fp := func(fsys fstest.MapFS, version int) string {
		t.Helper()
		s, err := fingerprintTemplates(fsys, version)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	want := fp(base, 1)
	if got := fp(fstest.MapFS{"site.yaml.tmpl": {Data: []byte("services: {}\n")}}, 1); got != want {
		t.Errorf("same templates: %q, want %q", got, want)
	}
	for name, other := range map[string]string{
		"changed template": fp(fstest.MapFS{"site.yaml.tmpl": {Data: []byte("services: {x: {}}\n")}}, 1),
		"renamed template": fp(fstest.MapFS{"other.yaml.tmpl": {Data: []byte("services: {}\n")}}, 1),
		"added template": fp(fstest.MapFS{
			"site.yaml.tmpl": {Data: []byte("services: {}\n")}, "extra.tmpl": {Data: nil},
		}, 1),
		"bumped render version": fp(base, 2),
	} {
		if other == want {
			t.Errorf("%s: the fingerprint didn't change", name)
		}
	}
	// The embedded templates hash without error.
	if templateFingerprint() == "" {
		t.Error("empty fingerprint of the embedded templates")
	}
}
