package repofacts

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// detectRust returns a LanguageFacts for every Cargo.toml found.
// Workspaces are flattened: the root Cargo.toml + each member crate
// produce separate ModuleFacts entries. The audit/preflight rule
// (`Cargo.lock` exists → cargo audit) keys off HasLockfile.
//
// ToolchainVersion comes from rust-toolchain or rust-toolchain.toml
// at the root, which most workspaces use to pin a stable channel.
func detectRust(files []string, root string) *LanguageFacts {
	var modules []ModuleFacts
	lock := false
	for _, p := range files {
		switch filepath.Base(p) {
		case "Cargo.toml":
			modules = append(modules, ModuleFacts{
				Path:     rel(root, filepath.Dir(p)),
				Manifest: "Cargo.toml",
				Name:     parseRustCrateName(p),
			})
		case "Cargo.lock":
			lock = true
		}
	}
	if len(modules) == 0 {
		return nil
	}
	return &LanguageFacts{
		Lang:             "rust",
		Modules:          modules,
		PackageManager:   "cargo",
		ToolchainVersion: detectRustToolchain(files, root),
		HasLockfile:      lock,
	}
}

// rustNameRe captures the [package].name line. We do not parse the
// whole TOML to keep the scanner dependency-free; the regex is
// sufficient for cargo's stable single-quoted / double-quoted forms.
var rustNameRe = regexp.MustCompile(`(?m)^\s*name\s*=\s*["']([^"']+)["']`)

func parseRustCrateName(path string) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	if m := rustNameRe.FindSubmatch(raw); m != nil {
		return strings.TrimSpace(string(m[1]))
	}
	return ""
}

// detectRustToolchain reads rust-toolchain (legacy plain text) or
// rust-toolchain.toml (newer structured format) and returns the
// channel string verbatim. We don't try to canonicalise "stable"
// vs "1.78.0" — the LLM will pass through whatever the user pinned.
func detectRustToolchain(files []string, root string) string {
	if path := findFileAt(files, root, "rust-toolchain.toml"); path != "" {
		raw, err := os.ReadFile(path)
		if err == nil {
			// Look for `channel = "..."` — the only field we care about.
			re := regexp.MustCompile(`(?m)^\s*channel\s*=\s*["']([^"']+)["']`)
			if m := re.FindSubmatch(raw); m != nil {
				return string(m[1])
			}
		}
	}
	if path := findFileAt(files, root, "rust-toolchain"); path != "" {
		raw, err := os.ReadFile(path)
		if err == nil {
			return trimSpace(string(raw))
		}
	}
	return ""
}
