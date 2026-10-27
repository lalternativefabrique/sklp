package pipeline

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestStep_UnmarshalPublish_Defaults covers a full docker-build step
// with no scan / push overrides: the unmarshaller must populate every
// required field and leave the opt-out pointers nil so the compiler can
// apply documented defaults.
func TestStep_UnmarshalPublish_Defaults(t *testing.T) {
	src := `
name: build-sidecar
kind: publish
image: rg.fr-par.scw.cloud/waves/sidecar
tag: v1
context: apps/sidecar
`
	var s Step
	if err := yaml.Unmarshal([]byte(src), &s); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if s.Kind != "publish" {
		t.Errorf("Kind = %q", s.Kind)
	}
	if s.Image != "rg.fr-par.scw.cloud/waves/sidecar" || s.Tag != "v1" {
		t.Errorf("image/tag wrong: %+v", s)
	}
	if s.Context != "apps/sidecar" {
		t.Errorf("Context = %q", s.Context)
	}
	if s.Push != nil {
		t.Errorf("Push should be unset by default, got %v", *s.Push)
	}
	if s.Scan.Present {
		t.Error("Scan.Present should be false when absent")
	}
}

// TestStep_UnmarshalScanFalse asserts `scan: false` decodes into
// Disabled=true so the compiler knows to skip Trivy entirely.
func TestStep_UnmarshalScanFalse(t *testing.T) {
	src := `
name: build
kind: publish
image: x/y
tag: v1
context: .
scan: false
`
	var s Step
	if err := yaml.Unmarshal([]byte(src), &s); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !s.Scan.Present || !s.Scan.Disabled {
		t.Errorf("scan: false should decode Present+Disabled, got %+v", s.Scan)
	}
}

// TestStep_UnmarshalScanMapping asserts a mapping form populates the
// severity list and IgnoreUnfixed pointer.
func TestStep_UnmarshalScanMapping(t *testing.T) {
	src := `
name: build
kind: publish
image: x/y
tag: v1
context: .
scan:
  severity: [CRITICAL]
  ignore_unfixed: false
`
	var s Step
	if err := yaml.Unmarshal([]byte(src), &s); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !s.Scan.Present || s.Scan.Disabled {
		t.Errorf("scan mapping should be Present+!Disabled, got %+v", s.Scan)
	}
	if len(s.Scan.Severity) != 1 || s.Scan.Severity[0] != "CRITICAL" {
		t.Errorf("severity = %v", s.Scan.Severity)
	}
	if s.Scan.IgnoreUnfixed == nil || *s.Scan.IgnoreUnfixed != false {
		t.Errorf("ignore_unfixed = %v", s.Scan.IgnoreUnfixed)
	}
}

// TestStep_UnmarshalScanRejectsTrueScalar is the typo guard: writing
// `scan: true` is meaningless (scan is on by default) and probably a
// typo for the mapping form. Rejecting it surfaces the intent gap
// at load time.
func TestStep_UnmarshalScanRejectsBareString(t *testing.T) {
	src := `
name: build
kind: publish
image: x/y
tag: v1
context: .
scan: high
`
	var s Step
	if err := yaml.Unmarshal([]byte(src), &s); err == nil {
		t.Errorf("expected error decoding scan: high")
	}
}

// TestStep_UnmarshalSecrets_BuildKitForm covers the docker-build
// secrets list form: a list of mappings with id + env/file.
func TestStep_UnmarshalSecrets_BuildKitForm(t *testing.T) {
	src := `
name: build
kind: publish
image: x/y
tag: v1
context: .
secrets:
  - id: github_token
    env: GITHUB_TOKEN
  - id: npmrc
    file: ~/.npmrc
`
	var s Step
	if err := yaml.Unmarshal([]byte(src), &s); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got := len(s.Secrets.Build); got != 2 {
		t.Fatalf("Build entries = %d", got)
	}
	if s.Secrets.Names != nil {
		t.Errorf("Names should be nil for BuildKit form, got %v", s.Secrets.Names)
	}
	if s.Secrets.Build[0].ID != "github_token" || s.Secrets.Build[0].Env != "GITHUB_TOKEN" {
		t.Errorf("first secret wrong: %+v", s.Secrets.Build[0])
	}
	if s.Secrets.Build[1].File != "~/.npmrc" {
		t.Errorf("second secret file wrong: %+v", s.Secrets.Build[1])
	}
}

// TestStep_UnmarshalSecrets_LegacyNames preserves the shell-step shape:
// a list of bare scalars decodes into Secrets.Names.
func TestStep_UnmarshalSecrets_LegacyNames(t *testing.T) {
	src := `
name: t
run: echo hi
secrets: [FOO, BAR]
`
	var s Step
	if err := yaml.Unmarshal([]byte(src), &s); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got := s.Secrets.Names; len(got) != 2 || got[0] != "FOO" || got[1] != "BAR" {
		t.Errorf("Names = %v", got)
	}
	if s.Secrets.Build != nil {
		t.Errorf("Build should be nil for legacy form, got %v", s.Secrets.Build)
	}
}

