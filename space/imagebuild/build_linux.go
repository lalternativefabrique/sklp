//go:build linux

// Package imagebuild (runtime) executes a parsed multi-stage Dockerfile into a
// final rootfs, daemonless. It is the runtime half of the application-image
// builder; the pure parser lives in runner/imagebuild.
//
// Each stage is materialised as a rootfs DIRECTORY: PrepareImage extracts the
// base (or a prior stage's rootfs is copied for `FROM <stage>`), then COPY/RUN
// instructions mutate it. COPY --from=<stage> is a directory copy between two
// rootfs we own — the cheap multi-stage trick. The final stage's rootfs is the
// build output (Phase 1); OCI layer/push is layered on top later (Phase 2).
package imagebuild

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	parse "github.com/lalternativefabrique/sklp/pipeline/imagebuild"
	"github.com/lalternativefabrique/sklp/space"
	"github.com/lalternativefabrique/sklp/space/platform"
	"github.com/lalternativefabrique/sklp/space/runnerbuild"
)

// Options drive a build. ContextDir is the host build context (where COPY <src>
// reads from). Secrets maps a secret id to a host file path (RUN --mount=
// type=secret,id=X bind-mounts Secrets["X"] at the target during that RUN).
// CacheDir, when set, is a persistent host dir whose subdirs are bind-mounted at
// the standard toolchain cache paths during RUNs (go mod/build, pnpm/npm store),
// so go build / pnpm install reuse downloads + incremental output across builds
// without the cache landing in any layer — the transparent equivalent of
// BuildKit --mount=type=cache, needing no Dockerfile change.
type Options struct {
	ContextDir string
	Secrets    map[string]string // id -> host file path
	BuildArgs  map[string]string // --build-arg overrides for ARG values
	Platform   platform.Platform // target os/arch; zero = host
	CacheDir   string            // persistent host cache root ("" = no build cache)
	// Progress, when set, receives one human-readable line per build phase
	// (pull base, copy rootfs, each RUN/COPY, export) so the long, otherwise
	// silent steps — the ~1 GB `cp -a` of a base rootfs, the context scan — show
	// activity instead of looking hung. nil = silent.
	Progress io.Writer

	ignorer *parse.Ignorer // loaded from ContextDir/.dockerignore at build start
}

// progressf writes one progress line (no-op when Progress is nil).
func (o Options) progressf(format string, a ...any) {
	if o.Progress == nil {
		return
	}
	fmt.Fprintf(o.Progress, "    [native] "+format+"\n", a...)
}

// timed runs fn and reports how long it took on the progress stream, so a build
// log says WHERE the wall-clock went instead of only what ran. Phases shorter
// than timedFloor stay silent: a 3-line build should not drown in "0.0s" noise.
// The label is emitted after fn so the duration is known — callers that must
// announce work BEFORE it starts (a long RUN) still progressf() first.
func (o Options) timed(label string, fn func() error) error {
	if o.Progress == nil {
		return fn()
	}
	start := time.Now()
	err := fn()
	if d := time.Since(start); d >= timedFloor {
		o.progressf("  ↳ %s took %.1fs", label, d.Seconds())
	}
	return err
}

// timedFloor is the shortest phase worth reporting a duration for.
const timedFloor = 500 * time.Millisecond

// standardCacheMounts derives the toolchain cache bind-mounts from a persistent
// host cache root. The targets are the default locations go/pnpm/npm read when
// HOME=/root (the step runner's HOME) — so an unmodified `go build` / `pnpm
// install` finds a warm cache. Each source is a subdir of cacheDir, shared
// across all builds on this host.
func standardCacheMounts(cacheDir string) []runnerbuild.CacheMount {
	if cacheDir == "" {
		return nil
	}
	j := func(p string) string { return filepath.Join(cacheDir, p) }
	return []runnerbuild.CacheMount{
		{Source: j("go-mod"), Target: "/root/go/pkg/mod"},
		{Source: j("go-build"), Target: "/root/.cache/go-build"},
		{Source: j("npm"), Target: "/root/.npm"},
		{Source: j("bun"), Target: "/root/.bun/install/cache"},
	}
}

