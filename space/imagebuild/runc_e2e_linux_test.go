//go:build linux

package imagebuild

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/lalternativefabrique/sklp/internal/integtest"
	parse "github.com/lalternativefabrique/sklp/pipeline/imagebuild"
	"github.com/lalternativefabrique/sklp/space/embed"
	"github.com/lalternativefabrique/sklp/space/ociproc"
	"github.com/lalternativefabrique/sklp/space/runnerbuild"
)

// skipUnlessRuncE2E gates the real `runc run` builds: they pull alpine and need
// network + rootless runc + subuid delegation, so they stay opt-in via
// SKLP_RUNC_E2E=1 (set by ci-nightly, never by the per-PR ci). This tier is NOT
// covered by SKLP_REQUIRE_INTEG — that guard governs the network-free userns
// tier the per-PR runner is expected to run; the heavy e2e belongs to nightly.
func skipUnlessRuncE2E(t *testing.T) {
	t.Helper()
	if os.Getenv("SKLP_RUNC_E2E") != "1" {
		t.Skip("set SKLP_RUNC_E2E=1 to run the real runc build (needs network + rootless runc)")
	}
}

// TestMain lets the test binary act as the createContainer blackhole hook when
// runc re-execs it (the build sets the hook path to os.Executable(), which under
// `go test` is this binary). Mirrors what sklp's real main() does: call
// MaybeRunBlackholeHook before any other work. Without it the re-exec'd hook
// would re-run the test suite instead of installing the routes.
func TestMain(m *testing.M) {
	ociproc.MaybeRunBlackholeHook() // no-op unless the hook marker env is set
	os.Exit(m.Run())
}

// TestRuncStepRunner_E2E builds a tiny single-stage image through the real runc
// step runner: pull alpine, run an apk-free RUN that writes a file, and assert
// the file landed in the built rootfs. It exercises the actual `runc run` path
// (uid mapping, mounts, host netns) end to end.
//
// Guarded by SKLP_RUNC_E2E=1 because it needs network (to pull alpine), a
// working rootless runc, and /etc/subuid delegation — not guaranteed in CI.
// Run locally with:
//
//	SKLP_RUNC_E2E=1 go test -tags 'embedded_runtime linux' -run E2E ./space/imagebuild/
func TestRuncStepRunner_E2E(t *testing.T) {
	skipUnlessRuncE2E(t)

	cache := t.TempDir()
	work := t.TempDir()

	df, err := parse.Parse("FROM alpine:3.20\nRUN echo built-by-runc > /marker.txt && id -u > /uid.txt\n")
	if err != nil {
		t.Fatal(err)
	}

	res, err := Build(context.Background(), cache, work, df,
		Options{ContextDir: t.TempDir(), Progress: os.Stderr},
		runnerbuild.RuncStepRunner{CacheRoot: cache, Stdout: os.Stderr, Stderr: os.Stderr})
	if err != nil {
		t.Fatalf("build via runc: %v", err)
	}

	marker := filepath.Join(res.Rootfs, "marker.txt")
	b, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("RUN did not write the rootfs (marker missing): %v", err)
	}
	if got := string(b); got != "built-by-runc\n" {
		t.Errorf("marker = %q, want built-by-runc", got)
	}

	// The RUN must have executed as container uid 0 (the uid mapping).
	uid, err := os.ReadFile(filepath.Join(res.Rootfs, "uid.txt"))
	if err != nil {
		t.Fatalf("uid.txt missing: %v", err)
	}
	if got := string(uid); got != "0\n" {
		t.Errorf("RUN ran as uid %q, want 0 (mapped root)", got)
	}
}

