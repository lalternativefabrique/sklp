//go:build linux

package imagebuild

import (
	"context"
	"strings"
	"testing"

	parse "github.com/lalternativefabrique/sklp/pipeline/imagebuild"
	"github.com/lalternativefabrique/sklp/space/runnerbuild"
)

// recordRunner is a StepRunner that records the RUN commands it is asked to
// execute and the per-stage env it was configured with via WithStage, without
// running anything. The recorded env lets us assert that Dockerfile ENV reaches
// the RUN *execution* environment (exported into the shell), not just the
// build-time ${VAR} expansion table.
type recordRunner struct {
	cmds *[]string
	env  *[]string // the imageEnv from the most recent WithStage at Exec time
}

func (r recordRunner) Exec(_ context.Context, _ string, runs []string) error {
	*r.cmds = append(*r.cmds, runs...)
	return nil
}
func (r recordRunner) WithStage(imageEnv []string, _ []runnerbuild.CacheMount) runnerbuild.StepRunner {
	*r.env = imageEnv
	return r
}

// runStageRecording parses a single-stage Dockerfile, runs only its
// instructions through runStage with a recording runner, and returns the
// recorded RUN commands + the captured Result.
func runStageRecording(t *testing.T, dockerfile string, args map[string]string) ([]string, Result) {
	cmds, _, res := runStageRecordingEnv(t, dockerfile, args, nil)
	return cmds, res
}

// runStageRecordingEnv is runStageRecording plus the base image env and the
// captured RUN execution env (the imageEnv WithStage was last called with).
func runStageRecordingEnv(t *testing.T, dockerfile string, args map[string]string, baseEnv []string) ([]string, []string, Result) {
	t.Helper()
	df, err := parse.Parse(dockerfile)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var cmds, runEnv []string
	rootfs := t.TempDir()
	res := Result{Rootfs: rootfs}
	globalArgs := resolveGlobalArgs(df.GlobalArgs, args)
	opts := Options{BuildArgs: args}
	if err := runStage(context.Background(), df, []string{rootfs}, len(df.Stages)-1,
		rootfs, opts, recordRunner{cmds: &cmds, env: &runEnv}, baseEnv, globalArgs, &res); err != nil {
		t.Fatalf("runStage: %v", err)
	}
	return cmds, runEnv, res
}

func assertRunContains(t *testing.T, cmds []string, want string) {
	t.Helper()
	for _, c := range cmds {
		if strings.Contains(c, want) {
			return
		}
	}
	t.Fatalf("no RUN command contains %q; got %v", want, cmds)
}

// TestRunStage_ArgDefaultExpandedIntoRun is the regression for the
// golang-migrate clone bug: `--branch ${MIGRATE_VERSION}` with a default ARG
// must reach the runner as `--branch v4.19.1`, not `--branch ` (empty).
func TestRunStage_ArgDefaultExpandedIntoRun(t *testing.T) {
	cmds, _ := runStageRecording(t, "FROM scratch\n"+
		"ARG MIGRATE_VERSION=v4.19.1\n"+
		"RUN git clone --branch ${MIGRATE_VERSION} https://example.com/r .\n", nil)
	assertRunContains(t, cmds, "--branch v4.19.1 https://example.com/r .")
}

func TestRunStage_BuildArgOverridesDefault(t *testing.T) {
	cmds, _ := runStageRecording(t, "FROM scratch\n"+
		"ARG MIGRATE_VERSION=v4.19.1\n"+
		"RUN echo ${MIGRATE_VERSION}\n", map[string]string{"MIGRATE_VERSION": "v9.9.9"})
	assertRunContains(t, cmds, "echo v9.9.9")
}

// A bare in-stage ARG pulls the value from the pre-FROM global scope.
func TestRunStage_GlobalArgVisibleAfterRedeclare(t *testing.T) {
	cmds, _ := runStageRecording(t, "ARG V=1.2.3\n"+
		"FROM scratch\n"+
		"ARG V\n"+
		"RUN echo ${V}\n", nil)
	assertRunContains(t, cmds, "echo 1.2.3")
}

// Without the in-stage redeclaration the global ARG is NOT visible (Docker
// semantics): ${V} expands to empty.
func TestRunStage_GlobalArgHiddenWithoutRedeclare(t *testing.T) {
	cmds, _ := runStageRecording(t, "ARG V=1.2.3\n"+
		"FROM scratch\n"+
		"RUN echo [${V}]\n", nil)
	assertRunContains(t, cmds, "echo []")
}

// ENV values join the variable table for later instructions.
func TestRunStage_EnvExpandedIntoLaterRun(t *testing.T) {
	cmds, res := runStageRecording(t, "FROM scratch\n"+
		"ENV APP_DIR=/srv/app\n"+
		"RUN mkdir -p ${APP_DIR}\n", nil)
	assertRunContains(t, cmds, "mkdir -p /srv/app")
	if got := envValue(res.Env, "APP_DIR"); got != "/srv/app" {
		t.Errorf("ENV APP_DIR = %q, want /srv/app", got)
	}
}