// Result is a completed build: the final stage's rootfs dir plus the OCI config
// metadata gathered from ENV/WORKDIR/USER/ENTRYPOINT/CMD/EXPOSE.
type Result struct {
	Rootfs     string
	Platform   platform.Platform
	Env        []string // KEY=value, declaration order
	Workdir    string
	User       string
	Entrypoint []string
	Cmd        []string
	Exposed    []string // ports as declared
}

// Build runs every stage of df in order and returns the final stage rootfs +
// config. workDir is a scratch dir (under the cache) where per-stage rootfs are
// materialised; it is the caller's to clean up. steps executes RUN commands
// (UnshareStepRunner in production).
func Build(ctx context.Context, cacheRoot, workDir string, df *parse.Dockerfile, opts Options, steps runnerbuild.StepRunner) (Result, error) {
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		return Result{}, err
	}
	// Load .dockerignore from the context root so host-context COPYs skip .git,
	// node_modules, secrets, etc. Absent file => ignore nothing. It applies ONLY
	// to copies from the host context, never to COPY --from=<stage>.
	if opts.ContextDir != "" {
		if b, err := os.ReadFile(filepath.Join(opts.ContextDir, ".dockerignore")); err == nil {
			opts.ignorer = parse.ParseDockerignore(string(b))
		}
	}
	// stageRootfs[i] is the on-disk rootfs of stage i once built. Needed so a
	// later COPY --from=<i or name> can read from it.
	stageRootfs := make([]string, len(df.Stages))

	// stageRes[i] is stage i's final Result (config after its instructions ran),
	// so a FROM <stage> inherits the referenced stage's full config the way Docker
	// does, not just its base image's.
	stageRes := make([]Result, len(df.Stages))

	// stageCfg[i] is stage i's inherited base config (env + default workdir/user/
	// entrypoint/cmd). A FROM <stage> inherits the referenced stage's config; a
	// FROM image inherits the pulled image's. The env is threaded into each RUN so
	// `go`/`node` resolve, and the rest seeds the stage Result as Docker defaults.
	stageCfg := make([]space.BaseConfig, len(df.Stages))

	if opts.Platform.IsZero() {
		opts.Platform = platform.Host()
	}
	opts.BuildArgs = withPlatformArgs(opts.BuildArgs, opts.Platform)

	// Global ARGs (pre-FROM) form the build-arg scope visible to FROM lines and,
	// after an in-stage redeclaration, to a stage. --build-arg overrides win.
	globalArgs := resolveGlobalArgs(df.GlobalArgs, opts.BuildArgs)

	for i := range df.Stages {
		st := &df.Stages[i]
		rootfs := filepath.Join(workDir, fmt.Sprintf("stage-%d", i))
		// Expand ${VAR} in the FROM image ref against the global-arg scope
		// (e.g. FROM golang:${GO_VERSION}). Only image-ref FROMs; a FROM <stage>
		// names a prior stage and is left untouched.
		if st.BaseStageRef == "" {
			st.BaseImage = parse.Expand(st.BaseImage, globalArgs)
		}
		stageLabel := st.BaseImage
		if st.Name != "" {
			stageLabel = st.Name + " (" + st.BaseImage + ")"
		}
		opts.progressf("stage %d/%d: %s", i+1, len(df.Stages), stageLabel)

		// Materialise the stage base: either a prior stage's rootfs (FROM build)
		// or a pulled image (FROM golang:1.25), capturing the inherited config.
		if st.BaseStageRef != "" {
			src, ok := stageRootfsByRef(df, stageRootfs, st.BaseStageRef)
			if !ok {
				return Result{}, fmt.Errorf("stage %d: FROM %q references an unbuilt stage", i, st.BaseStageRef)
			}
			opts.progressf("  copying stage %q rootfs", st.BaseStageRef)
			if err := opts.timed("copy stage rootfs", func() error { return copyTree(src, rootfs) }); err != nil {
				return Result{}, fmt.Errorf("stage %d: copy base stage: %w", i, err)
			}
			stageCfg[i] = cfgForStageRef(df, stageRes, st.BaseStageRef)
		} else {
			opts.progressf("  pulling + extracting base %s", st.BaseImage)
			var baseRootfs string
			var cfg space.BaseConfig
			err := opts.timed("pull+extract base", func() error {
				var e error
				baseRootfs, cfg, e = space.PrepareImageWithConfigFor(ctx, cacheRoot, st.BaseImage, opts.Platform.Arch)
				return e
			})
			if err != nil {
				return Result{}, fmt.Errorf("stage %d: pull base %q: %w", i, st.BaseImage, err)
			}
			opts.progressf("  copying base rootfs (this can take a moment for a large base)")
			if err := opts.timed("copy base rootfs", func() error { return copyTree(baseRootfs, rootfs) }); err != nil {
				return Result{}, fmt.Errorf("stage %d: copy base image: %w", i, err)
			}
			stageCfg[i] = cfg
		}
		stageRootfs[i] = rootfs

		// Seed the stage Result with the base config so a Dockerfile that omits
		// WORKDIR/USER/ENTRYPOINT/CMD inherits the base's, like `docker build`.
		// Instructions in runStage override these per Docker semantics.
		res := seedResult(rootfs, stageCfg[i])
		res.Platform = opts.Platform
		if err := runStage(ctx, df, stageRootfs, i, rootfs, opts, steps, stageCfg[i].Env, globalArgs, &res); err != nil {
			return Result{}, err
		}
		stageRes[i] = res
		// Only the final stage's config is the build output.
		if i == len(df.Stages)-1 {
			res.Rootfs = rootfs
			return res, nil
		}
	}
	return Result{}, fmt.Errorf("empty Dockerfile")
}

