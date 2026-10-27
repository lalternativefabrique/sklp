package devstack

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/lalternativefabrique/sklp/internal/toolchain"
	"github.com/lalternativefabrique/sklp/pipeline/runnerimage"
)

// Dev toolchain: a run: service runs the project's pinned go/node/pnpm WITHOUT
// baking an image. The pins are the SAME source of truth CI uses —
// .sklp/space.yaml `runner.toolchain` (+ `go_tools`) — so dev and CI never
// drift, and the developer declares nothing extra. skt installs them on the
// HOST (into the shared skt cache, already warm from CI), and the embedded
// backend bind-mounts that cache into the minimal-glibc space + prepends the
// resolved bin dirs to PATH. (A `dev.yaml tools:` override is a later step; the
// base config is the source today.)

// devToolchain is the resolved host-side toolchain for a `sklp dev` run: the
// skt cache root to bind-mount, and the bin directories to prepend to PATH.
type devToolchain struct {
	// CacheRoot is the host skt cache (shared with CI). Bind-mounted read-only
	// at the same path inside the space so absolute shebangs resolve.
	CacheRoot string
	// BinDirs are the per-tool bin directories, in declared order, prepended to
	// PATH so the pinned go/node/pnpm shadow anything in the base image.
	BinDirs []string
	// Env is space.yaml runner.env (e.g. GOPRIVATE=github.com/org), applied to
	// a run: service so go knows which modules are private — the SAME source CI
	// uses, so dev and CI agree without the developer re-declaring it.
	Env map[string]string
	// Secrets is space.yaml runner.secrets (names like GITHUB_TOKEN). For a
	// declared GITHUB_TOKEN whose value is present in the run: service env, dev
	// writes a ~/.netrc so git/go can fetch private modules — mirroring CI's
	// build-time netrc, but at run time since dev fetches modules at run time.
	Secrets []string
	// Recipe is the full space.yaml runner block. It is the source the embedded
	// backend uses to BUILD a runner rootfs (runtime/runnerbuild) for a service
	// that NeedsRunnerImage — i.e. declares system packages or opts in via
	// `image: runner`. Empty Recipe (no space.yaml) means no build, the run:
	// service stays on the minimal base + bind-mounted toolchain.
	Recipe runnerimage.Runner
}

// resolveDevToolchain reads .sklp/space.yaml `runner.toolchain` + `go_tools`
// and installs each pin via skt on the host, returning the cache root to mount
// and the bin dirs to put on PATH. It is a no-op (zero value, nil error) when
// the project declares no toolchain — a run: service then runs on the bare base
// image (whatever it provides). skt failures ARE returned: a declared toolchain
// that cannot be provisioned must fail loudly, not silently run without it.
func resolveDevToolchain(ctx context.Context, repoAbs string, progress func(string)) (devToolchain, error) {
	cfgPath := runnerimage.ResolvePath(repoAbs)
	if _, err := os.Stat(cfgPath); err != nil {
		return devToolchain{}, nil // no space.yaml — nothing to provision
	}
	cfg, err := runnerimage.Load(cfgPath)
	if err != nil {
		return devToolchain{}, fmt.Errorf("dev toolchain: load %s: %w", cfgPath, err)
	}
	specs := toolchainSpecs(cfg.Runner)
	if len(specs) == 0 {
		// No toolchain, but runner.env / runner.secrets / packages may still apply
		// (a project that declares only GOPRIVATE + a token, or system packages
		// with no pinned language tools).
		return devToolchain{Env: cfg.Runner.Env, Secrets: cfg.Runner.Secrets, Recipe: cfg.Runner}, nil
	}
	cacheDir := devToolsCacheDir()
	var binDirs []string
	for _, t := range specs {
		if progress != nil {
			progress(t.spec)
		}
		name, version, _ := strings.Cut(t.spec, "@")
		dir, ierr := toolchain.SktInstallTagged(ctx, name, version, t.tags, cacheDir)
		if ierr != nil {
			return devToolchain{}, fmt.Errorf("dev toolchain: install %s: %w", t.spec, ierr)
		}
		binDirs = append(binDirs, dir)
	}
	return devToolchain{
		CacheRoot: cacheDir,
		BinDirs:   binDirs,
		Env:       cfg.Runner.Env,
		Secrets:   cfg.Runner.Secrets,
		Recipe:    cfg.Runner,
	}, nil
}

