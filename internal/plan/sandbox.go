package plan

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// The in-process sandbox runs every CI step with a curated environment
// instead of the inherited host env. The composition rule is:
//
//   1. A short allowlist of host vars (PATH, HOME, USER, TZ, LANG,
//      LC_ALL) — anything else the host carries is dropped. The
//      allowlist exists because a CI step must be able to find tools
//      on PATH and respect the user's locale, but should not inherit
//      ambient credentials (AWS_*, GH_TOKEN, …) by accident.
//   2. Pipeline-declared env (CIPlan-level — carried on CIStep.Env via
//      the compiler).
//   3. Step-declared env (already overlaid on CIStep.Env at compile).
//   4. Secrets the step explicitly opted into via CIStep.Secrets.
//
// Step env wins over pipeline env wins over allowlist. Secrets are
// applied last so a step cannot redact a declared secret by reusing
// the same name in `env:`.

// sandboxAllowlist is the set of host environment variables the
// sandbox lets through by default. Order is preserved for determinism
// in tests.
var sandboxAllowlist = []string{
	"PATH",
	"HOME",
	"USER",
	"TZ",
	"LANG",
	"LC_ALL",
	// Where runtime unix sockets live (spacenet attach/ingress) and where
	// caches land. Dropping these makes RuntimeRunRoot and CacheRoot fall
	// back to the home dir — which, in the vz guest, is the virtiofs share:
	// sockets cannot bind there and userns apt cannot chown there.
	"XDG_RUNTIME_DIR",
	"XDG_CACHE_HOME",
	// skt resolves and bakes tool binaries into a stable directory
	// (/opt/sklp/tools/bin) that the runner image puts on PATH. Tools are
	// plain binaries reached via PATH — no shim relay and no per-tool env
	// var to allowlist.
}

// sandboxSpec is the resolved input the sandbox needs to build and
// run one step. The caller (ExecuteCI) assembles it from the compiled
// CIStep + per-run state (resolved secrets, tool bin dirs, repo root).
type sandboxSpec struct {
	Repo       string
	Cwd        string // already validated by the compiler (relative or "")
	Cmdline    string
	StepEnv    map[string]string // pipeline + step env (step wins)
	ToolDirs   []string          // prepended to PATH
	HostEnv    []string          // inherited environment to filter through the allowlist
	Secrets    []string          // names allowed for this step
	AllSecret  map[string]string // resolved name -> value (filtered by Secrets)
	Network    string            // "none" = unshare net ns (Linux), else host
	Filesystem string            // "open" = no FS confinement, else confined (default)
	CacheDirs  []string          // extra read-only roots (toolchain cache, SKLP_TOOLS_CACHE)
	// Resolved Go cache roots, filled by the caller (exec_ci.go) from the
	// step/host env with the standard defaults. GOMODCACHE and GOCACHE are
	// granted read-WRITE because `go build`/`go test`/`go mod download`
	// write into them; GoBin is granted read-only (Landlock ro still allows
	// executing the installed binaries — gotestsum, swag, …). Empty when not
	// applicable. The -oci path isolates caches via dedicated mounts and
	// never sets these.
	GoModCache string
	GoCache    string
	GoBin      string
	GitConfig  string // ~/.gitconfig, granted read-only (git reads it)
	// $XDG_CONFIG_HOME/git/ignore (~/.config/git/ignore by default): git's
	// global excludes file, consulted on every `git add`/`status` regardless
	// of core.excludesFile. Lives under the denied-by-default $HOME (or a
	// custom XDG_CONFIG_HOME), so it needs its own read-only grant same as
	// GitConfig. Empty when the file does not exist.
	GitIgnoreGlobal string
	// SklpUserConfig is the sklp CLI's own config file (config.Path(): $XDG_
	// CONFIG_HOME/sklp/config.yaml or ~/.config/sklp/config.yaml), read by
	// stackconfig.Load to resolve the active context. Lives under the
	// denied-by-default $HOME, so it needs its own read-only grant. Empty
	// when the file does not exist.
	SklpUserConfig string
	// ~/.docker config dir, granted read-WRITE for docker build/buildx/push
	// steps only. buildx takes a flock on ~/.docker/buildx/.lock and mutates
	// its store (instances/, refs/, current) on every invocation, so a
	// read-only grant fails with "permission denied" on the lock. Lives under
	// the denied-by-default $HOME. Empty for non-Docker steps.
	DockerConfigDir string
	// Corepack's download cache (COREPACK_HOME, default ~/.cache/node/corepack).
	// corepack lazily fetches the pinned pnpm/yarn into it and reads it back on
	// every later invocation — so it must be read-WRITE (cold cache writes) and
	// shared with the host (cross-run reuse). Lives under the denied-by-default
	// $HOME. Empty for steps with no node/pnpm tool. The Go-cache analogue.
	NodeCacheDir string
	SecretDir    string // root for materialised secret files
	Stdout       io.Writer
	Stderr       io.Writer
	Timeout      time.Duration
}