// TestRuncStepRunner_E2E_NextjsShape is the golden multi-stage build: a
// Dockerfile isomorphic to the canonical Next.js standalone image (the amv
// project), the hardest real-world shape sklp must handle. It exercises, through
// the REAL runc step runner, every feature whose interaction broke the amv build:
//
//   - cascade `FROM base AS <x>`: three stages (deps, builder, runner) all derive
//     from one intermediate `base` stage that itself RUNs (apk add). The stage
//     copy (copyTree, cp -a, real uid) must not choke on /dev nodes a RUN left in
//     the rootfs — fixed by the tmpfs /dev in the runc build spec.
//   - glob COPY: `COPY pkg.json lock* rc ./` must expand `lock*` like Docker.
//   - COPY --from=<stage> across the cascade, with --chown.
//   - a four-stage chain where the final image inherits from `base` again.
//
// Pre-fix this failed at stage 2 ("cp: cannot open .../dev/null: Permission
// denied"); the glob bug then surfaced at the COPY; this proves all of it green
// on the path that actually runs on a developer laptop (uid != 0 → runc).
func TestRuncStepRunner_E2E_NextjsShape(t *testing.T) {
	skipUnlessRuncE2E(t)

	cache := t.TempDir()
	work := t.TempDir()
	ctx := t.TempDir()

	// A build context mirroring a Next.js repo's root files. lock* tests the glob;
	// the rest are COPY'd individually and via `COPY . .`.
	for _, f := range []string{"package.json", "pnpm-lock.yaml", ".npmrc", "next.config.mjs"} {
		if err := os.WriteFile(filepath.Join(ctx, f), []byte("// "+f+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(ctx, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ctx, "src", "index.ts"), []byte("export {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	const df = `FROM alpine:3.20 AS base
RUN apk add --no-cache curl

FROM base AS deps
WORKDIR /app
RUN apk add --no-cache git
COPY package.json pnpm-lock.yaml* .npmrc ./
RUN mkdir -p /app/node_modules && echo dep > /app/node_modules/marker

FROM base AS builder
WORKDIR /app
COPY --from=deps /app/node_modules ./node_modules
COPY . .
RUN echo "built" > /app/dist-marker && cat next.config.mjs > /app/seen-config
RUN mkdir -p /app/standalone/.next/server && echo srv > /app/standalone/.next/server/page.js && echo BUILD > /app/standalone/.next/BUILD_ID

FROM base
WORKDIR /app
RUN addgroup --system app && adduser --system --ingroup app app
COPY --from=builder --chown=app:app /app/dist-marker ./dist-marker
COPY --from=builder --chown=app:app /app/node_modules ./node_modules
COPY --from=deps /app/.npmrc ./.npmrc
# The amv-shape trap: .next is pre-created and chowned to a MAPPED subuid (app),
# then a COPY --from writes a DIRECTORY into that pre-chowned dir. Outside the
# build userns the cp cannot create files under the subuid-owned dir nor preserve
# its times ("cp: cannot create directory .../.next/server: Permission denied").
RUN mkdir .next && chown app:app .next
COPY --from=builder --chown=app:app /app/standalone/.next ./.next
`
	parsed, err := parse.Parse(df)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	res, err := Build(context.Background(), cache, work, parsed,
		Options{ContextDir: ctx, Progress: os.Stderr},
		runnerbuild.RuncStepRunner{CacheRoot: cache, Stdout: os.Stderr, Stderr: os.Stderr})
	if err != nil {
		t.Fatalf("Next.js-shape multi-stage build via runc: %v", err)
	}

	// The final stage (FROM base again) must hold everything carried across,
	// including the COPY --from that writes a directory INTO the pre-chowned .next
	// (the exact instruction that failed in amv with "cp: cannot create ...
	// .next/server: Permission denied").
	for _, want := range []string{
		"dist-marker",          // COPY --from=builder
		"node_modules/marker",  // dep made in `deps`, carried builder→runner
		".npmrc",               // COPY --from=deps (glob-copied into deps first)
		".next/server/page.js", // dir copied INTO the subuid-owned .next
		".next/BUILD_ID",       // ditto
	} {
		if _, err := os.Stat(filepath.Join(res.Rootfs, "app", want)); err != nil {
			t.Errorf("final rootfs missing /app/%s: %v", want, err)
		}
	}
	// The --chown landed: dist-marker must be owned by the `app` user the stage
	// created (uid is whatever adduser picked; assert it is NOT root, i.e. chown
	// took effect across the stage copy).
	fi, err := os.Stat(filepath.Join(res.Rootfs, "app", "dist-marker"))
	if err == nil {
		if sysStat, ok := fi.Sys().(*syscall.Stat_t); ok && sysStat.Uid == 0 {
			t.Errorf("dist-marker owned by uid 0 — COPY --chown=app:app did not apply")
		}
	}
	// /dev must not carry leftover device-node mountpoints from any stage's RUN.
	if ents, _ := os.ReadDir(filepath.Join(res.Rootfs, "dev")); len(ents) > 0 {
		var names []string
		for _, e := range ents {
			names = append(names, e.Name())
		}
		t.Errorf("final /dev not empty (leftover nodes from a RUN): %v", names)
	}
}

// TestRuncStepRunner_E2E_Network exercises a RUN that needs egress + DNS:
// `apk add` reaches the alpine CDN through the host netns and the bound
// resolv.conf, and installs a package that chowns into the subuid range.
func TestRuncStepRunner_E2E_Network(t *testing.T) {
	skipUnlessRuncE2E(t)
	cache := t.TempDir()
	work := t.TempDir()
	df, err := parse.Parse("FROM alpine:3.20\nRUN apk add --no-cache git && git --version > /git.txt\n")
	if err != nil {
		t.Fatal(err)
	}
	res, err := Build(context.Background(), cache, work, df,
		Options{ContextDir: t.TempDir(), Progress: os.Stderr},
		runnerbuild.RuncStepRunner{CacheRoot: cache, Stdout: os.Stderr, Stderr: os.Stderr})
	if err != nil {
		t.Fatalf("network build via runc: %v", err)
	}
	if _, err := os.ReadFile(filepath.Join(res.Rootfs, "git.txt")); err != nil {
		t.Fatalf("apk add git did not work (no egress/DNS?): %v", err)
	}
}

// TestRuncStepRunner_E2E_NetnsIsolation is the step-2 security proof: with an
// isolated netns + slirp, a build can reach the public internet but the cloud
// metadata endpoint (169.254.169.254) is blackholed. A single RUN asserts both,
// so the test fails if egress is broken OR if the blackhole is not in effect.
func TestRuncStepRunner_E2E_NetnsIsolation(t *testing.T) {
	skipUnlessRuncE2E(t)
	cache := t.TempDir()
	work := t.TempDir()
	slirpBin, err := embed.ExtractSlirp(cache)
	if err != nil {
		t.Fatalf("extract slirp: %v", err)
	}

	// IMDS must FAIL fast (-T 2 = 2s timeout): the blackhole route drops it, so
	// wget can't even open the connection. A public host must SUCCEED. The RUN
	// inverts the IMDS check (`|| echo blocked`) so a blocked IMDS is the pass.
	df, err := parse.Parse("FROM alpine:3.20\n" +
		"RUN wget -q -O- -T 5 http://example.com > /dev/null && echo egress-ok > /egress.txt\n" +
		"RUN if wget -q -O- -T 2 http://169.254.169.254/ > /dev/null 2>&1; then echo IMDS-REACHABLE > /imds.txt; else echo imds-blocked > /imds.txt; fi\n")
	if err != nil {
		t.Fatal(err)
	}
	res, err := Build(context.Background(), cache, work, df,
		Options{ContextDir: t.TempDir(), Progress: os.Stderr},
		runnerbuild.RuncStepRunner{CacheRoot: cache, SlirpBin: slirpBin, Stdout: os.Stderr, Stderr: os.Stderr})
	if err != nil {
		t.Fatalf("isolated-netns build via runc: %v", err)
	}

	egress, err := os.ReadFile(filepath.Join(res.Rootfs, "egress.txt"))
	if err != nil || string(egress) != "egress-ok\n" {
		t.Fatalf("public egress failed through slirp (egress.txt=%q err=%v)", string(egress), err)
	}
	imds, err := os.ReadFile(filepath.Join(res.Rootfs, "imds.txt"))
	if err != nil {
		t.Fatalf("imds.txt missing: %v", err)
	}
	if string(imds) != "imds-blocked\n" {
		t.Errorf("IMDS 169.254.169.254 was REACHABLE (%q) — blackhole not in effect", string(imds))
	}
}

// TestRuncStepRunner_E2E_PidsLimit is the step-3 proof that the cgroup is
// actually applied: with PidsMax=64 a RUN that tries to spawn far more
// concurrent processes must fail (fork hits pids.max), while the same RUN
// without a limit succeeds. Host-net mode (no SlirpBin) isolates the cgroup
// effect from networking. Needs cgroups v2 delegation (cpu/memory/pids) — skip
// if the controller isn't delegated.
func TestRuncStepRunner_E2E_PidsLimit(t *testing.T) {
	skipUnlessRuncE2E(t)
	if ok, reason := ociproc.CgroupsV2Delegated(ociproc.Limits{PidsMax: 1}); !ok {
		integtest.Demand(t, "pids controller not delegated: "+reason)
		t.Skipf("pids controller not delegated: %s", reason)
	}

	// Spawn 200 background sleeps; with pids.max≈64 the shell can't fork them
	// all and the RUN aborts non-zero. `set -e` + the exit-file wrapper make
	// that a build failure.
	forkBomb := "i=0; while [ $i -lt 200 ]; do sleep 5 & i=$((i+1)); done; wait"

	build := func(t *testing.T, limits ociproc.Limits) error {
		cache := t.TempDir()
		work := t.TempDir()
		df, err := parse.Parse("FROM alpine:3.20\nRUN " + forkBomb + "\n")
		if err != nil {
			t.Fatal(err)
		}
		_, err = Build(context.Background(), cache, work, df,
			Options{ContextDir: t.TempDir(), Progress: os.Stderr},
			runnerbuild.RuncStepRunner{
				CacheRoot:     cache,
				Stdout:        os.Stderr,
				Stderr:        os.Stderr,
				Limits:        limits,
				RequireLimits: true, // fail rather than silently run unbounded
			})
		return err
	}

	// Unbounded: the fork storm should succeed.
	if err := build(t, ociproc.Limits{}); err != nil {
		t.Fatalf("unbounded build should succeed, got: %v", err)
	}

	// Bounded at 64 pids: the same RUN must fail (can't fork 200).
	if err := build(t, ociproc.Limits{PidsMax: 64}); err == nil {
		t.Error("build with PidsMax=64 should fail the fork storm, but it succeeded — pids.max not applied")
	}
}
