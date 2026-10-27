package repofacts

import (
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// SuggestedService is the rule-based skeleton emitted by
// SuggestDevServices. It mirrors the fields the devstack package
// expects in `.sklp/dev.yaml` so DevConfig's renderer stays a
// one-to-one translation. Keeping this shape decoupled from
// runner.Service avoids a cyclic import (repofacts → runner → ...).
type SuggestedService struct {
	Name      string          `yaml:"-"`
	Run       string          `yaml:"run"`
	Workdir   string          `yaml:"workdir,omitempty"`
	DependsOn []string        `yaml:"depends_on,omitempty"`
	Optional  bool            `yaml:"optional,omitempty"`
	Ready     *SuggestedReady `yaml:"ready,omitempty"`
	Notes     []string        `yaml:"-"` // surfaced as YAML comments by the caller
}

// SuggestedReady is the readiness probe shape the suggester produces.
// Only one of HTTP/TCP is set today — the rule-based heuristics don't
// guess File/Log probes (they're highly project-specific).
type SuggestedReady struct {
	HTTP    string `yaml:"http,omitempty"`
	TCP     string `yaml:"tcp,omitempty"`
	Timeout string `yaml:"timeout,omitempty"`
}

// SuggestDevServices turns the runnable-enriched module list into a
// deterministic skeleton for `.sklp/dev.yaml`. Output rules:
//
//   - One service per RunnableFacts entry where IsApp is true. Lib
//     modules (SDKs, shared types, workspace orchestrators, Tauri
//     plugins) are dropped — they would have produced dead services
//     the user has to delete by hand.
//   - compose.yml / docker-compose.yml are intentionally ignored: infra
//     (postgres, nats, …) is modelled as native image: services, not
//     docker compose wrappers. The sklp devstack runs everything inside
//     runc spaces via skt — no docker daemon needed.
//   - Web app depends on backend by convention, when both exist.
//
// Anything the rule-based pass can't infer (port, ready endpoint
// path) is left blank or guessed conservatively; a future --ai
// enrichment pass fills the gaps.
func SuggestDevServices(runnables []RunnableFacts) []SuggestedService {
	var services []SuggestedService
	seenNames := map[string]bool{}
	addUnique := func(s SuggestedService) {
		base := s.Name
		for i := 2; seenNames[s.Name]; i++ {
			s.Name = base + "-" + itoa(i)
		}
		seenNames[s.Name] = true
		services = append(services, s)
	}

	// Stable order: language asc, then module path asc. Keeps the
	// generated YAML stable across runs.
	sorted := append([]RunnableFacts{}, runnables...)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].Lang != sorted[j].Lang {
			return sorted[i].Lang < sorted[j].Lang
		}
		return sorted[i].Module.Path < sorted[j].Module.Path
	})

	for _, r := range sorted {
		if !r.IsApp {
			continue
		}
		if svc, ok := suggestForRunnable(r); ok {
			addUnique(svc)
		}
	}

	// Wire web → api convention. Multiple "api"-like or "web"-like
	// services would need user input; we only auto-link when there's
	// exactly one of each.
	linkWebToApi(services)

	return services
}

// SuggestInfraServices scans the repo root for non-Docker local-infra
// conventions (devbox, Procfile, Tiltfile, ...) and emits one
// optional service per convention found. Each convention gets a
// distinct name so a user with multiple coexisting setups (e.g. a
// compose file AND a devbox.json mid-migration) can target one
// specifically with a future `--with <name>` flag.
func SuggestInfraServices(repo string) []SuggestedService {
	var out []SuggestedService

	// devbox: native services via Nix, no daemon required. The
	// `devbox services up` command starts every service declared in
	// devbox.json's `init_hook` / `packages` section.
	if fileExists(filepath.Join(repo, "devbox.json")) {
		out = append(out, SuggestedService{
			Name: "devbox-infra",
			Run:  "devbox services up",

			Notes: []string{"detected devbox.json — native services via Nix (no Docker daemon)"},
		})
	}

	// Procfile: the Heroku-style format that overmind/foreman/hivemind
	// consume. We only honour the production-shaped Procfile (NOT
	// Procfile.dev) since the latter is typically already what you'd
	// be replacing with sklp dev itself.
	if fileExists(filepath.Join(repo, "Procfile")) {
		// Pick the first available runner found on PATH so the
		// generated command works without manual install. Falls back
		// to overmind (the most common modern choice) when none is
		// present — the user can edit if they prefer foreman.
		runner := pickProcfileRunner()
		out = append(out, SuggestedService{
			Name: "procfile-infra",
			Run:  runner + " start -f Procfile",

			Notes: []string{"detected Procfile — runs via " + runner + " (install with `brew install " + runner + "`)"},
		})
	}

	// Tiltfile: Tilt is k8s-oriented but lots of teams use it for
	// pure Docker dev too. We surface it but keep it optional.
	if fileExists(filepath.Join(repo, "Tiltfile")) {
		out = append(out, SuggestedService{
			Name: "tilt-infra",
			Run:  "tilt up",

			Notes: []string{"detected Tiltfile — opt-in via `sklp dev --with tilt-infra`"},
		})
	}

	return out
}

