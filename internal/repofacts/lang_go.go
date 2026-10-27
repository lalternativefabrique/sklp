package repofacts

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// detectGo finds every go.mod under the repo and returns one
// ModuleFacts per file. The Go module convention says a go.mod can
// live at any depth and is the canonical module boundary — we honour
// that strictly. apps/core/go.mod and apps/sdk/go.mod produce two
// independent modules; the LLM emits one govulncheck preflight per
// module.
//
// ToolchainVersion comes from the `go 1.25.0` directive at the top of
// the first go.mod we encounter (modules in the same workspace
// usually share the toolchain pin).
//
// HasLockfile is true when at least one go.sum exists; the audit-
// preflight rule "go.mod → govulncheck" is gated by this in the
// caller's prompt, not here. The scanner stays factual.
func detectGo(files []string, root string) *LanguageFacts {
	var (
		modules []ModuleFacts
		version string
		lock    bool
	)
	for _, p := range files {
		base := filepath.Base(p)
		switch base {
		case "go.mod":
			m := ModuleFacts{
				Path:     rel(root, filepath.Dir(p)),
				Manifest: "go.mod",
				Name:     parseGoModule(p),
			}
			if version == "" {
				version = parseGoVersion(p)
			}
			modules = append(modules, m)
		case "go.sum":
			lock = true
		}
	}
	if len(modules) == 0 {
		return nil
	}
	return &LanguageFacts{
		Lang:             "go",
		Modules:          modules,
		ToolchainVersion: version,
		HasLockfile:      lock,
		// Go has only one canonical tool (go itself); no need to set
		// PackageManager.
	}
}

// goModuleLineRe matches the `module <name>` directive in a go.mod.
// We use a regex (not a full parser) because the format is stable
// and the cost of pulling in golang.org/x/mod just to read one line
// is not justified — this is a scanner, not a build tool.
var goModuleLineRe = regexp.MustCompile(`(?m)^module\s+(\S+)\s*$`)

// goVersionLineRe matches the `go <version>` directive. We capture
// the bare version (1.25.0) so the caller can pass it through to
// `tools.go` without inventing digits.
var goVersionLineRe = regexp.MustCompile(`(?m)^go\s+([0-9][0-9.]+)\s*$`)

// parseGoModule extracts the `module` directive from a go.mod file.
// Returns "" on any error — the caller treats that as "no name
// declared", which is rare but legal for v0 modules.
func parseGoModule(path string) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	if m := goModuleLineRe.FindSubmatch(raw); m != nil {
		return strings.TrimSpace(string(m[1]))
	}
	return ""
}

// parseGoVersion extracts the `go <version>` directive from a go.mod.
// Same error policy as parseGoModule.
func parseGoVersion(path string) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	if m := goVersionLineRe.FindSubmatch(raw); m != nil {
		return strings.TrimSpace(string(m[1]))
	}
	return ""
}