// devTool is one `<name>@<version>` skt spec plus the Go build tags it must be
// compiled with, if any.
type devTool struct {
	spec string
	tags string
}

// toolchainSpecs flattens runner.toolchain (go/node/pnpm) + runner.go_tools into
// the ordered list skt installs. Toolchain entries come first (deterministic by
// name) so language runtimes resolve before the go-install tools that may need
// them.
func toolchainSpecs(r runnerimage.Runner) []devTool {
	var specs []devTool
	names := make([]string, 0, len(r.Toolchain))
	for k := range r.Toolchain {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, n := range names {
		specs = append(specs, devTool{spec: n + "@" + r.Toolchain[n]})
	}
	for _, mod := range r.GoTools {
		// Build tags travel with the spec: a tool compiled without them is a
		// different binary, and `migrate` without its driver cannot migrate.
		if s, tags, ok := runnerimage.GoToolSktSpec(mod); ok {
			specs = append(specs, devTool{spec: s, tags: tags})
		}
	}
	return specs
}

// prependToolchainPATH puts the toolchain bin dirs at the front of PATH in a
// KEY=VALUE env slice, so the pinned go/node/pnpm win over anything the base
// image ships. If env carries no PATH, a sane default tail is used (the base is
// debian-slim). Returns env unchanged when there are no bin dirs.
func prependToolchainPATH(env []string, binDirs []string) []string {
	if len(binDirs) == 0 {
		return env
	}
	const defaultPATH = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
	prefix := strings.Join(binDirs, ":")
	for i, kv := range env {
		if strings.HasPrefix(kv, "PATH=") {
			env[i] = "PATH=" + prefix + ":" + strings.TrimPrefix(kv, "PATH=")
			return env
		}
	}
	return append(env, "PATH="+prefix+":"+defaultPATH)
}

// withWritableToolCaches points HOME and the go/node/pnpm caches at the /tmp
// tmpfs, so a run: service can write its build + module caches despite the
// read-only rootfs (the base image's default HOME=/root is read-only). Only
// sets a key the caller did not already provide, so a dev.yaml env: override
// still wins. /tmp is the per-service tmpfs the spec always mounts.
func withWritableToolCaches(env []string) []string {
	defaults := map[string]string{
		"HOME":             "/tmp",
		"GOCACHE":          "/tmp/go-build",
		"GOMODCACHE":       "/tmp/go-mod",
		"GOPATH":           "/tmp/go",
		"XDG_CACHE_HOME":   "/tmp/.cache",
		"npm_config_cache": "/tmp/.npm",
	}
	for k, v := range defaults {
		if !hasEnvKey(env, k) {
			env = append(env, k+"="+v)
		}
	}
	return env
}

// devBuildCacheDir is the host directory holding the PERSISTENT toolchain build
// caches for run: services (go module + build cache, pnpm store). Unlike /tmp
// inside the container (an ephemeral tmpfs wiped on every container exit), this
// host dir survives across `sklp dev` runs, so go/pnpm do not re-download the
// world each launch. Shared by all run: services (one go mod cache for core +
// transcoder, etc.).
func devBuildCacheDir() string {
	if v := os.Getenv("XDG_CACHE_HOME"); v != "" {
		return filepath.Join(v, "sklp", "dev-buildcache")
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".cache", "sklp", "dev-buildcache")
	}
	return filepath.Join(os.TempDir(), "sklp-dev-buildcache")
}

