//go:build linux

package runnerbuild

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/lalternativefabrique/sklp/space/embed"
	"github.com/lalternativefabrique/sklp/space/ociproc"
)

// RuncStepRunner executes each RUN as a runc container built from a
// synthesised OCI config.json: uid-0 + subuid-range mapping (single-id when the
// caller is already uid 0 inside a Space), a private netns with slirp egress and
// the IMDS/RFC1918 blackhole, transparent cache mounts that never land in the
// layer, and cgroup limits where delegation allows. It is the only step engine
// for application-image builds, host or nested.
type RuncStepRunner struct {
	// CacheRoot is where the embedded runc binary is extracted (and reused).
	// Required: empty means runc cannot be resolved.
	CacheRoot string

	// SlirpBin is the slirp4netns binary path. When set, each step runs in an
	// ISOLATED network namespace with slirp egress + the IMDS/RFC1918 blackhole.
	// Empty falls back to the host netns (step-1 behaviour): egress still works,
	// but without the isolation — best-effort, like the run path.
	SlirpBin string

	ExtraEnv []string // extra KEY=value lines exported before each step (e.g. GITHUB_TOKEN)
	Stdout   *os.File
	Stderr   *os.File
	// ImageEnv is the base image's environment (KEY=value), exported inside each
	// RUN so the image's PATH and config take effect — e.g. golang's
	// /usr/local/go/bin, so `go` resolves. Set by the application-image builder
	// from the base image's OCI config; mirrors UnshareStepRunner.ImageEnv.
	ImageEnv []string
	// CacheMounts are persistent host dirs bind-mounted into the rootfs for the
	// duration of the RUNs (as OCI bind mounts, private to the container mount
	// ns), then dropped — never landing in the produced layer. Same contract as
	// UnshareStepRunner.CacheMounts.
	CacheMounts []CacheMount

	// Limits caps the build step's CPU/memory/pids via cgroups v2 (step 3),
	// using the same ociproc mapping + rootless-delegation check as the run
	// path. Zero fields mean unlimited. Applied best-effort by default; see
	// RequireLimits.
	Limits ociproc.Limits

	// RequireLimits makes a requested cap a hard requirement: when set and
	// cgroups v2 delegation is unavailable, Exec FAILS the step instead of
	// silently running unbounded. Mirrors runtime.Config.RequireLimits — a cap
	// is a contract, and an unbounded build can OOM the host. Set it in CI.
	RequireLimits bool
}

// WithStage returns a copy configured for one build stage: the base image env
// (so the image PATH wins) plus the stage's cache mounts appended. Satisfies
// the StepRunner stage-configuration hook used by imagebuild.Build.
const runcStepExports = "export PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin:/opt/sklp/tools/bin" +
	" DEBIAN_FRONTEND=noninteractive TMPDIR=/tmp HOME=/root LC_ALL=C\n"

func (r RuncStepRunner) WithStage(imageEnv []string, cacheMounts []CacheMount) StepRunner {
	r.ImageEnv = imageEnv
	r.CacheMounts = append(append([]CacheMount{}, r.CacheMounts...), cacheMounts...)
	return r
}

