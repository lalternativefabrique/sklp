package spec

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

// ExtendsList accepts either a single string or a list of strings in
// YAML. `extends: _base` and `extends: [_base, _node]` both decode to
// []string. Resolution order is left-to-right: each entry is merged on
// top of the previous one, and the file declaring `extends:` is merged
// last so its own fields always win.
type ExtendsList []string

// UnmarshalYAML implements gopkg.in/yaml.v3 Unmarshaler so the parser
// can keep accepting both shapes without callers paying any cost.
func (e *ExtendsList) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.ScalarNode:
		// `extends: _base` (single template). Empty scalars are ignored
		// — that matches user expectation when commenting out the line.
		if node.Value == "" {
			*e = nil
			return nil
		}
		*e = []string{node.Value}
		return nil

	case yaml.SequenceNode:
		// `extends: [_base, _node]`. Every entry must be a scalar.
		out := make([]string, 0, len(node.Content))
		for _, c := range node.Content {
			if c.Kind != yaml.ScalarNode {
				return fmt.Errorf("extends: each entry must be a string (line %d)", c.Line)
			}
			if c.Value != "" {
				out = append(out, c.Value)
			}
		}
		*e = out
		return nil

	default:
		return fmt.Errorf("extends: expected string or list of strings, got kind %d at line %d", node.Kind, node.Line)
	}
}
