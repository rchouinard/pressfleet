package config

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"
)

// ErrDeprecatedConflict reports that a config key (or environment variable)
// was set under both its old and its new name, so neither can be preferred
// without guessing.
var ErrDeprecatedConflict = errors.New("config set under both an old and a new name")

// deprecatedKey records a config key or environment variable that a release
// renamed or removed.
type deprecatedKey struct {
	// Old is the name the release retired: a dotted key path such as
	// "backup.window", or an environment variable name.
	Old string
	// New is the name that replaced Old. Empty means Old was removed.
	New string
	// Since is the release that made the change, such as "v0.5.0".
	Since string
	// Reason says briefly why a removed key went away. It is unused for
	// renames.
	Reason string
}

func (d deprecatedKey) removed() bool { return d.New == "" }

// Renamed and removed settings.
//
// Config loading is strict, so a binary that no longer knows a key would
// refuse to start against an operator's existing config, which would break
// `pressfleet upgrade` and the control plane after it. To rename or remove a
// key or a PRESSFLEET_* variable, add a row here as well as changing the
// field; never just delete or rename the field. A renamed key is moved to its
// new name before the config is decoded, a removed one is dropped, and either
// produces a warning (Config.Warnings) telling the operator to update their
// config.
//
// Rows are applied in order, so a key renamed twice is followed through both
// rows. Keep each row for at least one minor release after the change, the
// same window as the version skew policy (§4.7), and until the oldest
// release `pressfleet upgrade` accepts upgrading from already uses the new
// name.
//
// They are variables only so tests can swap them.
var (
	// deprecatedKeys lists config file keys as dotted paths.
	deprecatedKeys []deprecatedKey
	// deprecatedEnvVars lists PRESSFLEET_* environment variables.
	deprecatedEnvVars []deprecatedKey
)

// applyDeprecatedKeys rewrites a parsed config file in place according to
// deprecatedKeys. It reports whether anything changed and a warning for each
// key it moved or dropped.
func applyDeprecatedKeys(doc *yaml.Node, path string) (changed bool, warnings []string, err error) {
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 {
		return false, nil, nil
	}
	root := resolve(doc.Content[0])
	for _, d := range deprecatedKeys {
		oldPath := strings.Split(d.Old, ".")
		val, ok := removeKey(root, oldPath)
		if !ok {
			continue
		}
		changed = true
		if d.removed() {
			warnings = append(warnings, fmt.Sprintf(
				"config key %s was removed in %s (%s) and is ignored; remove it from %s", d.Old, d.Since, d.Reason, path))
			continue
		}
		switch err := insertKey(root, strings.Split(d.New, "."), val); {
		case errors.Is(err, ErrDeprecatedConflict):
			return false, nil, fmt.Errorf("%w: config keys %s and %s are both set in %s; %s was renamed to %s in %s, so remove %s",
				err, d.Old, d.New, path, d.Old, d.New, d.Since, d.Old)
		case err != nil:
			return false, nil, fmt.Errorf("config key %s was renamed to %s in %s: %w", d.Old, d.New, d.Since, err)
		}
		warnings = append(warnings, fmt.Sprintf(
			"config key %s was renamed to %s in %s; update %s", d.Old, d.New, d.Since, path))
	}
	return changed, warnings, nil
}

// resolve follows YAML aliases to the node they refer to.
func resolve(n *yaml.Node) *yaml.Node {
	for n != nil && n.Kind == yaml.AliasNode {
		n = n.Alias
	}
	return n
}

// lookupKey returns the index of key's key node in a mapping's Content, or -1.
func lookupKey(m *yaml.Node, key string) int {
	if m == nil || m.Kind != yaml.MappingNode {
		return -1
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return i
		}
	}
	return -1
}

// removeKey removes the key at path from the mapping m and returns its
// value. It reports false if the key isn't there. Sections left empty are
// removed too, since the section itself may be what the release retired
// (e.g. all of "logging" moving under "log").
func removeKey(m *yaml.Node, path []string) (*yaml.Node, bool) {
	i := lookupKey(m, path[0])
	if i < 0 {
		return nil, false
	}
	if len(path) == 1 {
		val := m.Content[i+1]
		m.Content = slices.Delete(m.Content, i, i+2)
		return val, true
	}
	child := resolve(m.Content[i+1])
	val, ok := removeKey(child, path[1:])
	if ok && len(child.Content) == 0 {
		m.Content = slices.Delete(m.Content, i, i+2)
	}
	return val, ok
}

// insertKey sets the key at path in the mapping m to val, creating parent
// mappings as needed. The key must not already be set.
func insertKey(m *yaml.Node, path []string, val *yaml.Node) error {
	for depth, key := range path {
		if m.Kind != yaml.MappingNode {
			return fmt.Errorf("%s is not a mapping", strings.Join(path[:depth], "."))
		}
		i := lookupKey(m, key)
		if depth == len(path)-1 {
			if i >= 0 {
				return ErrDeprecatedConflict
			}
			m.Content = append(m.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, val)
			return nil
		}
		if i < 0 {
			child := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
			m.Content = append(m.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, child)
			m = child
			continue
		}
		child := resolve(m.Content[i+1])
		if child.Kind == yaml.ScalarNode && child.Tag == "!!null" {
			// An empty section such as "backup:" with nothing under it.
			child = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
			m.Content[i+1] = child
		}
		m = child
	}
	return nil
}

// applyDeprecatedEnv wraps env so that renamed variables are read under their
// old names when the new ones are unset. It returns a warning for each
// deprecated variable that is set.
func applyDeprecatedEnv(env LookupEnv) (LookupEnv, []string, error) {
	var warnings []string
	for _, d := range deprecatedEnvVars {
		if _, ok := env(d.Old); !ok {
			continue
		}
		if d.removed() {
			warnings = append(warnings, fmt.Sprintf(
				"environment variable %s was removed in %s (%s) and is ignored; unset it", d.Old, d.Since, d.Reason))
			continue
		}
		if _, ok := env(d.New); ok {
			return nil, nil, fmt.Errorf("%w: environment variables %s and %s are both set; %s was renamed to %s in %s, so unset %s",
				ErrDeprecatedConflict, d.Old, d.New, d.Old, d.New, d.Since, d.Old)
		}
		warnings = append(warnings, fmt.Sprintf(
			"environment variable %s was renamed to %s in %s; set %s instead", d.Old, d.New, d.Since, d.New))
		// Wrapping per row lets a later row follow a variable renamed twice.
		env = renamedEnv(env, d.Old, d.New)
	}
	return env, warnings, nil
}

// renamedEnv returns env with newName read from oldName.
func renamedEnv(env LookupEnv, oldName, newName string) LookupEnv {
	return func(key string) (string, bool) {
		if key == newName {
			return env(oldName)
		}
		return env(key)
	}
}
