package repofacts

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// RunnableFacts is the per-module enrichment produced by ScanRunnable.
// Unlike Scan() — which only lists what's in the repo — this opens
// manifests / source files to decide whether each module is a runnable
// application or a library. The cost is paid only by callers that
// need it (today: `sklp init dev`); CI/publish stay fast on the cheap
// Scan() path.
type RunnableFacts struct {
	// Lang carries the language identity from the originating
	// LanguageFacts (go / node / python / rust). Lets the caller
	// dispatch on stack without holding a parallel index.
	Lang string

	// PackageManager mirrors LanguageFacts.PackageManager (pnpm/npm/
	// yarn/uv/poetry/...). Empty for stacks with one canonical tool.
	PackageManager string

	// Module is the underlying ModuleFacts entry. Path/Name/Manifest
	// stay accessible without callers having to keep a separate index.
	Module ModuleFacts

	// IsApp is the headline signal: true when this module looks like
	// a runnable application, false when it looks like a library.
	// The decision rule is per-stack — see analyzeRunnable.
	IsApp bool

	// Reason is a short human-readable explanation of the IsApp
	// decision ("has main package", "package.json scripts.dev",
	// "library: no entrypoint found"). Surfaced as a YAML comment
	// by the dev.yaml generator.
	Reason string

	// Entrypoint is the suggested run command when IsApp is true.
	// Empty when no good default exists (the suggester then picks a
	// stack-appropriate fallback). For Node we also embed the
	// `--filter <pkg>` form when the module sits inside a workspace.
	Entrypoint string

	// Binaries is populated for Go modules with one or more
	// `package main` files outside the module root (cmd/<name>/...).
	// The dev.yaml generator can fan out one service per binary in
	// future versions; v0 only emits the root binary when present.
	Binaries []string
}

// ScanRunnable runs Scan() then enriches each module by opening its
// manifest. Returns a flat slice indexed by (lang, module path) so
// callers don't have to traverse the nested LanguageFacts structure
// when they're only interested in the app/lib verdict.
//
// Errors during enrichment of one module degrade gracefully: that
// module's RunnableFacts has IsApp=false and Reason explaining the
// failure, but ScanRunnable as a whole still returns the rest. This
// matters because a single broken package.json should not stop the
// generator from producing a useful dev.yaml.
func ScanRunnable(repo string) ([]RunnableFacts, error) {
	facts, err := Scan(repo)
	if err != nil {
		return nil, err
	}
	var out []RunnableFacts
	for _, lang := range facts.Languages {
		for _, mod := range lang.Modules {
			out = append(out, analyzeRunnable(repo, lang, mod))
		}
	}
	return out, nil
}

