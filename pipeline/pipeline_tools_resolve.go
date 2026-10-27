package pipeline

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/lalternativefabrique/sklp/internal/toolchain"
)

// toolResolver memoises tool installations across a single pipeline
// run. The first request for a given tool name triggers either a host
// lookup (preferred) or a native registry install; every subsequent
// request returns the cached `bin dir` without touching the network.
//
// Lazy by construction: the executor only calls Get() right before a
// step that needs that tool is about to run. Steps gated out by their
// scope never trigger resolution, so a broken pnpm release no longer
// blocks a backend-only PR.
//
// Dependency install: after resolving a package-manager tool (pnpm,
// npm, yarn, bun, go) for the first time, the resolver runs the
// canonical install command if the matching manifest exists in
// repoPath. CI runs start with an empty workspace, so this is a
// universal "before any of these tools is useful, hydrate the deps"
// hook — memoised so repeated steps using the same tool don't pay
// the install cost twice.
//
// Concurrency: Get() is called by the scheduler from multiple
// goroutines (steps run in parallel). Per-tool serialisation is
// provided by a once-per-name latch (resolveOnce / depsOnce) so a slow
// install for tool A does not block resolution of tool B, and two
// concurrent requests for the same tool share the work instead of
// racing on the memo maps.
type toolResolver struct {
	versions map[string]string // tool name → pinned version (from Pipeline.Tools)
	cacheDir string
	repoPath string
	log      io.Writer

	// mu guards resolved, deps, resolveOnce/Err and depsOnce/Err.
	// Critical sections are tiny — map reads/writes plus the *sync.Once
	// lookup; the actual resolve/install runs OUTSIDE the lock via the
	// Once so concurrent requests for different tools never serialise.
	mu sync.Mutex
	// resolved memoises name → binary directory so PATH composition is
	// cheap and deterministic. Stored across multiple Get() calls in the
	// same run, dropped when the resolver goes out of scope.
	resolved map[string]string
	// deps memoises name → already-ran-install, so two steps using the
	// same tool share the install cost.
	deps map[string]bool
	// resolveOnce / depsOnce serialise the slow path per tool name —
	// concurrent Get() calls for the same name share one install /
	// resolve attempt, and the recorded error is replayed to losers.
	resolveOnce map[string]*sync.Once
	resolveErr  map[string]error
	depsOnce    map[string]*sync.Once
	depsErr     map[string]error

	// lastFail stores the most recent auto-install failure context so
	// the reporter can surface the underlying tool stderr in the
	// FAILURES section. The wrapped error returned by Get() only carries
	// the wrapper's "exit status N", which on its own is not actionable.
	lastFailMu sync.Mutex
	lastFail   *autoInstallFailure
}

// autoInstallFailure captures the context needed to render an
// actionable FAILURES entry when an auto-install step (pnpm install,
// go mod download, …) exits non-zero. The Tail is the last few lines
// of the tool's combined stdout+stderr — pnpm / npm / go all print
// their precise error message there, so showing it inline saves the
// user from having to open the log file.
type autoInstallFailure struct {
	Tool    string
	Cmdline string
	Tail    string // already newline-joined, no trailing newline
}

// newToolResolver builds a resolver bound to the pipeline's declared
// tools and the runner's cache directory. The returned resolver is safe
// for concurrent Get() calls from multiple goroutines.
func newToolResolver(versions map[string]string, cacheDir, repoPath string, log io.Writer) *toolResolver {
	return &toolResolver{
		versions:    versions,
		cacheDir:    cacheDir,
		repoPath:    repoPath,
		log:         log,
		resolved:    make(map[string]string),
		deps:        make(map[string]bool),
		resolveOnce: make(map[string]*sync.Once),
		resolveErr:  make(map[string]error),
		depsOnce:    make(map[string]*sync.Once),
		depsErr:     make(map[string]error),
	}
}

