package repofacts

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// detectPublishableArtifacts orchestrates the per-kind detectors and
// returns a populated PublishableArtifacts. It walks the pre-computed
// file list rather than the filesystem so the cost stays bounded
// even on large monorepos.
//
// Each kind has its own detector to keep the per-stack rules local:
// npm cares about publishConfig + scope, cargo cares about
// [package].publish, helm reads Chart.yaml, etc.
func detectPublishableArtifacts(files []string, root string) PublishableArtifacts {
	var pa PublishableArtifacts

	for _, p := range files {
		base := filepath.Base(p)
		switch base {
		case "package.json":
			if pathContainsSegment(p, "node_modules") {
				continue
			}
			if pkg, ok := parseNpmPackage(p, root); ok {
				pa.NpmPackages = append(pa.NpmPackages, pkg)
			}
		case "Cargo.toml":
			if crate, ok := parseCargoCrate(p, root); ok {
				pa.CargoCrates = append(pa.CargoCrates, crate)
			}
		case "pyproject.toml":
			if pkg, ok := parsePythonPyproject(p, root); ok {
				pa.PythonPackages = append(pa.PythonPackages, pkg)
			}
		case "Chart.yaml":
			if chart, ok := parseHelmChart(p, root); ok {
				pa.HelmCharts = append(pa.HelmCharts, chart)
			}
		case "go.mod":
			if mod, ok := parseGoSDKModule(p, root); ok {
				pa.GoModules = append(pa.GoModules, mod)
			}
		}
	}

	// Deterministic order for cache-friendly JSON output.
	sort.Slice(pa.NpmPackages, func(i, j int) bool { return pa.NpmPackages[i].Path < pa.NpmPackages[j].Path })
	sort.Slice(pa.CargoCrates, func(i, j int) bool { return pa.CargoCrates[i].Path < pa.CargoCrates[j].Path })
	sort.Slice(pa.PythonPackages, func(i, j int) bool { return pa.PythonPackages[i].Path < pa.PythonPackages[j].Path })
	sort.Slice(pa.HelmCharts, func(i, j int) bool { return pa.HelmCharts[i].Path < pa.HelmCharts[j].Path })
	sort.Slice(pa.GoModules, func(i, j int) bool { return pa.GoModules[i].Path < pa.GoModules[j].Path })

	return pa
}

// ── npm ──────────────────────────────────────────────────────

// npmPackageJSON is the minimal subset of package.json we read. We
// keep it small on purpose: parsing more fields means more risk of
// flagging false positives when a package has weird config.
type npmPackageJSON struct {
	Name          string `json:"name"`
	Version       string `json:"version"`
	Private       bool   `json:"private"`
	PublishConfig struct {
		Registry string `json:"registry"`
		Access   string `json:"access"`
	} `json:"publishConfig"`
}

// parseNpmPackage reads a package.json and emits one NpmPackage
// entry. Returns ok=false when the file is unreadable or has no
// `name` (the latter means it's a workspace-root manifest with no
// publishable artifact of its own — in that case the workspace
// members each have their own package.json which we'll catch).
func parseNpmPackage(path, root string) (NpmPackage, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return NpmPackage{}, false
	}
	var pkg npmPackageJSON
	if err := json.Unmarshal(raw, &pkg); err != nil {
		return NpmPackage{}, false
	}
	if strings.TrimSpace(pkg.Name) == "" {
		return NpmPackage{}, false
	}
	return NpmPackage{
		Path:     rel(root, filepath.Dir(path)),
		Name:     pkg.Name,
		Version:  strings.TrimSpace(pkg.Version),
		Private:  pkg.Private,
		Registry: resolveNpmRegistry(pkg, filepath.Dir(path)),
	}, true
}