// fsAccess describes one Landlock path grant computed by the policy.
// kind is "rw" (read-write dir subtree), "ro" (read-only dir subtree,
// execute included) or "rwfile" (read-write single file, for device
// nodes that processes legitimately write — e.g. /dev/null). Keeping it
// OS-independent lets the policy be unit-tested without touching the
// Landlock syscall (which only exists on Linux), mirroring how composeEnv
// is tested without spawning a shell.
type fsAccess struct {
	Kind  string // "rw" | "ro" | "rwfile"
	Paths []string
}

// defaultRORoots is the set of system directories a confined step may
// read (and execute from). It is deliberately generous: the goal of v1
// is to deny $HOME and ambient credentials, not to whittle the OS down
// to a minimum. A go build, pnpm install or sh -c must still work, and
// these all reach into /usr, /etc (resolv.conf, ssl certs), /proc, /dev.
// Missing entries are ignored at apply time (IgnoreIfMissing), so this
// list can stay portable across distros (/lib64 absent on some, etc.).
var defaultRORoots = []string{
	"/usr",
	"/bin",
	"/sbin",
	"/lib",
	"/lib64",
	"/etc",
	"/opt",
	"/proc",
	"/dev",
	"/sys",
	"/run",
	"/var",
}

// rwDevices are the standard device nodes a confined step writes to even
// though /dev itself is granted read-only. Tools open /dev/null (swag,
// git → "could not open /dev/null for reading and writing"), /dev/tty,
// the pty multiplexer, etc. These get an explicit read-write FILE grant
// (Landlock ro on the parent /dev dir does not permit writing them).
// Missing entries are ignored at apply time.
var rwDevices = []string{
	"/dev/null",
	"/dev/zero",
	"/dev/full",
	"/dev/random",
	"/dev/urandom",
	"/dev/tty",
	"/dev/ptmx",
	"/dev/pts",
}

