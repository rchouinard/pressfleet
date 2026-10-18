// Package archtest enforces architectural import rules across the repository.
//
// Design doc §4.2 requires that all orchestration go through the Provisioner
// interface, with no Docker/Compose calls outside the Compose driver. Rules
// catch forbidden imports, and CommandRules catch os/exec calls that run a
// forbidden program named by a string literal. A program name computed at
// run time (a variable, a constant from elsewhere) still needs code review.
package archtest

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path"
	"strconv"
	"strings"
)

// Rule forbids importing packages under any of Forbidden, except from files
// under one of the Allowed directories or whose names end in one of
// AllowedSuffixes. Paths are slash-separated and relative to the repository
// root.
type Rule struct {
	Name            string
	Forbidden       []string // import path prefixes, e.g. "github.com/docker/"
	Allowed         []string // directory prefixes, e.g. "control-plane/internal/provisioner/compose/"
	AllowedSuffixes []string // file name suffixes, e.g. "_docker_test.go"
}

// composeDriver is the Compose driver's directory.
const composeDriver = "control-plane/internal/provisioner/compose/"

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
	{
		// Sites are orchestrated through provisioner.Provisioner. The host
		// CLI, the doctor, and the platform package manage the platform's
		// own Compose stack (§4.7), and tests that need Docker drive
		// services with it.
		Name:      "The Compose driver package is only imported to manage the platform stack (§4.2, §4.7)",
		Forbidden: []string{"github.com/rchouinard/pressfleet/" + composeDriver},
		Allowed: []string{
			composeDriver,
			"control-plane/internal/cli/",
			"control-plane/internal/doctor/",
			"control-plane/internal/platform/",
			"control-plane/internal/e2e/",
		},
		AllowedSuffixes: []string{"_docker_test.go"},
	},
}

// CommandRule forbids running any of Programs with os/exec, except from
// files under one of the Allowed directories. A program matches by its base
// name, so "/usr/bin/docker" counts as "docker".
type CommandRule struct {
	Name     string
	Programs []string
	Allowed  []string
}

// CommandRules are the os/exec rules enforced on this repository.
var CommandRules = []CommandRule{
	{
		Name:     "The docker CLI is only run by the Compose driver (§4.2)",
		Programs: []string{"docker"},
		Allowed:  []string{composeDriver},
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

// walkGo calls fn with every .go file in fsys outside skipped directories.
func walkGo(fsys fs.FS, fn func(p string, src []byte) error) error {
	return fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
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
		return fn(p, src)
	})
}

// Check scans every .go file in fsys (the repository root) against rules.
func Check(fsys fs.FS, rules []Rule) ([]Violation, error) {
	var violations []Violation
	fset := token.NewFileSet()
	err := walkGo(fsys, func(p string, src []byte) error {
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
				if matchesAny(importPath, r.Forbidden) && !hasAnyPrefix(p, r.Allowed) && !hasAnySuffix(p, r.AllowedSuffixes) {
					violations = append(violations, Violation{File: p, Import: importPath, Rule: r.Name})
				}
			}
		}
		return nil
	})
	return violations, err
}

// CommandViolation is an os/exec call that runs a forbidden program.
type CommandViolation struct {
	File    string
	Line    int
	Program string
	Rule    string
}

func (v CommandViolation) String() string {
	return fmt.Sprintf("%s:%d runs %q: %s", v.File, v.Line, v.Program, v.Rule)
}

// CheckCommands scans every .go file in fsys that imports os/exec for
// exec.Command and exec.CommandContext calls whose program is a string
// literal one of rules forbids.
func CheckCommands(fsys fs.FS, rules []CommandRule) ([]CommandViolation, error) {
	var violations []CommandViolation
	fset := token.NewFileSet()
	err := walkGo(fsys, func(p string, src []byte) error {
		f, err := parser.ParseFile(fset, p, src, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		pkg := execImportName(f)
		if pkg == "" {
			return nil
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			prog, ok := execProgram(call, pkg)
			if !ok {
				return true
			}
			for _, r := range rules {
				for _, forbidden := range r.Programs {
					if path.Base(prog) == forbidden && !hasAnyPrefix(p, r.Allowed) {
						violations = append(violations, CommandViolation{
							File: p, Line: fset.Position(call.Pos()).Line, Program: prog, Rule: r.Name,
						})
					}
				}
			}
			return true
		})
		return nil
	})
	return violations, err
}

// execImportName returns the name f refers to os/exec by, or "" if f
// doesn't import it (or imports it as _ or .).
func execImportName(f *ast.File) string {
	for _, imp := range f.Imports {
		if p, err := strconv.Unquote(imp.Path.Value); err != nil || p != "os/exec" {
			continue
		}
		if imp.Name == nil {
			return "exec"
		}
		if imp.Name.Name != "_" && imp.Name.Name != "." {
			return imp.Name.Name
		}
	}
	return ""
}

// execProgram returns the program of an exec.Command or exec.CommandContext
// call when it is a string literal.
func execProgram(call *ast.CallExpr, pkg string) (string, bool) {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return "", false
	}
	if id, ok := sel.X.(*ast.Ident); !ok || id.Name != pkg {
		return "", false
	}
	arg := 0
	switch sel.Sel.Name {
	case "Command":
	case "CommandContext":
		arg = 1
	default:
		return "", false
	}
	if len(call.Args) <= arg {
		return "", false
	}
	lit, ok := call.Args[arg].(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	prog, err := strconv.Unquote(lit.Value)
	return prog, err == nil
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

func hasAnySuffix(s string, suffixes []string) bool {
	for _, suffix := range suffixes {
		if strings.HasSuffix(s, suffix) {
			return true
		}
	}
	return false
}
