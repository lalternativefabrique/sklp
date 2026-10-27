//go:build linux

package runnerbuild

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

// UnshareStepRunner is the MVP / proto step runner: it creates ONE user+mount
// +pid namespace, mapping container uid 0 -> our uid AND 1..65535 -> our subuid
// range (so apt/dpkg/chmod run as root and can chown to service uids), then
// chroots into the rootfs for each RUN. It uses the HOST network namespace, so
// apt-get reaches the internet without the space egress.
//
// The production runner will instead execute each step in a runc container
// attached to the space (egress via slirp), reusing the oneshot path — but the
// uid-0 + subuid-range mapping proven here is the shape it needs.
type UnshareStepRunner struct {
	ExtraEnv []string // extra KEY=value lines exported before each step (e.g. GITHUB_TOKEN)
	Stdout   *os.File
	Stderr   *os.File
	// ImageEnv is the base image's environment (KEY=value), exported inside each
	// RUN so the image's PATH and config take effect — e.g. golang's
	// /usr/local/go/bin, so `go` resolves. Empty = the baked runner PATH only
	// (the runner-image build path). Set by the application-image builder from
	// the base image's OCI config.
	ImageEnv []string
	// CacheMounts are persistent host dirs bind-mounted into the rootfs for the
	// duration of the RUNs, then dropped (the namespace exits). They make a build
	// cache survive across builds WITHOUT landing in the produced rootfs/layer —
	// e.g. $GOMODCACHE / $GOCACHE / the pnpm store, so `go build` / `pnpm install`
	// reuse downloads + incremental output instead of starting from scratch. The
	// content-cache equivalent of BuildKit's --mount=type=cache; empty = none.
	CacheMounts []CacheMount
	// NestedUserns forces the nested uid/gid map: uid 0 only, no subuid range,
	// plus the apt workarounds a single-uid namespace needs. Left false it is
	// detected from /proc/self/uid_map, because a caller cannot know at
	// construction time whether the run lands on a workstation or in an
	// already-namespaced CI pod. Forcing host is not offered: an unnested host
	// is what detection already reports.
	NestedUserns bool
	// detectNested overrides the /proc/self/uid_map probe. Tests set it to pin a
	// mapping mode, since the suite itself runs both on a host and inside the CI
	// runner's user namespace and must assert the same thing in both.
	detectNested func() bool
}

// resolveNesting turns on nesting when this process is itself in a user
// namespace, leaving an explicit NestedUserns:true untouched.
func (u UnshareStepRunner) resolveNesting() UnshareStepRunner {
	if u.NestedUserns {
		return u
	}
	detect := u.detectNested
	if detect == nil {
		detect = NestedUserns
	}
	u.NestedUserns = detect()
	return u
}

// CacheMount binds a persistent host directory at Target inside the rootfs while
// the RUNs execute. Source is created if missing; it persists across builds.
type CacheMount struct {
	Source string // host dir (persistent, created if missing)
	Target string // path inside the rootfs (absolute, e.g. /root/go/pkg/mod)
}

// cacheEnvForMounts returns the env exports that point each toolchain at the
// dir we actually bind-mounted, keyed by the mount Target.
//
// Without this the mounts silently miss: an official base image sets its OWN
// locations in ImageEnv (golang:* ships GOPATH=/go), so `go mod download`
// writes to /go/pkg/mod while the cache is mounted at /root/go/pkg/mod — the
// module cache stays empty and every build re-downloads the whole module graph
// over the network. GOCACHE only worked by luck: its default derives from HOME,
// which we already force to /root.
//
// GOMODCACHE (Go 1.15+) is set rather than GOPATH so GOBIN and tool resolution
// keep following the image's own layout — we redirect the cache, not the SDK.
// Emitted AFTER ImageEnv so these win over the base image's values.
// defaultStepExports is the env every RUN starts from, before the base image's
// own env is layered over it.
//
// The tools dir goes LAST. /opt/sklp/tools/bin belongs to the runner image the
// pipeline runs in; inside an application image's rootfs it is a different
// filesystem where those binaries either do not exist or no longer find what
// they need. First on PATH, it shadowed the base image's toolchain: a golang
// base ships go at /usr/local/go/bin, and the build instead picked the runner's
// trimmed `go`, which died with "cannot find GOROOT directory". Keeping it on
// PATH is what lets a runner-image build reach its baked tools; keeping it
// behind the image's own directories is what stops it hijacking someone else's.
const defaultStepExports = "export PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin:/opt/sklp/tools/bin" +
	" DEBIAN_FRONTEND=noninteractive TMPDIR=/tmp HOME=/root LC_ALL=C; "