// Exec runs every command in runs, in order, inside ONE runc container against
// rootfsDir. The steps are batched into a single `set -e` shell so they share
// the container's mount/uid/network setup and abort on the first failure — one
// container per batch, not per step.
//
// Networking (this is the step-2 change vs the host-netns step 1): the
// container gets an ISOLATED network namespace, and egress is provided by
// slirp4netns with the IMDS/RFC1918 blackhole — the same plumbing the run/space
// path uses, via the shared ociproc package. A build still reaches the internet
// (apt/go/npm) but can no longer see the host network or the cloud metadata
// endpoint. To avoid a race where a `RUN apk add` hits the network before slirp
// is wired, the container is created (namespaces up, PID live, workload NOT
// started), slirp is attached to that PID, THEN the workload is started.
//
// When SlirpBin is empty (extraction failed) we fall back to the host netns —
// best-effort, same spirit as the run path: a build that needs egress on a host
// without slirp keeps working rather than failing, just without the isolation.
func (r RuncStepRunner) Exec(ctx context.Context, rootfsDir string, runs []string) error {
	stdout, stderr := r.Stdout, r.Stderr
	if stdout == nil {
		stdout = os.Stdout
	}
	if stderr == nil {
		stderr = os.Stderr
	}
	if r.CacheRoot == "" {
		return fmt.Errorf("runc step runner: CacheRoot is required")
	}
	runcBin, err := embed.ExtractRunc(r.CacheRoot)
	if err != nil {
		return fmt.Errorf("runc step runner: resolve runc: %w", err)
	}

	// rootfsDir must be absolute: runc resolves root.path relative to the
	// bundle, and we point it at the stage rootfs which lives elsewhere.
	absRoot, err := filepath.Abs(rootfsDir)
	if err != nil {
		return err
	}

	// Ensure cache-mount sources exist on the host (the bind needs a source);
	// the targets inside the rootfs are created by runc when it sets up the
	// mount. mkdir is best-effort — a missing source is the only hard failure.
	for _, m := range r.CacheMounts {
		if err := os.MkdirAll(m.Source, 0o755); err != nil {
			return fmt.Errorf("runc step runner: cache source %s: %w", m.Source, err)
		}
		// Create the mountpoint inside the rootfs too: runc bind mounts require
		// the destination to exist for a directory bind on some kernels, and a
		// minimal base image may not ship e.g. /root/go.
		_ = os.MkdirAll(filepath.Join(absRoot, m.Target), 0o755)
	}

	bundle, err := os.MkdirTemp(r.CacheRoot, "stepbundle-*")
	if err != nil {
		return err
	}
	// TEMP DEBUG (remove before merging): preserve the bundle (config.json +
	// step.log) on a runc create failure so the exact spec that triggered
	// "nsexec: mapping tool not present" can be inspected instead of vanishing
	// with the deferred cleanup below.
	keepBundleOnFailure := false
	defer func() {
		if keepBundleOnFailure {
			fmt.Fprintf(stderr, "[sklp DEBUG] preserved failed bundle at %s\n", bundle)
			return
		}
		os.RemoveAll(bundle)
	}()

	// isolatedNet is true when we will attach slirp to a private netns. With no
	// slirp binary we fall back to the host netns (step-1 behaviour) so egress
	// still works; the spec omits the network namespace in that case.
	isolatedNet := r.SlirpBin != ""

	// The step's exit code is captured via a file on a bundle-backed bind mount
	// (/run/sklp), NOT the rootfs — so it never lands in the produced layer. The
	// script writes $? there after the batch; we read it back to fail the build
	// on a non-zero RUN. create/start (vs run) gives no exit code directly, so
	// this file is how a detached step reports failure.
	exitDir := filepath.Join(bundle, "ctl")
	if err := os.MkdirAll(exitDir, 0o755); err != nil {
		return err
	}

	// selfExe is the sklp binary re-exec'd as the createContainer blackhole
	// hook. If we can't resolve it, the isolated-net path can't install the
	// filter — fail rather than run an unfiltered isolated build.
	selfExe := ""
	resolvConf := ""
	if isolatedNet {
		selfExe, err = os.Executable()
		if err != nil {
			return fmt.Errorf("runc step runner: resolve self for blackhole hook: %w", err)
		}
		// In the isolated netns slirp serves DNS on 10.0.2.3, but nothing points
		// the container at it — write a resolv.conf in the bundle and bind it in.
		// (Host-net mode binds the host resolv.conf instead; see buildStepSpec.)
		resolvConf = filepath.Join(bundle, "resolv.conf")
		if err := os.WriteFile(resolvConf, []byte("nameserver 10.0.2.3\n"), 0o644); err != nil {
			return err
		}
	}

	runcRoot := filepath.Join(r.CacheRoot, "runc-build-state")
	id := "sklp-build-" + filepath.Base(bundle)
	logPath := filepath.Join(bundle, "step.log")

	// Resolve cgroup limits (step 3). Rootless cgroups v2 needs a delegated
	// subtree exposing every requested controller. Missing delegation splits on
	// RequireLimits: CI fails (a cap is a contract — an unbounded build can OOM
	// the host), dev warns and runs unbounded. Same policy as the run path.
	var resources map[string]any
	cgroupsPath := ""
	systemdCgroup := false
	if r.Limits.Any() {
		if res, ok := ociproc.ResourcesSpec(r.Limits); ok {
			if driver, reason := ociproc.ResolveCgroupDriver(r.Limits); driver != ociproc.CgroupNone {
				resources = res
				cgroupsPath = ociproc.CgroupsPath(driver, id)
				systemdCgroup = driver == ociproc.CgroupSystemd
			} else if r.RequireLimits {
				return fmt.Errorf("runc build step: resource limits required but cannot be applied (%s); "+
					"enable cgroups v2 delegation (systemd user@.service `Delegate=cpu memory pids`) or clear RequireLimits", reason)
			} else {
				fmt.Fprintf(stderr, "[sklp] build cgroups: limits requested but unavailable (%s); running unbounded\n", reason)
			}
		}
	}

	script := r.buildScript(runs)
	spec, err := r.buildStepSpec(absRoot, script, isolatedNet, exitDir, selfExe, resolvConf, resources, cgroupsPath)
	if err != nil {
		return err
	}

	b, err := json.MarshalIndent(spec, "", "  ")
	if err != nil {
		return err
	}
	// 0600: the config embeds ExtraEnv (e.g. GITHUB_TOKEN) in process.env.
	if err := os.WriteFile(filepath.Join(bundle, "config.json"), b, 0o600); err != nil {
		return err
	}

	// Best-effort cleanup of a same-id leftover from an interrupted run.
	ociproc.DeleteContainer(runcBin, runcRoot, id)
	defer ociproc.KillContainer(runcBin, runcRoot, id)

	// Host-netns fallback: a plain synchronous `runc run` (no slirp to attach,
	// so no need for the create/start split). Mirrors step-1 behaviour exactly.
	if !isolatedNet {
		args := []string{"--root", runcRoot}
		if systemdCgroup {
			args = append(args, "--systemd-cgroup")
		}
		args = append(args, "run", "--bundle", bundle, id)
		cmd := exec.CommandContext(ctx, runcBin, args...)
		cmd.Stdout = stdout
		cmd.Stderr = stderr
		if err := cmd.Run(); err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return fmt.Errorf("runc build step (host net): %w", ctxErr)
			}
			return fmt.Errorf("runc build step (host net): %w", err)
		}
		if err := r.checkExit(exitDir); err != nil {
			if log, rerr := os.ReadFile(logPath); rerr == nil {
				return withStepTail(err, string(log))
			}
			return err
		}
		return nil
	}

	// Isolated netns path: create (namespaces up, workload blocked) → attach
	// slirp+blackhole to the live PID → start the workload → wait → read $?.
	// The workload's stdio is wired to logPath at create time; we stream that
	// log to the caller's stdout/stderr after the step so RUN output is visible.
	pid, err := ociproc.CreateContainer(ctx, runcBin, runcRoot, bundle, id, logPath, systemdCgroup)
	if err != nil {
		keepBundleOnFailure = true
		return fmt.Errorf("runc build step: create: %w", err)
	}

	stopSlirp, serr := ociproc.StartSlirp(ctx, r.SlirpBin, pid)
	if serr != nil {
		return fmt.Errorf("runc build step: egress: %w", serr)
	}
	defer stopSlirp()

	if err := ociproc.StartContainer(ctx, runcBin, runcRoot, id); err != nil {
		return fmt.Errorf("runc build step: start: %w", err)
	}
	ociproc.WaitStopped(ctx, runcBin, runcRoot, id)

	// Stream the captured step output to the caller, then evaluate the exit
	// code. (The host-net path streams live; the isolated path captures to the
	// log so create/start can wire the console once.)
	var stepLog string
	if log, rerr := os.ReadFile(logPath); rerr == nil {
		_, _ = stdout.Write(log)
		stepLog = string(log)
	}
	if err := r.checkExit(exitDir); err != nil {
		return withStepTail(err, stepLog)
	}
	return nil
}

