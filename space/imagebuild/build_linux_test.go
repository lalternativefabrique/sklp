//go:build linux

package imagebuild

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lalternativefabrique/sklp/internal/integtest"
	parse "github.com/lalternativefabrique/sklp/pipeline/imagebuild"
	"github.com/lalternativefabrique/sklp/space/runnerbuild"
)

func skipUnlessBuildable(t *testing.T) { integtest.SkipUnlessRootlessBuild(t) }

// TestBuild_MultiStageCopyFrom proves the runtime builder: a build stage creates
// an artifact, the final stage COPY --from carries it across, and the final
// rootfs holds it — plus the OCI config (ENV/WORKDIR/ENTRYPOINT) is captured.
// Needs rootless userns + network (alpine pull); skipped otherwise / in -short.
func TestBuild_MultiStageCopyFrom(t *testing.T) {
	skipUnlessBuildable(t)
	if testing.Short() {
		t.Skip("pulls alpine + runs apk; skipped in -short")
	}

	const df = `FROM alpine:3.20 AS build
RUN mkdir -p /out && echo artifact > /out/x
FROM alpine:3.20
RUN mkdir -p /app
COPY --from=build /out/x /app/x
ENV K=v
WORKDIR /app
ENTRYPOINT ["/bin/cat", "/app/x"]
`
	parsed, err := parse.Parse(df)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	cache := t.TempDir()
	work := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	res, err := Build(ctx, cache, work, parsed, Options{ContextDir: t.TempDir()}, runnerbuild.UnshareStepRunner{})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	// Artifact carried by COPY --from.
	b, err := os.ReadFile(filepath.Join(res.Rootfs, "app", "x"))
	if err != nil {
		t.Fatalf("COPY --from artifact missing: %v", err)
	}
	if string(b) != "artifact\n" {
		t.Errorf("artifact = %q, want %q", b, "artifact\n")
	}
	// Config captured from the final stage.
	if res.Workdir != "/app" {
		t.Errorf("workdir = %q, want /app", res.Workdir)
	}
	if len(res.Entrypoint) != 2 || res.Entrypoint[0] != "/bin/cat" {
		t.Errorf("entrypoint = %v", res.Entrypoint)
	}
	// Env merges the base image env (alpine ships PATH) with the Dockerfile ENV.
	if !hasEnv(res.Env, "K", "v") {
		t.Errorf("env = %v, want it to contain K=v", res.Env)
	}
	if !hasEnvKey(res.Env, "PATH") {
		t.Errorf("env = %v, want it to inherit the base PATH", res.Env)
	}
	// The build stage's artifact must NOT be in the final rootfs at /out
	// (it lived only in stage 0).
	if _, err := os.Stat(filepath.Join(res.Rootfs, "out", "x")); err == nil {
		t.Error("build-stage /out leaked into the final image")
	}
}

// TestBuild_StageWithRunThenFromStage reproduces the amv multi-stage failure:
// a stage that RUNs (e.g. `apk add`) leaves /dev mountpoints behind, and a later
// `FROM <stage>` copies that rootfs (copyTree → cp -a, real uid). Pre-fix, the
// orphaned 0000-mode dev/null was unreadable and cp -a aborted with EACCES.
// The fix tears /dev down after the RUN; the copy now succeeds and /dev is empty.
func TestBuild_StageWithRunThenFromStage(t *testing.T) {
	skipUnlessBuildable(t)
	if testing.Short() {
		t.Skip("pulls alpine + runs apk; skipped in -short")
	}

	const df = `FROM alpine:3.20 AS base
RUN echo hi > /etc/marker
FROM base AS deps
RUN test -f /etc/marker
FROM base
COPY --from=deps /etc/marker /etc/marker
`
	parsed, err := parse.Parse(df)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	cache := t.TempDir()
	work := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	res, err := Build(ctx, cache, work, parsed, Options{ContextDir: t.TempDir()}, runnerbuild.UnshareStepRunner{})
	if err != nil {
		t.Fatalf("Build (FROM <stage> after RUN): %v", err)
	}
	// The RUN-produced file survived the stage copy.
	if _, err := os.Stat(filepath.Join(res.Rootfs, "etc", "marker")); err != nil {
		t.Fatalf("marker missing in final rootfs: %v", err)
	}
	// /dev must hold no leftover device-node mountpoints from the RUN (the runtime
	// rebuilds /dev at create time). A non-empty /dev here is the bug's signature.
	ents, _ := os.ReadDir(filepath.Join(res.Rootfs, "dev"))
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), "null") || strings.HasPrefix(e.Name(), "zero") ||
			strings.HasPrefix(e.Name(), "random") || strings.HasPrefix(e.Name(), "urandom") ||
			strings.HasPrefix(e.Name(), "full") || strings.HasPrefix(e.Name(), "tty") {
			t.Errorf("orphaned /dev mountpoint left in rootfs: %s", e.Name())
		}
	}
}