// Get resolves a single tool and returns the directory that should be
// prepended to PATH so a child shell sees the right binary.
//
// Resolution order, first hit wins:
//  1. Memoised result from a previous Get() in this run.
//  2. Host binary on PATH whose --version matches the pin on
//     (major, minor, patch>=want).
//  3. Native registry install (downloads + verifies sha256 + extracts).
//  4. skt for tools outside the native registry.
//
// An unknown tool name (absent from the effective toolchain — neither
// pipeline `tools:` nor config `runner.toolchain`) is a hard error:
// silent typos in a step's `tools:` list would otherwise let the step
// run with the host's whatever-version of the tool, which is exactly
// the surprise the host-preference path is trying to avoid.
//
// After the binary is resolved, Get runs the per-tool dependency
// install (once per run) so subsequent steps see populated
// node_modules / go module cache without each pipeline author having
// to remember a separate install step.
func (r *toolResolver) Get(ctx context.Context, name string) (string, error) {
	version, ok := r.versions[name]
	if !ok {
		return "", fmt.Errorf("tool %q not declared in pipeline `tools:` or config `runner.toolchain` (declared: %v)", name, keysOf(r.versions))
	}

	// Per-tool latch: at most one goroutine runs resolveBinary for a
	// given name; other concurrent callers wait on the same Once and
	// then read the memoised result. Once.Do is the synchronisation
	// point — no map access happens outside r.mu.
	r.mu.Lock()
	once, ok := r.resolveOnce[name]
	if !ok {
		once = &sync.Once{}
		r.resolveOnce[name] = once
	}
	r.mu.Unlock()

	once.Do(func() {
		dir, err := r.resolveBinary(ctx, name, version)
		r.mu.Lock()
		if err != nil {
			r.resolveErr[name] = err
		} else {
			r.resolved[name] = dir
		}
		r.mu.Unlock()
	})

	r.mu.Lock()
	dir, gotDir := r.resolved[name]
	err := r.resolveErr[name]
	r.mu.Unlock()
	if err != nil {
		return "", err
	}
	if !gotDir {
		// Defensive: Once ran without recording either outcome — should
		// never happen, but a clear error beats a silent empty PATH.
		return "", fmt.Errorf("tool %q: resolve produced no result", name)
	}

	if err := r.ensureDeps(ctx, name, dir); err != nil {
		return "", err
	}
	return dir, nil
}

// resolveBinary runs the host / native / skt lookup ladder. Split out
// from Get so the dependency-install step can be reasoned about
// independently of how the binary got there.
func (r *toolResolver) resolveBinary(ctx context.Context, name, version string) (string, error) {
	// Gate the YAML-supplied pair once, before any ladder rung can turn it into
	// a PATH lookup, a cache path, or an argv. The skt shape is the permissive
	// one (partial pins are legal there); the native path re-checks the strict
	// shape it needs for its own directory layout.
	if err := toolchain.ValidateSktRef(name, version); err != nil {
		return "", err
	}

	// 1. Host preference. Cheap (one exec) and usually hits in dev.
	if hostPath, ok := hostBinaryMatches(ctx, name, version); ok {
		fmt.Fprintf(r.log, "[sklp] tools: %s@%s (host: %s)\n", name, version, hostPath)
		return filepath.Dir(hostPath), nil
	}

	// 2. Native registry.
	if _, ok := toolchain.Get(name); ok {
		fmt.Fprintf(r.log, "[sklp] tools: installing %s@%s (native)\n", name, version)
		bin, err := toolchain.Install(ctx, name, version, r.cacheDir)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(r.log, "[sklp] tools: %s -> %s\n", name, bin)
		return filepath.Dir(bin), nil
	}

	// 3. skt for everything outside the native registry. Note: `node` and
	// `pnpm` are intentionally NOT registered in the native registry —
	// both upstream download URLs (nodejs.org/dist/v<X>,
	// registry.npmjs.org/@pnpm/...) require a FULL version (22.12.0,
	// 10.5.0) and 404 on common partial pins like "22" or "pnpm@10". skt
	// resolves partial versions to the latest matching release out of the
	// box, so we delegate both entirely. See removed
	// internal/toolchain/tools/{nodejs,pnpm}.go.
	fmt.Fprintf(r.log, "[sklp] tools: installing %s@%s (skt)\n", name, version)
	dir, err := toolchain.SktInstall(ctx, name, version, r.cacheDir, r.log)
	if err != nil {
		return "", fmt.Errorf("toolchain: %q via skt: %w", name, err)
	}
	fmt.Fprintf(r.log, "[sklp] tools: %s -> %s (skt)\n", name, dir)
	return dir, nil
}