// computeFSPolicy turns a sandboxSpec into the ordered list of Landlock
// grants for a confined step. It returns nil when confinement is
// disabled (Filesystem == "open"), so the caller applies nothing.
//
// Policy (confined, the implicit default):
//   - rw: the resolved workdir (repo/cwd subtree), /tmp + $TMPDIR, SecretDir,
//     and the Go caches GOMODCACHE + GOCACHE (the toolchain writes them);
//   - ro: tool bin dirs, toolchain cache dirs, $GOBIN, and defaultRORoots;
//   - rwfile: standard writable device nodes (/dev/null, …);
//   - everything else (notably $HOME outside the above) is denied by
//     Landlock's deny-by-default.
//
// workdir is the already-resolved cwd from resolveSandboxCwd; passing it
// in (rather than re-deriving) keeps the policy a pure function of its
// inputs for testing.
func computeFSPolicy(spec sandboxSpec, workdir string) []fsAccess {
	if spec.Filesystem == "open" {
		return nil
	}

	rw := make([]string, 0, 4)
	if workdir != "" {
		rw = append(rw, workdir)
	} else if spec.Repo != "" {
		rw = append(rw, spec.Repo)
	}
	rw = append(rw, "/tmp")
	if tmp := tmpdirFromEnv(spec.StepEnv, spec.HostEnv); tmp != "" && tmp != "/tmp" {
		rw = append(rw, tmp)
	}
	if spec.SecretDir != "" {
		rw = append(rw, spec.SecretDir)
	}
	// Go's build/test/mod tooling WRITES into the module and build caches
	// (go mod download stages tarballs in GOMODCACHE; go build/test write
	// objects to GOCACHE). The non-oci sandbox shares the host's Go caches,
	// so they must be read-write or every `go` invocation fails with
	// "permission denied". The -oci path never sets these (it mounts
	// isolated caches instead).
	if spec.GoModCache != "" {
		rw = append(rw, spec.GoModCache)
	}
	if spec.GoCache != "" {
		rw = append(rw, spec.GoCache)
	}
	// corepack writes the lazily-fetched pnpm/yarn into its cache on a cold
	// run and reads it back afterwards — same read-write, host-shared shape as
	// the Go caches above. Without this grant a `tools: [pnpm]` step fails with
	// "EACCES: opendir '~/.cache/node/corepack/...'" the first time corepack
	// has to materialise the pinned version.
	if spec.NodeCacheDir != "" {
		rw = append(rw, spec.NodeCacheDir)
	}
	// docker buildx locks and mutates ~/.docker (buildx/.lock, instances/,
	// refs/, current). Grant it read-write only for Docker steps so the
	// publish path works under confinement; non-Docker steps keep $HOME
	// denied. The Docker socket itself needs no write grant (Landlock does
	// not mediate connect(2) on the /run/var-resident socket).
	if spec.DockerConfigDir != "" && isDockerCmd(spec.Cmdline) {
		rw = append(rw, spec.DockerConfigDir)
	}

	ro := make([]string, 0, len(spec.ToolDirs)+len(spec.CacheDirs)+len(defaultRORoots))
	ro = append(ro, spec.ToolDirs...)
	// A tool dir often holds only symlink shims pointing OUTSIDE itself:
	// fnm/corepack lays out bin/pnpm -> ../lib/node_modules/corepack/dist/pnpm.js.
	// Mounting just bin/ ro leaves the target under ../lib unmapped, so the
	// shim fails to resolve in the sandbox (exit 126, "Permission denied").
	// Grant the parent dir of any escaping symlink target so the real file is
	// reachable. Best-effort: unreadable dirs are skipped (caught at apply).
	ro = append(ro, symlinkTargetDirs(spec.ToolDirs)...)
	ro = append(ro, spec.CacheDirs...)
	// $GOBIN / $GOPATH/bin holds `go install`-ed binaries (gotestsum, swag).
	// It lives under $HOME, which is denied by default, so without this grant
	// the shim cannot exec them. Landlock ro includes EXECUTE on the subtree.
	if spec.GoBin != "" {
		ro = append(ro, spec.GoBin)
	}
	ro = append(ro, defaultRORoots...)

	// git reads ~/.gitconfig (user.name, url rewrites, credential helper);
	// it lives under the denied-by-default $HOME, so grant it read-only.
	// It is a regular FILE — granting it via RODirs (the "ro" bucket) asks
	// Landlock for directory rights (read_dir/execute) on a non-directory,
	// which the kernel rejects with "inconsistent access rights", failing
	// the ENTIRE ruleset and silently degrading the sandbox to no-op. Use a
	// per-file read grant ("rofile") instead.
	rofile := make([]string, 0, 3)
	if spec.GitConfig != "" {
		rofile = append(rofile, spec.GitConfig)
	}
	if spec.GitIgnoreGlobal != "" {
		rofile = append(rofile, spec.GitIgnoreGlobal)
	}
	if spec.SklpUserConfig != "" {
		rofile = append(rofile, spec.SklpUserConfig)
	}

	return []fsAccess{
		{Kind: "rw", Paths: dedupePaths(rw)},
		{Kind: "ro", Paths: dedupePaths(ro)},
		{Kind: "rofile", Paths: dedupePaths(rofile)},
		// Writable device nodes: /dev is ro above, but tools open /dev/null
		// (and friends) for reading AND writing. A per-file rw grant lets
		// them through without opening /dev wholesale.
		{Kind: "rwfile", Paths: dedupePaths(rwDevices)},
	}
}