// withStepTail attaches the end of a failed step's output to its error.
// "RUN exited 1" alone says a command inside the container failed and nothing
// about which — the compiler diagnostic that explains it is in the step log,
// and the pipeline only ever surfaces the error.
func withStepTail(err error, log string) error {
	t := &tailBuffer{max: 2000}
	_, _ = t.Write([]byte(log))
	tail := strings.TrimSpace(t.String())
	if tail == "" {
		return err
	}
	return fmt.Errorf("%w: %s", err, tail)
}

// exitFilePath is where the step script writes its exit code, on a bundle-
// backed bind mount (NOT the rootfs, so it never lands in the layer). The Go
// side reads it back to fail the build on a non-zero RUN — necessary because
// the create/start launch (needed to wire slirp before the workload runs)
// gives no exit code directly.
const exitFilePath = "/run/sklp/exit"

// buildScript assembles the in-container shell: the env exports (ExtraEnv +
// baked/image PATH + load-bearing HOME/TMPDIR), then the batched RUNs in a
// sub-shell whose status is written to exitFilePath. The env-export shape
// mirrors UnshareStepRunner so a Dockerfile behaves the same under either
// engine; the exit-file wrapper is the runc-specific addition.
func (r RuncStepRunner) buildScript(runs []string) string {
	var b strings.Builder
	for _, e := range r.ExtraEnv {
		fmt.Fprintf(&b, "export %s\n", shellQuoteEnv(e))
	}
	// The tools dir goes LAST. Its binaries are symlinks into the runner
	// image's own tools cache (/opt/sklp/tools/go/<ver>/…), which does not
	// exist inside the base image's rootfs — so `go` there resolves to a dead
	// link and dies with "cannot find GOROOT directory". Relying on ImageEnv to
	// win on a later duplicate is not enough: an image that publishes no PATH
	// leaves the dead link in front. Ordering it behind the image's own
	// directories makes the base image's toolchain win either way, while still
	// letting a runner-image build reach its baked tools.
	b.WriteString(runcStepExports)
	for _, e := range r.ImageEnv {
		fmt.Fprintf(&b, "export %s\n", shellQuoteEnv(e))
	}
	// Re-assert HOME/TMPDIR last so an image that omits them still works.
	b.WriteString("export HOME=${HOME:-/root} TMPDIR=${TMPDIR:-/tmp}\n")
	// After ImageEnv: point each toolchain at the dir we actually mounted, or the
	// base image's own GOPATH/PNPM_HOME sends the writes past the cache.
	for _, e := range cacheEnvForMounts(r.CacheMounts) {
		fmt.Fprintf(&b, "export %s\n", shellQuoteEnv(e))
	}

	// Run the batch in a sub-shell with `set -e` so it aborts on the first
	// failing RUN, capture its status, write it to the exit file (which lives on
	// a bundle bind mount), then exit 0 — the CONTAINER always exits cleanly so
	// runc tears down normally; the Go side decides build success from the file.
	b.WriteString("(\n  set -e\n")
	dbg := stepEnvDebugPrefix()
	for _, run := range runs {
		b.WriteString("  ")
		b.WriteString(dbg)
		b.WriteString(run)
		b.WriteString("\n")
	}
	b.WriteString(")\n")
	fmt.Fprintf(&b, "code=$?\nmkdir -p %s 2>/dev/null || true\nprintf %%s \"$code\" > %s\nexit 0\n",
		filepath.Dir(exitFilePath), exitFilePath)
	return b.String()
}

