package repofacts

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// detectNode handles the Node/TypeScript ecosystem. The trickiest
// part is the package manager — npm, yarn, pnpm, and bun all use
// package.json but differ in their lockfile, which changes the audit
// command. We pick the package manager from the FIRST lockfile we
// encounter at the repo root; nested package.json files inherit it.
//
// Workspaces: pnpm-workspace.yaml or package.json#workspaces both
// declare them. We surface every workspace package as a ModuleFacts
// entry; the LLM uses that to decide whether to emit a scope per
// workspace.
//
// ToolchainVersion comes from package.json's `engines.node` when
// present, or from .nvmrc / .node-version files at the root.
func detectNode(files []string, root string) *LanguageFacts {
	pm := detectNodePackageManager(files, root)
	if pm == "" {
		// No lockfile means we're not confident enough to claim this
		// is a real Node project. A bare package.json with no
		// dependencies is sometimes scaffolding.
		hasPkg := false
		for _, p := range files {
			if filepath.Base(p) == "package.json" && filepath.Dir(p) == root {
				hasPkg = true
				break
			}
		}
		if !hasPkg {
			return nil
		}
		pm = "npm" // best guess; the LLM can override
	}

	var modules []ModuleFacts
	for _, p := range files {
		if filepath.Base(p) != "package.json" {
			continue
		}
		// Skip package.json inside node_modules — already excluded by
		// the walker, but defensive in case the ignore list changes.
		if pathContainsSegment(p, "node_modules") {
			continue
		}
		modules = append(modules, ModuleFacts{
			Path:     rel(root, filepath.Dir(p)),
			Manifest: "package.json",
			Name:     parseNodePackageName(p),
		})
	}
	if len(modules) == 0 {
		return nil
	}

	return &LanguageFacts{
		Lang:                  "node",
		Modules:               modules,
		PackageManager:        pm,
		PackageManagerVersion: detectNodePMVersion(files, root, pm),
		ToolchainVersion:      detectNodeVersion(files, root),
		HasLockfile:           pm != "npm" || hasFile(files, root, "package-lock.json"),
	}
}

// detectNodePackageManager looks at the root for the lockfile that
// pins the toolchain. Order matters: pnpm/yarn/bun all set
// packageManager fields too, but the lockfile is the ground truth.
func detectNodePackageManager(files []string, root string) string {
	switch {
	case hasFile(files, root, "pnpm-lock.yaml"):
		return "pnpm"
	case hasFile(files, root, "yarn.lock"):
		return "yarn"
	case hasFile(files, root, "bun.lock"), hasFile(files, root, "bun.lockb"):
		return "bun"
	case hasFile(files, root, "package-lock.json"):
		return "npm"
	}
	return ""
}

// nodePackageJSON is the minimal subset of package.json we parse.
// Other fields exist but we don't need them.
type nodePackageJSON struct {
	Name    string `json:"name"`
	Engines struct {
		Node string `json:"node"`
		Pnpm string `json:"pnpm"`
	} `json:"engines"`
	// PackageManager is the corepack pin, e.g. "pnpm@8.15.0". The most
	// reliable source for the package-manager version when present.
	PackageManager string `json:"packageManager"`
}

// parseNodePackageName returns the `name` field of a package.json,
// or "" on any parse error.
func parseNodePackageName(path string) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var pkg nodePackageJSON
	if err := json.Unmarshal(raw, &pkg); err != nil {
		return ""
	}
	return pkg.Name
}

// detectNodeVersion checks .nvmrc, .node-version, and engines.node, in
// that order. It looks at the root first, then ANY .nvmrc/.node-version
// in the tree (monorepos pin the version in frontend/.nvmrc, not the
// root) and any package.json's engines.node.
func detectNodeVersion(files []string, root string) string {
	// 1. root pin files (highest priority).
	for _, name := range []string{".nvmrc", ".node-version"} {
		if path := findFileAt(files, root, name); path != "" {
			if v := readTrimmed(path); v != "" {
				return v
			}
		}
	}
	// 2. root package.json engines.node.
	if path := findFileAt(files, root, "package.json"); path != "" {
		if pkg := readPackageJSON(path); pkg != nil && pkg.Engines.Node != "" {
			return pkg.Engines.Node
		}
	}
	// 3. any sub-directory .nvmrc/.node-version (e.g. frontend/.nvmrc).
	for _, p := range files {
		switch filepath.Base(p) {
		case ".nvmrc", ".node-version":
			if v := readTrimmed(p); v != "" {
				return v
			}
		}
	}
	// 4. any sub-directory package.json engines.node.
	for _, p := range files {
		if filepath.Base(p) == "package.json" && !pathContainsSegment(p, "node_modules") {
			if pkg := readPackageJSON(p); pkg != nil && pkg.Engines.Node != "" {
				return pkg.Engines.Node
			}
		}
	}
	return ""
}

// detectNodePMVersion returns the package manager's pinned version from
// the corepack `packageManager` field ("pnpm@8.15.0") or engines.pnpm,
// scanning root then sub-directory package.json files. Empty when none.
func detectNodePMVersion(files []string, root, pm string) string {
	read := func(path string) string {
		pkg := readPackageJSON(path)
		if pkg == nil {
			return ""
		}
		if pkg.PackageManager != "" {
			if name, ver, ok := strings.Cut(pkg.PackageManager, "@"); ok && name == pm {
				// strip a trailing build hash ("8.15.0+sha512...").
				if i := strings.IndexByte(ver, '+'); i >= 0 {
					ver = ver[:i]
				}
				return ver
			}
		}
		if pm == "pnpm" && pkg.Engines.Pnpm != "" {
			return pkg.Engines.Pnpm
		}
		return ""
	}
	if path := findFileAt(files, root, "package.json"); path != "" {
		if v := read(path); v != "" {
			return v
		}
	}
	for _, p := range files {
		if filepath.Base(p) == "package.json" && !pathContainsSegment(p, "node_modules") {
			if v := read(p); v != "" {
				return v
			}
		}
	}
	return ""
}

func readTrimmed(path string) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return trimSpace(string(raw))
}

func readPackageJSON(path string) *nodePackageJSON {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var pkg nodePackageJSON
	if json.Unmarshal(raw, &pkg) != nil {
		return nil
	}
	return &pkg
}
