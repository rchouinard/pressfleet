package archtest_test

import (
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"

	"github.com/rchouinard/pressfleet/control-plane/internal/archtest"
)

// TestRepository enforces archtest.Rules on the whole repository.
func TestRepository(t *testing.T) {
	violations, err := archtest.Check(os.DirFS(repoRoot(t)), archtest.Rules)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range violations {
		t.Error(v)
	}
}

func TestCheckDetectsViolations(t *testing.T) {
	src := func(imports ...string) *fstest.MapFile {
		s := "package p\nimport (\n"
		for _, imp := range imports {
			s += "\t" + `"` + imp + `"` + "\n"
		}
		return &fstest.MapFile{Data: []byte(s + ")\n")}
	}
	fsys := fstest.MapFS{
		"control-plane/internal/provisioner/compose/driver.go": src("github.com/docker/docker/client", "fmt"),
		"control-plane/internal/jobs/jobs.go":                  src("github.com/docker/docker/client"),
		"control-plane/internal/jobs/jobs_test.go":             src("github.com/compose-spec/compose-go/v2/types"),
		"control-plane/internal/sites/sites.go":                src("k8s.io/client-go/kubernetes"),
		"control-plane/internal/ok/ok.go":                      src("github.com/dockerized/unrelated", "fmt"),
		"ui/node_modules/x/x.go":                               src("github.com/docker/docker/client"),
		"control-plane/internal/x/testdata/x.go":               src("github.com/docker/docker/client"),
	}

	violations, err := archtest.Check(fsys, archtest.Rules)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, v := range violations {
		got[v.File] = v.Import
	}
	want := map[string]string{
		"control-plane/internal/jobs/jobs.go":      "github.com/docker/docker/client",
		"control-plane/internal/jobs/jobs_test.go": "github.com/compose-spec/compose-go/v2/types",
		"control-plane/internal/sites/sites.go":    "k8s.io/client-go/kubernetes",
	}
	if len(got) != len(want) {
		t.Errorf("got %d violations %v, want %d", len(got), got, len(want))
	}
	for file, imp := range want {
		if got[file] != imp {
			t.Errorf("violation for %s = %q, want %q", file, got[file], imp)
		}
	}
}

// repoRoot walks up from the test's working directory to the directory
// containing go.mod.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found")
		}
		dir = parent
	}
}
