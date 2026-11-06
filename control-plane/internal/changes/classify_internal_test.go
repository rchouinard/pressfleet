package changes

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/rchouinard/pressfleet/control-plane/internal/authz"
)

// TestEveryActionClassified keeps actionGroups complete: an action that
// changes something must say which group its changes are in, or that they
// aren't reported, rather than fall to the default.
func TestEveryActionClassified(t *testing.T) {
	for _, a := range authz.Actions() {
		if _, ok := actionGroups[string(a)]; a.Writes() && !ok {
			t.Errorf("action %s changes state but isn't in actionGroups", a)
		}
	}
	for a := range actionGroups {
		if a != "self.login" && !authz.Action(a).Known() {
			t.Errorf("actionGroups has %s, which isn't an action", a)
		}
	}
}

// TestEveryJobTypeClassified keeps jobGroups complete. Job types are
// constants named JobType or ending in it across the control plane's
// packages, so the test finds them in the source, and a new job type fails
// it until it is classified.
func TestEveryJobTypeClassified(t *testing.T) {
	types := map[string]string{} // type to where it is defined
	fset := token.NewFileSet()
	err := filepath.WalkDir("..", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		for _, decl := range f.Decls {
			g, ok := decl.(*ast.GenDecl)
			if !ok || g.Tok != token.CONST {
				continue
			}
			for _, spec := range g.Specs {
				vs := spec.(*ast.ValueSpec)
				for i, name := range vs.Names {
					if !strings.HasSuffix(name.Name, "JobType") || i >= len(vs.Values) {
						continue
					}
					lit, ok := vs.Values[i].(*ast.BasicLit)
					if !ok || lit.Kind != token.STRING {
						continue
					}
					v, err := strconv.Unquote(lit.Value)
					if err != nil {
						return err
					}
					types[v] = path + ": " + name.Name
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(types) < 30 {
		t.Fatalf("found only %d job types; is the search broken? %v", len(types), types)
	}
	for typ, where := range types {
		if _, ok := jobGroups[typ]; !ok {
			t.Errorf("job type %s (%s) isn't in jobGroups", typ, where)
		}
	}
	for typ := range jobGroups {
		if _, ok := types[typ]; !ok {
			t.Errorf("jobGroups has %s, which no constant defines", typ)
		}
	}
}