// staleCacheDirUnset drops cache env vars whose directory is not in this
// rootfs. The runner's own environment names paths under its /cache mount, and
// a build step chroots into an image rootfs where that mount does not exist:
// `go` refuses to create GOTMPDIR itself and stops with "creating work dir:
// stat /cache/go-tmp: no such file or directory". Unsetting leaves each tool
// on its own default, which is always valid inside the rootfs. The bind-mounted
// cache dirs are re-exported after this, so a mount that IS present still wins.
const staleCacheDirUnset = `for _v in GOTMPDIR GOCACHE GOMODCACHE GOPATH ` +
	`XDG_CACHE_HOME XDG_DATA_HOME XDG_CONFIG_HOME npm_config_cache PNPM_HOME; do ` +
	`eval _d=\$$_v; [ -n "$_d" ] && [ ! -d "$_d" ] && unset $_v; done; `

// gorootFallbackExport gives `go` a GOROOT when the image publishes none.
//
// A release toolchain is built with -trimpath, so it carries no compiled-in
// GOROOT and recovers one by reading /proc/self/exe. Nested inside the OCI
// runner /proc cannot be mounted (no CAP_SYS_ADMIN in the bounding set), and
// every Go command then fails with "cannot find GOROOT directory" — an error
// naming the toolchain rather than the missing mount. Deriving it from the
// resolved path of the image's own `go` keeps the base image authoritative,
// and the guard leaves an image that does export GOROOT untouched.
//
// It only fires when `go env GOROOT` itself fails, which is exactly the case
// /proc/self/exe was unreachable. Whenever go can answer, its own answer wins:
// forcing a GOROOT that belongs to a different toolchain than the running `go`
// makes every package fail with "compile: version does not match go tool
// version", which is worse than the missing GOROOT it replaces.
const gorootFallbackExport = `if [ -z "${GOROOT:-}" ]; then ` +
	`_gr=$(go env GOROOT 2>/dev/null); ` +
	`case "$_gr" in /*) ;; *) _gr="" ;; esac; ` +
	`if [ -z "$_gr" ] || [ ! -d "$_gr" ]; then ` +
	`_g=$(command -v go 2>/dev/null || true); ` +
	`if [ -n "$_g" ]; then _g=$(readlink -f "$_g" 2>/dev/null || echo "$_g"); ` +
	`_r=${_g%/bin/go}; ` +
	`[ -d "$_r/pkg" ] && [ -f "$_r/VERSION" ] && export GOROOT="$_r"; fi; fi; fi; `

func cacheEnvForMounts(mounts []CacheMount) []string {
	byTarget := map[string]string{}
	for _, m := range mounts {
		byTarget[m.Target] = m.Source
	}
	var out []string
	for _, v := range []struct{ target, key string }{
		{"/root/go/pkg/mod", "GOMODCACHE"},
		{"/root/.cache/go-build", "GOCACHE"},
		{"/root/.npm", "npm_config_cache"},
		// No bun entry: its default cache is $HOME/.bun/install/cache and the
		// step shell already forces HOME=/root, so the default IS the mount
		// target. Verified in-container — the e2e passes with and without an
		// explicit BUN_INSTALL_CACHE_DIR, so exporting one would be noise.
		//
		// No pnpm entry either, for the opposite reason: pnpm keeps its store on
		// the SAME filesystem as the install so it can hard-link into
		// node_modules. A store on a host bind mount is a different fs, so pnpm
		// ignores the setting and falls back to /.pnpm-store in the throwaway
		// rootfs (verified: PNPM_HOME was exported and `pnpm store path` still
		// resolved to /.pnpm-store/v11). Forcing it would trade downloads for a
		// full copy of node_modules. Caching pnpm needs a store on the stage's
		// own fs, which this hook cannot provide.
	} {
		if _, ok := byTarget[v.target]; ok {
			out = append(out, v.key+"="+v.target)
		}
	}
	return out
}