// analyzeRunnable opens the manifest of a single module and decides
// whether it's a runnable app. Per-stack rules:
//
//   - Go: must have a `package main` declaration somewhere — at the
//     module root for the simple case, or under cmd/<name>/ for
//     multi-binary repos. Modules without one are SDKs / shared libs.
//   - Node: package.json must declare `scripts.dev` (or `start`).
//     Workspaces without a dev script are libraries by definition;
//     the root package.json that only declares `workspaces:` is also
//     filtered out (it's the orchestrator, not an app).
//   - Python: pyproject.toml must declare `[project.scripts]` or
//     `[tool.poetry.scripts]`. Anything else is treated as a library.
//   - Rust: Cargo.toml must declare a `[[bin]]` or have `src/main.rs`.
//     Crates with only `[lib]` are filtered out (Tauri sidecars,
//     plugins).
//
// When the file can't be read, we default to IsApp=false and surface
// the reason — better to under-suggest than to emit broken services.
func analyzeRunnable(repo string, lang LanguageFacts, mod ModuleFacts) RunnableFacts {
	rf := RunnableFacts{
		Lang:           lang.Lang,
		PackageManager: lang.PackageManager,
		Module:         mod,
	}
	modPath := filepath.Join(repo, mod.Path)

	switch lang.Lang {
	case "go":
		bins, hasRoot := findGoBinaries(modPath)
		rf.Binaries = bins
		switch {
		case hasRoot:
			rf.IsApp = true
			rf.Reason = "has main package at module root"
			rf.Entrypoint = "go run ."
		case len(bins) > 0:
			rf.IsApp = true
			rf.Reason = "has main package under cmd/"
			rf.Entrypoint = "go run ./cmd/" + bins[0]
		default:
			rf.IsApp = false
			rf.Reason = "library: no `package main` found"
		}

	case "node":
		if mod.Manifest != "package.json" {
			rf.IsApp = false
			rf.Reason = "non-package.json manifest"
			return rf
		}
		hasDev, hasStart, isWorkspaceRoot, looksLikeLibrary, err := readNodePackageJSON(filepath.Join(modPath, "package.json"))
		switch {
		case err != nil:
			rf.IsApp = false
			rf.Reason = "package.json unreadable: " + err.Error()
		case isWorkspaceRoot:
			rf.IsApp = false
			rf.Reason = "workspace root (orchestrator, not an app)"
		case looksLikeLibrary:
			// Even when scripts.dev exists, a package that publishes a
			// build output (main/exports/types/files pointing at dist/)
			// is a library — `dev` is its incremental-build watcher
			// (tsup --watch, tsc --watch), not a dev server.
			rf.IsApp = false
			rf.Reason = "library: package.json declares published artifacts (main/exports/files)"
		case hasDev:
			rf.IsApp = true
			rf.Reason = "package.json scripts.dev present"
			rf.Entrypoint = nodeRunCommand(lang.PackageManager, mod.Name)
		case hasStart:
			rf.IsApp = true
			rf.Reason = "package.json scripts.start present (no dev script)"
			rf.Entrypoint = nodeStartCommand(lang.PackageManager, mod.Name)
		default:
			rf.IsApp = false
			rf.Reason = "library: no scripts.dev or scripts.start"
		}

	case "python":
		hasScripts, err := pyprojectHasScripts(filepath.Join(modPath, "pyproject.toml"))
		switch {
		case err != nil:
			rf.IsApp = false
			rf.Reason = "pyproject.toml unreadable: " + err.Error()
		case hasScripts:
			rf.IsApp = true
			rf.Reason = "pyproject.toml declares an entrypoint script"
			rf.Entrypoint = pythonRunCommand(lang.PackageManager)
		default:
			rf.IsApp = false
			rf.Reason = "library: no [project.scripts] / [tool.poetry.scripts]"
		}

	case "rust":
		hasBin, err := cargoHasBinary(modPath)
		switch {
		case err != nil:
			rf.IsApp = false
			rf.Reason = "Cargo.toml unreadable: " + err.Error()
		case hasBin:
			rf.IsApp = true
			rf.Reason = "Cargo.toml has [[bin]] or src/main.rs"
			rf.Entrypoint = "cargo run"
		default:
			rf.IsApp = false
			rf.Reason = "library: only [lib] target"
		}

	default:
		rf.IsApp = false
		rf.Reason = "unsupported language for runnable detection"
	}
	return rf
}

// findGoBinaries walks the module directory looking for `package main`
// declarations. Returns the names of cmd/<name>/ subdirs that contain
// a main package, plus a bool indicating whether the module root
// itself has one. We do not parse Go syntax — a regex on the package
// declaration is enough and avoids dragging go/parser as a dep.
func findGoBinaries(modRoot string) (binaries []string, hasRoot bool) {
	// Module root: scan top-level .go files.
	if hasMainPackageInDir(modRoot) {
		hasRoot = true
	}
	// cmd/<name>/: scan one level down.
	cmdDir := filepath.Join(modRoot, "cmd")
	entries, err := os.ReadDir(cmdDir)
	if err != nil {
		return binaries, hasRoot
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if hasMainPackageInDir(filepath.Join(cmdDir, e.Name())) {
			binaries = append(binaries, e.Name())
		}
	}
	return binaries, hasRoot
}

// hasMainPackageInDir checks whether any *.go file in dir starts (after
// comments / blanks) with `package main`. We bail on the first match
// for speed.
func hasMainPackageInDir(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		// Skip _test.go to avoid false positives from `package main_test`
		// in a few rare projects.
		if strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		if mainPkgRE.Match(raw) {
			return true
		}
	}
	return false
}

// mainPkgRE matches a `package main` declaration anywhere in the
// file body, ignoring leading comments / build tags. Multi-line mode
// so `^` matches at every newline, not just at byte 0.
var mainPkgRE = regexp.MustCompile(`(?m)^package\s+main\b`)