// ensureDeps runs the canonical dependency-install command for the
// given tool, once per run, when the manifest expected by the tool is
// present in repoPath. The install runs with the resolved binary
// directory prepended to PATH so it finds the right version (and so
// pnpm finds the right node, etc.).
//
// A missing manifest is a no-op: a `tools: [pnpm]` step in a repo
// without `package.json` is legitimate (a script that runs `pnpm
// publish` for instance), so silently skipping is the safe default.
func (r *toolResolver) ensureDeps(ctx context.Context, name, binDir string) error {
	// Per-tool latch — see Get() for the same pattern. Two concurrent
	// steps using `pnpm` share a single `pnpm install`, and a third
	// arriving mid-install simply waits on the same Once.
	r.mu.Lock()
	if r.deps[name] {
		r.mu.Unlock()
		return nil
	}
	once, ok := r.depsOnce[name]
	if !ok {
		once = &sync.Once{}
		r.depsOnce[name] = once
	}
	r.mu.Unlock()

	once.Do(func() {
		err := r.runDepsInstall(ctx, name, binDir)
		r.mu.Lock()
		if err != nil {
			r.depsErr[name] = err
		} else {
			r.deps[name] = true
		}
		r.mu.Unlock()
	})

	r.mu.Lock()
	err := r.depsErr[name]
	r.mu.Unlock()
	return err
}

// runDepsInstall is the unguarded body of ensureDeps — extracted so the
// sync.Once latch can call it exactly once per tool name. Callers MUST
// route through ensureDeps so the latch and memo maps stay consistent.
func (r *toolResolver) runDepsInstall(ctx context.Context, name, binDir string) error {
	cmdline, ok := autoInstallCommand(name)
	if !ok {
		// No auto-install convention for this tool.
		return nil
	}
	if !hasManifest(r.repoPath, name) {
		return nil
	}
	fmt.Fprintf(r.log, "[sklp] auto-install: %s\n", cmdline)

	cmd := exec.CommandContext(ctx, "sh", "-c", cmdline)
	cmd.Dir = r.repoPath
	// pnpm (and its own postinstall scripts, e.g. esbuild's `node
	// install.js`) needs node on PATH too, not just its own binDir — the
	// node tool is resolved as a sibling entry in r.resolved, never
	// folded in here otherwise. On a host with no system-wide node this
	// left every postinstall dying with "node: not found" even though
	// node had just been installed moments earlier for this very run.
	r.mu.Lock()
	dirs := make([]string, 0, len(r.resolved)+1)
	dirs = append(dirs, binDir)
	for toolName, d := range r.resolved {
		if toolName != name {
			dirs = append(dirs, d)
		}
	}
	r.mu.Unlock()
	// CI=1 puts pnpm / npm / bun / yarn into non-interactive mode, so
	// a pinned-version mismatch surfaces as a clean failure instead of
	// dropping into a `? Proceed? (Y/n)` prompt that hangs sklp run.
	// COREPACK_ENABLE_DOWNLOAD_PROMPT=0 silences corepack's own
	// "download pnpm@X?" interactive question when the project pins a
	// version via package.json's `packageManager:` field.
	envWithCI := append(prependPath(os.Environ(), dirs),
		"CI=1",
		"COREPACK_ENABLE_DOWNLOAD_PROMPT=0",
	)
	cmd.Env = envWithCI
	// Tee the tool's combined stdout+stderr through a ring buffer so we
	// can surface the last N lines under the failing step in the
	// FAILURES section. The full output still flows to r.log (which
	// fans out to the per-run log file).
	tail := newLineTail(autoInstallTailLines)
	cmd.Stdout = io.MultiWriter(r.log, tail)
	cmd.Stderr = io.MultiWriter(r.log, tail)
	if err := cmd.Run(); err != nil {
		r.lastFailMu.Lock()
		r.lastFail = &autoInstallFailure{
			Tool:    name,
			Cmdline: cmdline,
			Tail:    tail.String(),
		}
		r.lastFailMu.Unlock()
		return fmt.Errorf("auto-install %s: %w", name, err)
	}
	return nil
}

// autoInstallTailLines is the number of trailing lines of the tool's
// combined output kept for the FAILURES section. Twenty lines covers
// pnpm's "ERR_PNPM_…" block, npm's "npm ERR!" stack, and go's typical
// "go: …" error footer without burying the rest of the report.
const autoInstallTailLines = 20

