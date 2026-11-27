package repofacts

import (
	"fmt"
	"sort"
	"strings"
)

// CIConfig renders a deterministic `.sklp/tasks/ci.yaml` (kind:
// pipeline) from the scanned repo. No LLM — reproducible YAML.
//
// The generated pipeline is intentionally minimal and safe: one test
// step per buildable module, scoped to that module's path so the
// impacted-services filter only runs what changed. Lint/typecheck steps
// are added when the conventional command is unambiguous (a node module
// with a package.json, a Go module). Anything requiring judgement
// (integration tests with sidecars, custom make targets) is left to a
// NOTE — this is the "deterministic simple" fallback, not a substitute
// for an agent-authored pipeline.
func CIConfig(f *RepoFacts) (yaml string, notes []string) {
	var b strings.Builder
	b.WriteString("version: 1\n")
	b.WriteString("kind: pipeline\n")
	b.WriteString("name: ci\n")
	b.WriteString("description: |\n  Lint + test each module whose files changed since base_ref.\n")

	// tools: one entry per detected language with a pinned version.
	tools := ciToolchain(f)
	if len(tools) > 0 {
		b.WriteString("\ntools:\n")
		for _, k := range sortedToolKeys(tools) {
			fmt.Fprintf(&b, "  %s: %q\n", k, tools[k])
		}
	}

	// Build (scope, steps) pairs deterministically per module.
	type modStep struct {
		scope string
		globs []string
		steps []string // rendered step blocks
	}
	byPath := map[string][]langModule{}
	for _, lang := range f.Languages {
		for _, m := range lang.Modules {
			byPath[modulePath(m.Path)] = append(byPath[modulePath(m.Path)], langModule{lang, m})
		}
	}
	scopes := uniqueScopeNames(byPath)
	var mods []modStep
	for path, lms := range byPath {
		ms := modStep{scope: scopes[path], globs: scopeGlobs(path)}
		for _, lm := range lms {
			name := "test-" + ms.scope
			if len(lms) > 1 {
				name += "-" + lm.lang.Lang
			}
			ms.steps = append(ms.steps, ciSteps(lm.lang, lm.mod, ms.scope, name)...)
		}
		if len(ms.steps) > 0 {
			mods = append(mods, ms)
		}
	}
	// Stable order by scope name so output is reproducible.
	sort.Slice(mods, func(i, j int) bool { return mods[i].scope < mods[j].scope })

	if len(mods) == 0 {
		notes = append(notes, "no buildable modules detected — add a test step by hand")
		b.WriteString("\nsteps:\n  - name: test\n    run: echo 'add your test command' && false\n")
		return b.String(), notes
	}

	// scopes block.
	b.WriteString("\nscopes:\n")
	for _, ms := range mods {
		fmt.Fprintf(&b, "  %s:\n", ms.scope)
		for _, g := range ms.globs {
			fmt.Fprintf(&b, "    - %q\n", g)
		}
	}

	// steps block.
	b.WriteString("\nsteps:\n")
	for _, ms := range mods {
		for _, s := range ms.steps {
			b.WriteString(s)
		}
	}

	notes = append(notes, "deterministic baseline: one test step per module. Add lint/integration steps as your project needs them.")
	return b.String(), notes
}

// ciToolchain maps detected languages to a tools: map, reusing the same
// version pins RunnerConfig relies on.
func ciToolchain(f *RepoFacts) map[string]string {
	out := map[string]string{}
	for _, l := range f.Languages {
		if l.ToolchainVersion != "" {
			out[l.Lang] = l.ToolchainVersion
		}
		if (l.PackageManager == "pnpm" || l.PackageManager == "yarn" || l.PackageManager == "bun") && l.PackageManagerVersion != "" {
			out[l.PackageManager] = l.PackageManagerVersion
		}
	}
	return out
}

// ciSteps returns the rendered step block(s) for one module. Each block
// is a full YAML fragment ready to append under `steps:`.
func ciSteps(lang LanguageFacts, m ModuleFacts, scope, name string) []string {
	dir := m.Path
	if dir == "" {
		dir = "."
	}
	var steps []string
	add := func(tools []string, run string) {
		var b strings.Builder
		fmt.Fprintf(&b, "  - name: %s\n", name)
		fmt.Fprintf(&b, "    scope: [%s]\n", scope)
		if len(tools) > 0 {
			fmt.Fprintf(&b, "    tools: [%s]\n", strings.Join(tools, ", "))
		}
		fmt.Fprintf(&b, "    run: %s\n", run)
		steps = append(steps, b.String())
	}

	switch lang.Lang {
	case "go":
		add([]string{"go"}, fmt.Sprintf("cd %s && go test ./...", dir))
	case "node":
		pm := lang.PackageManager
		if pm == "" {
			pm = "npm"
		}
		run := nodeScript(pm, dir, "test")
		add(dedupeTools([]string{"node", pm}), run)
	case "rust":
		add([]string{"rust"}, fmt.Sprintf("cd %s && cargo test", dir))
	case "python":
		add([]string{"python"}, fmt.Sprintf("cd %s && python -m pytest", dir))
	default:
		// Unknown language: no deterministic command we'd trust.
	}
	return steps
}

// nodeScript builds a package-manager-aware run command for a named
// script, scoped to the module directory.
func nodeScript(pm, dir, script string) string {
	switch pm {
	case "pnpm":
		return fmt.Sprintf("cd %s && pnpm run %s", dir, script)
	case "yarn":
		return fmt.Sprintf("cd %s && yarn %s", dir, script)
	case "bun":
		return fmt.Sprintf("cd %s && bun run %s", dir, script)
	default:
		return fmt.Sprintf("cd %s && npm run %s", dir, script)
	}
}

func dedupeTools(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, t := range in {
		if t == "" || seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
	}
	return out
}

// scopeName derives a stable scope identifier from a module path.
// "services/api" → "api", "" → the language name.
type langModule struct {
	lang LanguageFacts
	mod  ModuleFacts
}

func modulePath(path string) string {
	if path == "" {
		return "."
	}
	return path
}

// uniqueScopeNames names each module path's scope after its last element, or
// after the whole path when that element is shared: a scopes: map with a
// repeated key is not valid YAML.
func uniqueScopeNames(byPath map[string][]langModule) map[string]string {
	base := func(path string) string {
		if path == "." {
			return byPath[path][0].lang.Lang
		}
		return path[strings.LastIndex(path, "/")+1:]
	}
	count := map[string]int{}
	for path := range byPath {
		count[base(path)]++
	}
	names := make(map[string]string, len(byPath))
	for path := range byPath {
		if name := base(path); count[name] == 1 {
			names[path] = name
		} else {
			names[path] = strings.ReplaceAll(path, "/", "-")
		}
	}
	return names
}

// scopeGlobs returns the doublestar globs that define a module's scope.
func scopeGlobs(path string) []string {
	if path == "" || path == "." {
		return []string{"**"}
	}
	return []string{path + "/**"}
}