// symlinkTargetDirs scans each tool dir for symlinks whose resolved target
// lives OUTSIDE that dir, and returns the parent dirs of those targets. This
// lets the sandbox map shim layouts (fnm/corepack: bin/pnpm -> ../lib/...)
// where the executable a bin/ entry points at sits in a sibling tree that
// mounting bin/ alone would not cover. Best-effort: an unreadable dir or a
// broken symlink is silently skipped — the missing grant surfaces as the
// same exec failure the user would have seen without this expansion.
func symlinkTargetDirs(toolDirs []string) []string {
	var extra []string
	for _, dir := range toolDirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.Type()&os.ModeSymlink == 0 {
				continue
			}
			target, err := filepath.EvalSymlinks(filepath.Join(dir, e.Name()))
			if err != nil {
				continue
			}
			targetDir := filepath.Dir(target)
			// Already covered by the bin dir itself (intra-dir symlink).
			if targetDir == filepath.Clean(dir) {
				continue
			}
			extra = append(extra, targetDir)
		}
	}
	return extra
}

// isDockerCmd reports whether the step cmdline invokes the docker CLI,
// which is the only case that needs the ~/.docker read-write grant. A
// plain prefix check on the trimmed cmdline is enough: steps run via
// `sh -c`, but the policy only cares whether docker is the program being
// driven, and the publish contract always begins these steps with
// "docker ".
func isDockerCmd(cmdline string) bool {
	return strings.HasPrefix(strings.TrimSpace(cmdline), "docker ")
}

// tmpdirFromEnv extracts TMPDIR from the step env (preferred) or the
// host env, so a step that relies on a non-default temp dir still gets
// write access to it under confinement.
func tmpdirFromEnv(stepEnv map[string]string, hostEnv []string) string {
	if v, ok := stepEnv["TMPDIR"]; ok && v != "" {
		return v
	}
	for _, kv := range hostEnv {
		if strings.HasPrefix(kv, "TMPDIR=") {
			return kv[len("TMPDIR="):]
		}
	}
	return ""
}

// cacheDirs returns the read-only cache roots for the FS policy. Empty
// input yields nil so computeFSPolicy adds no grant.
func cacheDirs(toolsCacheDir string) []string {
	if toolsCacheDir == "" {
		return nil
	}
	return []string{toolsCacheDir}
}