// ConsumeLastAutoInstallFailure returns the most recent auto-install
// failure context and clears it. Callers use this on the failing-step
// path to enrich the FAILURES section; consuming clears the slot so a
// later, unrelated failure does not reuse stale context.
func (r *toolResolver) ConsumeLastAutoInstallFailure() *autoInstallFailure {
	r.lastFailMu.Lock()
	defer r.lastFailMu.Unlock()
	f := r.lastFail
	r.lastFail = nil
	return f
}

// formatAutoInstallFailure renders the body of the FAILURES entry for
// an auto-install failure. Shape:
//
//	<cmdline> (last N lines):
//	  <line1>
//	  <line2>
//	  …
//	full output: <logPath>
//
// logPath may be empty when the per-run log file could not be created;
// the helper omits the trailing pointer in that case.
func formatAutoInstallFailure(f *autoInstallFailure, logPath string) string {
	if f == nil {
		return ""
	}
	var b strings.Builder
	tailLines := strings.Split(strings.TrimRight(f.Tail, "\n"), "\n")
	for len(tailLines) > 0 && strings.TrimSpace(tailLines[0]) == "" {
		tailLines = tailLines[1:]
	}
	fmt.Fprintf(&b, "%s (last %d lines):\n", f.Cmdline, len(tailLines))
	for _, ln := range tailLines {
		b.WriteString("  ")
		b.WriteString(ln)
		b.WriteByte('\n')
	}
	if hint := pnpmIgnoredBuildsHint(f.Tail); hint != "" {
		b.WriteString(hint)
		b.WriteByte('\n')
	}
	if logPath != "" {
		fmt.Fprintf(&b, "full output: %s", logPath)
	}
	return strings.TrimRight(b.String(), "\n")
}

// pnpmIgnoredBuildsHint returns an actionable fix when a pnpm auto-install
// failed because pnpm 10+ blocked dependency build scripts by default
// (ERR_PNPM_IGNORED_BUILDS). The fix is NOT ours to force — pinning pnpm 9
// or passing --dangerously-allow-all-builds would defeat pnpm's
// supply-chain guard. The right owner is the project.
//
// pnpm 11 moved this setting OUT of package.json (the old
// `pnpm.onlyBuiltDependencies` is now ignored with a warning) and into
// `pnpm-workspace.yaml` as an `allowBuilds:` map. We emit that exact
// snippet — verified working on pnpm 11.3 — with the packages parsed from
// the error, ready to paste and commit. Returns "" when the tail is not
// this specific failure.
func pnpmIgnoredBuildsHint(tail string) string {
	if !strings.Contains(tail, "ERR_PNPM_IGNORED_BUILDS") &&
		!strings.Contains(tail, "Ignored build scripts") {
		return ""
	}
	pkgs := parseIgnoredBuildPackages(tail)
	var b strings.Builder
	b.WriteString("\nhint: pnpm 10+ blocks dependency build scripts by default (supply-chain safety).\n")
	b.WriteString("      Allow the ones you trust in pnpm-workspace.yaml (pnpm 11's home for this), then commit:\n\n")
	b.WriteString("        # pnpm-workspace.yaml\n")
	b.WriteString("        allowBuilds:\n")
	if len(pkgs) > 0 {
		for _, p := range pkgs {
			fmt.Fprintf(&b, "          '%s': true\n", p)
		}
	} else {
		b.WriteString("          '<package>': true\n")
	}
	b.WriteString("\n      (package.json's `pnpm.onlyBuiltDependencies` is IGNORED by pnpm 11.)\n")
	b.WriteString("      Docs: https://pnpm.io/settings#allowbuilds")
	return b.String()
}

// parseIgnoredBuildPackages pulls the package names out of pnpm's
// "Ignored build scripts: a@1, b@2, c@3" line. The version suffix is
// stripped (scoped names keep their leading '@'), so the result drops
// straight into onlyBuiltDependencies. Order is preserved; duplicates are
// collapsed.
func parseIgnoredBuildPackages(tail string) []string {
	const marker = "Ignored build scripts:"
	idx := strings.Index(tail, marker)
	if idx < 0 {
		return nil
	}
	rest := tail[idx+len(marker):]
	if nl := strings.IndexByte(rest, '\n'); nl >= 0 {
		rest = rest[:nl]
	}
	seen := map[string]bool{}
	var out []string
	for _, raw := range strings.Split(rest, ",") {
		spec := strings.TrimSpace(strings.TrimRight(strings.TrimSpace(raw), "."))
		if spec == "" {
			continue
		}
		// Strip the trailing @version, keeping a leading @scope.
		if at := strings.LastIndex(spec, "@"); at > 0 {
			spec = spec[:at]
		}
		if spec != "" && !seen[spec] {
			seen[spec] = true
			out = append(out, spec)
		}
	}
	return out
}