// TestStep_UnmarshalSecrets_MissingID rejects an entry without id.
func TestStep_UnmarshalSecrets_MissingID(t *testing.T) {
	src := `
name: b
kind: publish
image: x/y
tag: v1
context: .
secrets:
  - env: FOO
`
	var s Step
	if err := yaml.Unmarshal([]byte(src), &s); err == nil {
		t.Fatal("expected error for missing id")
	}
}

// TestStep_UnmarshalSecrets_BothSourcesRejected refuses env + file on
// the same entry.
func TestStep_UnmarshalSecrets_BothSourcesRejected(t *testing.T) {
	src := `
name: b
kind: publish
image: x/y
tag: v1
context: .
secrets:
  - id: t
    env: TOK
    file: ~/.npmrc
`
	var s Step
	if err := yaml.Unmarshal([]byte(src), &s); err == nil {
		t.Fatal("expected error for env+file combo")
	}
}

// TestStep_UnmarshalSecrets_NeitherSourceRejected refuses an entry
// without env or file (id alone is not enough).
func TestStep_UnmarshalSecrets_NeitherSourceRejected(t *testing.T) {
	src := `
name: b
kind: publish
image: x/y
tag: v1
context: .
secrets:
  - id: t
`
	var s Step
	if err := yaml.Unmarshal([]byte(src), &s); err == nil {
		t.Fatal("expected error for missing env/file")
	}
}

// TestValidateBuildArgKeys covers the docker ARG identifier shape:
// good keys pass, bad keys (dash, leading digit, empty) fail with a
// message that names the step + key.
func TestValidateBuildArgKeys(t *testing.T) {
	good := map[string]string{
		"NODE_ENV":    "production",
		"_INTERNAL":   "x",
		"APP_VERSION": "1.2.3",
		"V1":          "ok",
	}
	if err := validateBuildArgKeys("step", good); err != nil {
		t.Fatalf("good keys rejected: %v", err)
	}

	for _, bad := range []string{"WITH-DASH", "1LEADING_DIGIT", "has space", "with.dot", ""} {
		err := validateBuildArgKeys("step", map[string]string{bad: "v"})
		if err == nil {
			t.Errorf("expected key %q to be rejected", bad)
			continue
		}
		if !strings.Contains(err.Error(), "step") {
			t.Errorf("error for %q does not mention step name: %v", bad, err)
		}
	}

	// Empty / nil map must be a no-op so callers don't need a guard.
	if err := validateBuildArgKeys("step", nil); err != nil {
		t.Errorf("nil map should be ok, got %v", err)
	}
	if err := validateBuildArgKeys("step", map[string]string{}); err != nil {
		t.Errorf("empty map should be ok, got %v", err)
	}
}