// resolveNpmRegistry picks the most authoritative registry signal.
// Order: publishConfig.registry on the package → .npmrc in the same
// dir (registry= or @scope:registry= line) → empty (callers default
// to npmjs.org).
func resolveNpmRegistry(pkg npmPackageJSON, dir string) string {
	if pkg.PublishConfig.Registry != "" {
		return strings.TrimRight(pkg.PublishConfig.Registry, "/")
	}
	// .npmrc in the same dir? Look for both unscoped and scoped lines.
	npmrcPath := filepath.Join(dir, ".npmrc")
	raw, err := os.ReadFile(npmrcPath)
	if err != nil {
		return ""
	}
	scope := ""
	if i := strings.Index(pkg.Name, "/"); i > 0 && strings.HasPrefix(pkg.Name, "@") {
		scope = pkg.Name[:i] // "@acme"
	}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if scope != "" && strings.HasPrefix(line, scope+":registry=") {
			return strings.TrimRight(strings.TrimPrefix(line, scope+":registry="), "/")
		}
		if strings.HasPrefix(line, "registry=") {
			return strings.TrimRight(strings.TrimPrefix(line, "registry="), "/")
		}
	}
	return ""
}

// ── cargo ────────────────────────────────────────────────────

var (
	cargoPackageBlockRe = regexp.MustCompile(`(?ms)^\s*\[package\]\s*$(.*?)^\s*\[`)
	cargoVersionRe      = regexp.MustCompile(`(?m)^\s*version\s*=\s*["']([^"']+)["']`)
	cargoPublishFalseRe = regexp.MustCompile(`(?m)^\s*publish\s*=\s*false\s*$`)
	cargoPublishListRe  = regexp.MustCompile(`(?m)^\s*publish\s*=\s*\[\s*["']([^"']+)["']`)
)

// parseCargoCrate reads a Cargo.toml. Workspace virtual manifests
// (root files with only [workspace] and no [package]) return
// ok=false — they are not themselves publishable. Member crates
// listed under workspace.members each have their own Cargo.toml and
// will be picked up by the walk.
func parseCargoCrate(path, root string) (CargoCrate, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return CargoCrate{}, false
	}
	// Anchor on [package]. Use a trailing sentinel so the regex can
	// match a [package] section that runs to EOF.
	hay := append([]byte{}, raw...)
	hay = append(hay, []byte("\n[__sentinel__]\n")...)
	m := cargoPackageBlockRe.FindSubmatch(hay)
	if m == nil {
		return CargoCrate{}, false // virtual workspace manifest
	}
	section := string(m[1])

	name := rustNameRe.FindStringSubmatch(section)
	if name == nil {
		return CargoCrate{}, false
	}
	version := ""
	if v := cargoVersionRe.FindStringSubmatch(section); v != nil {
		version = v[1]
	}
	publish := true
	registry := ""
	if cargoPublishFalseRe.FindString(section) != "" {
		publish = false
	}
	if reg := cargoPublishListRe.FindStringSubmatch(section); reg != nil {
		registry = reg[1]
	}
	return CargoCrate{
		Path:     rel(root, filepath.Dir(path)),
		Name:     name[1],
		Version:  version,
		Publish:  publish,
		Registry: registry,
	}, true
}

// ── python ───────────────────────────────────────────────────

var (
	pyProjectSectionRe = regexp.MustCompile(`(?ms)^\s*\[project\]\s*$(.*?)^\s*\[`)
	pyPoetrySectionRe  = regexp.MustCompile(`(?ms)^\s*\[tool\.poetry\]\s*$(.*?)^\s*\[`)
	pyBuildSystemRe    = regexp.MustCompile(`(?ms)^\s*\[build-system\]\s*$(.*?)^\s*\[`)
	pyFieldRe          = regexp.MustCompile(`(?m)^\s*([a-z_-]+)\s*=\s*["']([^"']+)["']`)
)