// WithStage returns a copy configured for one build stage: the base image env
// (so the image PATH wins) plus the stage's cache mounts appended. When running
// nested inside the embedded OCI runner, persistent cache bind-mounts are not
// permitted, so they are deliberately ignored.
func (u UnshareStepRunner) WithStage(imageEnv []string, cacheMounts []CacheMount) StepRunner {
	u = u.resolveNesting()
	u.ImageEnv = imageEnv
	if u.NestedUserns {
		u.CacheMounts = nil
		cacheMounts = nil
	}
	u.CacheMounts = append(append([]CacheMount{}, u.CacheMounts...), cacheMounts...)
	return u
}

// buildScript assembles the in-namespace shell: pseudo-filesystem mounts,
// cache binds, then each RUN chroot'd under the step env. Extracted so the
// generated script can be asserted on without entering a namespace.
// nestedAptConfPath is where the nested runner tells apt not to drop to _apt.
const nestedAptConfPath = "/etc/apt/apt.conf.d/99sklp-nested-userns"

func (u UnshareStepRunner) buildScript(rootfsDir string, runs []string) string {
	var b strings.Builder
	b.WriteString("set -e\n")
	for _, e := range u.ExtraEnv {
		fmt.Fprintf(&b, "export %s\n", shellQuoteEnv(e))
	}
	// Wire the pseudo-filesystems package scripts expect inside the chroot:
	// /proc (dpkg/apt read it), a fresh devpts + /dev/pts (debconf/perl open a
	// pty), and a clean /tmp owned by root (the host TMPDIR does not exist in the
	// rootfs — ca-certificates' post-install mktemp'd into it and failed).
	// Private to this namespace; they vanish when it exits. All best-effort
	// except /proc, see below.
	root := shellSingleQuote(rootfsDir)
	// A trimmed `go` has no built-in GOROOT and derives it from /proc/self/exe,
	// so a missing /proc makes every Go build die with "cannot find GOROOT
	// directory" — far from the mount that actually failed. Create the
	// mountpoint (a base image need not ship one) and, on the host, let a real
	// failure stop the build there.
	//
	// Nested inside the embedded OCI runner there is no CAP_SYS_ADMIN in the
	// bounding set and noNewPrivileges is set, so mount(2) cannot succeed at
	// all — the same reason WithStage drops cache bind-mounts. Keep it
	// best-effort there and give the toolchain an explicit GOROOT instead, so
	// it never has to consult /proc/self/exe.
	fmt.Fprintf(&b, "mkdir -p %s/proc\n", root)
	if u.NestedUserns {
		fmt.Fprintf(&b, "mount -t proc proc %s/proc 2>/dev/null || true\n", root)
	} else {
		fmt.Fprintf(&b, "mount -t proc proc %s/proc\n", root)
	}
	fmt.Fprintf(&b, "mount -t tmpfs tmpfs %s/tmp 2>/dev/null || true\n", root)
	// Nested, the user namespace maps uid 0 and nothing else, so apt's
	// privilege drop to _apt (uid 42) dies with "seteuid 42 failed: Invalid
	// argument" before it downloads a single package. Tell apt to stay root,
	// and dpkg not to open a pty for its log: /dev/pts cannot be mounted here
	// either. The file is removed at teardown so it never lands in the
	// produced layer; on the host a real subuid range maps 42 and apt drops
	// privileges as usual.
	if u.NestedUserns {
		fmt.Fprintf(&b, "if [ -d %s/etc/apt/apt.conf.d ]; then printf '%%s\\n' 'APT::Sandbox::User \"root\";' 'Dpkg::Use-Pty \"0\";' > %s%s; fi\n",
			root, root, nestedAptConfPath)
	}
	// Working DNS inside the chroot: the rootfs's own resolv.conf is stale,
	// unroutable (systemd-resolved's 127.0.0.53 stub) or absent, and `apk add` /
	// `apt-get` fail to resolve their mirrors without ours. On the host it is
	// bind-mounted: private to the namespace, never in the produced layer.
	//
	// Nested, mount(2) is refused outright, so the bind silently left the empty
	// file `touch` had created and musl fell back to 127.0.0.1 — "DNS: transient
	// error" from apk on every Alpine stage, while Debian stages survived only
	// because their base image happens to ship a resolv.conf. Copy the file
	// instead, keeping whatever the image had so teardown can put it back.
	if u.NestedUserns {
		fmt.Fprintf(&b, "if [ -e %s/etc/resolv.conf ] || [ -L %s/etc/resolv.conf ]; then mv %s/etc/resolv.conf %s/etc/resolv.conf.sklp-orig 2>/dev/null || true; fi\n",
			root, root, root, root)
		fmt.Fprintf(&b, "cp /etc/resolv.conf %s/etc/resolv.conf 2>/dev/null || true\n", root)
	} else {
		fmt.Fprintf(&b, "touch %s/etc/resolv.conf 2>/dev/null || true\n", root)
		fmt.Fprintf(&b, "mount --bind /etc/resolv.conf %s/etc/resolv.conf 2>/dev/null || true\n", root)
	}
	fmt.Fprintf(&b, "mkdir -p %s/dev/pts 2>/dev/null || true\n", root)
	fmt.Fprintf(&b, "mount -t devpts devpts %s/dev/pts 2>/dev/null || true\n", root)
	// Minimal /dev: many tools (go telemetry, anything writing to /dev/null,
	// reading /dev/urandom) need these device nodes. We can't mknod in a rootless
	// userns, so bind the host's nodes in. Best-effort. Each bind needs a regular
	// file as mountpoint (touch); that mountpoint MUST be cleaned up after the RUN
	// (see below) — left behind, it is a 0000-mode regular file that a later
	// `FROM <stage>` copy (cp -a, outside this userns) cannot read → EACCES.
	devNodes := []string{"null", "zero", "full", "random", "urandom", "tty"}
	for _, dev := range devNodes {
		t := fmt.Sprintf("%s/dev/%s", rootfsDir, dev)
		// Bind only from a real device node. This runner's own /dev/null can
		// itself be a leftover empty file: the teardown below replaces each
		// bound node with the regular file `touch` created, and a rootfs built
		// that way and later reused as a runner carries it. Binding that file
		// makes every writer see a 0-byte regular file instead of the sink —
		// gcc and the Go tool both stop with "open /dev/null: no such file or
		// directory" while the path plainly exists.
		fmt.Fprintf(&b, "if [ -c /dev/%s ]; then touch %s 2>/dev/null || true; "+
			"mount --bind /dev/%s %s 2>/dev/null || true; fi\n",
			dev, shellSingleQuote(t), dev, shellSingleQuote(t))
	}
	// Bind persistent cache dirs into the rootfs at their targets (go/pnpm caches
	// etc.). The bind is private to this namespace and gone when it exits, so the
	// cache content NEVER lands in the produced rootfs/layer — only the host dir
	// (Source) persists across builds. mkdir both ends first so the bind has a
	// mountpoint regardless of the base image's tree.
	for _, m := range u.CacheMounts {
		tgt := shellSingleQuote(rootfsDir + m.Target)
		fmt.Fprintf(&b, "mkdir -p %s 2>/dev/null || true\n", shellSingleQuote(m.Source))
		fmt.Fprintf(&b, "mkdir -p %s 2>/dev/null || true\n", tgt)
		fmt.Fprintf(&b, "mount --bind %s %s\n", shellSingleQuote(m.Source), tgt)
	}
	// Each step runs as `chroot <rootfs> /bin/sh -c '<exports>; <step>'`. Exporting
	// the env INSIDE the inner shell avoids a hard dependency on /usr/bin/env
	// existing in the rootfs (a minimal base may not ship coreutils env).
	//
	// PATH/env source: the runner-image build uses the baked toolchain PATH; an
	// application-image build must instead honour the BASE IMAGE's env (e.g.
	// golang:1.25 puts `go` on /usr/local/go/bin via its own PATH). ImageEnv,
	// when set, replaces the baked exports with the image's env — appended after
	// so its PATH wins. We still force HOME/TMPDIR/non-interactive defaults the
	// chroot needs, but let the image override them.
	// The tools dir goes LAST, not first. /opt/sklp/tools/bin belongs to the
	// runner image the pipeline itself runs in; inside an application image's
	// rootfs it is a different filesystem where those binaries either do not
	// exist or no longer find what they need. Putting it first shadowed the base
	// image's own toolchain: a golang base ships go at /usr/local/go/bin, and the
	// build instead picked the runner's trimmed `go`, which died with "cannot
	// find GOROOT directory". Keeping it on PATH at all is what lets a
	// runner-image build reach its baked tools; keeping it behind the image's
	// own directories is what stops it hijacking someone else's.
	innerExports := defaultStepExports
	for _, e := range u.ImageEnv {
		innerExports += "export " + shellQuoteEnv(e) + "; "
	}
	// HOME/TMPDIR are load-bearing for the chroot regardless of the image (a
	// read-only-ish base, mktemp targets); re-assert them last so an image that
	// omits them still works, without clobbering an image PATH.
	innerExports += "export HOME=${HOME:-/root} TMPDIR=${TMPDIR:-/tmp}; "
	innerExports += staleCacheDirUnset
	innerExports += gorootFallbackExport
	// After ImageEnv: point each toolchain at the dir we actually mounted, or the
	// base image's own GOPATH/PNPM_HOME sends the writes past the cache.
	for _, e := range cacheEnvForMounts(u.CacheMounts) {
		innerExports += "export " + shellQuoteEnv(e) + "; "
	}
	dbg := stepEnvDebugPrefix()
	for _, run := range runs {
		fmt.Fprintf(&b, "chroot %s /bin/sh -c %s\n",
			root, shellSingleQuote(innerExports+dbg+run))
	}
	// Tear down the /dev bind mountpoints before the namespace dies. The binds are
	// private to this mount ns and vanish anyway, but the regular files `touch`
	// created as mountpoints persist in the produced rootfs as 0000-mode files the
	// extraction would never have left (rootlessLayerFilter drops device nodes).
	// Unmount each (so rm sees the underlying mountpoint file, not the bound node)
	// then remove it, leaving /dev empty — the OCI runtime rebuilds /dev at create
	// time. Without this, a later `FROM <stage>` copyTree (cp -a, real uid) hits
	// EACCES reading the unreadable dev/null. All best-effort.
	if u.NestedUserns {
		fmt.Fprintf(&b, "rm -f %s%s 2>/dev/null || true\n", root, nestedAptConfPath)
		fmt.Fprintf(&b, "if [ -e %s/etc/resolv.conf.sklp-orig ] || [ -L %s/etc/resolv.conf.sklp-orig ]; then mv -f %s/etc/resolv.conf.sklp-orig %s/etc/resolv.conf 2>/dev/null || true; else rm -f %s/etc/resolv.conf 2>/dev/null || true; fi\n",
			root, root, root, root, root)
	}
	for _, dev := range devNodes {
		t := shellSingleQuote(fmt.Sprintf("%s/dev/%s", rootfsDir, dev))
		fmt.Fprintf(&b, "umount %s 2>/dev/null || true\n", t)
		// Remove only the placeholder file, never a real device node: a rootfs
		// reused as a build runner keeps its own /dev, and deleting the node
		// there leaves an empty file that later binds propagate into every
		// build.
		fmt.Fprintf(&b, "[ -c %s ] || rm -f %s 2>/dev/null || true\n", t, t)
	}

	return b.String()
}