// dedupePaths drops empty entries and duplicates while preserving order.
func dedupePaths(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, p := range in {
		if p == "" {
			continue
		}
		if _, ok := seen[p]; ok {
			continue
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}
	return out
}

// composeEnv builds the final KEY=VALUE slice for the sandboxed
// process. It is exposed (lower-case in-package) so the env tests can
// drive it directly without spawning a shell.
func composeEnv(spec sandboxSpec) []string {
	host := filterHostEnv(spec.HostEnv, sandboxAllowlist)
	host = prependEnvPath(host, spec.ToolDirs)

	// Layer step env (already pipeline+step merged) onto the
	// allowlist. Step env wins over allowlist.
	stepEnv := spec.StepEnv
	// Always tell Git to treat the repo as a safe directory. In hosted
	// CI the clone and the step often run under different UIDs, which
	// makes git refuse with "dubious ownership" (CVE-2022-24765). The
	// failure surfaces as the same exit-128 message Go prints for the
	// worktree bug, so dropping this would leave hosted runs broken in
	// a way that looks identical. Using GIT_CONFIG_COUNT/KEY/VALUE env
	// is the non-intrusive equivalent of `git config --global` — it
	// applies to every `git` invocation the step (or Go) makes,
	// without touching ~/.gitconfig.
	if spec.Repo != "" {
		stepEnv = withGitSafeDirectory(stepEnv, spec.Repo)
	}
	stepEnv = withGOMAXPROCS(stepEnv, cpuBudget())
	if !writesGoManifests(spec.Cmdline) {
		stepEnv = withGoflagsModReadonly(stepEnv)
	}
	if isGitWorktree(spec.Repo) {
		// Go ≤1.26 cannot read VCS info when `.git` is a worktree file
		// (golang/go#58218). Every `go build`/`go test`/`go vet`/
		// `go list` aborts with "error obtaining VCS status: exit
		// status 128". We work around by forcing `-buildvcs=false`
		// into GOFLAGS for the step. Production builds (CI hosted, no
		// worktree) are untouched and keep their vcs.revision stamp.
		// Slated for removal once Go 1.27 lands the upstream fix.
		stepEnv = withGoflagsBuildVCSOff(stepEnv)
	}
	out := overlayEnv(host, stepEnv)

	// Apply opted-in secrets last so they cannot be silently shadowed
	// by a `run:` author who reuses the same key in `env:`.
	if len(spec.Secrets) > 0 {
		secEnv := make(map[string]string, len(spec.Secrets))
		for _, name := range spec.Secrets {
			if v, ok := spec.AllSecret[name]; ok {
				secEnv[name] = v
			}
		}
		out = overlayEnv(out, secEnv)
	}
	return out
}

// isGitWorktree reports whether repoDir is the root of a Git worktree
// (as opposed to a plain checkout). A worktree has a regular file
// named `.git` containing a `gitdir:` pointer; a plain checkout has a
// directory. We test for the regular-file case directly because that
// is the trigger for golang/go#58218.
//
// Returns false on any stat error (missing, permission, ...): in those
// cases either there is no git at all (nothing to work around) or the
// sandbox cannot reach `.git` (a different problem with its own fix).
func isGitWorktree(repoDir string) bool {
	if repoDir == "" {
		return false
	}
	info, err := os.Lstat(filepath.Join(repoDir, ".git"))
	if err != nil {
		return false
	}
	return info.Mode().IsRegular()
}

// withGitSafeDirectory returns a copy of env that declares repo as a
// Git safe directory via the GIT_CONFIG_COUNT/KEY_n/VALUE_n protocol
// (git ≥2.31). If the caller already injected GIT_CONFIG_* entries
// (e.g. a `gitconfig:` step env), our entry is appended after the
// existing ones and the count is bumped accordingly. The user's
// entries keep precedence per Git's left-to-right resolution rules.
//
// Returns env unchanged if it already declares safe.directory for any
// path (we trust the caller knew what they were doing).
func withGitSafeDirectory(env map[string]string, repo string) map[string]string {
	if env != nil {
		// Detect existing safe.directory entries in any GIT_CONFIG_KEY_n.
		// Cheap O(n) scan over the small env map; we want the early-exit
		// so the caller's config wins.
		for k, v := range env {
			if strings.HasPrefix(k, "GIT_CONFIG_KEY_") && v == "safe.directory" {
				return env
			}
		}
	}
	out := make(map[string]string, len(env)+3)
	for k, v := range env {
		out[k] = v
	}
	// Parse existing count, default 0. Malformed values reset to 0
	// rather than erroring: the worst case is we shadow the user's
	// (already broken) value with our own correct one.
	count := 0
	if existing, ok := out["GIT_CONFIG_COUNT"]; ok {
		if n, err := strconv.Atoi(existing); err == nil && n >= 0 {
			count = n
		}
	}
	out["GIT_CONFIG_KEY_"+strconv.Itoa(count)] = "safe.directory"
	out["GIT_CONFIG_VALUE_"+strconv.Itoa(count)] = repo
	out["GIT_CONFIG_COUNT"] = strconv.Itoa(count + 1)
	return out
}

// goManifestWriters are the Go subcommands whose whole job is to
// rewrite go.mod/go.sum/go.work. `-mod=readonly` would make them fail
// instead of doing their work, so steps running one are left alone.
var goManifestWriters = []string{
	"go mod ",
	"go work ",
	"go get ",
	"go install ",
	"go generate ",
}

// writesGoManifests reports whether cmdline runs a Go subcommand that
// legitimately edits go.mod/go.sum. Matching is textual on the command
// line because the sandbox runs it through a shell and never parses it
// into argv; a step chaining several commands (`go mod tidy && go
// build`) counts as a writer, since the writing half must still work.
func writesGoManifests(cmdline string) bool {
	norm := strings.Join(strings.Fields(cmdline), " ")
	for _, w := range goManifestWriters {
		if strings.Contains(norm, w) {
			return true
		}
	}
	return false
}

// withGoflagsModReadonly returns a copy of env with `-mod=readonly`
// appended to GOFLAGS, unless the user already set `-mod=` explicitly
// (their choice wins — a step may legitimately want `-mod=mod`).
//
// Without it, any `go build`/`go test`/`go vet` run outside a workspace
// (GOWORK=off) resolves each module on its own and WRITES the missing
// requirements straight into the user's checkout, because the repo is
// bind-mounted read-write at /workspace. The rewrite is usually
// byte-identical, but it changes mtime/inode and so invalidates git's
// stat cache: `git status` reports modified files that `git diff` shows
// as empty, and a pipeline gated on `clean_tree` refuses the next run.
// A task must never mutate the caller's source tree.
func withGoflagsModReadonly(env map[string]string) map[string]string {
	const key, flag = "GOFLAGS", "-mod=readonly"
	existing := env[key]
	if strings.Contains(existing, "-mod=") {
		return env
	}
	out := make(map[string]string, len(env)+1)
	for k, v := range env {
		out[k] = v
	}
	if existing == "" {
		out[key] = flag
	} else {
		out[key] = existing + " " + flag
	}
	return out
}

// withGoflagsBuildVCSOff returns a copy of env with `-buildvcs=false`
// appended to GOFLAGS, unless the user already set `-buildvcs=` to
// some explicit value (in which case we respect their choice — they
// may be intentionally requesting buildvcs=true elsewhere). Nil/empty
// env is handled.
func withGoflagsBuildVCSOff(env map[string]string) map[string]string {
	const key, flag = "GOFLAGS", "-buildvcs=false"
	existing := env[key]
	if strings.Contains(existing, "-buildvcs=") {
		return env
	}
	out := make(map[string]string, len(env)+1)
	for k, v := range env {
		out[k] = v
	}
	if existing == "" {
		out[key] = flag
	} else {
		out[key] = existing + " " + flag
	}
	return out
}

// filterHostEnv keeps only the allowlisted keys from a KEY=VALUE list.
func filterHostEnv(env, allow []string) []string {
	if len(env) == 0 {
		return nil
	}
	allowSet := make(map[string]struct{}, len(allow))
	for _, k := range allow {
		allowSet[k] = struct{}{}
	}
	out := make([]string, 0, len(allow))
	for _, kv := range env {
		eq := strings.IndexByte(kv, '=')
		if eq <= 0 {
			continue
		}
		if _, ok := allowSet[kv[:eq]]; ok {
			out = append(out, kv)
		}
	}
	return out
}

// resolveSandboxCwd joins repo + cwd and asserts the result stays
// under the repo root after symlink-free cleaning. The compiler
// already rejects "../"-style escapes; this is the runtime backstop.
func resolveSandboxCwd(repo, cwd string) (string, error) {
	if cwd == "" {
		return repo, nil
	}
	joined := filepath.Clean(filepath.Join(repo, cwd))
	rel, err := filepath.Rel(repo, joined)
	if err != nil {
		return "", fmt.Errorf("resolve cwd %q: %w", cwd, err)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("cwd %q escapes the repo root", cwd)
	}
	return joined, nil
}

// materialiseSecrets writes each opted-in secret to <SecretDir>/<NAME>
// with 0600 permissions and returns a cleanup function. /run/secrets
// is preferred when writable; otherwise the caller passes a temp dir.
func materialiseSecrets(dir string, names []string, values map[string]string) (func(), error) {
	if len(names) == 0 {
		return func() {}, nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create secrets dir: %w", err)
	}
	written := make([]string, 0, len(names))
	for _, name := range names {
		v, ok := values[name]
		if !ok {
			continue
		}
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(v), 0o600); err != nil {
			for _, p := range written {
				_ = os.Remove(p)
			}
			return nil, fmt.Errorf("write secret %s: %w", name, err)
		}
		written = append(written, path)
	}
	cleanup := func() {
		for _, p := range written {
			_ = os.Remove(p)
		}
	}
	return cleanup, nil
}