// TestBuild_InheritsBaseConfig proves the native builder inherits the base
// image's default CMD/Entrypoint/Env the way `docker build` does when the
// Dockerfile omits them. alpine:3.20 ships Cmd ["/bin/sh"] and no entrypoint.
func TestBuild_InheritsBaseConfig(t *testing.T) {
	skipUnlessBuildable(t)
	if testing.Short() {
		t.Skip("pulls alpine; skipped in -short")
	}

	const df = `FROM alpine:3.20
RUN true
`
	parsed, err := parse.Parse(df)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	cache := t.TempDir()
	work := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	res, err := Build(ctx, cache, work, parsed, Options{ContextDir: t.TempDir()}, runnerbuild.UnshareStepRunner{})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	// No CMD in the Dockerfile => inherit alpine's default ["/bin/sh"].
	if len(res.Cmd) != 1 || res.Cmd[0] != "/bin/sh" {
		t.Errorf("cmd = %v, want [/bin/sh] inherited from base", res.Cmd)
	}
	if !hasEnvKey(res.Env, "PATH") {
		t.Errorf("env = %v, want inherited base PATH", res.Env)
	}
}

// TestBuild_EntrypointResetsInheritedCmd proves Docker semantics: declaring an
// ENTRYPOINT clears the base image's inherited CMD (no default args carry to a
// new entrypoint) unless the Dockerfile re-sets CMD.
func TestBuild_EntrypointResetsInheritedCmd(t *testing.T) {
	skipUnlessBuildable(t)
	if testing.Short() {
		t.Skip("pulls alpine; skipped in -short")
	}

	const df = `FROM alpine:3.20
ENTRYPOINT ["/bin/echo"]
`
	parsed, err := parse.Parse(df)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	cache := t.TempDir()
	work := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	res, err := Build(ctx, cache, work, parsed, Options{ContextDir: t.TempDir()}, runnerbuild.UnshareStepRunner{})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(res.Entrypoint) != 1 || res.Entrypoint[0] != "/bin/echo" {
		t.Errorf("entrypoint = %v, want [/bin/echo]", res.Entrypoint)
	}
	if res.Cmd != nil {
		t.Errorf("cmd = %v, want nil (ENTRYPOINT resets inherited CMD)", res.Cmd)
	}
}

func hasEnvKey(env []string, key string) bool {
	for _, kv := range env {
		if k, _, _ := strings.Cut(kv, "="); k == key {
			return true
		}
	}
	return false
}

func hasEnv(env []string, key, val string) bool {
	for _, kv := range env {
		if k, v, _ := strings.Cut(kv, "="); k == key && v == val {
			return true
		}
	}
	return false
}

func TestFromExpandsGlobalArgWithBuildArgOverride(t *testing.T) {
	df, err := parse.Parse("ARG BASE_IMAGE=postgres:17-alpine\nFROM ${BASE_IMAGE}\nRUN true\n")
	if err != nil {
		t.Fatal(err)
	}
	def := resolveGlobalArgs(df.GlobalArgs, nil)
	if got := parse.Expand(df.Stages[0].BaseImage, def); got != "postgres:17-alpine" {
		t.Fatalf("default: %q", got)
	}
	over := resolveGlobalArgs(df.GlobalArgs, map[string]string{"BASE_IMAGE": "pgvector/pgvector:pg17"})
	if got := parse.Expand(df.Stages[0].BaseImage, over); got != "pgvector/pgvector:pg17" {
		t.Fatalf("override: %q", got)
	}
}