func (u UnshareStepRunner) Exec(ctx context.Context, rootfsDir string, runs []string) error {
	u = u.resolveNesting()
	stdout, stderr := u.Stdout, u.Stderr
	if stdout == nil {
		stdout = os.Stdout
	}
	if stderr == nil {
		stderr = os.Stderr
	}

	// In-namespace script: chroot each step under a baked PATH. `set -e` aborts
	// on the first failing step. /proc is mounted inside the rootfs so tools that
	// read it (dpkg, apt) work; it is private to this namespace.
	script := u.buildScript(rootfsDir, runs)

	// Nested AND already real uid 0 (a privileged CI pod, not a rootless Space):
	// `unshare --user --map-root-user` writes a fresh uid_map, and that write
	// can be refused with EPERM depending on the parent's own capability/
	// securebits state (observed on the in-cluster CI runner container; not
	// reproduced with a bare unshare locally, so the exact kernel gate is
	// unconfirmed — but the remap buys nothing here regardless: uid 0 is
	// already uid 0). Skip unshare entirely and run the same chroot script
	// directly — every mount/bind inside it is already best-effort
	// (`|| true`) for the nested case, so dropping the wrapping namespace
	// changes nothing else it depends on.
	if u.NestedUserns && os.Getuid() == 0 {
		return u.execDirect(ctx, script, stdout, stderr)
	}

	args := unshareBuildArgs(script, u.NestedUserns)
	cmd := exec.CommandContext(ctx, "unshare", args...)
	// Build the environment instead of inheriting one. exec.Command would hand
	// the caller's env to the namespace and chroot keeps it, so the runner's own
	// variables reached a rootfs where their paths do not exist: GOTMPDIR named
	// the CI space's /cache mount and `go`, which refuses to create that dir,
	// stopped with "creating work dir: stat /cache/go-tmp: no such file or
	// directory". The RUN's environment is the image's, assembled in the script;
	// runc gets this for free from process.env in the OCI config.
	cmd.Env = []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"}
	cmd.Stdout = stdout
	// Tee stderr: it still streams to the caller, but the tail is kept so the
	// returned error can carry WHY the build stopped. "exit status 2" on its own
	// says a command inside the namespace failed and nothing about which — the
	// script runs under `set -e`, so the last lines are the failure.
	tail := &tailBuffer{max: 2000}
	cmd.Stderr = io.MultiWriter(stderr, tail)
	if err := cmd.Run(); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return fmt.Errorf("unshare build namespace: %w", ctxErr)
		}
		if last := tail.String(); last != "" {
			return fmt.Errorf("unshare build namespace: %w: %s", err, last)
		}
		return fmt.Errorf("unshare build namespace: %w", err)
	}
	return nil
}