// cfgForStageRef returns the inherited config of the stage a FROM <stage>
// references, taken from that stage's final Result, so a stage built on a prior
// stage inherits its PATH and default workdir/user/entrypoint/cmd (Docker copies
// the referenced stage's full config to the new stage's base).
func cfgForStageRef(df *parse.Dockerfile, stageRes []Result, ref string) space.BaseConfig {
	for i := range df.Stages {
		if df.Stages[i].Name == ref {
			r := stageRes[i]
			return space.BaseConfig{
				Env:        r.Env,
				WorkingDir: r.Workdir,
				User:       r.User,
				Entrypoint: r.Entrypoint,
				Cmd:        r.Cmd,
			}
		}
	}
	return space.BaseConfig{}
}

// seedResult builds the initial stage Result from the inherited base config:
// the base Env (so the final image keeps the base's PATH and a RUN resolves
// go/node), and the default Workdir/User/Entrypoint/Cmd an omitted instruction
// inherits. A Dockerfile ENV upserts onto this base env (Docker semantics).
func seedResult(rootfs string, base space.BaseConfig) Result {
	env := make([]string, len(base.Env))
	copy(env, base.Env)
	return Result{
		Rootfs:     rootfs,
		Env:        env,
		Workdir:    base.WorkingDir,
		User:       base.User,
		Entrypoint: base.Entrypoint,
		Cmd:        base.Cmd,
	}
}

// withPlatformArgs adds Docker's predefined platform ARGs (TARGETPLATFORM,
// TARGETOS, TARGETARCH, BUILDPLATFORM, BUILDOS, BUILDARCH) unless the caller
// set them, so a Dockerfile can cross-compile with GOARCH=$TARGETARCH.
func withPlatformArgs(args map[string]string, target platform.Platform) map[string]string {
	build := platform.Host()
	out := make(map[string]string, len(args)+6)
	for k, v := range args {
		out[k] = v
	}
	for k, v := range map[string]string{
		"TARGETPLATFORM": target.String(),
		"TARGETOS":       target.OS,
		"TARGETARCH":     target.Arch,
		"BUILDPLATFORM":  build.String(),
		"BUILDOS":        build.OS,
		"BUILDARCH":      build.Arch,
	} {
		if _, ok := out[k]; !ok {
			out[k] = v
		}
	}
	return out
}

// resolveGlobalArgs evaluates the pre-FROM ARG declarations into a Vars table:
// each ARG's default (with ${...} expanded against args resolved so far), then
// overridden by any matching --build-arg.
func resolveGlobalArgs(decls []parse.Instruction, buildArgs map[string]string) parse.Vars {
	v := parse.Vars{}
	for _, in := range decls {
		name, def, hasDefault := parse.ParseArg(in.Args)
		if hasDefault {
			v[name] = parse.Expand(def, v)
		}
		if ov, ok := buildArgs[name]; ok {
			v[name] = ov
		}
	}
	return v
}