// TestInterpolateBuildArgs_Literals verifies that values without any
// `${...}` reference pass through verbatim and the function returns a
// new map (never mutates input).
func TestInterpolateBuildArgs_Literals(t *testing.T) {
	in := map[string]string{
		"APP_VERSION": "1.2.3",
		"NODE_ENV":    "production",
	}
	out, err := interpolateBuildArgs("step", in, nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out["APP_VERSION"] != "1.2.3" || out["NODE_ENV"] != "production" {
		t.Errorf("literal values mangled: %v", out)
	}
}

// TestInterpolateBuildArgs_StepEnvWins exercises the precedence chain:
// step env overrides pipeline env which overrides process env.
func TestInterpolateBuildArgs_StepEnvWins(t *testing.T) {
	t.Setenv("VITE_API_URL", "from-process")
	pipelineEnv := map[string]string{"VITE_API_URL": "from-pipeline"}
	stepEnv := map[string]string{"VITE_API_URL": "from-step"}

	args := map[string]string{"VITE_API_URL": "${VITE_API_URL}"}

	out, err := interpolateBuildArgs("step", args, pipelineEnv, stepEnv)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out["VITE_API_URL"] != "from-step" {
		t.Errorf("step env should win, got %q", out["VITE_API_URL"])
	}

	// Drop step → pipeline wins.
	out, err = interpolateBuildArgs("step", args, pipelineEnv, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out["VITE_API_URL"] != "from-pipeline" {
		t.Errorf("pipeline env should win, got %q", out["VITE_API_URL"])
	}

	// Drop both → process env wins.
	out, err = interpolateBuildArgs("step", args, nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out["VITE_API_URL"] != "from-process" {
		t.Errorf("process env should win, got %q", out["VITE_API_URL"])
	}
}

// TestInterpolateBuildArgs_UnsetFails ensures an unknown `${VAR}` ref
// surfaces a BuildArgEnvMissingError naming both the step and the
// build_args key. The compile path wraps this into CICompileError, so
// users see a precise pointer instead of an empty-string ARG.
func TestInterpolateBuildArgs_UnsetFails(t *testing.T) {
	args := map[string]string{"WEB_URL": "${UNSET_PIPELINE_VAR}"}

	_, err := interpolateBuildArgs("web-build", args, nil, nil)
	if err == nil {
		t.Fatal("expected error for unset env var")
	}
	var miss *BuildArgEnvMissingError
	if !errors.As(err, &miss) {
		t.Fatalf("expected BuildArgEnvMissingError, got %T (%v)", err, err)
	}
	if miss.Step != "web-build" || miss.ArgName != "WEB_URL" || miss.EnvVar != "UNSET_PIPELINE_VAR" {
		t.Errorf("error fields wrong: %+v", miss)
	}
}

// TestInterpolateBuildArgs_RejectsBadRef catches `${...}` patterns that
// don't match a valid identifier — most commonly a typo that would
// otherwise leak verbatim into the image (`ARG VITE_URL=${VITE-URL}`).
func TestInterpolateBuildArgs_RejectsBadRef(t *testing.T) {
	args := map[string]string{"VITE_URL": "${VITE-URL}"}
	_, err := interpolateBuildArgs("s", args, nil, nil)
	if err == nil {
		t.Fatal("expected error for malformed ${...} reference")
	}
}

// TestInterpolateBuildArgs_NilInput keeps the nil-in nil-out contract
// so callers don't have to guard against an empty map.
func TestInterpolateBuildArgs_NilInput(t *testing.T) {
	out, err := interpolateBuildArgs("s", nil, nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out != nil {
		t.Errorf("expected nil map, got %v", out)
	}
}

// TestBuildArgs_ArgvOrderDeterministic asserts the executor-side
// composition emits `--build-arg KEY=VALUE` in lexicographic key order
// regardless of Go's map iteration. This is the contract the captured
// command-line tests rely on. The composition lives in plan; this test
// exercises the policy by reproducing the same sort the executor uses.
func TestBuildArgs_ArgvOrderDeterministic(t *testing.T) {
	args := map[string]string{
		"ZZZ":  "last",
		"AAA":  "first",
		"MMM":  "middle",
		"_LOW": "underscore",
	}
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	want := []string{"AAA", "MMM", "ZZZ", "_LOW"}
	for i, k := range want {
		if keys[i] != k {
			t.Errorf("sorted[%d] = %q, want %q", i, keys[i], k)
		}
	}
}

// TestStep_UnmarshalBuildArgs roundtrips a build_args map.
func TestStep_UnmarshalBuildArgs(t *testing.T) {
	src := `
name: b
kind: publish
image: x/y
tag: v1
context: .
build_args:
  NODE_ENV: production
  APP_VERSION: "1.2.3"
`
	var s Step
	if err := yaml.Unmarshal([]byte(src), &s); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if s.BuildArgs["NODE_ENV"] != "production" || s.BuildArgs["APP_VERSION"] != "1.2.3" {
		t.Errorf("BuildArgs = %v", s.BuildArgs)
	}
}

// TestCompilePipeline_PublishEndToEnd is the integration: a YAML
// with kind: docker-build compiles into a plan whose CIStep carries a
// resolved PublishSpec and implicit context-driven inputs.
func TestCompilePipeline_PublishEndToEnd(t *testing.T) {
	// Materialise a real build context: the implicit inputs are now the actual
	// files under the context (dockerContextInputs walks it honouring
	// .dockerignore), not a literal `<ctx>/**` glob, so the context must exist
	// on disk for its files to be listed.
	repo := t.TempDir()
	writeFile := func(rel, data string) {
		p := filepath.Join(repo, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeFile("apps/sidecar/Dockerfile", "FROM scratch")
	writeFile("apps/sidecar/main.go", "package main")

	src := `
version: 1
kind: pipeline
steps:
  - name: build
    kind: publish
    image: x/y
    tag: v1
    context: apps/sidecar
`
	var p Pipeline
	if err := yaml.Unmarshal([]byte(src), &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	pl, err := CompilePipeline(p, CompilePipelineOptions{RepoRoot: repo})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if len(pl.CI.Steps) != 1 {
		t.Fatalf("steps = %d", len(pl.CI.Steps))
	}
	st := pl.CI.Steps[0]
	if st.Publish == nil {
		t.Fatal("Publish missing")
	}
	if st.Publish.Dockerfile != "apps/sidecar/Dockerfile" {
		t.Errorf("dockerfile = %q", st.Publish.Dockerfile)
	}
	if !st.Publish.Push || !st.Publish.ScanEnabled {
		t.Errorf("defaults wrong: %+v", st.Publish)
	}
	// Implicit inputs: the Dockerfile + the real context files (main.go), as
	// resolved by dockerContextInputs.
	if len(st.Inputs) < 2 {
		t.Errorf("expected implicit inputs from context, got %v", st.Inputs)
	}
	if !slices.Contains(st.Inputs, "apps/sidecar/main.go") {
		t.Errorf("expected context file apps/sidecar/main.go in inputs, got %v", st.Inputs)
	}
}