// execDirect runs the chroot script in the current namespaces — no unshare at
// all. Only reached when already real uid 0 and nested, so there is no uid to
// remap and no host-side subuid range to grant; the script's mounts/binds are
// already best-effort for that case (see buildScript), so losing the private
// mount namespace loses only their cleanliness on a crash, not their effect.
func (u UnshareStepRunner) execDirect(ctx context.Context, script string, stdout, stderr io.Writer) error {
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", script)
	cmd.Env = []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"}
	cmd.Stdout = stdout
	tail := &tailBuffer{max: 2000}
	cmd.Stderr = io.MultiWriter(stderr, tail)
	if err := cmd.Run(); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return fmt.Errorf("chroot build (direct): %w", ctxErr)
		}
		if last := tail.String(); last != "" {
			return fmt.Errorf("chroot build (direct): %w: %s", err, last)
		}
		return fmt.Errorf("chroot build (direct): %w", err)
	}
	return nil
}

// tailBuffer keeps the last max bytes written to it, so a long build log costs
// a bounded amount of memory while its ending survives.
type tailBuffer struct {
	max int
	buf []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.max {
		t.buf = t.buf[len(t.buf)-t.max:]
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	return strings.TrimSpace(string(t.buf))
}

func unshareBuildArgs(script string, nested bool) []string {
	args := []string{"--user", "--map-root-user"}
	if !nested {
		// Host-side: map container uids 1..65535 -> subuid range 100000..
		args = append(args, "--map-users=1:100000:65535", "--map-groups=1:100000:65535")
	}
	// Nested (inside OCI userns): writing a child uid_map with a secondary range
	// fails with EPERM — the parent userns only exposes uid 0 via --map-root-user.
	return append(args, "--mount", "--pid", "--fork", "/bin/sh", "-c", script)
}

// shellSingleQuote wraps s in single quotes, escaping any embedded single quote,
// so an arbitrary RUN body survives as one argument to the inner `sh -c`.
func shellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// shellQuoteEnv quotes the value half of a KEY=value export line.
func shellQuoteEnv(kv string) string {
	k, v, ok := strings.Cut(kv, "=")
	if !ok {
		return kv
	}
	return k + "=" + shellSingleQuote(v)
}