// lineTail is a tiny ring buffer that keeps only the last `max` lines
// written to it. Lines are split on '\n'; partial trailing bytes are
// preserved across Write calls so a tool that writes a half line and
// flushes later still ends up correctly tailed.
type lineTail struct {
	mu      sync.Mutex
	max     int
	lines   []string
	pending bytes.Buffer
}

func newLineTail(max int) *lineTail {
	if max <= 0 {
		max = 1
	}
	return &lineTail{max: max}
}

func (t *lineTail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.pending.Write(p)
	for {
		raw := t.pending.Bytes()
		i := bytes.IndexByte(raw, '\n')
		if i < 0 {
			break
		}
		t.appendLine(string(raw[:i]))
		t.pending.Next(i + 1)
	}
	return len(p), nil
}

func (t *lineTail) appendLine(line string) {
	t.lines = append(t.lines, line)
	if len(t.lines) > t.max {
		t.lines = t.lines[len(t.lines)-t.max:]
	}
}

// String returns the captured tail joined with '\n', including any
// unterminated trailing line still in the pending buffer (the tool may
// have died mid-line, and that fragment is often the most useful one).
func (t *lineTail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := append([]string(nil), t.lines...)
	if t.pending.Len() > 0 {
		out = append(out, t.pending.String())
		if len(out) > t.max {
			out = out[len(out)-t.max:]
		}
	}
	return strings.Join(out, "\n")
}

// autoInstallCommand returns the canonical install command for a
// package-manager tool, or ("", false) when the tool has no install
// step (kubectl, helm, …). Commands favour deterministic, lockfile-
// respecting variants — `npm ci` not `npm install`, `--frozen-lockfile`
// for pnpm/yarn/bun — to match CI semantics.
func autoInstallCommand(name string) (string, bool) {
	switch name {
	case "pnpm":
		// pnpm picks its content store by default at <project>/.pnpm-store,
		// which on the k8s runner lives in the per-run emptyDir /workspace
		// and is thrown away every run (logs show `reused 0, downloaded N`
		// each time). pnpm does NOT honour any env var for the store —
		// verified on pnpm 11.3 in-runner: PNPM_STORE_DIR, PNPM_STORE_PATH,
		// npm_config_store_dir, NPM_CONFIG_STORE_DIR and even a global
		// .npmrc were all ignored; only the `--store-dir` flag took effect.
		// So when the spawner points PNPM_STORE_DIR at the persistent
		// hostPath cache (/cache/pnpm-store), pass it as the flag. Empty
		// (local `sklp run`) keeps the default behaviour untouched.
		if store := strings.TrimSpace(os.Getenv("PNPM_STORE_DIR")); store != "" {
			return "pnpm install --frozen-lockfile --store-dir=" + store, true
		}
		return "pnpm install --frozen-lockfile", true
	case "npm":
		return "npm ci", true
	case "yarn":
		return "yarn install --frozen-lockfile", true
	case "bun":
		return "bun install --frozen-lockfile", true
	case "go":
		// `-x` logs network ops (`# get https://...`) but stays silent on
		// cache hits — that's a UX gap (warm cache looks identical to a
		// stuck process). The `-v` flag DOES NOT EXIST for `go mod
		// download` (it's a `go build` / `go get` flag), so a previous
		// attempt to add it crashed the step with `flag provided but not
		// defined: -v`. Live-progress feedback on warm caches has to
		// come from elsewhere (e.g. a heartbeat in the resolver).
		//
		// In a multi-module go workspace (go.work with several go.mod
		// files), a single `go mod download` at the workspace root only
		// fetches the module graph the workspace itself references — it
		// does NOT cover transitive deps that a subsequent `cd apps/X &&
		// go test ./...` step needs. The result is that every `go test`
		// re-fetches a chunk of modules even though the .zip caches are
		// warm. So we download per workspace member instead.
		//
		// `go work sync` would do that in one call, but it is a WRITE:
		// it propagates each member's `require` block into the others
		// and rewrites their go.mod. Warming a cache must not edit the
		// caller's checkout — and a half-applied sync (go.mod bumped,
		// go.sum entry missing) breaks the build it was meant to speed
		// up. `go list -m` reads the same member list without writing.
		return "for d in $(go list -m -f '{{.Dir}}' 2>/dev/null); do (cd \"$d\" && go mod download -x); done; go mod download -x", true
	default:
		return "", false
	}
}

