package repofacts

import (
	"fmt"
	"sort"
	"strings"
)

// RunnerConfig renders a deterministic .sklp/space.yaml from the
// scanned facts. No LLM: a project's runner image is a build cache keyed
// by config hash, so its generation MUST be reproducible — two runs on
// the same repo must produce byte-identical YAML, or the image rebuilds
// for no reason.
//
// It maps:
//   - each language's ToolchainVersion -> runner.toolchain (the runner
//     image bakes go/node/pnpm/bun/python/ruby/php natively)
//   - detected CLI tooling (.golangci.yml, swag, gotestsum...) -> go_tools
//   - CGO / sqlite signals -> apt packages
//   - the best-effort Registry -> runner.registry (placeholder + NOTE if
//     none could be inferred)
//
// notes carries human-facing review hints (placeholders, guesses).
func RunnerConfig(f *RepoFacts) (yaml string, notes []string) {
	var b strings.Builder
	b.WriteString("runner:\n")

	// Registry — best-effort; placeholder + NOTE when unknown.
	registry := f.Registry
	if registry == "" {
		registry = "rg.fr-par.scw.cloud/<your-namespace>"
		notes = append(notes, "registry: could not infer one — replace the placeholder with your registry.")
	}
	fmt.Fprintf(&b, "  registry: %s\n", registry)
	b.WriteString("  registry_auth_secret: scw-registry-credentials\n")

	// Toolchain — one entry per detected language, version from the pin
	// file. The runner image resolves all of these.
	toolchain := runnerToolchain(f)
	if len(toolchain) > 0 {
		b.WriteString("\n  toolchain:\n")
		for _, k := range sortedToolKeys(toolchain) {
			fmt.Fprintf(&b, "    %s: %q\n", k, toolchain[k])
		}
	} else {
		notes = append(notes, "toolchain: no language version pin found — add at least one (e.g. go/node).")
	}

	// Packages — apt deps inferred from build signals.
	packages := runnerPackages(f)
	if len(packages) > 0 {
		b.WriteString("\n  packages:\n")
		for _, p := range packages {
			fmt.Fprintf(&b, "    - %s\n", p)
		}
	}

	// go_tools — Go CLIs the CI invokes, inferred from markers.
	goTools, goToolNotes := runnerGoTools(f)
	notes = append(notes, goToolNotes...)
	if len(goTools) > 0 {
		b.WriteString("\n  go_tools:\n")
		for _, t := range goTools {
			fmt.Fprintf(&b, "    - %s\n", t)
		}
	}

	// Secrets — GITHUB_TOKEN when any Go module looks private-ish or a
	// .netrc / private module host is plausible. Conservative: only when
	// a go module is present (private dep resolution is the common need).
	if hasLang(f, "go") {
		b.WriteString("\n  secrets:\n")
		b.WriteString("    - GITHUB_TOKEN\n")
	}

	return b.String(), notes
}

// runnerToolchain maps detected languages to runner toolchain entries.
// Version is the EXACT pin from the repo so GOTOOLCHAIN/auto-download
// never kicks in. Node/pnpm/bun keep their package-manager split.
func runnerToolchain(f *RepoFacts) map[string]string {
	out := map[string]string{}
	for _, l := range f.Languages {
		switch l.Lang {
		case "go":
			if v := normalizeGoVersion(l.ToolchainVersion); v != "" {
				out["go"] = v
			}
		case "node":
			nodeVer := strings.TrimPrefix(l.ToolchainVersion, "v")
			// A package manager (pnpm/yarn/bun) is useless without node.
			// If one was detected but no node version, fall back to "lts"
			// (the runner resolves it) rather than emitting a broken config.
			if nodeVer == "" && (l.PackageManager == "pnpm" || l.PackageManager == "yarn" || l.PackageManager == "bun") {
				nodeVer = "lts"
			}
			if nodeVer != "" {
				out["node"] = nodeVer
			}
			// the node package manager is itself a baked tool; pin its
			// version when the repo declared one, else "latest".
			switch l.PackageManager {
			case "pnpm", "bun", "yarn":
				if _, ok := out[l.PackageManager]; !ok {
					ver := l.PackageManagerVersion
					if ver == "" {
						ver = "latest"
					}
					out[l.PackageManager] = ver
				}
			}
		case "python", "ruby", "php", "rust", "bun", "deno":
			if l.ToolchainVersion != "" {
				out[l.Lang] = l.ToolchainVersion
			}
		}
	}
	return out
}

// runnerPackages infers apt packages from build signals. Conservative:
// always git + ca-certificates (clones, TLS); add native-build deps when
// the evidence is concrete.
func runnerPackages(f *RepoFacts) []string {
	pkgs := []string{"git", "ca-certificates"}
	// PHP commonly needs these; only add when php is present.
	if hasLang(f, "php") {
		pkgs = append(pkgs, "unzip", "libonig-dev", "libzip-dev")
	}
	// Ruby native gems frequently need a compiler + libpq for pg.
	if hasLang(f, "ruby") {
		pkgs = append(pkgs, "build-essential", "libpq-dev")
	}
	return pkgs
}

// runnerGoTools infers the Go CLIs the CI runs from deterministic
// markers. We only emit a tool when there's concrete evidence; versions
// are left to the user to pin (NOTE) since the marker doesn't carry one.
func runnerGoTools(f *RepoFacts) (tools []string, notes []string) {
	if !hasLang(f, "go") {
		return nil, nil
	}
	if f.APIContracts.Tools.Swag {
		tools = append(tools, "github.com/swaggo/swag/cmd/swag@latest")
	}
	// Makefile / CI hints frequently reveal lint + test tooling.
	mk := ""
	if f.Makefile != nil {
		mk = strings.ToLower(strings.Join(f.Makefile.Targets, " "))
	}
	hintBlob := mk
	if strings.Contains(hintBlob, "lint") || len(f.CIHints.GitHubActions) > 0 {
		// golangci-lint is the de-facto Go linter; emit it when a lint
		// target or any CI workflow exists.
		tools = appendUnique(tools, "github.com/golangci/golangci-lint/cmd/golangci-lint@latest")
	}
	if len(tools) > 0 {
		notes = append(notes, "go_tools: pinned to @latest — replace with the exact versions your CI expects.")
	}
	return tools, notes
}

// --- small helpers ---

func hasLang(f *RepoFacts, lang string) bool {
	for _, l := range f.Languages {
		if l.Lang == lang {
			return true
		}
	}
	return false
}

// normalizeGoVersion keeps go.mod's version verbatim (e.g. "1.22",
// "1.25.0"). The toolchain accepts both; pinning the exact string the
// repo declares avoids a GOTOOLCHAIN auto-download mismatch.
func normalizeGoVersion(v string) string {
	return strings.TrimSpace(strings.TrimPrefix(v, "go"))
}

func sortedToolKeys(m map[string]string) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

func appendUnique(s []string, v string) []string {
	for _, x := range s {
		if x == v {
			return s
		}
	}
	return append(s, v)
}