// parsePythonPyproject reads pyproject.toml looking for [project] or
// [tool.poetry]. setup.py / setup.cfg projects are NOT detected here
// (they exist but the format is too varied to parse reliably) — we
// surface them via the existing python language detector and let the
// LLM ask the user if it needs more.
func parsePythonPyproject(path, root string) (PythonPackage, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return PythonPackage{}, false
	}
	hay := append([]byte{}, raw...)
	hay = append(hay, []byte("\n[__sentinel__]\n")...)

	name, version := "", ""
	if m := pyProjectSectionRe.FindSubmatch(hay); m != nil {
		section := string(m[1])
		for _, f := range pyFieldRe.FindAllStringSubmatch(section, -1) {
			switch f[1] {
			case "name":
				name = f[2]
			case "version":
				version = f[2]
			}
		}
	}
	// Fallback to [tool.poetry] when PEP 621 [project] is absent.
	if name == "" {
		if m := pyPoetrySectionRe.FindSubmatch(hay); m != nil {
			section := string(m[1])
			for _, f := range pyFieldRe.FindAllStringSubmatch(section, -1) {
				switch f[1] {
				case "name":
					name = f[2]
				case "version":
					version = f[2]
				}
			}
		}
	}
	if name == "" {
		return PythonPackage{}, false
	}

	// Build backend from [build-system].build-backend.
	backend := ""
	if m := pyBuildSystemRe.FindSubmatch(hay); m != nil {
		section := string(m[1])
		for _, f := range pyFieldRe.FindAllStringSubmatch(section, -1) {
			if f[1] == "build-backend" {
				backend = strings.ToLower(f[2])
				break
			}
		}
	}

	return PythonPackage{
		Path:    rel(root, filepath.Dir(path)),
		Name:    name,
		Version: version,
		Backend: backend,
		// Registry resolution is intentionally out of scope for v1.
		// Poetry / uv configure it in a separate [[tool.poetry.source]]
		// or [[tool.uv.index]] block which is harder to parse without
		// a real TOML parser. The LLM will pick PyPI by default.
	}, true
}

// ── helm ─────────────────────────────────────────────────────

var (
	helmNameRe       = regexp.MustCompile(`(?m)^name:\s*(\S+)\s*$`)
	helmVersionRe    = regexp.MustCompile(`(?m)^version:\s*(\S+)\s*$`)
	helmAppVersionRe = regexp.MustCompile(`(?m)^appVersion:\s*['"]?([^'"\s]+)['"]?\s*$`)
)

// parseHelmChart reads a Chart.yaml. We pick name + version +
// appVersion via line-anchored regex because the file is small
// (~10 lines typically) and Helm guarantees those fields appear at
// the top level.
func parseHelmChart(path, root string) (HelmChart, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return HelmChart{}, false
	}
	nameMatch := helmNameRe.FindSubmatch(raw)
	if nameMatch == nil {
		return HelmChart{}, false
	}
	chart := HelmChart{
		Path: rel(root, filepath.Dir(path)),
		Name: string(nameMatch[1]),
	}
	if m := helmVersionRe.FindSubmatch(raw); m != nil {
		chart.Version = string(m[1])
	}
	if m := helmAppVersionRe.FindSubmatch(raw); m != nil {
		chart.AppVersion = string(m[1])
	}
	return chart, true
}

// ── go SDK ──────────────────────────────────────────────────

// parseGoSDKModule reuses the go.mod parser from lang_go.go. We
// surface the module only when it looks like a publishable SDK:
//
//   - module path on a known public VCS host
//   - dir basename hints at SDK / client / api (rules of thumb,
//     avoids surfacing every apps/<svc>/go.mod as "publishable")
//
// The result drives a NOTE in the LLM prompt about git-tag-based
// publishing — there is no automated publish command to emit for Go.
func parseGoSDKModule(path, root string) (GoSDKModule, bool) {
	module := parseGoModule(path)
	if module == "" {
		return GoSDKModule{}, false
	}
	dir := filepath.Dir(path)
	base := strings.ToLower(filepath.Base(dir))

	sdkLike := strings.Contains(base, "sdk") ||
		strings.Contains(base, "client") ||
		strings.HasSuffix(base, "-api")
	if !sdkLike {
		return GoSDKModule{}, false
	}

	public := false
	for _, host := range []string{"github.com/", "gitlab.com/", "bitbucket.org/", "golang.org/", "k8s.io/"} {
		if strings.HasPrefix(module, host) {
			public = true
			break
		}
	}
	// Internal-marker overrides public host inference. A
	// github.com/foo/bar/internal/x module is not publishable.
	if strings.Contains(module, "/internal/") {
		public = false
	}

	return GoSDKModule{
		Path:   rel(root, dir),
		Module: module,
		Public: public,
	}, true
}