// fileExists is a tiny helper so the suggester doesn't import os
// across every detection branch. The cost of an extra Stat per
// convention is negligible.
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// pickProcfileRunner returns the first Procfile-compatible launcher
// found on PATH. Order reflects current popularity: overmind (modern,
// tmux-based), then hivemind (overmind without tmux), then foreman
// (the original Ruby one). When none is installed we still emit
// overmind as the run command — the user gets a clear error pointing
// at the install.
func pickProcfileRunner() string {
	for _, name := range []string{"overmind", "hivemind", "foreman"} {
		if _, err := exec.LookPath(name); err == nil {
			return name
		}
	}
	return "overmind"
}

// suggestForRunnable maps an enriched RunnableFacts entry to a
// SuggestedService. The IsApp filter is assumed to have run before
// this function is called; we only worry about translating the
// per-language Entrypoint into a fully-shaped service.
func suggestForRunnable(r RunnableFacts) (SuggestedService, bool) {
	fallback := "app"
	switch r.Lang {
	case "go", "python":
		fallback = "api"
	case "node":
		fallback = "web"
	}
	name := svcNameFromPath(r.Module.Path, fallback)
	svc := SuggestedService{
		Name:    name,
		Workdir: r.Module.Path,
		Run:     r.Entrypoint,
	}
	// Backends get a guessed ready probe; frontends don't (Vite/Next
	// expose a port but no /health route by default).
	if r.Lang == "go" || r.Lang == "python" {
		svc.Ready = &SuggestedReady{
			HTTP:    "http://localhost:8080/health",
			Timeout: "60s",
		}
		svc.Notes = []string{"port + ready endpoint guessed — verify against your code"}
	}
	if r.Reason != "" {
		svc.Notes = append(svc.Notes, "detected: "+r.Reason)
	}
	return svc, true
}

// svcNameFromPath turns a module path into a service name. The
// convention is to use the leaf directory name; falls back to a
// language-appropriate default for root-level modules.
func svcNameFromPath(path, fallback string) string {
	if path == "" || path == "." {
		return fallback
	}
	parts := strings.Split(strings.Trim(path, "/"), "/")
	leaf := parts[len(parts)-1]
	if leaf == "" {
		return fallback
	}
	return leaf
}

// nodeRunCommand picks the right `<pm> run dev` invocation. For
// workspaced monorepos with a package name, we use --filter so the
// command works from the repo root regardless of the cwd setting.
func nodeRunCommand(pm, pkgName string) string {
	if pm == "" {
		pm = "npm"
	}
	if pkgName != "" {
		switch pm {
		case "pnpm":
			return "pnpm --filter " + pkgName + " dev"
		case "yarn":
			return "yarn workspace " + pkgName + " dev"
		}
	}
	switch pm {
	case "npm":
		return "npm run dev"
	case "pnpm":
		return "pnpm dev"
	case "yarn":
		return "yarn dev"
	case "bun":
		return "bun dev"
	}
	return pm + " run dev"
}

// pythonRunCommand picks the entrypoint for the detected Python
// package manager. We bias toward `uvicorn` because that's the most
// common modern stack; the user can swap in `python manage.py
// runserver` etc. for Django-style apps.
func pythonRunCommand(pm string) string {
	switch pm {
	case "uv":
		return "uv run uvicorn app.main:app --reload"
	case "poetry":
		return "poetry run uvicorn app.main:app --reload"
	default:
		return "python -m uvicorn app.main:app --reload"
	}
}

// linkWebToApi adds `depends_on: [<api>]` to the unique web-like
// service when there is exactly one api-like service in the slice.
// The convention is loose by design: user-defined names that don't
// match this heuristic just stay independent.
func linkWebToApi(services []SuggestedService) {
	apiIdx, apiCount := -1, 0
	webIdx, webCount := -1, 0
	for i, s := range services {
		switch {
		case isAPILike(s.Name):
			apiIdx, apiCount = i, apiCount+1
		case isWebLike(s.Name):
			webIdx, webCount = i, webCount+1
		}
	}
	if apiCount == 1 && webCount == 1 && apiIdx != webIdx {
		services[webIdx].DependsOn = append(services[webIdx].DependsOn, services[apiIdx].Name)
	}
}

func isAPILike(name string) bool {
	switch strings.ToLower(name) {
	case "api", "core", "backend", "server":
		return true
	}
	return false
}

func isWebLike(name string) bool {
	switch strings.ToLower(name) {
	case "web", "frontend", "ui", "client", "app":
		return true
	}
	return false
}

// itoa is a no-import int-to-string for the rare suffix path. Avoids
// pulling strconv just for one call.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