// declaredNames collects every variable name stage idx could legitimately refer
// to: the pre-FROM global ARGs, every ARG the stage itself declares (wherever it
// sits, so a forward reference is still recognised as a Dockerfile name), and
// the base image's env keys. Anything outside this set is a shell variable as
// far as the builder is concerned and must survive expansion untouched.
func declaredNames(df *parse.Dockerfile, idx int, globalArgs parse.Vars, baseEnv []string) map[string]bool {
	out := make(map[string]bool, len(globalArgs)+len(baseEnv))
	for name := range globalArgs {
		out[name] = true
	}
	for _, in := range df.GlobalArgs {
		if name, _, _ := parse.ParseArg(in.Args); name != "" {
			out[name] = true
		}
	}
	for _, in := range df.Stages[idx].Instructions {
		if in.Op != parse.OpArg {
			continue
		}
		if name, _, _ := parse.ParseArg(in.Args); name != "" {
			out[name] = true
		}
	}
	for _, e := range baseEnv {
		if k, _, ok := strings.Cut(e, "="); ok {
			out[k] = true
		}
	}
	return out
}

// runStage executes one stage's instructions against its rootfs, accumulating
// config metadata into res. globalArgs is the pre-FROM build-arg scope; an
// in-stage `ARG NAME` pulls NAME into this stage's variable table (Docker
// semantics: global args are not visible until redeclared).
func runStage(ctx context.Context, df *parse.Dockerfile, stageRootfs []string, idx int, rootfs string, opts Options, steps runnerbuild.StepRunner, baseEnv []string, globalArgs parse.Vars, res *Result) error {
	st := &df.Stages[idx]
	// vars is this stage's build-time variable table (in-scope ARG + ENV values)
	// used to expand ${VAR}/$VAR in instruction arguments.
	vars := parse.Vars{}
	// declared holds every name the Dockerfile knows about, whether or not it is
	// in scope here. A name in vars expands to its value; a declared name absent
	// from vars expands to "" (an unredeclared global ARG, Docker semantics); a
	// name in neither is left verbatim for the shell, since a RUN body may define
	// and use its own shell variables the builder must not touch.
	declared := declaredNames(df, idx, globalArgs, baseEnv)
	var pendingRuns []string
	// curWorkdir tracks the in-stage WORKDIR: every RUN runs from it (Docker
	// semantics) and a relative COPY dest is resolved against it. Seeded from the
	// base image's WorkingDir (res.Workdir); "/" when the base sets none.
	curWorkdir := res.Workdir
	if curWorkdir == "" {
		curWorkdir = "/"
	}

	// stageRunner re-stages the runner with the base image env plus the
	// Dockerfile ENV accumulated so far (res.Env), so RUN sees ENV declared
	// earlier in the stage — e.g. www's VITE_* set from ARG before
	// `pnpm run build`. WithStage exports imageEnv before every RUN, identically
	// across all runner impls. Re-staged per flush since res.Env grows as ENV
	// instructions are processed.
	stageRunner := func() runnerbuild.StepRunner {
		return steps.WithStage(mergeEnv(baseEnv, res.Env), standardCacheMounts(opts.CacheDir))
	}

	flushRuns := func() error {
		if len(pendingRuns) == 0 {
			return nil
		}
		// Batched RUNs execute here, not where their progress line was printed,
		// so this is the only place their real cost can be attributed.
		label := fmt.Sprintf("%d RUN(s)", len(pendingRuns))
		err := opts.timed(label, func() error { return stageRunner().Exec(ctx, rootfs, pendingRuns) })
		pendingRuns = nil
		return err
	}
	// withCd prefixes a RUN body with `cd <curWorkdir> &&` so it executes in the
	// current WORKDIR. At "/" it is a no-op prefix (cd / is harmless).
	withCd := func(cmd string) string {
		return "cd " + shellQuoteArg(curWorkdir) + " && " + cmd
	}

	for _, in := range st.Instructions {
		// ARG controls the variable table and must be handled BEFORE expansion of
		// other instructions; it never itself expands a command. Everything else
		// gets ${VAR}/$VAR expanded against the current stage vars (Docker
		// build-time expansion) — except ENTRYPOINT/CMD, which Docker does not
		// expand at build time.
		if in.Op == parse.OpArg {
			name, def, hasDefault := parse.ParseArg(in.Args)
			switch {
			case opts.BuildArgs[name] != "":
				vars[name] = opts.BuildArgs[name]
			case hasDefault:
				vars[name] = parse.Expand(def, vars)
			default:
				// Bare in-stage `ARG NAME`: pull the value from the global scope
				// if declared there (Docker semantics), else it stays unset.
				if gv, ok := globalArgs[name]; ok {
					vars[name] = gv
				}
			}
			continue
		}
		if in.Op != parse.OpEntrypoint && in.Op != parse.OpCmd {
			in.Args = parse.ExpandKnown(in.Args, vars, declared)
		}
		switch in.Op {
		case parse.OpRun:
			r, err := parse.ParseRun(in.Args)
			if err != nil {
				return fmt.Errorf("stage %d line %d: %w", idx, in.Line, err)
			}
			// A RUN with secret mounts must run on its own with the secret bound,
			// so flush any batched runs first, then run it isolated.
			if len(r.Secrets) > 0 {
				if err := flushRuns(); err != nil {
					return fmt.Errorf("stage %d: run: %w", idx, err)
				}
				opts.progressf("  RUN (secret) %s", abbrev(r.Cmd))
				r.Cmd = withCd(r.Cmd)
				if err := opts.timed("RUN (secret)", func() error {
					return runWithSecrets(ctx, rootfs, r, opts, stageRunner())
				}); err != nil {
					return fmt.Errorf("stage %d line %d: run(secret): %w", idx, in.Line, err)
				}
				continue
			}
			opts.progressf("  RUN %s", abbrev(r.Cmd))
			pendingRuns = append(pendingRuns, withCd(r.Cmd))

		case parse.OpCopy:
			if err := flushRuns(); err != nil {
				return fmt.Errorf("stage %d: run before copy: %w", idx, err)
			}
			opts.progressf("  COPY %s", abbrev(in.Args))
			if err := opts.timed("COPY", func() error {
				return doCopy(df, stageRootfs, idx, rootfs, in, opts, curWorkdir)
			}); err != nil {
				return fmt.Errorf("stage %d line %d: copy: %w", idx, in.Line, err)
			}

		case parse.OpEnv:
			// One ENV may declare several KEY=value pairs (and they may be
			// backslash-continued across lines, joined by the parser). Handle them
			// all, not just the first '=' — else `ENV A=a B=b` mis-parses as
			// A="a B=b". This is what carries www's VITE_* into the build.
			for _, e := range parse.ParseEnv(in.Args) {
				// ENV joins the variable table so later instructions can ${VAR} it.
				vars[e.Key] = e.Value
				declared[e.Key] = true
				res.Env = upsertEnv(res.Env, e.Key, e.Value)
			}
		case parse.OpWorkdir:
			w := strings.TrimSpace(in.Args)
			// WORKDIR may be relative to the previous one (Docker semantics).
			if !strings.HasPrefix(w, "/") {
				w = filepath.Join(curWorkdir, w)
			}
			curWorkdir = w
			res.Workdir = w
			// Create it so a subsequent COPY/RUN into it works even before any
			// command mkdir's it (Docker auto-creates WORKDIR).
			if err := os.MkdirAll(filepath.Join(rootfs, w), 0o755); err != nil {
				return fmt.Errorf("stage %d: workdir %s: %w", idx, w, err)
			}
		case parse.OpUser:
			res.User = strings.TrimSpace(in.Args)
		case parse.OpEntrypoint:
			res.Entrypoint = parse.ParseExec(in.Args)
			// Docker semantics: declaring ENTRYPOINT resets any inherited CMD to
			// nil (the base's default arguments no longer apply to a new entrypoint).
			// A CMD later in this stage re-sets it.
			res.Cmd = nil
		case parse.OpCmd:
			res.Cmd = parse.ParseExec(in.Args)
		case parse.OpExpose:
			res.Exposed = append(res.Exposed, strings.Fields(in.Args)...)
		}
	}
	return flushRuns()
}