// chooseSecretDir prefers /run/secrets when writable (matches the
// container spawners' convention) and falls back to a per-run temp
// directory for dev hosts where /run/secrets is read-only or missing.
//
// The fallback is SCOPED under <cacheRoot>/secrets ("run-*") instead of
// the /tmp root: a SIGKILL'd run skips the deferred cleanup, leaving an
// orphan dir, and a scoped parent lets reapOrphanSecretDirs sweep only
// our own residue (never arbitrary /tmp). The deferred cleanup stays the
// primary path; the reaper is the safety net for killed runs.
func chooseSecretDir() (dir string, cleanup func(), err error) {
	const preferred = "/run/secrets"
	if probeWritable(preferred) {
		return preferred, func() {}, nil
	}
	root, err := secretsCacheRoot()
	if err != nil {
		return "", nil, fmt.Errorf("secrets dir: %w", err)
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", nil, fmt.Errorf("create secrets root: %w", err)
	}
	tmp, err := os.MkdirTemp(root, "run-")
	if err != nil {
		return "", nil, fmt.Errorf("temp secrets dir: %w", err)
	}
	return tmp, func() { _ = os.RemoveAll(tmp) }, nil
}

// secretsCacheRoot is the dedicated parent of the per-run secret dirs:
// $XDG_CACHE_HOME/sklp/secrets (or ~/.cache/sklp/secrets), a sibling of
// the plan cache. Scoping the fallback here (instead of /tmp) bounds what
// reapOrphanSecretDirs may delete to a directory we exclusively own.
func secretsCacheRoot() (string, error) {
	if x := strings.TrimSpace(os.Getenv("XDG_CACHE_HOME")); x != "" {
		return filepath.Join(x, "sklp", "secrets"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".cache", "sklp", "secrets"), nil
}

// reapOrphanSecretDirs removes leftover per-run secret dirs ("run-*")
// under the scoped secrets root — the residue of runs killed by SIGKILL
// before their deferred cleanup ran. Best-effort and SAFE: it only ever
// touches entries under the directory we own, never /tmp or any path we
// did not create. Errors are ignored (a missing root just means nothing
// to reap). Call it early in a run as the safety net behind the per-run
// defer cleanup.
func reapOrphanSecretDirs() {
	root, err := secretsCacheRoot()
	if err != nil {
		return
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return // no root yet (fresh host) or unreadable: nothing to reap
	}
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), "run-") {
			continue
		}
		_ = os.RemoveAll(filepath.Join(root, e.Name()))
	}
}

