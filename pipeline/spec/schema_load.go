package spec

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// LoadPipeline reads a YAML file at path, resolves `extends:` against
// sibling files in the same directory, and returns the merged Pipeline.
//
// Resolution rules:
//   - `extends: <name>` looks for `<dir>/<name>.yaml` first, then
//     `<dir>/_<name>.yaml` (the "_" prefix is the project convention
//     for non-executable templates).
//   - The chain is resolved left-to-right; each parent is itself
//     recursively loaded, so a template can extend another template.
//   - Cycles are detected and rejected with a clear error.
//   - The file declaring `extends:` is merged last, so its own fields
//     always win over inherited values.
//
// Files prefixed with "_" cannot be loaded as the top-level entry point
// — LoadPipeline rejects them with ErrTemplateNotRunnable. That keeps
// `sklp run _base` from doing anything surprising.
func LoadPipeline(path string) (Pipeline, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return Pipeline{}, fmt.Errorf("resolve path %q: %w", path, err)
	}
	base := filepath.Base(abs)
	if strings.HasPrefix(base, "_") {
		return Pipeline{}, fmt.Errorf("%w: %s", ErrTemplateNotRunnable, base)
	}
	p, err := loadPipelineFile(abs, map[string]struct{}{})
	if err != nil {
		return Pipeline{}, err
	}
	if err := validatePipeline(p); err != nil {
		return Pipeline{}, err
	}
	return p, nil
}

// ErrTemplateNotRunnable is returned when LoadPipeline is invoked on a
// file whose name starts with "_". Templates exist to be `extends:`-ed,
// never executed directly.
var ErrTemplateNotRunnable = fmt.Errorf("template files (prefixed _) cannot be run directly")

// loadPipelineFile is the recursive worker. The `seen` set tracks
// absolute paths already in the current resolution chain so an
// `extends:` cycle is detected on the offending edge rather than after
// stack overflow.
func loadPipelineFile(absPath string, seen map[string]struct{}) (Pipeline, error) {
	if _, looped := seen[absPath]; looped {
		return Pipeline{}, fmt.Errorf("extends cycle detected at %s", absPath)
	}
	seen[absPath] = struct{}{}
	// Make sure each branch of the recursion gets its own visited set —
	// otherwise two siblings extending the same parent would falsely
	// flag as a cycle. We clone-on-recurse below instead.

	var raw []byte
	var err error
	if strings.HasPrefix(absPath, builtinPrefix) {
		raw, err = readBuiltin(absPath)
	} else {
		raw, err = os.ReadFile(absPath)
	}
	if err != nil {
		return Pipeline{}, fmt.Errorf("read %s: %w", absPath, err)
	}

	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return Pipeline{}, fmt.Errorf("parse %s: %w", absPath, err)
	}
	if err := checkUnknownKeys(absPath, &doc); err != nil {
		return Pipeline{}, err
	}

	var p Pipeline
	if doc.Kind == 0 {
		return p, nil
	}
	if err := doc.Decode(&p); err != nil {
		return Pipeline{}, fmt.Errorf("parse %s: %w", absPath, err)
	}

	if len(p.Extends) == 0 {
		return p, nil
	}

	dir := filepath.Dir(absPath)
	merged := Pipeline{}

	for _, name := range p.Extends {
		parentPath, err := resolveExtendsTarget(dir, name)
		if err != nil {
			return Pipeline{}, fmt.Errorf("%s: %w", absPath, err)
		}

		childSeen := cloneSet(seen)
		parent, err := loadPipelineFile(parentPath, childSeen)
		if err != nil {
			return Pipeline{}, err
		}
		merged = mergePipeline(merged, parent)
	}

	// The current file is the rightmost layer — its own fields override
	// everything inherited.
	merged = mergePipeline(merged, p)

	// Clear `extends:` on the resolved output: callers that re-serialise
	// the merged Pipeline shouldn't accidentally reapply the chain.
	merged.Extends = nil
	return merged, nil
}

// resolveExtendsTarget maps an `extends: <name>` reference to a real
// file on disk. We try, in order:
//
//	<dir>/<name>             (literal, allows `extends: ../shared/foo.yaml`)
//	<dir>/<name>.yaml
//	<dir>/_<name>.yaml       (the "_" template convention)
//
// The first hit wins. Missing target is an explicit error mentioning
// every path we probed so the user can fix the typo.
func resolveExtendsTarget(dir, name string) (string, error) {
	if strings.HasPrefix(name, builtinPrefix) {
		if _, err := readBuiltin(name); err != nil {
			return "", err
		}
		return name, nil
	}
	candidates := []string{
		filepath.Join(dir, name),
		filepath.Join(dir, name+".yaml"),
	}
	// Only add the "_" variant when `name` isn't already prefixed, so
	// `extends: _base` doesn't probe `__base.yaml`.
	if !strings.HasPrefix(filepath.Base(name), "_") {
		candidates = append(candidates, filepath.Join(dir, "_"+name+".yaml"))
	}

	for _, c := range candidates {
		if st, err := os.Stat(c); err == nil && !st.IsDir() {
			abs, _ := filepath.Abs(c)
			return abs, nil
		}
	}
	return "", fmt.Errorf("extends target %q not found (tried %v)", name, candidates)
}

func cloneSet(s map[string]struct{}) map[string]struct{} {
	out := make(map[string]struct{}, len(s))
	for k := range s {
		out[k] = struct{}{}
	}
	return out
}