// doCopy executes a COPY instruction: from a prior stage rootfs (--from) or the
// host build context, into the current rootfs.
func doCopy(df *parse.Dockerfile, stageRootfs []string, idx int, rootfs string, in parse.Instruction, opts Options, curWorkdir string) error {
	c, err := parse.ParseCopy(in.Args)
	if err != nil {
		return err
	}
	var srcRoot string
	var ig *parse.Ignorer // applied ONLY to host-context copies, never COPY --from
	if c.From != "" {
		src, ok := stageRootfsByRef(df, stageRootfs, c.From)
		if !ok {
			return fmt.Errorf("COPY --from=%q: stage not built", c.From)
		}
		srcRoot = src
	} else {
		srcRoot = opts.ContextDir
		ig = opts.ignorer
	}
	// A relative COPY dest (e.g. `./lib/transcribe/`) is relative to the current
	// WORKDIR (Docker semantics); an absolute dest is used as-is. filepath.Join
	// drops a trailing slash, so carry the "is a directory dest" marker (the
	// original trailing "/") separately for copyInto's file-vs-dir decision.
	dst := c.Dst
	if !strings.HasPrefix(dst, "/") {
		dst = filepath.Join(curWorkdir, dst)
	}
	dstDecl := dst
	if strings.HasSuffix(c.Dst, "/") {
		dstDecl += "/"
	}
	dstAbs := filepath.Join(rootfs, dst)
	for _, s := range c.Srcs {
		// Docker expands wildcards in COPY sources (e.g. `pnpm-lock.yaml*`,
		// `*.json`). Glob each source against its root; a source with no meta
		// character globs to itself, so the literal path is preserved. A pattern
		// that matches nothing is an error (Docker fails the build the same way),
		// surfaced via the stat in copyInto on the un-expanded path.
		matches, gerr := filepath.Glob(filepath.Join(srcRoot, s))
		if gerr != nil || len(matches) == 0 {
			// No match (or a malformed pattern): fall back to the literal path so
			// the error message names the source the user wrote.
			matches = []string{filepath.Join(srcRoot, s)}
		}
		for _, srcAbs := range matches {
			if err := copyInto(srcAbs, dstAbs, dstDecl, srcRoot, ig); err != nil {
				return fmt.Errorf("%s -> %s: %w", s, c.Dst, err)
			}
		}
	}
	return nil
}