// readNodePackageJSON reads the JSON manifest and classifies the
// package along three axes the suggester needs:
//
//   - hasDev / hasStart: the obvious "is there a launch script"
//     signals.
//   - isWorkspaceRoot: presence of `workspaces:` makes it the
//     orchestrator, not a runnable app.
//   - looksLikeLibrary: presence of `main` / `exports` / `types` /
//     `files` pointing at a build output (`./dist/...`, `./build/`,
//     `./lib/`) marks the package as a publishable library — even
//     when its `dev` script is `tsc --watch` for incremental builds.
//     Without this signal we'd surface every SDK as a service.
//
// Returns isApp = (hasDev || hasStart) && !isWorkspaceRoot &&
// !looksLikeLibrary so the caller has one boolean to act on while
// still seeing the underlying inputs through the dedicated returns.
func readNodePackageJSON(path string) (hasDev, hasStart, isWorkspaceRoot, looksLikeLibrary bool, err error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return false, false, false, false, err
	}
	var doc struct {
		Scripts    map[string]string `json:"scripts"`
		Workspaces any               `json:"workspaces"`
		Private    bool              `json:"private"`
		Main       string            `json:"main"`
		Module     string            `json:"module"`
		Types      string            `json:"types"`
		Exports    any               `json:"exports"`
		Files      []string          `json:"files"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return false, false, false, false, err
	}
	_, hasDev = doc.Scripts["dev"]
	_, hasStart = doc.Scripts["start"]
	isWorkspaceRoot = doc.Workspaces != nil

	// Library heuristic: any of main/module/types pointing at a build
	// output, or `exports` declared (modern lib pattern), or `files`
	// listing build artefacts. Apps usually omit all of these — they
	// don't get published.
	looksLikeLibrary = pointsToBuildOutput(doc.Main) ||
		pointsToBuildOutput(doc.Module) ||
		pointsToBuildOutput(doc.Types) ||
		doc.Exports != nil ||
		filesListsBuildOutput(doc.Files)
	return hasDev, hasStart, isWorkspaceRoot, looksLikeLibrary, nil
}

// pointsToBuildOutput reports whether a package.json `main`/`module`/
// `types` value targets a typical compiled-output directory. Plain
// `index.js` at the package root doesn't count — that's a one-file
// app pattern.
func pointsToBuildOutput(s string) bool {
	if s == "" {
		return false
	}
	s = strings.TrimPrefix(s, "./")
	for _, pfx := range []string{"dist/", "build/", "lib/", "out/"} {
		if strings.HasPrefix(s, pfx) {
			return true
		}
	}
	return false
}

// filesListsBuildOutput inspects a package.json `files:` array for
// build-output directory names. Same intent as pointsToBuildOutput
// but for the array-shape variant.
func filesListsBuildOutput(files []string) bool {
	for _, f := range files {
		f = strings.TrimSuffix(strings.TrimPrefix(f, "./"), "/")
		switch f {
		case "dist", "build", "lib", "out":
			return true
		}
	}
	return false
}

// nodeStartCommand mirrors nodeRunCommand but for `start` instead of
// `dev`. Kept separate so the suggester surfaces the right script
// name in the generated dev.yaml.
func nodeStartCommand(pm, pkgName string) string {
	if pm == "" {
		pm = "npm"
	}
	if pkgName != "" {
		switch pm {
		case "pnpm":
			return "pnpm --filter " + pkgName + " start"
		case "yarn":
			return "yarn workspace " + pkgName + " start"
		}
	}
	return pm + " start"
}

// pyprojectHasScripts reports whether pyproject.toml declares an
// entrypoint script under [project.scripts] or [tool.poetry.scripts].
// We do not parse the TOML — a substring scan is enough for this
// boolean signal and avoids pulling a TOML dep just for one check.
func pyprojectHasScripts(path string) (bool, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	body := string(raw)
	return strings.Contains(body, "[project.scripts]") ||
		strings.Contains(body, "[tool.poetry.scripts]"), nil
}

// cargoHasBinary reports whether the Rust crate produces a binary,
// either via an explicit [[bin]] table in Cargo.toml or via the
// implicit src/main.rs convention.
func cargoHasBinary(modRoot string) (bool, error) {
	if _, err := os.Stat(filepath.Join(modRoot, "src", "main.rs")); err == nil {
		return true, nil
	}
	raw, err := os.ReadFile(filepath.Join(modRoot, "Cargo.toml"))
	if err != nil {
		return false, err
	}
	return strings.Contains(string(raw), "[[bin]]"), nil
}