// TestRunStage_EnvExportedIntoRunShell is the regression for the native-builder
// ENV→RUN bug: a Dockerfile ENV must be present in the RUN *execution* env, not
// only in the build-time expansion table. This mirrors www's VITE_* set from ARG
// before `pnpm run build` — Vite reads them from the process env, so expansion
// alone is not enough.
func TestRunStage_EnvExportedIntoRunShell(t *testing.T) {
	_, runEnv, _ := runStageRecordingEnv(t, "FROM scratch\n"+
		"ARG VITE_API_URL=https://api.example.dev\n"+
		"ENV VITE_API_URL=$VITE_API_URL\n"+
		"RUN pnpm run build\n", nil, nil)
	if got := envValue(runEnv, "VITE_API_URL"); got != "https://api.example.dev" {
		t.Errorf("RUN env VITE_API_URL = %q, want https://api.example.dev", got)
	}
}

// www declares all four VITE_* in a single backslash-continued ENV (one OpEnv
// with a multi-key args string). Each key must reach the RUN env, not collapse
// into the first pair's value.
func TestRunStage_MultiKeyEnvExportedIntoRunShell(t *testing.T) {
	_, runEnv, _ := runStageRecordingEnv(t, "FROM scratch\n"+
		"ARG VITE_API_URL=https://api.example.dev\n"+
		"ARG VITE_PROJECT_ID=p123\n"+
		"ENV VITE_API_URL=$VITE_API_URL VITE_PROJECT_ID=$VITE_PROJECT_ID\n"+
		"RUN pnpm run build\n", nil, nil)
	if got := envValue(runEnv, "VITE_API_URL"); got != "https://api.example.dev" {
		t.Errorf("RUN env VITE_API_URL = %q", got)
	}
	if got := envValue(runEnv, "VITE_PROJECT_ID"); got != "p123" {
		t.Errorf("RUN env VITE_PROJECT_ID = %q, want p123", got)
	}
}

// The base image env (PATH etc.) must survive the merge so `go`/`node`/`pnpm`
// still resolve when a stage also declares ENV.
func TestRunStage_BaseEnvPreservedWithDockerfileEnv(t *testing.T) {
	base := []string{"PATH=/usr/local/go/bin:/usr/bin"}
	_, runEnv, _ := runStageRecordingEnv(t, "FROM scratch\n"+
		"ENV APP=foo\n"+
		"RUN go build\n", nil, base)
	if got := envValue(runEnv, "PATH"); got != "/usr/local/go/bin:/usr/bin" {
		t.Errorf("RUN env PATH = %q, want base image PATH preserved", got)
	}
	if got := envValue(runEnv, "APP"); got != "foo" {
		t.Errorf("RUN env APP = %q, want foo", got)
	}
}

// A Dockerfile ENV shadows the same key from the base image (Docker semantics).
func TestRunStage_DockerfileEnvShadowsBase(t *testing.T) {
	base := []string{"NODE_ENV=development"}
	_, runEnv, _ := runStageRecordingEnv(t, "FROM scratch\n"+
		"ENV NODE_ENV=production\n"+
		"RUN pnpm run build\n", nil, base)
	if got := envValue(runEnv, "NODE_ENV"); got != "production" {
		t.Errorf("RUN env NODE_ENV = %q, want production (ENV shadows base)", got)
	}
}

func envValue(env []string, key string) string {
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok && k == key {
			return v
		}
	}
	return ""
}

// A RUN body may define and use its own shell variables. The builder does not
// know them and must leave the references alone: expanding them to "" turns
// `case "${arch}"` into `case ""`, which silently mis-executes instead of
// failing, and a URL built from the variable 404s.
func TestRunStage_ShellVarInRunSurvivesExpansion(t *testing.T) {
	cmds, _ := runStageRecording(t, "FROM scratch\n"+
		`RUN arch="$(dpkg --print-architecture)" && case "${arch}" in x86_64) arch=amd64 ;; esac && curl -o skt "https://example.invalid/skt-linux-${arch}"`+"\n", nil)
	assertRunContains(t, cmds, `case "${arch}" in`)
	assertRunContains(t, cmds, "skt-linux-${arch}")
}

// The unbraced $NAME form is left alone too.
func TestRunStage_UnbracedShellVarInRunSurvives(t *testing.T) {
	cmds, _ := runStageRecording(t, "FROM scratch\n"+
		"RUN d=/tmp/x && mkdir -p $d && echo $d/done\n", nil)
	assertRunContains(t, cmds, "mkdir -p $d")
	assertRunContains(t, cmds, "echo $d/done")
}

// A declared ARG still wins over the shell: same name, but the Dockerfile knows
// it, so it expands at build time as before.
func TestRunStage_DeclaredArgStillExpandsInRun(t *testing.T) {
	cmds, _ := runStageRecording(t, "FROM scratch\n"+
		"ARG arch=amd64\n"+
		"RUN echo skt-linux-${arch}\n", nil)
	assertRunContains(t, cmds, "echo skt-linux-amd64")
}

// A base-image env key is a name the builder knows about, so it is expanded at
// build time (to "", since base env does not join the expansion table) rather
// than leaking through to the shell as a literal ${BASEVAR}.
func TestRunStage_BaseEnvNameIsDeclared(t *testing.T) {
	cmds, _, _ := runStageRecordingEnv(t, "FROM scratch\n"+
		"RUN echo [${BASEVAR}]\n", nil, []string{"BASEVAR=frombase"})
	assertRunContains(t, cmds, "echo []")
}