func probeWritable(dir string) bool {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return false
	}
	f, err := os.CreateTemp(dir, ".sklp-probe-")
	if err != nil {
		return false
	}
	name := f.Name()
	_ = f.Close()
	_ = os.Remove(name)
	return true
}

// runSandboxed is the shared spawn helper. exec_ci.go calls it for
// each included step; the cache path (2.3) will wrap it without
// changing this signature.
func runSandboxed(ctx context.Context, spec sandboxSpec) (int, error) {
	workdir, err := resolveSandboxCwd(spec.Repo, spec.Cwd)
	if err != nil {
		return -1, err
	}

	cleanup, err := materialiseSecrets(spec.SecretDir, spec.Secrets, spec.AllSecret)
	if err != nil {
		return -1, err
	}
	defer cleanup()

	runCtx := ctx
	if spec.Timeout > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, spec.Timeout)
		defer cancel()
	}

	env := composeEnv(spec)
	build := func(withNetIsolation bool) *exec.Cmd {
		c := exec.CommandContext(runCtx, "sh", "-c", spec.Cmdline)
		c.Dir = workdir
		c.Env = env
		if spec.Stdout != nil {
			c.Stdout = spec.Stdout
		}
		if spec.Stderr != nil {
			c.Stderr = spec.Stderr
		}
		attr := &syscall.SysProcAttr{Setpgid: true}
		// Reap the child if the parent dies brutally (killed go test / sklp run):
		// without this its namespaced descendants reparent to PID 1 and linger —
		// repeated interrupted runs leaked 1000+ orphan sandbox processes.
		applyParentDeathSignal(attr)
		if withNetIsolation {
			applyNetworkIsolation(attr, spec)
		}
		c.SysProcAttr = attr
		// Filesystem confinement self-restricts the child before exec
		// (Linux/Landlock); composes with the netns unshare above. On
		// macOS / Landlock-less kernels it degrades to a logged no-op.
		applyFilesystemIsolation(c, spec, workdir)
		return c
	}

	cmd := build(true)
	if err := cmd.Start(); err != nil {
		// On hosts without CAP_SYS_ADMIN / userns nesting Linux refuses
		// the unshare. Fall back to host network with a clear warning
		// rather than failing the step.
		if spec.Network == "none" && isNetnsPermError(err) {
			if spec.Stderr != nil {
				fmt.Fprintf(spec.Stderr, "[sklp] network: none requested but namespace setup failed (%v); running with host network\n", err)
			}
			cmd = build(false)
			if err := cmd.Start(); err != nil {
				return -1, err
			}
		} else {
			return -1, err
		}
	}

	// Propagate cancellation to the whole process group. exec.Cmd's
	// own ctx hook only sends SIGKILL to the leader; here we send
	// SIGTERM to the group first so well-behaved children get a chance
	// to clean up before SIGKILL.
	doneCh := make(chan error, 1)
	go func() { doneCh <- cmd.Wait() }()

	select {
	case <-runCtx.Done():
		signalGroup(cmd.Process.Pid, syscall.SIGTERM)
		select {
		case err := <-doneCh:
			return exitCodeFrom(err), err
		case <-time.After(2 * time.Second):
			signalGroup(cmd.Process.Pid, syscall.SIGKILL)
			err := <-doneCh
			return exitCodeFrom(err), err
		}
	case err := <-doneCh:
		if err != nil {
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				return ee.ExitCode(), nil
			}
			return -1, err
		}
		return 0, nil
	}
}

func signalGroup(pid int, sig syscall.Signal) {
	// Negative PID targets the process group on POSIX. Errors are
	// expected when the group is already gone; ignore them.
	_ = syscall.Kill(-pid, sig)
}

func exitCodeFrom(err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return -1
}