// shellQuoteArg single-quotes a path for use in the in-rootfs shell (the WORKDIR
// in a `cd` prefix), escaping embedded single quotes.
func shellQuoteArg(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// abbrev collapses a multi-line / long instruction body to one short line for a
// progress message (the full text is in the captured RUN output anyway).
func abbrev(s string) string {
	s = strings.Join(strings.Fields(s), " ") // collapse newlines/continuations
	const max = 70
	if len(s) > max {
		return s[:max] + "…"
	}
	return s
}

// stageRootfsByRef resolves a --from / FROM stage reference (name or index) to
// its built rootfs path.
func stageRootfsByRef(df *parse.Dockerfile, stageRootfs []string, ref string) (string, bool) {
	for i := range df.Stages {
		if df.Stages[i].Name == ref && stageRootfs[i] != "" {
			return stageRootfs[i], true
		}
	}
	var idx int
	if _, err := fmt.Sscanf(ref, "%d", &idx); err == nil && idx >= 0 && idx < len(stageRootfs) && stageRootfs[idx] != "" {
		return stageRootfs[idx], true
	}
	return "", false
}

func upsertEnv(env []string, k, v string) []string {
	for i, kv := range env {
		if strings.HasPrefix(kv, k+"=") {
			env[i] = k + "=" + v
			return env
		}
	}
	return append(env, k+"="+v)
}

// mergeEnv overlays Dockerfile ENV (over) onto the base image env (base): a key
// in over replaces the same key in base (Docker semantics — ENV shadows the base
// image's value), new keys are appended in declaration order. Neither input is
// mutated; over is already deduped (built via upsertEnv).
func mergeEnv(base, over []string) []string {
	out := make([]string, len(base))
	copy(out, base)
	for _, kv := range over {
		k, v, _ := strings.Cut(kv, "=")
		out = upsertEnv(out, k, v)
	}
	return out
}

// cpA runs `cp -a <args>` from inside the SAME mapped-root user namespace the RUN
// steps (and tarRootfsInUserns) use, so it can read/create/chown files owned by
// the build's mapped subuid range. A stage rootfs frequently holds such files:
// `RUN chown app:app .next` (Docker's Next.js standalone pattern) makes .next
// owned by container uid 1001 → host subuid 101000, which the caller's real uid
// cannot write into. Outside the userns `cp` fails with "Permission denied" /
// "Operation not permitted" (preserving times on a subuid-owned dir).
//
// When this process is itself already nested in a user namespace (a Space CI
// runner), it is already root against that mapping — cp runs directly, with no
// `unshare --user`: a nested child uid_map may only name uids the parent maps,
// and writing even --map-root-user's identity map is refused there.
//
// Real root of the initial namespace (the macOS vz guest, a privileged CI pod)
// also runs cp directly: --map-root-user alone maps only id 0, so cp's chown to
// any other owner of the source (gid shadow, utmp, mail…) fails with EINVAL.
func cpA(args ...string) error {
	argv := cpArgv(runnerbuild.NestedUserns(), os.Getuid(), args)
	out, err := exec.Command(argv[0], argv[1:]...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, string(out))
	}
	return nil
}