// hasManifest reports whether the manifest a tool's install command
// needs is present in repoPath. The check is per-tool because each
// ecosystem disagrees on the canonical filename (package.json is
// shared by node tools but go uses go.mod, bun additionally accepts
// bun.lockb without a package.json on rare standalone scripts).
func hasManifest(repoPath, tool string) bool {
	candidates := manifestCandidates(tool)
	for _, c := range candidates {
		if _, err := os.Stat(filepath.Join(repoPath, c)); err == nil {
			return true
		}
	}
	return false
}

func manifestCandidates(tool string) []string {
	switch tool {
	case "pnpm", "npm", "yarn":
		return []string{"package.json"}
	case "bun":
		return []string{"package.json", "bun.lockb"}
	case "go":
		return []string{"go.mod", "go.work"}
	default:
		return nil
	}
}

// stepTools returns the tool names a step needs. The map it validates
// against is the EFFECTIVE toolchain (pipeline `tools:` ∪ config
// `runner.toolchain`), so a step may name any tool the execution env
// provides. The YAML `tools:` field has three meaningful states,
// distinguished by yaml.v3:
//   - omitted entirely (Tools == nil): legacy / unannotated step,
//     inherits every tool in the effective toolchain (the full env).
//   - explicit empty list (Tools != nil, len == 0): the user opted out
//     — branch-context style "pure git" steps that need no toolchain.
//   - explicit non-empty list: only those names, must all be declared
//     in pipeline `tools:` or the runner config's `runner.toolchain`.
//
// Returns an error when the step asks for a tool name that the effective
// toolchain doesn't provide — that's a YAML mistake we want to surface
// before running anything.
func stepTools(step Step, effective map[string]string) ([]string, error) {
	if step.Tools == nil {
		return keysOf(effective), nil
	}
	if len(step.Tools) == 0 {
		return nil, nil
	}
	for _, name := range step.Tools {
		if _, ok := effective[name]; !ok {
			return nil, fmt.Errorf("step %q lists tool %q which is not declared in pipeline `tools:` or config `runner.toolchain`", step.Name, name)
		}
	}
	return step.Tools, nil
}

// mergeToolchain resolves the EFFECTIVE tool→version map a pipeline run
// exposes: the UNION of the runner config's `runner.toolchain`
// (config.yaml — the execution env baked into the runner image) and the
// pipeline's own `tools:`. The config defines the env; the pipeline may
// override or extend it.
//
// Rules (see issue feat/pipeline-uses-config-toolchain):
//   - Every config tool is in the result (a step may use it without
//     redeclaring it in pipeline `tools:`).
//   - On a name collision, a pipeline tool with a NON-EMPTY version wins
//     (an explicit pin overrides the config default).
//   - A pipeline tool with an EMPTY version defers to the config version
//     when config provides one; otherwise it stays "" (the host /
//     skt partial-resolution path fills it, exactly as before).
//   - A pipeline-only tool (absent from config) is included with its
//     own version (or "").
//
// Degradation: when configToolchain is nil/empty the result equals
// pipelineTools; when pipelineTools is nil it equals configToolchain.
func mergeToolchain(pipelineTools, configToolchain map[string]string) map[string]string {
	out := make(map[string]string, len(configToolchain)+len(pipelineTools))
	for k, v := range configToolchain {
		out[k] = v
	}
	for k, v := range pipelineTools {
		if strings.TrimSpace(v) != "" {
			out[k] = v // pipeline non-empty version overrides config.
		} else if _, ok := out[k]; !ok {
			out[k] = "" // pipeline-only key, no version anywhere.
		}
		// else: empty pipeline version, config already supplied one → keep config.
	}
	return out
}

// keysOf returns the keys of m, in no particular order. Cheap helper
// so the resolver doesn't reach for a generics-heavy alternative for
// what is always a tiny map (the pipeline-level `tools:`).
func keysOf(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
