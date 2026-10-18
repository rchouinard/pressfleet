package config

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// Every environment variable ApplyEnv reads has a row in
// docs/configuration.md (CLAUDE.md, "Config").
func TestEnvVarsAreDocumented(t *testing.T) {
	doc, err := os.ReadFile(filepath.Join("..", "..", "..", "docs", "configuration.md"))
	if err != nil {
		t.Fatal(err)
	}
	names := envVarNames()
	if len(names) < 50 {
		t.Fatalf("only %d environment variables found; envVarNames misses a table", len(names))
	}
	seen := map[string]bool{}
	for _, name := range names {
		if seen[name] {
			t.Errorf("%s is read twice", name)
		}
		seen[name] = true
		if !strings.Contains(string(doc), "`"+name+"`") {
			t.Errorf("%s is not documented in docs/configuration.md", name)
		}
	}
}

// Every table entry is applied, and badly formed values name their
// variable.
func TestApplyEnvTables(t *testing.T) {
	for _, v := range intEnvVars {
		var cfg Config
		if err := ApplyEnv(&cfg, envOf(v.name, "42")); err != nil || *v.field(&cfg) != 42 {
			t.Errorf("%s=42: field %d, err %v", v.name, *v.field(&cfg), err)
		}
		if err := ApplyEnv(&cfg, envOf(v.name, "x")); err == nil || !strings.Contains(err.Error(), v.name+" must be a whole number") {
			t.Errorf("%s=x: err %v", v.name, err)
		}
	}
	for _, v := range boolEnvVars {
		var cfg Config
		if err := ApplyEnv(&cfg, envOf(v.name, "true")); err != nil || !*v.field(&cfg) {
			t.Errorf("%s=true: field %v, err %v", v.name, *v.field(&cfg), err)
		}
		if err := ApplyEnv(&cfg, envOf(v.name, "maybe")); err == nil || !strings.Contains(err.Error(), v.name+" must be true or false") {
			t.Errorf("%s=maybe: err %v", v.name, err)
		}
	}
	for _, v := range listEnvVars {
		var cfg Config
		if err := ApplyEnv(&cfg, envOf(v.name, " a, ,b ")); err != nil || strconv.Quote(strings.Join(*v.field(&cfg), "|")) != `"a|b"` {
			t.Errorf("%s: field %q, err %v", v.name, *v.field(&cfg), err)
		}
	}
}

func envOf(name, val string) LookupEnv {
	return func(key string) (string, bool) {
		if key == name {
			return val, true
		}
		return "", false
	}
}