func cpArgv(nested bool, uid int, args []string) []string {
	if nested || uid == 0 {
		return append([]string{"cp", "-a"}, args...)
	}
	argv := []string{"unshare", "--user", "--map-root-user",
		"--map-users=1:100000:65535", "--map-groups=1:100000:65535",
		"--fork", "cp", "-a"}
	return append(argv, args...)
}

// copyTree copies a whole rootfs (cp -a, preserves modes/owners/symlinks/devs)
// inside the mapped-root userns so subuid-owned files copy faithfully.
func copyTree(src, dst string) error {
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	return cpA(src+"/.", dst)
}

// copyInto copies a host/stage path into the destination, creating parent dirs.
// If dst ends in "/" (or src is a dir), src is copied as a child; otherwise src
// is copied to the exact dst path (Docker COPY file semantics). When ig is non-
// nil (a host-context copy), paths matching .dockerignore are skipped; ctxRoot
// is the context root so ignore patterns are matched against context-relative
// paths.
func copyInto(srcAbs, dstAbs, dstDecl, ctxRoot string, ig *parse.Ignorer) error {
	fi, err := os.Stat(srcAbs)
	if err != nil {
		return err
	}
	// A single ignored file: nothing to copy.
	if ig != nil && !fi.IsDir() {
		if rel, ok := contextRel(ctxRoot, srcAbs); ok && ig.Ignored(rel) {
			return nil
		}
	}
	// Docker COPY semantics:
	//   - a DIRECTORY source: its CONTENTS are copied into dst (the dir itself is
	//     never nested), dst created as needed.
	//   - a FILE source with dst ending in "/": the file lands at dst/<basename>.
	//   - a FILE source with an exact dst: copied to that exact path.
	if fi.IsDir() {
		if err := os.MkdirAll(dstAbs, 0o755); err != nil {
			return err
		}
		if ig != nil {
			return copyTreeFiltered(srcAbs, dstAbs, ctxRoot, ig)
		}
		return cpA(srcAbs+"/.", dstAbs)
	}
	// File source.
	target := dstAbs
	if strings.HasSuffix(dstDecl, "/") {
		if err := os.MkdirAll(dstAbs, 0o755); err != nil {
			return err
		}
		target = filepath.Join(dstAbs, filepath.Base(srcAbs))
	} else if err := os.MkdirAll(filepath.Dir(dstAbs), 0o755); err != nil {
		return err
	}
	return cpA(srcAbs, target)
}

// copyTreeFiltered walks src (a context dir) and copies it to dst, skipping any
// path the .dockerignore matches (relative to ctxRoot). An ignored directory is
// pruned (its subtree not walked).
func copyTreeFiltered(src, dst, ctxRoot string, ig *parse.Ignorer) error {
	return filepath.WalkDir(src, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, ok := contextRel(ctxRoot, p)
		if ok && ig.Ignored(rel) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		inner, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, inner)
		if d.IsDir() {
			info, e := d.Info()
			if e != nil {
				return e
			}
			return os.MkdirAll(target, info.Mode().Perm())
		}
		// cp -a one file preserves mode/owner/symlink; simplest correct copy.
		out, e := exec.Command("cp", "-a", p, target).CombinedOutput()
		if e != nil {
			return fmt.Errorf("%w: %s", e, string(out))
		}
		return nil
	})
}

// contextRel turns an absolute path under ctxRoot into a slash, context-relative
// path for ignore matching. ok=false if p is not under ctxRoot.
func contextRel(ctxRoot, p string) (string, bool) {
	rel, err := filepath.Rel(ctxRoot, p)
	if err != nil || strings.HasPrefix(rel, "..") {
		return "", false
	}
	return filepath.ToSlash(rel), true
}