// devBuildCacheMount is the in-container path the host build cache is bind-
// mounted at. A fixed, neutral path (NOT under /tmp, which is a tmpfs) so the
// cache env below resolves to the persistent bind, not the ephemeral tmpfs.
const devBuildCacheMount = "/sklp-cache"

// withPersistentBuildCaches points the go + pnpm caches at subdirs of the
// bind-mounted persistent cache (devBuildCacheMount), overriding the /tmp
// defaults from withWritableToolCaches so they survive across runs. Appended
// last so these values win in the process env. A dev.yaml env: override still
// wins because applyConfigEnv/spec env is folded before this.
func withPersistentBuildCaches(env []string) []string {
	caches := map[string]string{
		"GOCACHE":          devBuildCacheMount + "/go-build",
		"GOMODCACHE":       devBuildCacheMount + "/go-mod",
		"GOPATH":           devBuildCacheMount + "/go",
		"npm_config_cache": devBuildCacheMount + "/npm",
		"XDG_CACHE_HOME":   devBuildCacheMount + "/xdg",
	}
	out := env
	for k, v := range caches {
		if !hasEnvKey(out, k) {
			out = append(out, k+"="+v)
		}
	}
	return out
}

// withNonInteractiveEnv marks the environment as non-interactive so tools that
// otherwise prompt for confirmation run unattended in the space (a run: service
// container has NO TTY). pnpm aborts a node_modules purge without a TTY unless
// CI=true (ERR_PNPM_ABORTED_REMOVE_MODULES_DIR_NO_TTY); the same flag puts most
// CLIs in their non-interactive path. Only sets keys the dev did not provide.
func withNonInteractiveEnv(env []string) []string {
	defaults := map[string]string{
		"CI":              "true",
		"DEBIAN_FRONTEND": "noninteractive",
	}
	for k, v := range defaults {
		if !hasEnvKey(env, k) {
			env = append(env, k+"="+v)
		}
	}
	return env
}

// caBundleDest is where we bind-mount the host CA bundle inside the space.
// It sits DIRECTLY under /etc (which always exists) rather than the usual
// /etc/ssl/certs/ca-certificates.crt: the embedded runtime does not create
// missing mount-point parent dirs, and a minimal base (debian-slim) ships no
// /etc/ssl/certs, so a deep target would fail `runc create`. SSL_CERT_FILE
// points the toolchain here, so the exact path is irrelevant to go/git/curl.
const caBundleDest = "/etc/sklp-ca-bundle.pem"

// hostCABundle resolves the host's CA bundle to a concrete file (following the
// /etc/ssl/certs symlink) and returns (hostPath, containerPath, ok). ok is
// false when no bundle is found, so a run: service simply launches without it
// (TLS to public roots then fails, but that is the host's missing-CA problem,
// surfaced clearly rather than silently).
func hostCABundle() (string, string, bool) {
	for _, p := range []string{
		"/etc/ssl/certs/ca-certificates.crt", // Debian/Ubuntu/Arch
		"/etc/pki/tls/certs/ca-bundle.crt",   // Fedora/RHEL
		"/etc/ssl/cert.pem",                  // Alpine/BSD-ish
	} {
		if real, err := filepath.EvalSymlinks(p); err == nil {
			if fi, serr := os.Stat(real); serr == nil && !fi.IsDir() {
				return real, caBundleDest, true
			}
		}
	}
	return "", "", false
}

// applyConfigEnv folds space.yaml runner.env (e.g. GOPRIVATE) into a run:
// service's env, WITHOUT overriding a key the service/dev.yaml already set —
// the project's own value wins, the config is the fallback. Used so dev honours
// the same GOPRIVATE etc. CI uses without the developer re-declaring it.
func applyConfigEnv(env []string, cfg map[string]string) []string {
	for _, k := range sortedEnvKeys(cfg) {
		if !hasEnvKey(env, k) {
			env = append(env, k+"="+cfg[k])
		}
	}
	return env
}