// checkExit reads the exit code the step script wrote to the bundle ctl dir
// (bind-mounted at /run/sklp inside the container). A missing file means the
// script never reached the write — treat that as a failure too. Non-zero means
// a RUN failed; surface it so the build aborts.
func (r RuncStepRunner) checkExit(exitDir string) error {
	raw, err := os.ReadFile(filepath.Join(exitDir, "exit"))
	if err != nil {
		return fmt.Errorf("runc build step: no exit status recorded (step likely crashed): %w", err)
	}
	code, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		return fmt.Errorf("runc build step: unparseable exit status %q", string(raw))
	}
	if code != 0 {
		return fmt.Errorf("runc build step: RUN exited %d", code)
	}
	return nil
}

// buildStepSpec synthesises the OCI runtime-spec for a build-step container.
// It deliberately mirrors the run-path spec shape (uid/gid mappings, caps,
// noNewPrivileges) but with the build-specific differences:
//
//   - root.readonly = false: a build MUST write its rootfs (apt installs).
//   - NO /workspace bind and NO `sklp run` argv: the command is the build
//     script; the COPY is already materialised into the rootfs by build_linux.
//   - cache mounts appended as private bind mounts.
//   - a bundle-backed /run/sklp bind mount carrying the exit-status file.
//
// isolatedNet selects the network model: true gives a fresh private network
// namespace (slirp + IMDS blackhole are attached to it by Exec); false omits
// the network namespace so the container shares the host netns (the step-1
// fallback when slirp is unavailable). When sharing the host netns the host
// resolv.conf is bound in for DNS; with the isolated netns slirp provides DNS
// (10.0.2.3) so no resolv.conf bind is needed.
func (r RuncStepRunner) buildStepSpec(absRoot, script string, isolatedNet bool, exitDir, selfExe, resolvConf string, resources map[string]any, cgroupsPath string) (map[string]any, error) {
	uid, gid := os.Getuid(), os.Getgid()
	uidMappings, gidMappings, err := ociproc.ResolveIDMappings(uid, gid)
	if err != nil {
		return nil, err
	}
	return r.buildStepSpecWithMappings(absRoot, script, isolatedNet, exitDir, selfExe, resolvConf, resources, cgroupsPath, uidMappings, gidMappings), nil
}

