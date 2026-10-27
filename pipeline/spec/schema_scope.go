package spec

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

// ScopeRef captures the value of Step.Scope, which the user can write
// in two YAML shapes:
//
//	scope: backend           # single scope
//	scope: [backend, shared] # multiple, OR semantics
//
// Both decode to a []string. The empty list (and the absent field) mean
// "this step has no scope gate — always run", consistent with the
// design doc.
type ScopeRef []string

// UnmarshalYAML accepts either a scalar string or a sequence of
// strings. Other shapes (map, mixed types) produce a parse error so
// typos surface early, not at run time.
func (s *ScopeRef) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.ScalarNode:
		if node.Value == "" {
			*s = nil
			return nil
		}
		*s = []string{node.Value}
		return nil

	case yaml.SequenceNode:
		out := make([]string, 0, len(node.Content))
		for _, c := range node.Content {
			if c.Kind != yaml.ScalarNode {
				return fmt.Errorf("scope: each entry must be a string (line %d)", c.Line)
			}
			if c.Value != "" {
				out = append(out, c.Value)
			}
		}
		// `scope: []` is treated as "no scope" (same as absent). That
		// avoids a noisy parse error for the empty-list edge case
		// while keeping the typo detection on map / mixed types.
		*s = out
		return nil

	default:
		return fmt.Errorf("scope: expected string or list of strings, got kind %d at line %d", node.Kind, node.Line)
	}
}