// sortedEnvKeys returns the keys of a string map in deterministic order, so the
// appended env is stable across runs.
func sortedEnvKeys(m map[string]string) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

// gitTokenSecretNames are the env-var names a declared secret may use to carry
// a GitHub token. A project commonly declares GITHUB_TOKEN but loads the value
// under a namespaced var (e.g. SKALPAI_GITHUB_TOKEN) — both are accepted.
var gitTokenSecretNames = []string{"GITHUB_TOKEN", "GH_TOKEN"}

// privateAuthPreamble returns a shell snippet, prepended to a run: command,
// that sets up git auth for private modules when:
//   - space.yaml declared a GitHub-token secret, AND
//   - a matching token var is actually present in the service env.
//
// It writes $HOME/.netrc from the token var (the VALUE never appears in the
// command — only the var name is referenced, expanded by the in-container
// shell) and forces an HTTPS rewrite so a gitconfig SSH rule does not divert
// the clone to ssh (which the space has no key for). Returns "" when no token
// secret applies, so a public project's command is unchanged.
func privateAuthPreamble(secrets []string, env []string) string {
	tokenVar := resolveTokenVar(secrets, env)
	if tokenVar == "" {
		return ""
	}
	// Single-quoted heredoc-free one-liner; ${VAR} is expanded by the container
	// shell at run time, so the token value is never embedded here.
	return "umask 077; " +
		"printf 'machine github.com login oauth2 password %s\\n' \"${" + tokenVar + "}\" > \"$HOME/.netrc\"; " +
		"git config --global url.\"https://github.com/\".insteadOf \"git@github.com:\" 2>/dev/null || true; "
}

// resolveTokenVar picks the env-var name to read the GitHub token from: a
// declared token secret (GITHUB_TOKEN/GH_TOKEN) takes priority, but only if its
// value is actually present in env; otherwise any known token var present in
// env is used. Returns "" when no usable token var is present.
func resolveTokenVar(secrets []string, env []string) string {
	declared := map[string]bool{}
	for _, s := range secrets {
		declared[s] = true
	}
	// Prefer an exact declared secret name that has a value.
	for _, name := range gitTokenSecretNames {
		if declared[name] && envValuePresent(env, name) {
			return name
		}
	}
	// Fall back to any known token var present in env (covers the namespaced
	// case, e.g. SKALPAI_GITHUB_TOKEN, when a token secret was declared at all).
	if len(secrets) > 0 {
		for _, kv := range env {
			k, _, ok := strings.Cut(kv, "=")
			if !ok {
				continue
			}
			if strings.HasSuffix(k, "GITHUB_TOKEN") || strings.HasSuffix(k, "GH_TOKEN") {
				if envValuePresent(env, k) {
					return k
				}
			}
		}
	}
	return ""
}

// envValuePresent reports whether env defines key with a non-empty value.
func envValuePresent(env []string, key string) bool {
	pfx := key + "="
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, pfx); ok {
			return v != ""
		}
	}
	return false
}

// hasEnvKey reports whether a KEY=VALUE slice already defines key.
func hasEnvKey(env []string, key string) bool {
	pfx := key + "="
	for _, kv := range env {
		if strings.HasPrefix(kv, pfx) {
			return true
		}
	}
	return false
}

// devToolsCacheDir is the host skt cache dev installs into — the SAME default
// CI uses ($XDG_CACHE_HOME/skt, else ~/.cache/skt), so a tool already pulled by
// `sklp run ci` is reused, not re-downloaded.
func devToolsCacheDir() string {
	if v := os.Getenv("XDG_CACHE_HOME"); v != "" {
		return filepath.Join(v, "skt")
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".cache", "skt")
	}
	return filepath.Join(os.TempDir(), "skt")
}
