package repofacts

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Scan walks the repository rooted at repoPath and returns a fully
// populated RepoFacts. The scan is read-only — it never modifies the
// repo. It also never follows symlinks, never descends into common
// "obviously not source" directories (node_modules, .git, vendor,
// dist, build, target, .venv, …), and caps depth at 5 levels to
// prevent runaway crawls on pathological structures.
//
// Concurrency: safe to call from multiple goroutines; each invocation
// has its own state.
func Scan(repoPath string) (*RepoFacts, error) {
	abs, err := filepath.Abs(repoPath)
	if err != nil {
		return nil, fmt.Errorf("repofacts: abs %q: %w", repoPath, err)
	}
	st, err := os.Stat(abs)
	if err != nil {
		return nil, fmt.Errorf("repofacts: stat %q: %w", abs, err)
	}
	if !st.IsDir() {
		return nil, fmt.Errorf("repofacts: %q is not a directory", abs)
	}

	facts := &RepoFacts{Root: abs}

	// First pass: walk the tree once, collecting every interesting
	// file path. We do the parsing in a second pass so the walker
	// stays simple and we don't pay i/o for files we will ignore.
	walked, err := walkRepo(abs)
	if err != nil {
		return nil, err
	}

	// Languages — each detector is independent and runs against the
	// pre-computed file list. Adding a stack means adding one
	// detector function below and one entry in this slice; the rest
	// of the pipeline doesn't change.
	detectors := []func([]string, string) *LanguageFacts{
		detectGo,
		detectNode,
		detectRust,
		detectPython,
	}
	for _, d := range detectors {
		if lf := d(walked, abs); lf != nil {
			facts.Languages = append(facts.Languages, *lf)
		}
	}
	sort.Slice(facts.Languages, func(i, j int) bool {
		return facts.Languages[i].Lang < facts.Languages[j].Lang
	})

	// Dockerfiles + compose. Distinct lists because the user-facing
	// "do we publish a Docker image?" question is answered by the
	// Dockerfile list alone — compose files describe local dev setup.
	for _, p := range walked {
		base := filepath.Base(p)
		switch {
		case base == "Dockerfile" || strings.HasPrefix(base, "Dockerfile."):
			facts.Dockerfiles = append(facts.Dockerfiles, rel(abs, p))
		case base == "Containerfile":
			facts.Dockerfiles = append(facts.Dockerfiles, rel(abs, p))
		case isComposeFile(base):
			facts.ComposeFiles = append(facts.ComposeFiles, rel(abs, p))
		}
	}
	sort.Strings(facts.Dockerfiles)
	sort.Strings(facts.ComposeFiles)

	// Makefile — root only. Nested Makefiles are too varied to
	// generalise; the LLM can still discover them via list_dir if it
	// needs to.
	if mk := detectMakefile(abs); mk != nil {
		facts.Makefile = mk
	}

	// Registry — best-effort, never blocks. Empty result means "the
	// LLM should ask the user or use a placeholder".
	facts.Registry = detectRegistry(abs, facts.Makefile)

	// CI hints — just enumerate the files. We do not parse them.
	facts.CIHints = detectCIHints(walked, abs)

	// API contracts — OpenAPI / AsyncAPI / GraphQL / Protobuf / Avro.
	// Filename-based detection plus tool markers (buf, swag,
	// graphql-codegen) so the LLM picks the right breaking-change
	// checker instead of a generic git diff.
	facts.APIContracts = detectAPIContracts(walked, abs)

	// Publishable artifacts — npm packages, cargo crates, python
	// distributions, helm charts, go SDK modules. Each entry carries
	// the declared version so the LLM emits a publish step that
	// matches what's actually in the manifest.
	facts.PublishableArtifacts = detectPublishableArtifacts(walked, abs)

	// Integration tests — naming/build-tag heuristics over the Go
	// test files, plus a substring match against the (already
	// detected) compose files. Drives the `services:` block and
	// dedicated test-integration step in `sklp init -only ci`.
	facts.IntegrationTests = detectIntegrationTests(walked, abs, facts.ComposeFiles)

	return facts, nil
}

// walkRepo returns every file path under root that survives the
// ignore list, up to depth 5. Directories are NOT included in the
// output — detectors only care about manifest files.
func walkRepo(root string) ([]string, error) {
	var out []string

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrPermission) {
				// Skip directories we can't read rather than aborting
				// the whole scan — partial output beats no output.
				if d != nil && d.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
			return err
		}

		// Skip the root itself but keep its children.
		if path == root {
			return nil
		}

		// Hard depth cap. A 5-level walk catches every monorepo we
		// have realistically seen (apps/<svc>/<sub>/<sub>/<file>);
		// going deeper is almost always noise.
		if depth(root, path) > 5 {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}

		// Skip common "definitely not source" directories. Listed
		// here rather than fed to each detector so the walker is the
		// single place that knows about exclusions — easier to audit.
		if d.IsDir() && isIgnoredDir(filepath.Base(path)) {
			return fs.SkipDir
		}

		// Symlinks — never follow; treating them as files keeps the
		// walker bounded but we don't try to read them.
		if d.Type()&fs.ModeSymlink != 0 {
			return nil
		}

		if !d.IsDir() {
			out = append(out, path)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("repofacts: walk: %w", err)
	}
	return out, nil
}

// isIgnoredDir lists directory basenames that never contain source
// we want to detect. The list is intentionally conservative — a name
// must be both (a) common across ecosystems and (b) very unlikely to
// contain a real manifest to make this list.
func isIgnoredDir(name string) bool {
	switch name {
	case ".git", ".svn", ".hg":
		return true
	case "node_modules", "vendor":
		return true
	case "dist", "build", "out", "bin", "obj":
		return true
	case "target": // rust build output
		return true
	case ".venv", "venv", "env", ".env": // python virtualenvs
		return true
	case "__pycache__", ".pytest_cache", ".mypy_cache", ".ruff_cache":
		return true
	case ".next", ".nuxt", ".astro", ".svelte-kit": // js framework build dirs
		return true
	case ".gradle", ".idea", ".vscode":
		return true
	case ".terraform":
		return true
	case "coverage", ".coverage":
		return true
	case ".cache":
		return true
	}
	return false
}

// depth counts how many path separators are between root and path,
// after both are cleaned. depth(root, root) == 0; a file directly
// under root has depth 1; etc.
func depth(root, path string) int {
	r, _ := filepath.Rel(root, path)
	if r == "" || r == "." {
		return 0
	}
	return strings.Count(r, string(os.PathSeparator)) + 1
}

// rel returns path relative to root with forward slashes (so the
// JSON serialised output is the same across Linux/macOS/Windows).
func rel(root, path string) string {
	r, err := filepath.Rel(root, path)
	if err != nil {
		return path
	}
	return filepath.ToSlash(r)
}