func (r RuncStepRunner) buildStepSpecWithMappings(absRoot, script string, isolatedNet bool, exitDir, selfExe, resolvConf string, resources map[string]any, cgroupsPath string, uidMappings, gidMappings []map[string]any) map[string]any {
	mounts := []map[string]any{
		{"destination": "/proc", "type": "proc", "source": "proc"},
		{"destination": "/tmp", "type": "tmpfs", "source": "tmpfs",
			"options": []string{"nosuid", "nodev", "mode=1777", "size=2g"}},
		// A tmpfs /dev so runc populates the default device nodes (null/zero/full/
		// random/urandom/tty) into an EPHEMERAL layer, not the rootfs. Without it
		// the rootfs's real /dev (empty after rootless extraction drops the image's
		// device nodes) receives them: a RUN's mknod fails without CAP_MKNOD and
		// busybox/apk falls back to a 0000-mode regular file, which a later
		// `FROM <stage>` copyTree (cp -a, real uid) cannot read → EACCES.
		{"destination": "/dev", "type": "tmpfs", "source": "tmpfs",
			"options": []string{"nosuid", "strictatime", "mode=0755", "size=64k"}},
		{"destination": "/dev/pts", "type": "devpts", "source": "devpts",
			"options": []string{"nosuid", "noexec", "newinstance", "ptmxmode=0666", "mode=0620"}},
		// The exit-status channel: a writable bind from the bundle ctl dir, so
		// the step script can report $? out of the container without touching the
		// rootfs/layer. Private to this container.
		{"destination": "/run/sklp", "type": "bind", "source": exitDir,
			"options": []string{"rbind", "rw"}},
	}
	// DNS: host-net binds the host resolv.conf (the host's resolver is the right
	// one); isolated-net binds a bundle resolv.conf pointing at slirp's DNS
	// (10.0.2.3), since nothing else points the netns at it. Both are read-only
	// binds, private to this container, never in the layer.
	resolvSrc := "/etc/resolv.conf"
	if isolatedNet {
		resolvSrc = resolvConf
	}
	if resolvSrc != "" {
		mounts = append(mounts, map[string]any{
			"destination": "/etc/resolv.conf", "type": "bind", "source": resolvSrc,
			"options": []string{"rbind", "ro"},
		})
	}
	for _, m := range r.CacheMounts {
		mounts = append(mounts, map[string]any{
			"destination": m.Target,
			"type":        "bind",
			"source":      m.Source,
			"options":     []string{"rbind", "rw"},
		})
	}

	// Namespaces: always user + mount + pid + ipc + uts (the user ns carries
	// the uid-0 + subuid-range mapping apt/dpkg need). A network namespace is
	// added ONLY for the isolated-net path; omitting it shares the host netns.
	namespaces := []map[string]any{
		{"type": "pid"},
		{"type": "ipc"},
		{"type": "uts"},
		{"type": "mount"},
		{"type": "user"},
	}
	if isolatedNet {
		namespaces = append(namespaces, map[string]any{"type": "network"})
	}
	linux := map[string]any{
		"namespaces":    namespaces,
		"uidMappings":   uidMappings,
		"gidMappings":   gidMappings,
		"maskedPaths":   ociproc.DefaultMaskedPaths(),
		"readonlyPaths": ociproc.DefaultReadonlyPaths(),
		// Same allowlist as the run/service path; the build's extra mount/chroot/
		// keyctl/unshare syscalls (dpkg, apt, layer setup) are already in it.
		"seccomp": ociproc.DefaultSeccomp(),
	}
	// cgroup limits (step 3): only emitted when delegation let Exec resolve
	// them, so an unbounded build stays byte-identical to the pre-cgroups spec.
	if resources != nil {
		linux["resources"] = resources
	}
	if cgroupsPath != "" {
		linux["cgroupsPath"] = cgroupsPath
	}

	spec := map[string]any{
		"ociVersion": "1.0.2",
		"process": map[string]any{
			"terminal": false,
			"user":     map[string]any{"uid": 0, "gid": 0},
			"args":     []string{"/bin/sh", "-c", script},
			"env": []string{
				"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
			},
			"cwd": "/",
			"capabilities": map[string]any{
				"bounding":    buildStepCaps(),
				"effective":   buildStepCaps(),
				"permitted":   buildStepCaps(),
				"inheritable": buildStepCaps(),
				"ambient":     buildStepCaps(),
			},
			"noNewPrivileges": true,
		},
		"root": map[string]any{
			"path":     absRoot,
			"readonly": false, // a build writes its rootfs
		},
		"hostname": "sklp-build",
		"mounts":   mounts,
		"linux":    linux,
	}
	if isolatedNet && selfExe != "" {
		// createContainer hook: installs the IMDS/RFC1918 blackhole INSIDE the
		// container netns, the only context with CAP_NET_ADMIN over a rootless
		// netns (host nsenter fails EPERM — issue #610). Fail-closed: a hook
		// error fails `runc create`, so a step never runs without the filter.
		spec["hooks"] = map[string]any{
			"createContainer": ociproc.BlackholeHookSpec(selfExe)["createContainer"],
		}
	}
	return spec
}

// buildStepCaps is the capability set granted to a build step: the shared
// ociproc.DefaultCapabilities baseline (chown/dac_override/setuid/… for
// apt/dpkg, mknod, net_raw — SYS_ADMIN deliberately absent) PLUS CAP_NET_ADMIN.
// The extra cap is the ONLY divergence from the run/service path, and it is
// added EXPLICITLY here (not hidden in a forked constant) so the privilege is
// visible: the isolated-net build wires a createContainer blackhole hook that
// adds routes with netlink inside the container netns, which needs CAP_NET_ADMIN
// over that netns (host nsenter fails EPERM — issue #610). Sharing the baseline
// kills the silent drift; the append documents the one intentional difference.
func buildStepCaps() []string {
	return append(ociproc.DefaultCapabilities(), "CAP_NET_ADMIN")
}
