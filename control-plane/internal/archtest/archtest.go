// Package archtest enforces architectural import rules across the repository.
//
// Design doc §4.2 requires that all orchestration go through the Provisioner
// interface, with no Docker/Compose calls outside the Compose driver. The
// rules here catch forbidden imports; they cannot catch a driver-less package
// shelling out to the docker CLI, which code review must still watch for.
package archtest

import (
	"fmt"
	"go/parser"
	"go/token"
	"io/fs"
	"path"
	"strconv"
	"strings"
)

// Rule forbids importing packages under any of Forbidden, except from files
// under one of the Allowed directories. Paths are slash-separated and
// relative to the repository root.
type Rule struct {
	Name      string
	Forbidden []string // import path prefixes, e.g. "github.com/docker/"
	Allowed   []string // directory prefixes, e.g. "control-plane/internal/provisioner/compose/"
}

// Rules are the import rules enforced on this repository.
var Rules = []Rule{
	{
		Name:      "Docker/Compose SDKs are only used by the Compose driver (§4.2)",
		Forbidden: []string{"github.com/docker/", "github.com/moby/", "github.com/compose-spec/"},
		Allowed:   []string{"control-plane/internal/provisioner/compose/"},
	},
	{
		Name:      "Kubernetes SDKs are only used by the Kubernetes driver (§4.2)",
		Forbidden: []string{"k8s.io/", "sigs.k8s.io/"},
		Allowed:   []string{"control-plane/internal/provisioner/kubernetes/"},
	},
}

// Violation is a forbidden import found in a file.
type Violation struct {
	File   string
	Import string
	Rule   string
}

func (v Violation) String() string {
	return fmt.Sprintf("%s imports %q: %s", v.File, v.Import, v.Rule)
}

// skipDir reports whether a directory should not be scanned.
func skipDir(name string) bool {
	return name == "node_modules" || name == "testdata" || name == "vendor" ||
		(strings.HasPrefix(name, ".") && name != ".")
}

// Check scans every .go file in fsys (the repository root) against rules.
func Check(fsys fs.FS, rules []Rule) ([]Violation, error) {
	var violations []Violation
	fset := token.NewFileSet()
	err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if skipDir(d.Name()) {
				return fs.SkipDir
			}
			return nil
		}
		if path.Ext(p) != ".go" {
			return nil
		}
		src, err := fs.ReadFile(fsys, p)
		if err != nil {
			return err
		}
		f, err := parser.ParseFile(fset, p, src, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, imp := range f.Imports {
			importPath, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				return fmt.Errorf("%s: %w", p, err)
			}
			for _, r := range rules {
				if matchesAny(importPath, r.Forbidden) && !hasAnyPrefix(p, r.Allowed) {
					violations = append(violations, Violation{File: p, Import: importPath, Rule: r.Name})
				}
			}
		}
		return nil
	})
	return violations, err
}

// matchesAny reports whether importPath is, or is under, one of the prefixes.
func matchesAny(importPath string, prefixes []string) bool {
	for _, prefix := range prefixes {
		if strings.HasPrefix(importPath, prefix) || importPath == strings.TrimSuffix(prefix, "/") {
			return true
		}
	}
	return false
}

func hasAnyPrefix(s string, prefixes []string) bool {
	for _, prefix := range prefixes {
		if strings.HasPrefix(s, prefix) {
			return true
		}
	}
	return false
}
