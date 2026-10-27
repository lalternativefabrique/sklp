package spec

import (
	"fmt"
	"reflect"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// checkUnknownKeys rejects mapping keys that no field of the schema
// claims, for the two shapes where a typo is both likely and silent:
// the document root and each entry of `steps:`.
//
// gopkg.in/yaml.v3 drops unknown keys without a word, so a contract
// that says `workdir:` instead of `cwd:` parses clean and the step runs
// at the repo root — a CI step that compiles nothing and passes. The
// alternative, decoder.KnownFields(true), cannot be used here: several
// schema types (ScopeRef, StepSecrets, ScanConfigYAML) decode
// polymorphic YAML through custom unmarshalers, and free-form fields
// (targets, inputs, tasks) are map[string]any by design.
func checkUnknownKeys(path string, root *yaml.Node) error {
	doc := root
	if doc.Kind == yaml.DocumentNode && len(doc.Content) > 0 {
		doc = doc.Content[0]
	}
	if doc.Kind != yaml.MappingNode {
		return nil
	}

	// Scoped to `kind: pipeline`. devstack contracts share this loader
	// but carry their own fields (allowHost, …) and already get a strict
	// pre-pass in devstack.LoadOpts; deploy/config/space files have other
	// schemas entirely. A contract inheriting its kind through `extends:`
	// is checked when the parent that declares the steps is loaded.
	kind := mappingValue(doc, "kind")
	declaresSteps := mappingValue(doc, "steps") != nil
	isPipeline := (kind != nil && kind.Value == "pipeline") ||
		(kind == nil && declaresSteps)
	if !isPipeline {
		return nil
	}

	if err := checkMapping(path, doc, yamlFields(reflect.TypeOf(Pipeline{})), ""); err != nil {
		return err
	}

	steps := mappingValue(doc, "steps")
	if steps == nil || steps.Kind != yaml.SequenceNode {
		return nil
	}
	stepFields := yamlFields(reflect.TypeOf(Step{}))
	for _, item := range steps.Content {
		if item.Kind != yaml.MappingNode {
			continue
		}
		where := "step"
		if n := mappingValue(item, "name"); n != nil && n.Value != "" {
			where = fmt.Sprintf("step %q", n.Value)
		}
		if err := checkMapping(path, item, stepFields, where); err != nil {
			return err
		}
	}
	return nil
}

func checkMapping(path string, node *yaml.Node, known map[string]struct{}, where string) error {
	for i := 0; i+1 < len(node.Content); i += 2 {
		key := node.Content[i]
		if _, ok := known[key.Value]; ok {
			continue
		}
		return &UnknownKeyError{
			File:       path,
			Line:       key.Line,
			Column:     key.Column,
			Key:        key.Value,
			Where:      where,
			Suggestion: nearestField(key.Value, known),
			Known:      sortedKeys(known),
		}
	}
	return nil
}

// UnknownKeyError names the offending key, where it sits, and the field
// the author most likely meant.
type UnknownKeyError struct {
	File       string
	Line       int
	Column     int
	Key        string
	Where      string
	Suggestion string
	Known      []string
}

func (e *UnknownKeyError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s:%d:%d: unknown field %q", e.File, e.Line, e.Column, e.Key)
	if e.Where != "" {
		fmt.Fprintf(&b, " in %s", e.Where)
	}
	if e.Suggestion != "" {
		fmt.Fprintf(&b, "\n  did you mean `%s:`?", e.Suggestion)
	}
	fmt.Fprintf(&b, "\n  sklp ignores nothing: a field it does not know is a field that would never take effect.")
	fmt.Fprintf(&b, "\n  valid fields here: %s", strings.Join(e.Known, ", "))
	return b.String()
}

func yamlFields(t reflect.Type) map[string]struct{} {
	out := make(map[string]struct{}, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		tag := t.Field(i).Tag.Get("yaml")
		if tag == "" || tag == "-" {
			continue
		}
		name, _, _ := strings.Cut(tag, ",")
		if name != "" {
			out[name] = struct{}{}
		}
	}
	return out
}

func mappingValue(node *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			return node.Content[i+1]
		}
	}
	return nil
}

func sortedKeys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// aliases maps field names borrowed from other CI formats onto the sklp
// field that does the same job. A synonym is never close enough in edit
// distance to be guessed, and it is the mistake an author carries in
// from Docker, GitHub Actions or CircleCI.
var aliases = map[string]string{
	"workdir":           "cwd",
	"working_dir":       "cwd",
	"working_directory": "cwd",
	"working-directory": "cwd",
	"dir":               "cwd",
	"path":              "cwd",
	"command":           "run",
	"cmd":               "run",
	"script":            "run",
	"shell":             "run",
	"steps":             "run",
	"needs":             "depends_on",
	"requires":          "depends_on",
	"after":             "depends_on",
	"environment":       "env",
	"when":              "if",
	"only":              "scope",
	"paths":             "inputs",
	"artifacts":         "outputs",
	"image":             "runner",
}

// nearestField returns the known field closest to key, or "" when none
// is close enough to be worth naming. Known aliases from other CI
// formats resolve first; otherwise the threshold scales with the key's
// length so short names don't collect unrelated suggestions.
func nearestField(key string, known map[string]struct{}) string {
	if target, ok := aliases[strings.ToLower(key)]; ok {
		if _, valid := known[target]; valid {
			return target
		}
	}
	limit := len(key)/3 + 1
	best, bestDist := "", limit+1
	for _, candidate := range sortedKeys(known) {
		d := editDistance(strings.ToLower(key), candidate)
		if d < bestDist {
			best, bestDist = candidate, d
		}
	}
	if bestDist > limit {
		return ""
	}
	return best
}

func editDistance(a, b string) int {
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, min(cur[j-1]+1, prev[j-1]+cost))
		}
		prev, cur = cur, prev
	}
	return prev[len(b)]
}
